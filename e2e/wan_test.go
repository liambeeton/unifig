package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// WAN slots are the first Setting, and these tests state what that word means
// where an operator can see it: unifig updates a slot the router already has,
// never invents or removes one, and stops to ask before touching the connection
// the whole site depends on.
//
// They run against the recorded stand-in rather than the dockerized Controller
// (see replay_test.go), and nothing else about them changes: the real binary,
// the same base URL, the same assertions on what a shell would see and on what
// the Controller holds afterwards.

// The PPPoE password used throughout. Like the WLAN suite's passphrase it is a
// fixture rather than a secret, and every assertion treats it as one — a value
// that never appeared in the first place could not prove it stays out of the
// output.
const testWANPassword = "correct-horse-battery"

// withWANPassword is the environment a config with a ${WAN_PASSWORD} reference
// needs, pointed at the stand-in.
func withWANPassword(r *replay) map[string]string {
	env := r.env()
	env["WAN_PASSWORD"] = testWANPassword
	return env
}

// dhcpWAN is a slot as a router hands it over: an uplink on DHCP, with no
// credentials on it.
func dhcpWAN(t *testing.T, r *replay) {
	t.Helper()
	r.seedSlot(t, "WAN", map[string]any{
		"wan_type":                   "dhcp",
		"wan_username":               "",
		"x_wan_password":             "",
		"wan_pppoe_username_enabled": false,
		"wan_pppoe_password_enabled": false,
	})
}

// pppoeConfig moves the primary slot onto PPPoE with credentials from the
// environment — the change this whole issue exists for.
const pppoeConfig = `wan:
  - slot: WAN
    type: pppoe
    username: isp-user
    password: ${WAN_PASSWORD}
`

func TestPlanShowsAWANSlotToUpdateAndNeverPrintsItsPassword(t *testing.T) {
	r := startReplay(t)
	dhcpWAN(t, r)
	path := configFile(t, pppoeConfig)

	res := planEnv(t, withWANPassword(r), path)

	if res.ExitCode != exitChangesPending {
		t.Fatalf("plan exited %d, want %d\nstderr: %s", res.ExitCode, exitChangesPending, res.Stderr)
	}
	stdout := string(res.Stdout)
	for _, fragment := range []string{
		`~ wan "WAN"`, "dhcp -> pppoe", "isp-user", "password", "(hidden)", "1 to update",
	} {
		if !strings.Contains(stdout, fragment) {
			t.Errorf("plan output should mention %q, got:\n%s", fragment, stdout)
		}
	}
	// The plan is where an operator finds out this one is dangerous, before
	// anyone asks them to approve anything.
	if !strings.Contains(stdout, "! ") || !strings.Contains(stdout, "internet") {
		t.Errorf("plan should say what a WAN change risks, got:\n%s", stdout)
	}
	assertNoWANPasswordIn(t, "the plan", res.Stdout, res.Stderr)
}

// A pipeline gating on the changes that can cut a site off the internet needs
// to see which ones those are without keeping its own list of dangerous kinds.
func TestPlanJSONMarksAWANChangeAsRiskyAndItsPasswordAsSecret(t *testing.T) {
	r := startReplay(t)
	dhcpWAN(t, r)
	path := configFile(t, pppoeConfig)

	res := planEnv(t, withWANPassword(r), "--json", path)

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
	if change.Action != "update" || change.Kind != "wan" || change.Name != "WAN" {
		t.Errorf("the change is not an update to the WAN slot: %+v", change)
	}
	if change.Risk == "" {
		t.Errorf("a WAN change carries no risk in the JSON: %+v", change)
	}
	for _, field := range change.Fields {
		if field.Name != "password" {
			continue
		}
		if !field.Secret || field.From != nil || field.To != nil {
			t.Errorf("the password field is not redacted: %+v", field)
		}
	}
	assertNoWANPasswordIn(t, "plan --json", res.Stdout, res.Stderr)
}

func TestApplyMovesAWANSlotOntoPPPoEAndTheNextPlanIsEmpty(t *testing.T) {
	r := startReplay(t)
	dhcpWAN(t, r)
	path := configFile(t, pppoeConfig)

	res := applyEnv(t, withWANPassword(r), "--allow-risky", path)
	if !strings.Contains(string(res.Stdout), `~ wan "WAN" updated`) {
		t.Errorf("apply should report what it updated, got:\n%s", res.Stdout)
	}
	assertNoWANPasswordIn(t, "the apply", res.Stdout, res.Stderr)

	live := r.slot(t, "WAN")
	if live["wan_type"] != "pppoe" {
		t.Errorf("wan_type = %#v, want %q", live["wan_type"], "pppoe")
	}
	if live["wan_username"] != "isp-user" {
		t.Errorf("wan_username = %#v, want the one the config named", live["wan_username"])
	}
	if live["x_wan_password"] != testWANPassword {
		t.Errorf("the Controller holds password %#v, want the one the environment supplied", live["x_wan_password"])
	}
	// A credential the Controller is told to ignore is an uplink that quietly
	// fails to sign in, so writing one switches its flag on.
	for _, flag := range []string{"wan_pppoe_username_enabled", "wan_pppoe_password_enabled"} {
		if live[flag] != true {
			t.Errorf("%s = %#v, so the credential unifig wrote would not be used", flag, live[flag])
		}
	}

	assertNoChangesPendingEnv(t, withWANPassword(r), path)
}

// The Risky-change contract: an apply the operator already approved wholesale
// still stops at a WAN change and asks about that one on its own.
func TestApplyAsksAboutAWANChangeEvenWhenItIsAutoApproved(t *testing.T) {
	r := startReplay(t)
	dhcpWAN(t, r)
	path := configFile(t, pppoeConfig)

	res := testRig.runUnifigWithInput(t,
		[]string{"apply", "--auto-approve", path}, withWANPassword(r), "y\n")
	t.Logf("unifig apply --auto-approve -> exit %d\n%s\n%s", res.ExitCode, res.Stdout, res.Stderr)

	if res.ExitCode != 0 {
		t.Fatalf("apply exited %d\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}
	stdout := string(res.Stdout)
	if !strings.Contains(stdout, "Risky change") || !strings.Contains(stdout, "[y/N]") {
		t.Errorf("--auto-approve should still ask about the WAN change, got:\n%s", stdout)
	}
	if live := r.slot(t, "WAN"); live["wan_type"] != "pppoe" {
		t.Errorf("the operator said yes and the slot did not change: %#v", live["wan_type"])
	}
}

// Two questions, two answers, one stdin. It reads like a formality and is not:
// a reader per question would buffer whatever followed the first answer and
// throw it away, and the second question would take the silence for a no.
func TestAnOperatorAnswersBothQuestionsOnOneStdin(t *testing.T) {
	r := startReplay(t)
	dhcpWAN(t, r)
	path := configFile(t, pppoeConfig)

	res := testRig.runUnifigWithInput(t, []string{"apply", path}, withWANPassword(r), "y\ny\n")
	t.Logf("unifig apply -> exit %d\n%s\n%s", res.ExitCode, res.Stdout, res.Stderr)

	if res.ExitCode != 0 {
		t.Fatalf("apply exited %d\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}
	stdout := string(res.Stdout)
	if !strings.Contains(stdout, "Apply these changes") || !strings.Contains(stdout, "Risky change") {
		t.Errorf("apply should have asked twice, got:\n%s", stdout)
	}
	if live := r.slot(t, "WAN"); live["wan_type"] != "pppoe" {
		t.Errorf("the operator approved both questions and the slot did not change: %#v", live["wan_type"])
	}
}

// Refusing one is not cancelling the apply: the question was about that change,
// and the rest of the file was still asked for. Nothing is hard-blocked either
// — the same run with --allow-risky applies it.
func TestARefusedWANChangeIsSkippedAndTheRestIsApplied(t *testing.T) {
	r := startReplay(t)
	dhcpWAN(t, r)
	path := configFile(t, `networks:
  - name: Default
    subnet: 192.168.1.1/24
    vlan: 40
`+pppoeConfig)

	res := testRig.runUnifigWithInput(t,
		[]string{"apply", "--auto-approve", path}, withWANPassword(r), "n\n")
	t.Logf("unifig apply --auto-approve -> exit %d\n%s\n%s", res.ExitCode, res.Stdout, res.Stderr)

	if res.ExitCode != 0 {
		t.Fatalf("refusing a Risky change should not be a failure: exit %d\nstdout: %s\nstderr: %s",
			res.ExitCode, res.Stdout, res.Stderr)
	}
	stdout := string(res.Stdout)
	for _, fragment := range []string{`~ network "Default" updated`, `wan "WAN"`, "--allow-risky"} {
		if !strings.Contains(stdout, fragment) {
			t.Errorf("apply should report %q, got:\n%s", fragment, stdout)
		}
	}
	if live := r.slot(t, "WAN"); live["wan_type"] != "dhcp" {
		t.Errorf("the operator said no and the slot changed anyway: %#v", live["wan_type"])
	}

	// Never hard-blocked: the operator who means it has a way to say so, and it
	// is a flag rather than an argument with the tool.
	applyEnv(t, withWANPassword(r), "--allow-risky", path)
	if live := r.slot(t, "WAN"); live["wan_type"] != "pppoe" {
		t.Errorf("--allow-risky did not apply the change: %#v", live["wan_type"])
	}
}

// An apply left running unattended has nobody to answer, and EOF is not a yes.
func TestAWANChangeWithNoOneToAskIsLeftUnapplied(t *testing.T) {
	r := startReplay(t)
	dhcpWAN(t, r)
	path := configFile(t, pppoeConfig)

	res := testRig.runUnifig(t, []string{"apply", "--auto-approve", path}, withWANPassword(r))
	t.Logf("unifig apply --auto-approve -> exit %d\n%s\n%s", res.ExitCode, res.Stdout, res.Stderr)

	if live := r.slot(t, "WAN"); live["wan_type"] != "dhcp" {
		t.Errorf("a WAN slot changed with nobody there to approve it: %#v", live["wan_type"])
	}
	if !strings.Contains(string(res.Stdout), "--allow-risky") {
		t.Errorf("apply should say how to approve it in advance, got:\n%s", res.Stdout)
	}
}

// A Setting is update-only, and a slot the router does not have is the case
// where that stops being a definition and starts being an error message.
func TestPlanNeverProposesCreatingAWANSlot(t *testing.T) {
	r := startReplay(t)
	r.removeSlot(t, "WAN2")
	path := configFile(t, `wan:
  - slot: WAN2
    type: dhcp
`)

	res := planEnv(t, r.env(), path)

	if res.ExitCode != exitError {
		t.Fatalf("plan exited %d, want %d — it has an opinion about a slot the Controller does not have\nstdout: %s",
			res.ExitCode, exitError, res.Stdout)
	}
	stderr := string(res.Stderr)
	for _, fragment := range []string{"WAN2", "never creates", `"WAN"`} {
		if !strings.Contains(stderr, fragment) {
			t.Errorf("stderr should mention %q, got: %s", fragment, stderr)
		}
	}
}

// Prune deletes Resources of a managed type. A WAN slot is not one — it is a
// Setting, and the difference here is the difference between deleting a spare
// VLAN and taking the site off the internet.
func TestPruneNeverDeletesAWANSlotTheConfigDoesNotName(t *testing.T) {
	r := startReplay(t)
	path := configFile(t, `networks:
  - name: Default
wan:
  - slot: WAN
`)

	res := planEnv(t, r.env(), "--prune", path)

	if res.ExitCode != exitNoChanges {
		t.Fatalf("plan --prune exited %d, want %d\nstdout: %s\nstderr: %s",
			res.ExitCode, exitNoChanges, res.Stdout, res.Stderr)
	}
	if strings.Contains(string(res.Stdout), "WAN2") {
		t.Errorf("plan --prune has an opinion about a WAN slot the config does not name:\n%s", res.Stdout)
	}
}

// unifig owns the fields its config models and nothing else. A WAN slot carries
// the ISP's DNS servers, its failover priority and its VLAN tag, and an operator
// who changed a PPPoE password must not lose any of them.
func TestApplyLeavesWANSettingsUnifigDoesNotModelAlone(t *testing.T) {
	r := startReplay(t)
	dhcpWAN(t, r)
	r.seedSlot(t, "WAN", map[string]any{
		"wan_dns_preference":    "manual",
		"wan_dns1":              "1.1.1.1",
		"wan_failover_priority": 3,
		"wan_vlan_enabled":      true,
		"wan_vlan":              911,
	})
	path := configFile(t, pppoeConfig)

	applyEnv(t, withWANPassword(r), "--allow-risky", path)

	live := r.slot(t, "WAN")
	if live["wan_type"] != "pppoe" {
		t.Fatalf("the change under test did not happen: wan_type = %#v", live["wan_type"])
	}
	for field, want := range map[string]any{
		"wan_dns_preference":    "manual",
		"wan_dns1":              "1.1.1.1",
		"wan_failover_priority": float64(3),
		"wan_vlan_enabled":      true,
		"wan_vlan":              float64(911),
	} {
		if live[field] != want {
			t.Errorf("%s = %#v after apply, want %#v — unifig does not model this field and must not touch it",
				field, live[field], want)
		}
	}
}

// The config says `type: pppoe` and stops; what it does not say is that a PPPoE
// uplink with no credentials anywhere cannot sign in. So the plan says it,
// before anyone approves anything.
func TestPlanWarnsThatAPPPoESlotWithNoCredentialsCannotSignIn(t *testing.T) {
	r := startReplay(t)
	dhcpWAN(t, r)
	path := configFile(t, `wan:
  - slot: WAN
    type: pppoe
`)

	res := planEnv(t, r.env(), path)

	if res.ExitCode != exitChangesPending {
		t.Fatalf("plan exited %d, want %d\nstderr: %s", res.ExitCode, exitChangesPending, res.Stderr)
	}
	if !strings.Contains(string(res.Stdout), "sign in") {
		t.Errorf("plan should warn that this uplink has no credentials, got:\n%s", res.Stdout)
	}
}

func TestExportWritesTheWANSlotsAndRedactsThePPPoEPassword(t *testing.T) {
	r := startReplay(t)
	r.seedSlot(t, "WAN", map[string]any{
		"wan_type": "pppoe", "wan_username": "isp-user", "x_wan_password": testWANPassword,
	})

	res := testRig.runUnifig(t, []string{"export"}, r.env())
	if res.ExitCode != 0 {
		t.Fatalf("export exited %d\nstderr: %s", res.ExitCode, res.Stderr)
	}

	stdout := string(res.Stdout)
	for _, fragment := range []string{
		"wan:", "slot: WAN", "type: pppoe", "username: isp-user",
		"password: ${UNIFIG_WAN_PASSWORD}", "slot: WAN2", "type: disabled",
	} {
		if !strings.Contains(stdout, fragment) {
			t.Errorf("export should write %q, got:\n%s", fragment, stdout)
		}
	}
	if !strings.Contains(string(res.Stderr), "UNIFIG_WAN_PASSWORD") {
		t.Errorf("export should say which variable to set, got: %s", res.Stderr)
	}
	assertNoWANPasswordIn(t, "the export", res.Stdout)

	// And the plaintext form is there for the operator who asks for it.
	with := testRig.runUnifig(t, []string{"export", "--with-secrets"}, r.env())
	if !strings.Contains(string(with.Stdout), testWANPassword) {
		t.Errorf("export --with-secrets should write the password inline, got:\n%s", with.Stdout)
	}
}

// The brownfield path, with an uplink in it: what export writes plans clean,
// down to the PPPoE password it read back off the Controller.
func TestExportedConfigWithAWANSlotPlansClean(t *testing.T) {
	r := startReplay(t)
	r.seedSlot(t, "WAN", map[string]any{
		"wan_type": "pppoe", "wan_username": "isp-user", "x_wan_password": testWANPassword,
	})

	exported := testRig.runUnifig(t, []string{"export"}, r.env())
	if exported.ExitCode != 0 {
		t.Fatalf("export exited %d\nstderr: %s", exported.ExitCode, exported.Stderr)
	}
	path := filepath.Join(t.TempDir(), "unifig.yaml")
	if err := os.WriteFile(path, exported.Stdout, 0o600); err != nil {
		t.Fatalf("writing exported config: %v", err)
	}

	// The secrets export redacted have to come back from the environment, which
	// is the workflow the file it wrote describes.
	env := r.env()
	env["UNIFIG_WAN_PASSWORD"] = testWANPassword
	env["UNIFIG_WLAN_HOME_PASSPHRASE"] = "recorded-wlan-passphrase"

	res := planEnv(t, env, path)
	if res.ExitCode != exitNoChanges {
		t.Fatalf("plan of a freshly exported config exited %d, want %d\nexported:\n%s\nplan:\n%s",
			res.ExitCode, exitNoChanges, exported.Stdout, res.Stdout)
	}
}

// A slot that connects in a way unifig does not model is written as its slot
// and nothing else — the WAN spelling of `- name: IoT`. Saying so is what stops
// the file reading as though unifig had taken charge of it.
func TestASlotUnifigCannotDescribeIsExportedAsItsSlotAlone(t *testing.T) {
	r := startReplay(t)
	r.seedSlot(t, "WAN2", map[string]any{
		"wan_type": "static", "wan_ip": "203.0.113.9",
		"wan_netmask": "255.255.255.0", "wan_gateway": "203.0.113.1",
	})

	exported := testRig.runUnifig(t, []string{"export"}, r.env())
	if exported.ExitCode != 0 {
		t.Fatalf("export exited %d — one slot it cannot describe should not stop it\nstderr: %s",
			exported.ExitCode, exported.Stderr)
	}
	stdout := string(exported.Stdout)
	if !strings.Contains(stdout, "slot: WAN2") {
		t.Errorf("export should still say the slot exists, got:\n%s", stdout)
	}
	if strings.Contains(stdout, "static") {
		t.Errorf("export wrote a connection type the schema does not accept:\n%s", stdout)
	}
	if !strings.Contains(string(exported.Stderr), "WAN2") {
		t.Errorf("export should say which slot it could only name, got: %s", exported.Stderr)
	}

	// And what it wrote is still config that plans clean: a slot unifig manages
	// nothing about is not a change.
	path := filepath.Join(t.TempDir(), "unifig.yaml")
	if err := os.WriteFile(path, exported.Stdout, 0o600); err != nil {
		t.Fatalf("writing exported config: %v", err)
	}
	env := r.env()
	env["UNIFIG_WLAN_HOME_PASSPHRASE"] = "recorded-wlan-passphrase"
	if res := planEnv(t, env, path); res.ExitCode != exitNoChanges {
		t.Fatalf("plan of the exported config exited %d, want %d\nplan:\n%s",
			res.ExitCode, exitNoChanges, res.Stdout)
	}
}

// Which slots a router has is the router's answer, not unifig's. A gateway with
// a third uplink has to export and plan like any other, or the brownfield path
// would end at "unifig wrote you a file it will not read back".
func TestASlotUnifigHasNeverHeardOfStillRoundTrips(t *testing.T) {
	r := startReplay(t)
	r.addSlot(t, "WAN3", map[string]any{"wan_type": "dhcp"})

	exported := testRig.runUnifig(t, []string{"export"}, r.env())
	if exported.ExitCode != 0 {
		t.Fatalf("export exited %d\nstderr: %s", exported.ExitCode, exported.Stderr)
	}
	if !strings.Contains(string(exported.Stdout), "slot: WAN3") {
		t.Errorf("export left out an uplink the router has:\n%s", exported.Stdout)
	}

	path := filepath.Join(t.TempDir(), "unifig.yaml")
	if err := os.WriteFile(path, exported.Stdout, 0o600); err != nil {
		t.Fatalf("writing exported config: %v", err)
	}
	env := r.env()
	env["UNIFIG_WLAN_HOME_PASSPHRASE"] = "recorded-wlan-passphrase"

	res := planEnv(t, env, path)
	if res.ExitCode != exitNoChanges {
		t.Fatalf("plan of the exported config exited %d, want %d — unifig will not read back what it wrote\nexported:\n%s\nstderr: %s",
			res.ExitCode, exitNoChanges, exported.Stdout, res.Stderr)
	}
}

// assertNoWANPasswordIn fails the test if the PPPoE password appears anywhere in
// what unifig wrote. Every stream is checked rather than the one under
// suspicion: a secret that leaks onto the wrong stream has still leaked.
func assertNoWANPasswordIn(t *testing.T, what string, streams ...[]byte) {
	t.Helper()
	for _, stream := range streams {
		if strings.Contains(string(stream), testWANPassword) {
			t.Errorf("%s printed the PPPoE password:\n%s", what, stream)
		}
	}
}
