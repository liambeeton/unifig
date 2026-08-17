package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// WLANs are where secrets, cross-resource references and ordering all arrive at
// once, so these tests state three things an operator would recognise: a
// passphrase goes in from the environment and never comes back out in anything
// unifig prints, a config declaring a network and a WLAN on it works in one
// run, and an apply that fails partway says exactly what it did and did not do.
//
// As everywhere in this suite, the assertions are on what a shell would see or
// on what the Controller itself reports afterwards.

// The passphrase used throughout. It is a test fixture rather than a secret,
// but every assertion below treats it as one: what matters is that unifig never
// prints it, and a value that never appeared could not prove that.
const testPassphrase = "correct horse battery"

// managedWLAN writes a config file and deletes the named WLANs from the
// Controller when the test ends — managedNetwork's counterpart. A test that
// creates both registers its networks with cleanupNetworks.
func managedWLAN(t *testing.T, body string, names ...string) string {
	t.Helper()
	cleanupWLANs(t, names...)
	return configFile(t, body)
}

func cleanupWLANs(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		t.Cleanup(func() { testRig.deleteWLANsNamed(t, name) })
	}
}

// seedWLANOn puts a WLAN on the Controller through its own API, attached to a
// live network by name.
func seedWLANOn(t *testing.T, name, network, passphrase string) {
	t.Helper()
	testRig.seedWLAN(t, map[string]any{
		"name": name, "enabled": true, "security": "wpapsk",
		"x_passphrase": passphrase, "networkconf_id": testRig.networkID(t, network),
	})
	cleanupWLANs(t, name)
}

// seedOpenWLAN puts a WLAN on the Controller in the state a physical UDR was
// found in: `security: open`, with an x_passphrase still sitting on it from
// whenever it was last WPA-PSK. The Controller keeps the value rather than
// clearing it, exactly as it keeps PPPoE credentials on a slot that has since
// moved to DHCP.
//
// Open is the mode these tests use, and it is not the only mode the rule covers
// — the engine reads a passphrase off WPA-PSK and nothing else, so an
// enterprise (`wpaeap`) WLAN keeps its stale passphrase out of an export in
// exactly the same way. That half is not exercised here because this Controller
// cannot hold it: a wpaeap wlanconf is refused without a RADIUS profile
// (`api.err.WlanConfRadiusProfileNull`), the built-in profile is refused in turn
// because the RADIUS server is off (`api.err.RadiusServerNotEnabled`), and the
// dockerized Controller ships no `radius` setting to switch on. It is the same
// shortfall that put the WAN slots on a recording — see testdata/udr/README.md —
// and a recording holding an enterprise WLAN would close it.
//
// What was stored is read back and checked rather than assumed, because it is
// the whole subject of the tests below: a Controller that dropped the
// passphrase on the way in would leave them passing against a state that cannot
// occur, which is the one failure a fixture must not have.
func seedOpenWLAN(t *testing.T, name, network, passphrase string) {
	t.Helper()
	testRig.seedWLAN(t, map[string]any{
		"name": name, "enabled": true, "security": "open",
		"x_passphrase": passphrase, "networkconf_id": testRig.networkID(t, network),
	})
	cleanupWLANs(t, name)

	live := testRig.liveWLAN(t, name)
	if live["security"] != "open" || live["x_passphrase"] != passphrase {
		t.Fatalf("the Controller did not store the state these tests are about: security = %#v, x_passphrase = %#v",
			live["security"], live["x_passphrase"])
	}
}

// exportedWLANSecretEnv is the environment a freshly exported config needs in
// order to plan: every passphrase export redacted, put back to the value the
// Controller holds for it.
//
// It is the WAN suite's exportedSecretEnv against the live Controller instead of
// against the recording, and it exists for the same reason: a test that wrote
// the variable names down again would only ever be right about the WLANs it
// happened to seed itself, and would fail on the next fixture somebody adds for
// a reason that has nothing to do with what it is testing.
func exportedWLANSecretEnv(t *testing.T, exported []byte) map[string]string {
	t.Helper()

	env := map[string]string{}
	for _, wlan := range exportedYAML(t, exported).WLANs {
		name, ok := envReference(wlan.Passphrase)
		if !ok {
			continue
		}
		passphrase, _ := testRig.liveWLAN(t, wlan.Name)["x_passphrase"].(string)
		env[name] = passphrase
	}
	return env
}

// withPassphrase is the environment a config with a ${WLAN_PASSPHRASE}
// reference in it needs.
func withPassphrase(value string) map[string]string {
	return map[string]string{"WLAN_PASSPHRASE": value}
}

func TestPlanShowsAWLANToCreateWithoutPrintingItsPassphrase(t *testing.T) {
	path := managedWLAN(t, `networks:
  - name: Default
    subnet: 192.168.1.1/24
wlans:
  - name: WLAN Plan Create
    network: Default
    passphrase: ${WLAN_PASSPHRASE}
`, "WLAN Plan Create")

	res := planEnv(t, withPassphrase(testPassphrase), path)

	if res.ExitCode != exitChangesPending {
		t.Fatalf("plan exited %d, want %d\nstderr: %s", res.ExitCode, exitChangesPending, res.Stderr)
	}
	stdout := string(res.Stdout)
	for _, fragment := range []string{`+ wlan "WLAN Plan Create"`, "Default", "passphrase", "(hidden)", "1 to create"} {
		if !strings.Contains(stdout, fragment) {
			t.Errorf("plan output should mention %q, got:\n%s", fragment, stdout)
		}
	}
	assertNoPassphraseIn(t, "the plan", res.Stdout, res.Stderr)
}

// The plan is the operator's whole view of what is about to happen, and it is
// also the thing that gets pasted into a ticket and captured by a CI log. A
// passphrase must not survive that trip in either shape unifig prints plans in.
func TestNeitherPlanShapeEverPrintsAPassphrase(t *testing.T) {
	seedWLANOn(t, "WLAN Never Printed", "Default", "the-old-passphrase")

	path := managedWLAN(t, `networks:
  - name: Default
    subnet: 192.168.1.1/24
wlans:
  - name: WLAN Never Printed
    network: Default
    passphrase: ${WLAN_PASSPHRASE}
`)

	for _, args := range [][]string{{path}, {"--json", path}} {
		res := planEnv(t, withPassphrase(testPassphrase), args...)
		if res.ExitCode != exitChangesPending {
			t.Fatalf("plan %v exited %d, want %d\nstderr: %s", args, res.ExitCode, exitChangesPending, res.Stderr)
		}
		assertNoPassphraseIn(t, "plan "+strings.Join(args, " "), res.Stdout, res.Stderr)
		if strings.Contains(string(res.Stdout), "the-old-passphrase") {
			t.Errorf("plan %v printed the passphrase the Controller already held:\n%s", args, res.Stdout)
		}
	}
}

// A pipeline gating on drift has to be able to see that a passphrase is
// changing without being handed the passphrase to do it.
func TestPlanJSONMarksAPassphraseAsSecretWithNoValueOnEitherSide(t *testing.T) {
	path := managedWLAN(t, `networks:
  - name: Default
    subnet: 192.168.1.1/24
wlans:
  - name: WLAN JSON Secret
    network: Default
    passphrase: ${WLAN_PASSPHRASE}
`, "WLAN JSON Secret")

	res := planEnv(t, withPassphrase(testPassphrase), "--json", path)

	if res.ExitCode != exitChangesPending {
		t.Fatalf("plan --json exited %d, want %d\nstderr: %s", res.ExitCode, exitChangesPending, res.Stderr)
	}

	var out struct {
		Changes []struct {
			Action string `json:"action"`
			Name   string `json:"name"`
			Fields []struct {
				Name   string `json:"name"`
				From   any    `json:"from"`
				To     any    `json:"to"`
				Secret bool   `json:"secret"`
			} `json:"fields"`
		} `json:"changes"`
	}
	if err := json.Unmarshal(res.Stdout, &out); err != nil {
		t.Fatalf("plan --json is not valid JSON: %v\nstdout: %s", err, res.Stdout)
	}
	if len(out.Changes) != 1 {
		t.Fatalf("plan reported %d changes, want 1\nstdout: %s", len(out.Changes), res.Stdout)
	}

	var found bool
	for _, field := range out.Changes[0].Fields {
		if field.Name != "passphrase" {
			continue
		}
		found = true
		if !field.Secret {
			t.Errorf("the passphrase field is not marked secret: %+v", field)
		}
		if field.From != nil || field.To != nil {
			t.Errorf("the passphrase field carries a value: %+v", field)
		}
	}
	if !found {
		t.Errorf("plan --json does not say the passphrase is being set:\n%s", res.Stdout)
	}
}

func TestApplyCreatesAWLANWithAPassphraseFromTheEnvironment(t *testing.T) {
	path := managedWLAN(t, `networks:
  - name: Default
    subnet: 192.168.1.1/24
wlans:
  - name: WLAN Apply Create
    network: Default
    passphrase: ${WLAN_PASSPHRASE}
`, "WLAN Apply Create")

	res := applyEnv(t, withPassphrase(testPassphrase), path)
	if !strings.Contains(string(res.Stdout), `+ wlan "WLAN Apply Create" created`) {
		t.Errorf("apply should report what it created, got:\n%s", res.Stdout)
	}
	assertNoPassphraseIn(t, "the apply", res.Stdout, res.Stderr)

	live := testRig.liveWLAN(t, "WLAN Apply Create")
	if live["x_passphrase"] != testPassphrase {
		t.Errorf("the Controller holds passphrase %#v, want the one the environment supplied", live["x_passphrase"])
	}
	if live["security"] != "wpapsk" {
		t.Errorf("security = %#v, want %q — a stated passphrase is a statement of WPA-PSK", live["security"], "wpapsk")
	}
	if live["enabled"] != true {
		t.Errorf("enabled = %#v, so the WLAN would not be broadcast at all", live["enabled"])
	}
	if live["networkconf_id"] != testRig.networkID(t, "Default") {
		t.Errorf("the WLAN is not attached to the network the config named: %#v", live["networkconf_id"])
	}
	// The Controller refuses a WLAN with nothing to broadcast it, so unifig
	// puts a new one on the default AP group the way its own UI does.
	if groups, ok := live["ap_group_ids"].([]any); !ok || len(groups) == 0 {
		t.Errorf("ap_group_ids = %#v, so no access point would carry this WLAN", live["ap_group_ids"])
	}

	assertNoChangesPendingEnv(t, withPassphrase(testPassphrase), path)
}

// A WLAN unifig creates from a config with no passphrase is open, which is a
// consequence the config does not state — so the plan states it, before anyone
// is asked to approve anything.
func TestPlanWarnsThatAWLANWithNoPassphraseWillBeOpen(t *testing.T) {
	path := managedWLAN(t, `networks:
  - name: Default
    subnet: 192.168.1.1/24
wlans:
  - name: WLAN Open
    network: Default
`, "WLAN Open")

	res := plan(t, path)

	if res.ExitCode != exitChangesPending {
		t.Fatalf("plan exited %d, want %d\nstderr: %s", res.ExitCode, exitChangesPending, res.Stderr)
	}
	if !strings.Contains(string(res.Stdout), "open") {
		t.Errorf("plan should warn that this WLAN will be open, got:\n%s", res.Stdout)
	}

	apply(t, path)

	live := testRig.liveWLAN(t, "WLAN Open")
	if live["security"] != "open" {
		t.Errorf("security = %#v, want %q — the plan said open", live["security"], "open")
	}
	assertNoChangesPending(t, path)
}

// The Internal API hands a WLAN's passphrase back in the clear (ADR-0007), and
// the whole diff rests on that: without it, a passphrase could be written but
// never compared. These two tests are that fact stated as behaviour — a
// passphrase that agrees with the Controller is not a change, and one that does
// not is.
func TestAPassphraseThatMatchesTheControllerIsNotAChange(t *testing.T) {
	seedWLANOn(t, "WLAN Readback Same", "Default", testPassphrase)

	path := managedWLAN(t, `networks:
  - name: Default
    subnet: 192.168.1.1/24
wlans:
  - name: WLAN Readback Same
    network: Default
    passphrase: ${WLAN_PASSPHRASE}
`)

	res := planEnv(t, withPassphrase(testPassphrase), path)

	if res.ExitCode != exitNoChanges {
		t.Fatalf("plan exited %d, want %d — it cannot see the passphrase the Controller already holds\nplan:\n%s",
			res.ExitCode, exitNoChanges, res.Stdout)
	}
}

func TestApplyRotatesAPassphraseAndTheNextPlanIsEmpty(t *testing.T) {
	seedWLANOn(t, "WLAN Rotate", "Default", "the-old-passphrase")

	path := managedWLAN(t, `networks:
  - name: Default
    subnet: 192.168.1.1/24
wlans:
  - name: WLAN Rotate
    network: Default
    passphrase: ${WLAN_PASSPHRASE}
`)

	res := applyEnv(t, withPassphrase(testPassphrase), path)
	if !strings.Contains(string(res.Stdout), `~ wlan "WLAN Rotate" updated`) {
		t.Errorf("apply should report what it updated, got:\n%s", res.Stdout)
	}

	live := testRig.liveWLAN(t, "WLAN Rotate")
	if live["x_passphrase"] != testPassphrase {
		t.Errorf("the Controller still holds %#v, want the rotated passphrase", live["x_passphrase"])
	}

	assertNoChangesPendingEnv(t, withPassphrase(testPassphrase), path)
}

// A passphrase the config leaves out is one unifig does not manage (ADR-0004),
// so an operator can put a WLAN under unifig's care without handing over its
// passphrase at the same time.
func TestAWLANWithNoPassphraseInTheConfigKeepsTheControllersOwn(t *testing.T) {
	seedWLANOn(t, "WLAN Unmanaged Secret", "Default", "the-old-passphrase")

	path := managedWLAN(t, `networks:
  - name: Default
    subnet: 192.168.1.1/24
wlans:
  - name: WLAN Unmanaged Secret
    network: Default
`)

	res := plan(t, path)
	if res.ExitCode != exitNoChanges {
		t.Fatalf("plan exited %d, want %d — it is proposing to change a passphrase the config never mentioned\nplan:\n%s",
			res.ExitCode, exitNoChanges, res.Stdout)
	}
	if live := testRig.liveWLAN(t, "WLAN Unmanaged Secret"); live["x_passphrase"] != "the-old-passphrase" {
		t.Errorf("passphrase = %#v, want the Controller's own", live["x_passphrase"])
	}
}

// A passphrase left behind on a WLAN the Controller no longer joins clients to
// with a pre-shared key describes nothing about how they join it — the same
// rule the WAN slots already follow, where credentials are read only for a slot
// actually using PPPoE.
//
// Export is where it bites hardest. Writing that stale value would put a
// passphrase in the file for a WLAN that has none, and the brownfield path
// would then be "invent a secret for a WLAN with no secret" — which is also the
// one thing that turns the open WLAN into a WPA-PSK one and locks out every
// guest on it.
func TestAnOpenWLANExportsWithNoPassphraseAndPlansClean(t *testing.T) {
	seedOpenWLAN(t, "WLAN Open Export", "Default", testPassphrase)

	exported := testRig.runUnifig(t, []string{"export"}, nil)
	if exported.ExitCode != 0 {
		t.Fatalf("unifig export exited %d\nstderr: %s", exported.ExitCode, exported.Stderr)
	}
	assertNoPassphraseIn(t, "the export", exported.Stdout, exported.Stderr)

	var found bool
	for _, wlan := range exportedYAML(t, exported.Stdout).WLANs {
		if wlan.Name != "WLAN Open Export" {
			continue
		}
		found = true
		if wlan.Passphrase != "" {
			t.Errorf("export wrote passphrase %q for a WLAN the Controller holds as open", wlan.Passphrase)
		}
		if wlan.Network != "Default" {
			t.Errorf("network = %q, want %q", wlan.Network, "Default")
		}
	}
	if !found {
		t.Fatalf("export left out the seeded open WLAN entirely:\n%s", exported.Stdout)
	}
	// Nothing was redacted for it either, so the operator is not asked to set a
	// variable for a secret that does not exist.
	if strings.Contains(string(exported.Stderr), "UNIFIG_WLAN_WLAN_OPEN_EXPORT_PASSPHRASE") {
		t.Errorf("export asked for a secret this WLAN does not have: %s", exported.Stderr)
	}

	// And the file it wrote is one the operator can walk straight into: nothing
	// to invent for this WLAN, and the very first plan is empty.
	path := filepath.Join(t.TempDir(), "unifig.yaml")
	if err := os.WriteFile(path, exported.Stdout, 0o600); err != nil {
		t.Fatalf("writing exported config: %v", err)
	}
	res := planEnv(t, exportedWLANSecretEnv(t, exported.Stdout), path)
	if res.ExitCode != exitNoChanges {
		t.Fatalf("plan of a freshly exported config exited %d, want %d\nexported:\n%s\nplan:\n%s",
			res.ExitCode, exitNoChanges, exported.Stdout, res.Stdout)
	}
}

// The other half of the same rule, and the one that stops an over-correction:
// an open WLAN the config describes by name and network alone is a WLAN unifig
// manages no security about, so an unchanged Controller is an empty plan. The
// stale passphrase is neither exported nor diffed nor cleared.
func TestAnOpenWLANWithAStalePassphraseIsNotAChange(t *testing.T) {
	seedOpenWLAN(t, "WLAN Open Unchanged", "Default", testPassphrase)

	path := managedWLAN(t, `networks:
  - name: Default
    subnet: 192.168.1.1/24
wlans:
  - name: WLAN Open Unchanged
    network: Default
`)

	res := plan(t, path)
	if res.ExitCode != exitNoChanges {
		t.Fatalf("plan exited %d, want %d — it is proposing to change a WLAN nothing has asked for\nplan:\n%s",
			res.ExitCode, exitNoChanges, res.Stdout)
	}
	live := testRig.liveWLAN(t, "WLAN Open Unchanged")
	if live["security"] != "open" {
		t.Errorf("security = %#v, want %q — a plan that changed nothing changed this", live["security"], "open")
	}
}

// A config that does state a passphrase for an open WLAN is the operator asking
// for WPA-PSK, so it applies. What they must not have to discover afterwards is
// that everyone currently on that WLAN was disconnected by it — unifig does
// not model security, so the file cannot say it and the plan does.
//
// The passphrase here is the one the Controller already has sitting on the WLAN,
// which is the case the defect was latent in: with the stale value taken for the
// current one, the two agreed and the plan was empty, while an apply of any
// other field on the WLAN would have flipped its security as a side effect.
func TestAPassphraseForAnOpenWLANSaysTheSecurityIsChangingBeforeItDoes(t *testing.T) {
	seedOpenWLAN(t, "WLAN Open To WPA", "Default", testPassphrase)

	path := managedWLAN(t, `networks:
  - name: Default
    subnet: 192.168.1.1/24
wlans:
  - name: WLAN Open To WPA
    network: Default
    passphrase: ${WLAN_PASSPHRASE}
`)

	res := planEnv(t, withPassphrase(testPassphrase), path)
	if res.ExitCode != exitChangesPending {
		t.Fatalf("plan exited %d, want %d — the WLAN is open and the config asks for a passphrase\nplan:\n%s",
			res.ExitCode, exitChangesPending, res.Stdout)
	}
	stdout := string(res.Stdout)
	for _, fragment := range []string{
		`~ wlan "WLAN Open To WPA"`, "passphrase", "(hidden)", "open", "WPA-PSK", "disconnect",
	} {
		if !strings.Contains(stdout, fragment) {
			t.Errorf("plan should mention %q so the security change is not a surprise, got:\n%s", fragment, stdout)
		}
	}
	assertNoPassphraseIn(t, "the plan", res.Stdout, res.Stderr)

	applyEnv(t, withPassphrase(testPassphrase), path)

	live := testRig.liveWLAN(t, "WLAN Open To WPA")
	if live["security"] != "wpapsk" {
		t.Errorf("security = %#v, want %q — the operator asked for a passphrase", live["security"], "wpapsk")
	}
	if live["x_passphrase"] != testPassphrase {
		t.Errorf("the Controller holds passphrase %#v, want the one the environment supplied", live["x_passphrase"])
	}

	assertNoChangesPendingEnv(t, withPassphrase(testPassphrase), path)
}

// unifig owns the fields its config models and nothing else. A WLAN carries far
// more than a name, a network and a passphrase, and an operator who set band
// selection or a MAC filter in the Controller's UI must not lose it because
// they rotated a passphrase in YAML.
func TestApplyLeavesWLANSettingsUnifigDoesNotModelAlone(t *testing.T) {
	testRig.seedWLAN(t, map[string]any{
		"name": "WLAN Preserve", "enabled": true, "security": "wpapsk",
		"x_passphrase": "the-old-passphrase", "networkconf_id": testRig.networkID(t, "Default"),
		"hide_ssid": true, "wlan_band": "5g", "wlan_bands": []string{"5g"},
		"mac_filter_enabled": true, "mac_filter_policy": "allow",
		"mac_filter_list": []string{"00:11:22:33:44:55"},
	})
	cleanupWLANs(t, "WLAN Preserve")

	path := managedWLAN(t, `networks:
  - name: Default
    subnet: 192.168.1.1/24
wlans:
  - name: WLAN Preserve
    network: Default
    passphrase: ${WLAN_PASSPHRASE}
`)

	applyEnv(t, withPassphrase(testPassphrase), path)

	live := testRig.liveWLAN(t, "WLAN Preserve")
	if live["x_passphrase"] != testPassphrase {
		t.Fatalf("the change under test did not happen: passphrase = %#v", live["x_passphrase"])
	}
	for field, want := range map[string]any{
		"hide_ssid":          true,
		"wlan_band":          "5g",
		"mac_filter_enabled": true,
		"mac_filter_policy":  "allow",
	} {
		if live[field] != want {
			t.Errorf("%s = %#v after apply, want %#v — unifig does not model this field and must not touch it",
				field, live[field], want)
		}
	}
	if macs, ok := live["mac_filter_list"].([]any); !ok || len(macs) != 1 {
		t.Errorf("mac_filter_list = %#v after apply, want the operator's own single entry", live["mac_filter_list"])
	}
}

func TestApplyMovesAWLANToAnotherNetworkAndTheNextPlanIsEmpty(t *testing.T) {
	testRig.seedNetwork(t, map[string]any{
		"name": "WLAN Move Target", "purpose": "corporate", "enabled": true,
		"vlan_enabled": true, "vlan": 161, "ip_subnet": "10.161.0.1/24",
	})
	cleanupNetworks(t, "WLAN Move Target")
	seedWLANOn(t, "WLAN Move", "Default", testPassphrase)

	path := managedWLAN(t, `networks:
  - name: Default
    subnet: 192.168.1.1/24
  - name: WLAN Move Target
    vlan: 161
    subnet: 10.161.0.1/24
wlans:
  - name: WLAN Move
    network: WLAN Move Target
`)

	res := plan(t, path)
	if !strings.Contains(string(res.Stdout), "Default -> WLAN Move Target") {
		t.Errorf("plan should show both ends of the move, got:\n%s", res.Stdout)
	}

	apply(t, path)

	live := testRig.liveWLAN(t, "WLAN Move")
	if live["networkconf_id"] != testRig.networkID(t, "WLAN Move Target") {
		t.Errorf("the WLAN did not move to the network the config named: %#v", live["networkconf_id"])
	}

	assertNoChangesPending(t, path)
}

// The one-pass promise: a config that declares a network and a WLAN that joins
// it applies in a single run, because the WLAN's create reads the network's
// Controller ID at the moment it runs rather than the moment it was planned.
func TestApplyCreatesANetworkAndAWLANOnItInOnePass(t *testing.T) {
	cleanupNetworks(t, "WLAN One Pass Net")
	path := managedWLAN(t, `networks:
  - name: WLAN One Pass Net
    vlan: 162
    subnet: 10.162.0.1/24
wlans:
  - name: WLAN One Pass
    network: WLAN One Pass Net
    passphrase: ${WLAN_PASSPHRASE}
`, "WLAN One Pass")

	res := applyEnv(t, withPassphrase(testPassphrase), path)

	created, joined := strings.Index(string(res.Stdout), `network "WLAN One Pass Net" created`),
		strings.Index(string(res.Stdout), `wlan "WLAN One Pass" created`)
	if created < 0 || joined < 0 {
		t.Fatalf("apply did not create both:\n%s", res.Stdout)
	}
	if created > joined {
		t.Errorf("apply created the WLAN before the network it joins:\n%s", res.Stdout)
	}

	live := testRig.liveWLAN(t, "WLAN One Pass")
	if live["networkconf_id"] != testRig.networkID(t, "WLAN One Pass Net") {
		t.Errorf("the WLAN is not on the network created alongside it: %#v", live["networkconf_id"])
	}

	assertNoChangesPendingEnv(t, withPassphrase(testPassphrase), path)
}

// Building goes from the ground up and dismantling goes the other way, and the
// plan prints the order apply will use — so an operator sees it before agreeing
// to it, and the two cannot disagree.
func TestPlanOrdersWLANsAfterNetworksAndDeletionsTheOtherWayAround(t *testing.T) {
	testRig.seedNetwork(t, map[string]any{
		"name": "WLAN Order Old Net", "purpose": "corporate", "enabled": true,
		"vlan_enabled": true, "vlan": 163, "ip_subnet": "10.163.0.1/24",
	})
	cleanupNetworks(t, "WLAN Order Old Net", "WLAN Order New Net")
	seedWLANOn(t, "WLAN Order Old", "WLAN Order Old Net", testPassphrase)

	// Everything live except the pair being pruned, plus a network and a WLAN
	// on it that do not exist yet — so the plan holds both directions at once.
	path := managedWLAN(t, liveNetworksExcept(t, "WLAN Order Old Net")+
		`  - name: WLAN Order New Net
    vlan: 164
    subnet: 10.164.0.1/24
`+wlanSection(liveWLANEntriesExcept(t, "WLAN Order Old")+
		`  - name: WLAN Order New
    network: WLAN Order New Net
`), "WLAN Order New")

	res := plan(t, "--prune", path)

	if res.ExitCode != exitChangesPending {
		t.Fatalf("plan --prune exited %d, want %d\nstdout: %s\nstderr: %s",
			res.ExitCode, exitChangesPending, res.Stdout, res.Stderr)
	}
	stdout := string(res.Stdout)
	at := map[string]int{}
	for _, name := range []string{
		`+ network "WLAN Order New Net"`,
		`+ wlan "WLAN Order New"`,
		`- wlan "WLAN Order Old"`,
		`- network "WLAN Order Old Net"`,
	} {
		if at[name] = strings.Index(stdout, name); at[name] < 0 {
			t.Fatalf("plan does not mention %q:\n%s", name, stdout)
		}
	}
	if at[`+ network "WLAN Order New Net"`] > at[`+ wlan "WLAN Order New"`] {
		t.Errorf("plan creates the WLAN before the network it joins:\n%s", stdout)
	}
	if at[`- wlan "WLAN Order Old"`] > at[`- network "WLAN Order Old Net"`] {
		t.Errorf("plan deletes the network before the WLAN on it:\n%s", stdout)
	}
}

// Apply stops at the first error and says what state it left the Controller in.
//
// The trigger is a subnet that overlaps one already on the Controller, which
// unifig has no way to know offline: it models no relationship between two
// networks' subnets, and the file that would clash is perfectly valid on its
// own. That is what makes it the right failure to test — stop-on-first-error
// exists precisely for what only the Controller knows, so a trigger validate
// could have caught would be testing the wrong thing.
func TestApplyStoppingPartWaySaysExactlyWhatDidAndDidNotHappen(t *testing.T) {
	// Live, and not named by the config below — so the config's own second
	// network is the one that collides with it.
	testRig.seedNetwork(t, map[string]any{
		"name": "WLAN Partial Taken", "purpose": "corporate", "enabled": true,
		"vlan_enabled": true, "vlan": 168, "ip_subnet": "10.168.0.1/24",
	})
	cleanupNetworks(t, "WLAN Partial Taken", "WLAN Partial Net", "WLAN Partial Zulu")

	// Creates run networks-then-WLANs, alphabetical within a type, so this is
	// "WLAN Partial Net", then "WLAN Partial Zulu", then the WLAN — and the
	// failure lands in the middle, with work either side of it.
	path := managedWLAN(t, `networks:
  - name: WLAN Partial Net
    vlan: 169
    subnet: 10.169.0.1/24
  - name: WLAN Partial Zulu
    vlan: 170
    subnet: 10.168.0.1/24
wlans:
  - name: WLAN Partial
    network: WLAN Partial Net
    passphrase: ${WLAN_PASSPHRASE}
`, "WLAN Partial")

	res := testRig.runUnifig(t, []string{"apply", "--auto-approve", path}, withPassphrase(testPassphrase))
	t.Logf("unifig apply -> exit %d\n%s\n%s", res.ExitCode, res.Stdout, res.Stderr)

	if res.ExitCode != exitError {
		t.Fatalf("apply exited %d, want %d — the Controller was supposed to refuse the overlapping subnet\nstdout: %s",
			res.ExitCode, exitError, res.Stdout)
	}

	stdout := string(res.Stdout)
	for _, fragment := range []string{
		`+ network "WLAN Partial Net" created`, // what did happen
		"Applied 1 of 3 changes",
		"These were not applied:",
		`+ network "WLAN Partial Zulu"`, // the one that failed
		`+ wlan "WLAN Partial"`,         // and the one never attempted
		"safe to run again",
	} {
		if !strings.Contains(stdout, fragment) {
			t.Errorf("a stopped apply should report %q, got:\n%s", fragment, stdout)
		}
	}
	if !strings.Contains(string(res.Stderr), "WLAN Partial Zulu") {
		t.Errorf("stderr should name the change that failed, got: %s", res.Stderr)
	}
	assertNoPassphraseIn(t, "a stopped apply", res.Stdout, res.Stderr)

	// And the Controller matches the report exactly — the report is only worth
	// anything if an operator can act on it without going and looking.
	if found := testRig.networksNamed(t, "WLAN Partial Net"); len(found) != 1 {
		t.Errorf("the network the apply said it created is not there: %v", found)
	}
	if found := testRig.networksNamed(t, "WLAN Partial Zulu"); len(found) != 0 {
		t.Errorf("the network the apply said it did not create is there anyway: %v", found)
	}
	if found := testRig.wlansNamed(t, "WLAN Partial"); len(found) != 0 {
		t.Errorf("the WLAN the apply said it did not create is there anyway: %v", found)
	}
}

func TestApplyWithPruneDeletesWLANsTheConfigDoesNotName(t *testing.T) {
	seedWLANOn(t, "WLAN Prune Applied", "Default", testPassphrase)

	path := managedWLAN(t, liveConfigExcept(t, "WLAN Prune Applied"))

	res := apply(t, "--prune", path)

	if !strings.Contains(string(res.Stdout), `- wlan "WLAN Prune Applied" deleted`) {
		t.Errorf("apply --prune should report what it deleted, got:\n%s", res.Stdout)
	}
	if found := testRig.wlansNamed(t, "WLAN Prune Applied"); len(found) != 0 {
		t.Fatalf("apply --prune left a WLAN the config does not name: %v", found)
	}

	assertNoChangesPending(t, "--prune", path)
}

// ADR-0006. A file with no `wlans:` key says nothing about WLANs, so a prune it
// takes part in has no business deleting one — which is what stops an operator
// who has only ever managed networks from losing their wireless the first time
// they reach for --prune.
func TestPruneLeavesASectionTheConfigDoesNotHaveAlone(t *testing.T) {
	seedWLANOn(t, "WLAN Prune Out Of Scope", "Default", testPassphrase)
	testRig.seedNetwork(t, map[string]any{
		"name": "WLAN Prune Bystander", "purpose": "corporate", "enabled": true,
		"vlan_enabled": true, "vlan": 166, "ip_subnet": "10.166.0.1/24",
	})
	cleanupNetworks(t, "WLAN Prune Bystander")

	// Networks only, and one of them missing — so the prune under test really
	// runs and really deletes something, and the WLAN is spared on purpose
	// rather than because nothing happened.
	path := managedWLAN(t, liveNetworksExcept(t, "WLAN Prune Bystander"))

	res := apply(t, "--prune", path)

	if found := testRig.networksNamed(t, "WLAN Prune Bystander"); len(found) != 0 {
		t.Fatalf("the prune under test did not happen: %v", found)
	}
	if strings.Contains(string(res.Stdout), "WLAN Prune Out Of Scope") {
		t.Errorf("apply --prune has an opinion about a section the config does not have:\n%s", res.Stdout)
	}
	if found := testRig.wlansNamed(t, "WLAN Prune Out Of Scope"); len(found) != 1 {
		t.Fatalf("prune deleted a WLAN from a config with no wlans section: %v", found)
	}
}

// `wlans: []` is the other half of the same rule: an empty section is a
// statement, and it says there should be none.
func TestAnEmptyWLANSectionPutsEveryWLANAtStake(t *testing.T) {
	seedWLANOn(t, "WLAN Prune Emptied", "Default", testPassphrase)

	path := managedWLAN(t, liveNetworksExcept(t)+"wlans: []\n")

	res := apply(t, "--prune", path)

	if !strings.Contains(string(res.Stdout), `- wlan "WLAN Prune Emptied" deleted`) {
		t.Errorf("apply --prune should report what it deleted, got:\n%s", res.Stdout)
	}
	if found := testRig.wlansNamed(t, "WLAN Prune Emptied"); len(found) != 0 {
		t.Fatalf("an empty wlans section left a WLAN behind: %v", found)
	}
}

// A WLAN unifig manages is one bound to a network unifig manages. One bound to
// a WAN slot — or, as the dockerized Controller's own demo WLAN does, to nothing
// at all — cannot be written as config, because `network` has to name a network
// the same file defines and there is no name to put there. So it is out of
// unifig's reach entirely: not exported, not matched, and above all not pruned.
//
// The last of those is the one that would hurt. A WLAN export left out of the
// file is a WLAN the file does not name, and if prune could still see it, the
// brownfield path would be "adopt your Controller, then delete the thing the
// adoption could not describe".
func TestAWLANBoundToSomethingUnifigDoesNotManageIsOutOfItsReach(t *testing.T) {
	testRig.seedNetwork(t, map[string]any{
		"name": "WLAN Scope WAN Slot", "purpose": "wan", "enabled": true,
		"wan_networkgroup": "WAN2", "wan_type": "dhcp",
	})
	cleanupNetworks(t, "WLAN Scope WAN Slot")
	testRig.seedWLAN(t, map[string]any{
		"name": "WLAN Scope Unreachable", "enabled": true, "security": "wpapsk",
		"x_passphrase":   testPassphrase,
		"networkconf_id": testRig.networkID(t, "WLAN Scope WAN Slot"),
	})
	cleanupWLANs(t, "WLAN Scope Unreachable")

	// Something for the prune to actually delete, so this is a test about what a
	// working prune spares rather than about a prune that did nothing.
	seedWLANOn(t, "WLAN Scope Bystander", "Default", testPassphrase)

	exported := testRig.runUnifig(t, []string{"export"}, nil)
	if exported.ExitCode != 0 {
		t.Fatalf("export exited %d — one odd WLAN should not stop it\nstderr: %s", exported.ExitCode, exported.Stderr)
	}
	if strings.Contains(string(exported.Stdout), "WLAN Scope Unreachable") {
		t.Errorf("export wrote a WLAN it cannot name a network for:\n%s", exported.Stdout)
	}
	// Left out, but not left unsaid: export is the adoption path, and an
	// operator who is told the file describes their Controller should not
	// discover otherwise later.
	if !strings.Contains(string(exported.Stderr), "WLAN Scope Unreachable") {
		t.Errorf("export should say which WLAN it left out, got: %s", exported.Stderr)
	}

	path := managedWLAN(t, liveConfigExcept(t, "WLAN Scope Bystander"))
	res := apply(t, "--prune", path)

	if found := testRig.wlansNamed(t, "WLAN Scope Bystander"); len(found) != 0 {
		t.Fatalf("the prune under test did not happen: %v", found)
	}
	if strings.Contains(string(res.Stdout), "WLAN Scope Unreachable") {
		t.Errorf("apply --prune has an opinion about a WLAN unifig cannot describe:\n%s", res.Stdout)
	}
	if found := testRig.wlansNamed(t, "WLAN Scope Unreachable"); len(found) != 1 {
		t.Fatalf("prune deleted a WLAN unifig could not have exported: %v", found)
	}
}

// ADR-0001 matches Resources by name and calls a duplicate a hard error rather
// than a guess. The Controller does permit two WLANs to share an SSID, so this
// is a state unifig can really meet.
func TestPlanRefusesToGuessBetweenTwoWLANsSharingAName(t *testing.T) {
	seedWLANOn(t, "WLAN Twice", "Default", testPassphrase)
	// seedWLAN clears same-named entries first, so the twin goes in through the
	// Controller's own API directly.
	testRig.addWLAN(t, map[string]any{
		"name": "WLAN Twice", "enabled": true, "security": "wpapsk",
		"x_passphrase": "a-different-passphrase", "networkconf_id": testRig.networkID(t, "Default"),
	})

	path := managedWLAN(t, `networks:
  - name: Default
    subnet: 192.168.1.1/24
wlans:
  - name: WLAN Twice
    network: Default
`)

	res := plan(t, path)

	if res.ExitCode != exitError {
		t.Fatalf("plan exited %d, want %d — it picked one of two WLANs named the same\nstdout: %s",
			res.ExitCode, exitError, res.Stdout)
	}
	stderr := string(res.Stderr)
	for _, fragment := range []string{"WLAN Twice", "UI"} {
		if !strings.Contains(stderr, fragment) {
			t.Errorf("stderr should mention %q, got: %s", fragment, stderr)
		}
	}
}

// assertNoPassphraseIn fails the test if the test passphrase appears anywhere in
// what unifig wrote. Every stream is checked rather than the one under
// suspicion: a secret that leaks onto the wrong stream has still leaked.
func assertNoPassphraseIn(t *testing.T, what string, streams ...[]byte) {
	t.Helper()
	for _, stream := range streams {
		if strings.Contains(string(stream), testPassphrase) {
			t.Errorf("%s printed the passphrase:\n%s", what, stream)
		}
	}
}
