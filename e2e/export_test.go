package e2e

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// exportedConfig is the operator-facing shape of `unifig export` output. The
// expected values are independent literals: the demo Controller's built-in
// Default network, and networks the rig seeds through the Controller's API.
type exportedConfig struct {
	Networks []exportedNetwork `yaml:"networks"`
	WLANs    []exportedWLAN    `yaml:"wlans"`
	WAN      []exportedWANSlot `yaml:"wan"`
}

type exportedNetwork struct {
	Name   string `yaml:"name"`
	VLAN   int    `yaml:"vlan"`
	Subnet string `yaml:"subnet"`
}

type exportedWLAN struct {
	Name       string `yaml:"name"`
	Network    string `yaml:"network"`
	Passphrase string `yaml:"passphrase"`
}

// exportedWANSlot is the Setting half of the same shape. Only the WAN suite
// reads it — the dockerized Controller has no uplinks to export — and it lives
// here so that what export writes is described in one place.
type exportedWANSlot struct {
	Slot     string `yaml:"slot"`
	Type     string `yaml:"type"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// exportedYAML parses what export wrote. It lives beside the shape it parses,
// so that a test which has to know what export said — which secrets it
// redacted, which slots it wrote — reads the file rather than grepping it.
func exportedYAML(t *testing.T, exported []byte) exportedConfig {
	t.Helper()
	var cfg exportedConfig
	if err := yaml.Unmarshal(exported, &cfg); err != nil {
		t.Fatalf("export wrote something that is not YAML: %v\n%s", err, exported)
	}
	return cfg
}

func exportNetworks(t *testing.T) exportedConfig {
	t.Helper()
	res := testRig.runUnifig(t, []string{"export"}, nil)
	if res.ExitCode != 0 {
		t.Fatalf("unifig export exited %d\nstderr: %s", res.ExitCode, res.Stderr)
	}
	var cfg exportedConfig
	if err := yaml.Unmarshal(res.Stdout, &cfg); err != nil {
		t.Fatalf("stdout is not valid YAML: %v\nstdout: %s", err, res.Stdout)
	}
	t.Logf("unifig export stdout:\n%s", res.Stdout)
	return cfg
}

func TestExportPrintsTheSitesNetworksAsYAML(t *testing.T) {
	cfg := exportNetworks(t)

	for _, n := range cfg.Networks {
		if n.Name == "Default" {
			if n.Subnet != "192.168.1.1/24" {
				t.Errorf("Default network subnet = %q, want %q", n.Subnet, "192.168.1.1/24")
			}
			return
		}
	}
	t.Errorf("exported networks %v do not include the Controller's Default network", cfg.Networks)
}

func TestExportListsVLANsByNameAndLeavesWANSlotsOut(t *testing.T) {
	// A VLAN network is a Resource and must be exported; a WAN entry is a
	// Setting (fixed slot) and must not appear among networks.
	testRig.seedNetwork(t, map[string]any{
		"name": "IoT", "purpose": "corporate", "enabled": true,
		"vlan_enabled": true, "vlan": 20, "ip_subnet": "10.20.0.1/24",
	})
	testRig.seedNetwork(t, map[string]any{
		"name": "Primary (WAN1)", "purpose": "wan",
		"wan_networkgroup": "WAN", "wan_type": "dhcp",
	})

	cfg := exportNetworks(t)

	byName := map[string]exportedNetwork{}
	var names []string
	for _, n := range cfg.Networks {
		byName[n.Name] = n
		names = append(names, n.Name)
	}

	if !slices.IsSorted(names) {
		t.Errorf("exported network names %v are not in name order", names)
	}
	if _, ok := byName["Primary (WAN1)"]; ok {
		t.Errorf("exported networks %v include the WAN slot", names)
	}
	iot, ok := byName["IoT"]
	if !ok {
		t.Fatalf("exported networks %v do not include the seeded IoT VLAN", names)
	}
	if iot.VLAN != 20 || iot.Subnet != "10.20.0.1/24" {
		t.Errorf("IoT exported as vlan=%d subnet=%q, want vlan=20 subnet=%q", iot.VLAN, iot.Subnet, "10.20.0.1/24")
	}
	if def := byName["Default"]; def.VLAN != 0 {
		t.Errorf("Default network exported with vlan=%d, want no vlan", def.VLAN)
	}
}

// The brownfield adoption path is export-then-validate, so the config unifig
// generates from a real Controller has to be config unifig accepts. Nothing
// short of this proves it: a hand-written literal only tests the literal, and
// a Go-struct round-trip never sees what the Controller actually returns.
func TestExportedConfigValidates(t *testing.T) {
	testRig.seedNetwork(t, map[string]any{
		"name": "IoT", "purpose": "corporate", "enabled": true,
		"vlan_enabled": true, "vlan": 20, "ip_subnet": "10.20.0.1/24",
	})

	exported := testRig.runUnifig(t, []string{"export"}, nil)
	if exported.ExitCode != 0 {
		t.Fatalf("unifig export exited %d\nstderr: %s", exported.ExitCode, exported.Stderr)
	}

	path := filepath.Join(t.TempDir(), "unifig.yaml")
	if err := os.WriteFile(path, exported.Stdout, 0o600); err != nil {
		t.Fatalf("writing exported config: %v", err)
	}

	// No connection config: validate is offline, and export's output must not
	// need a Controller to check.
	validated := testRig.runUnifig(t, []string{"validate", path},
		map[string]string{"UNIFIG_HOST": "", "UNIFIG_API_KEY": ""})
	if validated.ExitCode != 0 {
		t.Fatalf("validate rejected export's own output (exit %d)\nstderr: %s\nexported:\n%s",
			validated.ExitCode, validated.Stderr, exported.Stdout)
	}
}

// Export promises that what it writes is config the other verbs can read, and
// two live networks sharing a name is the one live state that cannot be
// written as such config: matching is by name (ADR-0001), and the file unifig
// would emit — the same `- name: X` twice — is a file its own validate
// rejects. So export stops on the ambiguity, exactly as plan does, rather than
// handing the operator YAML that cannot be used.
func TestExportRefusesAControllerWithTwoNetworksSharingAName(t *testing.T) {
	testRig.seedNetwork(t, map[string]any{
		"name": "Export Twice", "purpose": "corporate", "enabled": true,
		"vlan_enabled": true, "vlan": 152, "ip_subnet": "10.152.0.1/24",
	})
	// seedNetwork clears same-named entries first, so the twin goes in through
	// the Controller's own API directly.
	testRig.addNetwork(t, map[string]any{
		"name": "Export Twice", "purpose": "corporate", "enabled": true,
		"vlan_enabled": true, "vlan": 252, "ip_subnet": "10.252.0.1/24",
	})
	t.Cleanup(func() { testRig.deleteNetworksNamed(t, "Export Twice") })

	res := testRig.runUnifig(t, []string{"export"}, nil)

	if res.ExitCode != 1 {
		t.Fatalf("export exited %d, want 1 — it wrote config naming one network twice\nstdout: %s",
			res.ExitCode, res.Stdout)
	}
	if len(res.Stdout) != 0 {
		t.Errorf("stdout should stay empty rather than carry unusable config, got: %s", res.Stdout)
	}
	if !strings.Contains(string(res.Stderr), "Export Twice") {
		t.Errorf("stderr should name the ambiguous network, got: %s", res.Stderr)
	}
}

// Export's output goes straight into a git repository, so the safe form is the
// one you get without asking. A file that arrived with a live passphrase in it
// has already been committed by the time anyone notices.
func TestExportRedactsPassphrasesAndNamesTheVariablesToSet(t *testing.T) {
	seedWLANOn(t, "Export Redacted", "Default", testPassphrase)

	res := testRig.runUnifig(t, []string{"export"}, nil)
	if res.ExitCode != 0 {
		t.Fatalf("unifig export exited %d\nstderr: %s", res.ExitCode, res.Stderr)
	}

	stdout := string(res.Stdout)
	if strings.Contains(stdout, testPassphrase) {
		t.Fatalf("export wrote the passphrase into the config it just told you to commit:\n%s", stdout)
	}

	const wantVar = "UNIFIG_WLAN_EXPORT_REDACTED_PASSPHRASE"
	if !strings.Contains(stdout, "${"+wantVar+"}") {
		t.Errorf("export should redact the passphrase to ${%s}, got:\n%s", wantVar, stdout)
	}
	// On stderr, so `unifig export > unifig.yaml` still tells the operator what
	// to set while leaving stdout carrying nothing but YAML.
	if !strings.Contains(string(res.Stderr), wantVar) {
		t.Errorf("export should name the variable to set, got: %s", res.Stderr)
	}
	if strings.Contains(string(res.Stderr), testPassphrase) {
		t.Errorf("export printed the passphrase it had just redacted: %s", res.Stderr)
	}
}

func TestExportWithSecretsWritesThePassphraseInline(t *testing.T) {
	seedWLANOn(t, "Export Plaintext", "Default", testPassphrase)

	res := testRig.runUnifig(t, []string{"export", "--with-secrets"}, nil)
	if res.ExitCode != 0 {
		t.Fatalf("unifig export --with-secrets exited %d\nstderr: %s", res.ExitCode, res.Stderr)
	}

	var cfg exportedConfig
	if err := yaml.Unmarshal(res.Stdout, &cfg); err != nil {
		t.Fatalf("stdout is not valid YAML: %v\nstdout: %s", err, res.Stdout)
	}
	for _, wlan := range cfg.WLANs {
		if wlan.Name != "Export Plaintext" {
			continue
		}
		if wlan.Passphrase != testPassphrase {
			t.Errorf("passphrase = %q, want the Controller's own %q", wlan.Passphrase, testPassphrase)
		}
		if wlan.Network != "Default" {
			t.Errorf("network = %q, want %q", wlan.Network, "Default")
		}
		// Nothing was redacted, so there is nothing to tell the operator to set.
		if strings.Contains(string(res.Stderr), "export UNIFIG_WLAN") {
			t.Errorf("--with-secrets should not ask for variables it did not create: %s", res.Stderr)
		}
		return
	}
	t.Errorf("exported WLANs %v do not include the seeded one", cfg.WLANs)
}

// The brownfield path, end to end and with a secret in it: export a configured
// Controller, put the passphrase it asked for into the environment, and the
// very first plan is empty.
func TestARedactedExportPlansCleanOnceItsVariablesAreSet(t *testing.T) {
	seedWLANOn(t, "Export Round Trip WLAN", "Default", testPassphrase)

	exported := testRig.runUnifig(t, []string{"export"}, nil)
	if exported.ExitCode != 0 {
		t.Fatalf("unifig export exited %d\nstderr: %s", exported.ExitCode, exported.Stderr)
	}

	path := filepath.Join(t.TempDir(), "unifig.yaml")
	if err := os.WriteFile(path, exported.Stdout, 0o600); err != nil {
		t.Fatalf("writing exported config: %v", err)
	}

	res := planEnv(t, map[string]string{
		"UNIFIG_WLAN_EXPORT_ROUND_TRIP_WLAN_PASSPHRASE": testPassphrase,
	}, path)
	if res.ExitCode != exitNoChanges {
		t.Fatalf("plan of a freshly exported config exited %d, want %d\nexported:\n%s\nplan:\n%s",
			res.ExitCode, exitNoChanges, exported.Stdout, res.Stdout)
	}
}

func TestExportWritesToTheFileNamedByMinusO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unifig.yaml")

	res := testRig.runUnifig(t, []string{"export", "-o", path}, nil)
	if res.ExitCode != 0 {
		t.Fatalf("unifig export -o exited %d\nstderr: %s", res.ExitCode, res.Stderr)
	}
	if len(res.Stdout) != 0 {
		t.Errorf("stdout should stay empty when the config went to a file, got: %s", res.Stdout)
	}

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("export wrote no file: %v", err)
	}
	var cfg exportedConfig
	if err := yaml.Unmarshal(written, &cfg); err != nil {
		t.Fatalf("the file export wrote is not valid YAML: %v\n%s", err, written)
	}
	if len(cfg.Networks) == 0 {
		t.Errorf("the file export wrote describes no networks:\n%s", written)
	}

	// It may hold plaintext secrets under --with-secrets, so it is the owner's
	// to read and nobody else's — and one mode either way means there is no
	// case where the strict one was the one that got forgotten.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("export wrote %s with mode %04o, want 0600", path, mode)
	}
}

// An export that cannot reach the Controller must not take the operator's
// existing config with it. The file is opened only once everything that could
// fail has succeeded.
func TestAFailedExportLeavesTheOutputFileAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unifig.yaml")
	const existing = "networks:\n  - name: Written By Hand\n"
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	res := testRig.runUnifig(t, []string{"export", "-o", path},
		map[string]string{"UNIFIG_API_KEY": "not-the-rigs-key"})

	if res.ExitCode != 1 {
		t.Errorf("exit code = %d, want 1", res.ExitCode)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the file back: %v", err)
	}
	if string(after) != existing {
		t.Errorf("a failed export overwrote the file it was pointed at:\n%s", after)
	}
}

func TestExportWithBadAPIKeyFailsAndPrintsNoYAML(t *testing.T) {
	res := testRig.runUnifig(t, []string{"export"}, map[string]string{"UNIFIG_API_KEY": "not-the-rigs-key"})

	if res.ExitCode != 1 {
		t.Errorf("exit code = %d, want 1", res.ExitCode)
	}
	if len(res.Stdout) != 0 {
		t.Errorf("stdout should stay empty on auth failure, got: %s", res.Stdout)
	}
	stderr := strings.ToLower(string(res.Stderr))
	if !strings.Contains(stderr, "401") && !strings.Contains(stderr, "unauthorized") {
		t.Errorf("stderr should report the auth failure, got: %s", res.Stderr)
	}
}

func TestExportWithoutConnectionConfigFailsNamingTheVariable(t *testing.T) {
	res := testRig.runUnifig(t, []string{"export"}, map[string]string{"UNIFIG_API_KEY": ""})

	if res.ExitCode != 1 {
		t.Errorf("exit code = %d, want 1", res.ExitCode)
	}
	if !strings.Contains(string(res.Stderr), "UNIFIG_API_KEY") {
		t.Errorf("stderr should name the missing variable, got: %s", res.Stderr)
	}
}
