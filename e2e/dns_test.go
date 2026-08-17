package e2e

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// Encrypted DNS — DNS Shield in the Controller's UI — is the second Setting,
// and these tests state what the word "singleton" adds to it: there is exactly
// one, so nothing matches it, the plan names no name, and unifig can no more
// create it than it can create an uplink.
//
// They run against the recorded stand-in for the same reason the WAN tests do
// (see replay_test.go), and with the same discipline: nothing here depends on
// what the committed recording happens to hold. Every test seeds the state it
// starts from, so a re-recording from any router leaves the suite stating the
// same things.

// The stamps used throughout. Like the WLAN suite's passphrase and the WAN
// suite's password they are fixtures rather than secrets, and every assertion
// treats them as secrets — a value that never appeared in the first place could
// not prove it stays out of the output.
const (
	testDNSStamp     = "sdns://AgcAAAAAAAAAAAAPZG5zLmV4YW1wbGUuY29tOnByaXZhdGUtZW5kcG9pbnQ"
	testDNSStampTwo  = "sdns://AgcAAAAAAAAAAAAQZG5zMi5leGFtcGxlLmNvbQ"
	testDNSServer    = "AdGuard-DNS"
	testDNSServerTwo = "Quad9"
)

// withDNSStamp is the environment a config with a ${DNS_STAMP} reference needs,
// pointed at the stand-in.
func withDNSStamp(r *replay) map[string]string {
	env := r.env()
	env["DNS_STAMP"] = testDNSStamp
	return env
}

// dnsOff puts the setting into the state a Controller that has never had
// encrypted DNS configured hands over: switched off, with no custom resolvers
// on it.
func dnsOff(t *testing.T, r *replay) {
	t.Helper()
	r.seedDoH(t, map[string]any{"state": "off", "custom_servers": []any{}})
}

// dnsCustom is the other starting state: already resolving over the stamp every
// assertion here watches for.
func dnsCustom(t *testing.T, r *replay) {
	t.Helper()
	r.seedDoH(t, map[string]any{
		"state":          "custom",
		"custom_servers": []any{customServer(testDNSServer, testDNSStamp, true)},
	})
}

// customDNSConfig is the change this whole issue exists for: a custom resolver
// declared by stamp, with the stamp coming from the environment.
func customDNSConfig() string { return customDNSConfigIn(stateCustom) }

// customDNSConfigIn is the same file in a state the test names, for the cases
// that are about a resolver declared in a state that will not use it.
func customDNSConfigIn(state string) string {
	return fmt.Sprintf(`encrypted-dns:
  state: %s
  servers:
    - name: %s
      stamp: ${DNS_STAMP}
`, state, testDNSServer)
}

// stateCustom is spelled out here rather than imported from the engine, for the
// same reason the rig spells out the network purposes: a test that asked the
// code under test what the Controller's own word is could not catch it using
// the wrong one.
const stateCustom = "custom"

func TestPlanShowsEncryptedDNSToUpdateAndNeverPrintsTheStamp(t *testing.T) {
	r := startReplay(t)
	dnsOff(t, r)
	path := configFile(t, customDNSConfig())

	res := planEnv(t, withDNSStamp(r), path)

	if res.ExitCode != exitChangesPending {
		t.Fatalf("plan exited %d, want %d\nstderr: %s", res.ExitCode, exitChangesPending, res.Stderr)
	}
	stdout := string(res.Stdout)
	for _, fragment := range []string{
		"~ encrypted-dns", "off -> custom",
		fmt.Sprintf("server %q", testDNSServer), "(hidden)", "1 to update",
	} {
		if !strings.Contains(stdout, fragment) {
			t.Errorf("plan output should mention %q, got:\n%s", fragment, stdout)
		}
	}
	// A singleton has no name to match on, so the plan has none to print —
	// `~ encrypted-dns ""` would be reporting an identity that does not exist.
	if strings.Contains(stdout, `encrypted-dns ""`) {
		t.Errorf("the plan invented an empty name for a singleton Setting, got:\n%s", stdout)
	}
	assertNoStampIn(t, "the plan", res.Stdout, res.Stderr)
}

// A pipeline reading the machine face gets the same two facts: which kind
// changed, and that a field it must not log is being written.
func TestPlanJSONNamesTheKindAndMarksTheStampSecret(t *testing.T) {
	r := startReplay(t)
	dnsOff(t, r)
	path := configFile(t, customDNSConfig())

	res := planEnv(t, withDNSStamp(r), "--json", path)

	if res.ExitCode != exitChangesPending {
		t.Fatalf("plan --json exited %d, want %d\nstderr: %s", res.ExitCode, exitChangesPending, res.Stderr)
	}

	var out struct {
		Changes []struct {
			Action string `json:"action"`
			Kind   string `json:"kind"`
			Name   string `json:"name"`
			Risk   string `json:"risk"`
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

	change := out.Changes[0]
	if change.Action != "update" || change.Kind != "encrypted-dns" {
		t.Errorf("the change is not an update to the Encrypted DNS setting: %+v", change)
	}
	// A singleton reports the empty name rather than omitting the field, so a
	// consumer reads "" instead of having to know which kinds have one.
	if change.Name != "" {
		t.Errorf("the change carries the name %q, and a singleton Setting has none", change.Name)
	}

	var stamped bool
	for _, field := range change.Fields {
		if !strings.HasPrefix(field.Name, "server ") {
			continue
		}
		stamped = true
		if !field.Secret || field.From != nil || field.To != nil {
			t.Errorf("the stamp field is not redacted: %+v", field)
		}
	}
	if !stamped {
		t.Errorf("the plan says nothing about the stamp it is writing: %+v", change.Fields)
	}
	assertNoStampIn(t, "plan --json", res.Stdout, res.Stderr)
}

func TestApplyPutsACustomDNSServerOnTheControllerAndTheNextPlanIsEmpty(t *testing.T) {
	r := startReplay(t)
	dnsOff(t, r)
	path := configFile(t, customDNSConfig())

	res := applyEnv(t, withDNSStamp(r), path)
	if !strings.Contains(string(res.Stdout), "~ encrypted-dns updated") {
		t.Errorf("apply should report what it updated, got:\n%s", res.Stdout)
	}
	assertNoStampIn(t, "the apply", res.Stdout, res.Stderr)

	if state := r.doh(t)["state"]; state != "custom" {
		t.Errorf("state = %#v, want %q", state, "custom")
	}
	servers := r.dohServers(t)
	if len(servers) != 1 {
		t.Fatalf("the Controller holds %d custom DNS servers, want 1: %+v", len(servers), servers)
	}
	if servers[0]["server_name"] != testDNSServer {
		t.Errorf("server_name = %#v, want the one the config named", servers[0]["server_name"])
	}
	if servers[0]["sdns_stamp"] != testDNSStamp {
		t.Errorf("the Controller holds stamp %#v, want the one the environment supplied", servers[0]["sdns_stamp"])
	}
	// A resolver the Controller is told to ignore is a config line that does
	// nothing, so adding one switches it on.
	if servers[0]["enabled"] != true {
		t.Errorf("enabled = %#v, so the resolver unifig wrote would not be used", servers[0]["enabled"])
	}

	assertNoChangesPendingEnv(t, withDNSStamp(r), path)
}

// A rotated stamp is an ordinary diff on an ordinary field, because the
// Controller hands the old one back (ADR-0007). Nothing about the resolver's
// identity changes, so the list is not what the plan talks about.
func TestApplyRotatesAStampAndTheNextPlanIsEmpty(t *testing.T) {
	r := startReplay(t)
	r.seedDoH(t, map[string]any{
		"state":          "custom",
		"custom_servers": []any{customServer(testDNSServer, "sdns://AgcAAAAAAAAAAAAOb2xkLmV4YW1wbGUuY29t", true)},
	})
	path := configFile(t, customDNSConfig())

	res := planEnv(t, withDNSStamp(r), path)
	if res.ExitCode != exitChangesPending {
		t.Fatalf("plan exited %d, want %d — a rotated stamp is a change\nstdout: %s", res.ExitCode, exitChangesPending, res.Stdout)
	}
	if strings.Contains(string(res.Stdout), "servers:") {
		t.Errorf("the resolver list did not change, so the plan should not say it did:\n%s", res.Stdout)
	}

	applyEnv(t, withDNSStamp(r), path)

	if servers := r.dohServers(t); servers[0]["sdns_stamp"] != testDNSStamp {
		t.Errorf("the Controller holds stamp %#v, want the rotated one", servers[0]["sdns_stamp"])
	}
	assertNoChangesPendingEnv(t, withDNSStamp(r), path)
}

// A Controller already in the state the file describes is no change at all —
// the plainest statement that the stamp reads back and diffs like any other
// field, rather than being written on every run.
func TestAControllerAlreadyResolvingOverTheDeclaredStampIsNoChange(t *testing.T) {
	r := startReplay(t)
	dnsCustom(t, r)
	path := configFile(t, customDNSConfig())

	assertNoChangesPendingEnv(t, withDNSStamp(r), path)
}

// The list is one field, so stating it states the list — and a resolver leaving
// it is announced in the plan before it goes, which is the whole of what makes
// that safe.
func TestStatingTheServerListSaysWhichResolversLeaveIt(t *testing.T) {
	r := startReplay(t)
	r.seedDoH(t, map[string]any{
		"state": "custom",
		"custom_servers": []any{
			customServer(testDNSServer, testDNSStamp, true),
			customServer(testDNSServerTwo, testDNSStampTwo, true),
		},
	})
	path := configFile(t, customDNSConfig())

	res := planEnv(t, withDNSStamp(r), path)
	if res.ExitCode != exitChangesPending {
		t.Fatalf("plan exited %d, want %d\nstdout: %s", res.ExitCode, exitChangesPending, res.Stdout)
	}
	stdout := string(res.Stdout)
	if !strings.Contains(stdout, "servers:") || !strings.Contains(stdout, testDNSServerTwo) {
		t.Errorf("the plan should name the resolver the file stopped naming, got:\n%s", stdout)
	}

	applyEnv(t, withDNSStamp(r), path)

	servers := r.dohServers(t)
	if len(servers) != 1 || servers[0]["server_name"] != testDNSServer {
		t.Errorf("the Controller holds %+v, want only the resolver the file names", servers)
	}
	assertNoChangesPendingEnv(t, withDNSStamp(r), path)
}

// And the other half of that rule: a file with no `servers:` key states nothing
// about the list, so unifig leaves every resolver on the Controller alone even
// while changing the setting around them (ADR-0004).
func TestASectionWithNoServerListLeavesTheControllersResolversAlone(t *testing.T) {
	r := startReplay(t)
	dnsCustom(t, r)
	path := configFile(t, "encrypted-dns:\n  state: off\n")

	res := applyEnv(t, r.env(), path)
	if !strings.Contains(string(res.Stdout), "custom -> off") {
		t.Errorf("apply should report the state it changed, got:\n%s", res.Stdout)
	}

	servers := r.dohServers(t)
	if len(servers) != 1 || servers[0]["sdns_stamp"] != testDNSStamp {
		t.Errorf("the Controller holds %+v, want the resolver the file said nothing about", servers)
	}
	assertNoChangesPendingEnv(t, r.env(), path)
}

// `servers: []` is the opposite instruction, and the difference between the two
// is a nil the loader is careful to preserve.
func TestAnEmptyServerListClearsTheControllersResolvers(t *testing.T) {
	r := startReplay(t)
	dnsCustom(t, r)
	path := configFile(t, "encrypted-dns:\n  servers: []\n")

	res := planEnv(t, r.env(), path)
	if res.ExitCode != exitChangesPending {
		t.Fatalf("plan exited %d, want %d — clearing the list is a change\nstdout: %s",
			res.ExitCode, exitChangesPending, res.Stdout)
	}
	if !strings.Contains(string(res.Stdout), "-> (none)") {
		t.Errorf("the plan should say the list is being emptied, got:\n%s", res.Stdout)
	}

	applyEnv(t, r.env(), path)

	if servers := r.dohServers(t); len(servers) != 0 {
		t.Errorf("the Controller still holds %+v, and the file said there should be none", servers)
	}
	assertNoChangesPendingEnv(t, r.env(), path)
}

// A file that says nothing about encrypted DNS is a file unifig does not read
// the setting for, let alone change it (ADR-0006).
func TestAFileWithNoEncryptedDNSSectionChangesNothingAboutIt(t *testing.T) {
	r := startReplay(t)
	dnsCustom(t, r)
	path := configFile(t, fmt.Sprintf("networks:\n  - name: %s\n", r.aNetwork(t)))

	assertNoChangesPendingEnv(t, r.env(), path)

	// And prune cannot reach it either: a Setting is not something prune can
	// see, whatever the file leaves out.
	res := planEnv(t, r.env(), "--prune", path)
	if strings.Contains(string(res.Stdout), "encrypted-dns") {
		t.Errorf("prune reached the Encrypted DNS setting, and a Setting is not prunable:\n%s", res.Stdout)
	}
}

// unifig can no more create this Setting than it can create an uplink. A
// Controller without one gets an explanation, not an attempt.
func TestAControllerWithNoEncryptedDNSSettingIsExplainedNotCreated(t *testing.T) {
	r := startReplay(t)
	r.withoutEncryptedDNS(t)
	path := configFile(t, customDNSConfig())

	res := planEnv(t, withDNSStamp(r), path)

	if res.ExitCode != 1 {
		t.Fatalf("plan exited %d, want 1\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}
	for _, fragment := range []string{"Encrypted DNS", "never creates"} {
		if !strings.Contains(string(res.Stderr), fragment) {
			t.Errorf("the error should mention %q, got:\n%s", fragment, res.Stderr)
		}
	}
	if strings.Contains(string(res.Stdout), "+ encrypted-dns") {
		t.Errorf("unifig planned to create a Setting:\n%s", res.Stdout)
	}
}

// The plan says the two things the config states without stating: a mode with
// nothing to resolve with, and resolvers that will not be used.
func TestThePlanSaysWhenTheConfigWouldEncryptNothing(t *testing.T) {
	r := startReplay(t)
	dnsOff(t, r)

	res := planEnv(t, r.env(), configFile(t, "encrypted-dns:\n  state: custom\n"))
	if !strings.Contains(string(res.Stdout), "nothing for this to encrypt with") {
		t.Errorf("plan should say that custom with no resolvers does nothing, got:\n%s", res.Stdout)
	}
}

func TestThePlanSaysWhenTheDeclaredResolversWillNotBeUsed(t *testing.T) {
	r := startReplay(t)
	dnsOff(t, r)
	path := configFile(t, fmt.Sprintf(`encrypted-dns:
  state: auto
  servers:
    - name: %s
      stamp: ${DNS_STAMP}
`, testDNSServer))

	res := planEnv(t, withDNSStamp(r), path)
	if !strings.Contains(string(res.Stdout), "will be stored and not used") {
		t.Errorf("plan should say the resolvers are not the ones in use, got:\n%s", res.Stdout)
	}
}

// Both notes are about the state and the resolver list together, so neither may
// depend on which of the two the operator happens to be changing. These are the
// two cases where the field the note reads best under is not in the plan at
// all, and the note has to find the one that is.
func TestANoteSurvivesWhenTheFieldItReadsBestUnderIsNotChanging(t *testing.T) {
	t.Run("the state is already custom and the file empties the list", func(t *testing.T) {
		r := startReplay(t)
		dnsCustom(t, r)
		path := configFile(t, "encrypted-dns:\n  state: custom\n  servers: []\n")

		res := planEnv(t, r.env(), path)
		if strings.Contains(string(res.Stdout), "state:") {
			t.Fatalf("the state is not changing, so this case is not the one under test:\n%s", res.Stdout)
		}
		if !strings.Contains(string(res.Stdout), "nothing for this to encrypt with") {
			t.Errorf("plan should still say the setting will encrypt nothing, got:\n%s", res.Stdout)
		}
	})

	t.Run("the state is already auto and only a stamp rotates", func(t *testing.T) {
		r := startReplay(t)
		r.seedDoH(t, map[string]any{
			"state":          "auto",
			"custom_servers": []any{customServer(testDNSServer, testDNSStampTwo, true)},
		})
		path := configFile(t, customDNSConfigIn("auto"))

		res := planEnv(t, withDNSStamp(r), path)
		if strings.Contains(string(res.Stdout), "servers:") {
			t.Fatalf("the list is not changing, so this case is not the one under test:\n%s", res.Stdout)
		}
		if !strings.Contains(string(res.Stdout), "will be stored and not used") {
			t.Errorf("plan should still say the resolver is not the one in use, got:\n%s", res.Stdout)
		}
	})
}

// Export is the adoption path, and a stamp is a secret: the file it writes is
// committable as it stands, and planning it changes nothing.
func TestExportRedactsTheStampAndWhatItWroteThenPlansClean(t *testing.T) {
	r := startReplay(t)
	dnsCustom(t, r)

	exported := exportEnv(t, r.env())
	cfg := exportedYAML(t, exported.Stdout)

	if cfg.EncryptedDNS == nil {
		t.Fatalf("export wrote no encrypted-dns section:\n%s", exported.Stdout)
	}
	if cfg.EncryptedDNS.State != "custom" {
		t.Errorf("export wrote state %q, want the one the Controller holds", cfg.EncryptedDNS.State)
	}
	if len(cfg.EncryptedDNS.Servers) != 1 {
		t.Fatalf("export wrote %d resolvers, want 1:\n%s", len(cfg.EncryptedDNS.Servers), exported.Stdout)
	}
	name, ok := envReference(cfg.EncryptedDNS.Servers[0].Stamp)
	if !ok {
		t.Fatalf("export wrote the stamp as %q rather than as a ${VAR} reference",
			cfg.EncryptedDNS.Servers[0].Stamp)
	}
	if !strings.Contains(string(exported.Stderr), name) {
		t.Errorf("export should tell the operator to set %s, got:\n%s", name, exported.Stderr)
	}
	assertNoStampIn(t, "the export", exported.Stdout, exported.Stderr)

	res := planExportedConfig(t, r, exported.Stdout)
	if res.ExitCode != exitNoChanges {
		t.Fatalf("plan of the exported config exited %d, want %d — unifig will not read back what it wrote\nexported:\n%s\nstderr: %s",
			res.ExitCode, exitNoChanges, exported.Stdout, res.Stderr)
	}
}

// --with-secrets is the operator asking for the values, and the point of the
// flag is that it is the one they have to ask for.
func TestExportWithSecretsWritesTheStampInline(t *testing.T) {
	r := startReplay(t)
	dnsCustom(t, r)

	exported := exportEnv(t, r.env(), "--with-secrets")
	cfg := exportedYAML(t, exported.Stdout)

	if len(cfg.EncryptedDNS.Servers) != 1 || cfg.EncryptedDNS.Servers[0].Stamp != testDNSStamp {
		t.Errorf("export --with-secrets wrote %+v, want the stamp the Controller holds", cfg.EncryptedDNS)
	}
}

// The one test that seeds nothing. Everything else here puts the setting into
// the state it needs first, which is the discipline that keeps this suite from
// depending on which router the recording came from — but it also means every
// other assertion is against values the test itself wrote, and a UDR that
// spelled `sdns_stamp` or `server_name` differently would sail through all of
// them.
//
// So this one reads the recording as it stands and asks the stand-in what it
// holds, rather than naming anything. It is the assertion that the shape
// recorded off the real router is the shape unifig's projection reads.
func TestExportDescribesTheRecordedSettingWithoutBeingToldWhatItHolds(t *testing.T) {
	r := startReplay(t)

	recorded := r.dohServers(t)
	if len(recorded) == 0 {
		t.Skip("the recording holds no custom DNS server, so there is no recorded resolver to describe")
	}

	exported := exportEnv(t, r.env())
	cfg := exportedYAML(t, exported.Stdout)
	if cfg.EncryptedDNS == nil {
		t.Fatalf("export wrote no encrypted-dns section for a Controller that has the setting:\n%s", exported.Stdout)
	}

	if want := r.doh(t)["state"]; cfg.EncryptedDNS.State != want {
		t.Errorf("export wrote state %q, want the recording's %v", cfg.EncryptedDNS.State, want)
	}
	if len(cfg.EncryptedDNS.Servers) != len(recorded) {
		t.Fatalf("export wrote %d resolvers, want the recording's %d:\n%s",
			len(cfg.EncryptedDNS.Servers), len(recorded), exported.Stdout)
	}
	for _, server := range recorded {
		name, _ := server["server_name"].(string)
		written, found := exportedServer(cfg.EncryptedDNS.Servers, name)
		if !found {
			t.Fatalf("export left out the recorded resolver %q:\n%s", name, exported.Stdout)
		}
		// Redacted, so what proves the stamp was read is the variable standing
		// where it was — and that putting the recorded value back plans clean.
		if _, ok := envReference(written.Stamp); !ok {
			t.Errorf("export wrote %q's stamp as %q rather than as a ${VAR} reference", name, written.Stamp)
		}
	}

	res := planExportedConfig(t, r, exported.Stdout)
	if res.ExitCode != exitNoChanges {
		t.Fatalf("plan of the exported recording exited %d, want %d — the recorded shape does not round-trip\nexported:\n%s\nstderr: %s",
			res.ExitCode, exitNoChanges, exported.Stdout, res.Stderr)
	}
}

func exportedServer(servers []exportedDNSServer, name string) (exportedDNSServer, bool) {
	for _, server := range servers {
		if server.Name == name {
			return server, true
		}
	}
	return exportedDNSServer{}, false
}

// A Controller with the setting but no resolvers has to export as `servers: []`
// — the file saying the list is empty. Dropping the key would say the opposite,
// that the list is not unifig's to manage, and the round trip would quietly
// stop describing the Controller it came from.
func TestExportWritesAnEmptyServerListRatherThanNoList(t *testing.T) {
	r := startReplay(t)
	dnsOff(t, r)

	exported := exportEnv(t, r.env())

	if !strings.Contains(string(exported.Stdout), "servers: []") {
		t.Errorf("export wrote no empty server list for a Controller that has none:\n%s", exported.Stdout)
	}
	cfg := exportedYAML(t, exported.Stdout)
	if cfg.EncryptedDNS == nil || cfg.EncryptedDNS.Servers == nil {
		t.Fatalf("the exported section does not state the list: %+v", cfg.EncryptedDNS)
	}

	// And the round trip: seeding a resolver behind that file's back makes it a
	// change, which is the proof the exported file really does manage the list.
	// The environment is the one export asked for, since the file it wrote
	// redacts every secret on the Controller, not only this section's.
	env := exportedSecretEnv(t, r, exported.Stdout)
	dnsCustom(t, r)
	path := configFile(t, string(exported.Stdout))
	res := planEnv(t, env, path)
	if res.ExitCode != exitChangesPending {
		t.Errorf("plan of the exported file exited %d, want %d — the file does not manage the list it wrote\nstdout: %s",
			res.ExitCode, exitChangesPending, res.Stdout)
	}
}

// Two live resolvers under one name is the ambiguity a Resource gets refused
// for, and it bites harder here: matching by name, unifig would collapse the
// pair into one on the next apply, and export would write a file its own
// validate rejects.
func TestTwoLiveResolversSharingANameStopEverythingRatherThanCollapse(t *testing.T) {
	r := startReplay(t)
	r.seedDoH(t, map[string]any{
		"state": "custom",
		"custom_servers": []any{
			customServer(testDNSServer, testDNSStamp, true),
			customServer(testDNSServer, testDNSStampTwo, true),
		},
	})

	res := planEnv(t, withDNSStamp(r), configFile(t, customDNSConfig()))
	if res.ExitCode != 1 {
		t.Fatalf("plan exited %d, want 1 — unifig cannot tell the two resolvers apart\nstdout: %s", res.ExitCode, res.Stdout)
	}
	if !strings.Contains(string(res.Stderr), testDNSServer) {
		t.Errorf("the error should name the resolver it cannot tell apart, got:\n%s", res.Stderr)
	}

	// Export stops on the same ambiguity rather than writing YAML that names one
	// resolver twice, which is exactly what its own validate refuses.
	exported := testRig.runUnifig(t, []string{"export"}, r.env())
	if exported.ExitCode != 1 {
		t.Errorf("export exited %d, want 1 — it wrote config naming one resolver twice\nstdout: %s",
			exported.ExitCode, exported.Stdout)
	}
	if len(exported.Stdout) != 0 {
		t.Errorf("stdout should stay empty rather than carry unusable config, got: %s", exported.Stdout)
	}
}

// A state unifig does not model is left out of the file rather than copied
// through: the schema's `state` is a closed set, so writing it would produce a
// file unifig's own validate rejects on the next firmware that adds one.
func TestAnUnmodelledStateIsLeftOutOfTheExportAndSaidOutLoud(t *testing.T) {
	r := startReplay(t)
	r.seedDoH(t, map[string]any{"state": "quantum", "custom_servers": []any{}})

	exported := exportEnv(t, r.env())

	cfg := exportedYAML(t, exported.Stdout)
	if cfg.EncryptedDNS == nil {
		t.Fatalf("export dropped the whole section over one unmodelled field:\n%s", exported.Stdout)
	}
	if cfg.EncryptedDNS.State != "" {
		t.Errorf("export wrote state %q, which its own schema does not allow", cfg.EncryptedDNS.State)
	}
	if !strings.Contains(string(exported.Stderr), "quantum") {
		t.Errorf("export should say which state it could not describe, got:\n%s", exported.Stderr)
	}

	// The file it wrote is one validate accepts — the whole point of leaving the
	// field out. It is validated with the secrets export told the operator to
	// set and without the connection config, so what passes here is the file
	// itself rather than anything unifig could have gone back and asked.
	path := configFile(t, string(exported.Stdout))
	offline := exportedSecretEnv(t, r, exported.Stdout)
	offline["UNIFIG_HOST"], offline["UNIFIG_API_KEY"] = "", ""
	res := testRig.runUnifig(t, []string{"validate", path}, offline)
	if res.ExitCode != 0 {
		t.Errorf("validate rejected export's own output (exit %d)\nstderr: %s\nexported:\n%s",
			res.ExitCode, res.Stderr, exported.Stdout)
	}
}

// A Controller with no such setting is described by a file that leaves the
// section out — and export says so, rather than letting the operator read the
// gap as unifig having forgotten.
func TestExportSaysWhenThereIsNoEncryptedDNSSettingToDescribe(t *testing.T) {
	r := startReplay(t)
	r.withoutEncryptedDNS(t)

	exported := exportEnv(t, r.env())

	if cfg := exportedYAML(t, exported.Stdout); cfg.EncryptedDNS != nil {
		t.Errorf("export invented an encrypted-dns section: %+v", cfg.EncryptedDNS)
	}
	if !strings.Contains(string(exported.Stderr), "no Encrypted DNS setting") {
		t.Errorf("export should say why the section is missing, got:\n%s", exported.Stderr)
	}
}

// exportEnv runs an export against the stand-in and fails if it did not
// succeed, which is what every test here needs before it can read the output.
func exportEnv(t *testing.T, env map[string]string, args ...string) result {
	t.Helper()
	res := testRig.runUnifig(t, append([]string{"export"}, args...), env)
	t.Logf("unifig export %v -> exit %d\n%s\n%s", args, res.ExitCode, res.Stdout, res.Stderr)
	if res.ExitCode != 0 {
		t.Fatalf("unifig export exited %d\nstderr: %s", res.ExitCode, res.Stderr)
	}
	return res
}

// assertNoStampIn fails the test if a stamp appears anywhere in what unifig
// wrote. Every stream is checked rather than the one under suspicion: a secret
// that leaks onto the wrong stream has still leaked.
func assertNoStampIn(t *testing.T, what string, streams ...[]byte) {
	t.Helper()
	for _, stream := range streams {
		for _, stamp := range []string{testDNSStamp, testDNSStampTwo} {
			if strings.Contains(string(stream), stamp) {
				t.Errorf("%s printed a DNS stamp:\n%s", what, stream)
			}
		}
	}
}
