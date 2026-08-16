package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
//
// Nothing here names a slot, a WLAN or a secret that only the committed
// recording supplies. Which uplinks a router has is the router's answer, so
// these tests ask the recording for it — replay.slots, replay.aSlot,
// replay.absentSlot — or seed what they need onto a slot every router has. That
// is what makes re-recording from a real UDR a matter of dropping the files in,
// which is the promise testdata/udr/README.md makes to whoever does it.

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

// dhcpSlot puts a slot in the state a router hands one over in — an uplink on
// DHCP, with no credentials on it — and names the slot it used, which the test
// then writes into its config.
//
// The slot is whichever one the recording holds first rather than a name
// written down here: what these tests state is true of any uplink, and a
// recording is one router's answer about which uplinks it has.
func dhcpSlot(t *testing.T, r *replay) string {
	t.Helper()
	slot := r.aSlot(t)
	r.seedSlot(t, slot, map[string]any{
		"wan_type":                   "dhcp",
		"wan_username":               "",
		"x_wan_password":             "",
		"wan_pppoe_username_enabled": false,
		"wan_pppoe_password_enabled": false,
	})
	return slot
}

// pppoeSlot is the other starting state: an uplink already signed in with the
// password every assertion here watches for.
func pppoeSlot(t *testing.T, r *replay) string {
	t.Helper()
	slot := r.aSlot(t)
	r.seedSlot(t, slot, map[string]any{
		"wan_type": "pppoe", "wan_username": "isp-user", "x_wan_password": testWANPassword,
	})
	return slot
}

// pppoeConfig moves a slot onto PPPoE with credentials from the environment —
// the change this whole issue exists for.
func pppoeConfig(slot string) string {
	return fmt.Sprintf(`wan:
  - slot: %s
    type: pppoe
    username: isp-user
    password: ${WAN_PASSWORD}
`, slot)
}

// exportedSecretEnv is the environment a freshly exported config needs in order
// to plan: every secret export redacted, put back to the value the Controller
// holds for it.
//
// The variable names are read out of export's own output rather than written
// down again here. Export names them after the WLAN or the slot they belong to
// and de-duplicates the result, so a second copy of that rule in the test suite
// is a thing that can drift — and it would only ever be right for the WLANs and
// slots the committed recording happens to hold. What the file says is `${VAR}`
// beside the natural key of the thing the secret belongs to, which is all it
// takes to ask the recording for the value.
func exportedSecretEnv(t *testing.T, r *replay, exported []byte) map[string]string {
	t.Helper()

	cfg := exportedYAML(t, exported)
	env := r.env()
	for _, wlan := range cfg.WLANs {
		if name, ok := envReference(wlan.Passphrase); ok {
			env[name] = r.wlanPassphrase(t, wlan.Name)
		}
	}
	for _, slot := range cfg.WAN {
		if name, ok := envReference(slot.Password); ok {
			env[name] = r.slotPassword(t, slot.Slot)
		}
	}
	return env
}

// planExportedConfig is the brownfield path as an operator walks it: the file
// export wrote, saved where it told them to save it, planned with the secrets
// it told them to set. Three tests end this way, and what differs between them
// is what they put on the Controller first.
func planExportedConfig(t *testing.T, r *replay, exported []byte) result {
	t.Helper()
	path := filepath.Join(t.TempDir(), "unifig.yaml")
	if err := os.WriteFile(path, exported, 0o600); err != nil {
		t.Fatalf("writing exported config: %v", err)
	}
	return planEnv(t, exportedSecretEnv(t, r, exported), path)
}

// envReference reads the variable name out of a `${VAR}` reference and reports
// whether the value was one. A value that is not a reference is not an error
// here — what to make of a secret sitting in the file in the clear is the
// caller's to decide, and they do not all decide the same way.
func envReference(value string) (string, bool) {
	name, ok := strings.CutPrefix(value, "${")
	if !ok {
		return "", false
	}
	return strings.CutSuffix(name, "}")
}

func TestPlanShowsAWANSlotToUpdateAndNeverPrintsItsPassword(t *testing.T) {
	r := startReplay(t)
	slot := dhcpSlot(t, r)
	path := configFile(t, pppoeConfig(slot))

	res := planEnv(t, withWANPassword(r), path)

	if res.ExitCode != exitChangesPending {
		t.Fatalf("plan exited %d, want %d\nstderr: %s", res.ExitCode, exitChangesPending, res.Stderr)
	}
	stdout := string(res.Stdout)
	for _, fragment := range []string{
		fmt.Sprintf("~ wan %q", slot), "dhcp -> pppoe", "isp-user", "password", "(hidden)", "1 to update",
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
	slot := dhcpSlot(t, r)
	path := configFile(t, pppoeConfig(slot))

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
	if change.Action != "update" || change.Kind != "wan" || change.Name != slot {
		t.Errorf("the change is not an update to the %s slot: %+v", slot, change)
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
	slot := dhcpSlot(t, r)
	path := configFile(t, pppoeConfig(slot))

	res := applyEnv(t, withWANPassword(r), "--allow-risky", path)
	if !strings.Contains(string(res.Stdout), fmt.Sprintf("~ wan %q updated", slot)) {
		t.Errorf("apply should report what it updated, got:\n%s", res.Stdout)
	}
	assertNoWANPasswordIn(t, "the apply", res.Stdout, res.Stderr)

	live := r.slot(t, slot)
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
	slot := dhcpSlot(t, r)
	path := configFile(t, pppoeConfig(slot))

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
	if live := r.slot(t, slot); live["wan_type"] != "pppoe" {
		t.Errorf("the operator said yes and the slot did not change: %#v", live["wan_type"])
	}
}

// Two questions, two answers, one stdin. It reads like a formality and is not:
// a reader per question would buffer whatever followed the first answer and
// throw it away, and the second question would take the silence for a no.
func TestAnOperatorAnswersBothQuestionsOnOneStdin(t *testing.T) {
	r := startReplay(t)
	slot := dhcpSlot(t, r)
	path := configFile(t, pppoeConfig(slot))

	res := testRig.runUnifigWithInput(t, []string{"apply", path}, withWANPassword(r), "y\ny\n")
	t.Logf("unifig apply -> exit %d\n%s\n%s", res.ExitCode, res.Stdout, res.Stderr)

	if res.ExitCode != 0 {
		t.Fatalf("apply exited %d\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}
	stdout := string(res.Stdout)
	if !strings.Contains(stdout, "Apply these changes") || !strings.Contains(stdout, "Risky change") {
		t.Errorf("apply should have asked twice, got:\n%s", stdout)
	}
	if live := r.slot(t, slot); live["wan_type"] != "pppoe" {
		t.Errorf("the operator approved both questions and the slot did not change: %#v", live["wan_type"])
	}
}

// Refusing one is not cancelling the apply: the question was about that change,
// and the rest of the file was still asked for. Nothing is hard-blocked either
// — the same run with --allow-risky applies it.
func TestARefusedWANChangeIsSkippedAndTheRestIsApplied(t *testing.T) {
	r := startReplay(t)
	slot := dhcpSlot(t, r)
	// The rest of the file is a change to something that is not the uplink, so
	// that "the rest was still applied" has something to be true of. Both halves
	// come from the recording: any network it holds will do, and any VLAN tag
	// nothing is on is a change.
	network := r.aNetwork(t)
	path := configFile(t, fmt.Sprintf("networks:\n  - name: %q\n    vlan: %d\n", network, r.unusedVLAN(t))+
		pppoeConfig(slot))

	res := testRig.runUnifigWithInput(t,
		[]string{"apply", "--auto-approve", path}, withWANPassword(r), "n\n")
	t.Logf("unifig apply --auto-approve -> exit %d\n%s\n%s", res.ExitCode, res.Stdout, res.Stderr)

	if res.ExitCode != 0 {
		t.Fatalf("refusing a Risky change should not be a failure: exit %d\nstdout: %s\nstderr: %s",
			res.ExitCode, res.Stdout, res.Stderr)
	}
	stdout := string(res.Stdout)
	for _, fragment := range []string{
		fmt.Sprintf("~ network %q updated", network), fmt.Sprintf("wan %q", slot), "--allow-risky",
	} {
		if !strings.Contains(stdout, fragment) {
			t.Errorf("apply should report %q, got:\n%s", fragment, stdout)
		}
	}
	if live := r.slot(t, slot); live["wan_type"] != "dhcp" {
		t.Errorf("the operator said no and the slot changed anyway: %#v", live["wan_type"])
	}

	// Never hard-blocked: the operator who means it has a way to say so, and it
	// is a flag rather than an argument with the tool.
	applyEnv(t, withWANPassword(r), "--allow-risky", path)
	if live := r.slot(t, slot); live["wan_type"] != "pppoe" {
		t.Errorf("--allow-risky did not apply the change: %#v", live["wan_type"])
	}
}

// An apply left running unattended has nobody to answer, and EOF is not a yes.
func TestAWANChangeWithNoOneToAskIsLeftUnapplied(t *testing.T) {
	r := startReplay(t)
	slot := dhcpSlot(t, r)
	path := configFile(t, pppoeConfig(slot))

	res := testRig.runUnifig(t, []string{"apply", "--auto-approve", path}, withWANPassword(r))
	t.Logf("unifig apply --auto-approve -> exit %d\n%s\n%s", res.ExitCode, res.Stdout, res.Stderr)

	if live := r.slot(t, slot); live["wan_type"] != "dhcp" {
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
	absent := r.absentSlot(t)
	path := configFile(t, fmt.Sprintf("wan:\n  - slot: %s\n    type: dhcp\n", absent))

	res := planEnv(t, r.env(), path)

	if res.ExitCode != exitError {
		t.Fatalf("plan exited %d, want %d — it has an opinion about a slot the Controller does not have\nstdout: %s",
			res.ExitCode, exitError, res.Stdout)
	}
	stderr := string(res.Stderr)
	// The slot that is missing, that unifig will not make one, and which slots
	// this router does have — the answer an operator who misread their router
	// actually needs.
	fragments := []string{fmt.Sprintf("%q", absent), "never creates"}
	for _, held := range r.slotNames(t) {
		fragments = append(fragments, fmt.Sprintf("%q", held))
	}
	for _, fragment := range fragments {
		if !strings.Contains(stderr, fragment) {
			t.Errorf("stderr should mention %s, got: %s", fragment, stderr)
		}
	}
}

// Prune deletes Resources of a managed type. A WAN slot is not one — it is a
// Setting, and the difference here is the difference between deleting a spare
// VLAN and taking the site off the internet.
func TestPruneNeverDeletesAWANSlotTheConfigDoesNotName(t *testing.T) {
	r := startReplay(t)
	// The file has a `networks:` section, because without one prune would not
	// reach the networkconf collection at all (ADR-0006) and the question this
	// test asks would not get asked. It names every network the recording holds,
	// so none of those is a prune candidate either, and exactly one thing is
	// left at stake: the WAN slots, which share that collection with the
	// networks and which the config does not name.
	named := r.aSlot(t)
	var config strings.Builder
	config.WriteString("networks:\n")
	for _, network := range r.managedNetworkNames(t) {
		fmt.Fprintf(&config, "  - name: %q\n", network)
	}
	fmt.Fprintf(&config, "wan:\n  - slot: %s\n", named)
	path := configFile(t, config.String())

	res := planEnv(t, r.env(), "--prune", path)

	if res.ExitCode != exitNoChanges {
		t.Fatalf("plan --prune exited %d, want %d\nstdout: %s\nstderr: %s",
			res.ExitCode, exitNoChanges, res.Stdout, res.Stderr)
	}
	for _, slot := range r.slotNames(t) {
		if slot == named {
			continue
		}
		if strings.Contains(string(res.Stdout), slot) {
			t.Errorf("plan --prune has an opinion about the %s slot, which the config does not name:\n%s",
				slot, res.Stdout)
		}
	}
}

// unifig owns the fields its config models and nothing else. A WAN slot carries
// the ISP's DNS servers, its failover priority and its VLAN tag, and an operator
// who changed a PPPoE password must not lose any of them.
func TestApplyLeavesWANSettingsUnifigDoesNotModelAlone(t *testing.T) {
	r := startReplay(t)
	slot := dhcpSlot(t, r)
	r.seedSlot(t, slot, map[string]any{
		"wan_dns_preference":    "manual",
		"wan_dns1":              "1.1.1.1",
		"wan_failover_priority": 3,
		"wan_vlan_enabled":      true,
		"wan_vlan":              911,
	})
	path := configFile(t, pppoeConfig(slot))

	applyEnv(t, withWANPassword(r), "--allow-risky", path)

	live := r.slot(t, slot)
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
	slot := dhcpSlot(t, r)
	path := configFile(t, fmt.Sprintf("wan:\n  - slot: %s\n    type: pppoe\n", slot))

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
	slot := pppoeSlot(t, r)

	res := testRig.runUnifig(t, []string{"export"}, r.env())
	if res.ExitCode != 0 {
		t.Fatalf("export exited %d\nstderr: %s", res.ExitCode, res.Stderr)
	}

	stdout := string(res.Stdout)
	for _, fragment := range []string{"wan:", "slot: " + slot, "type: pppoe", "username: isp-user"} {
		if !strings.Contains(stdout, fragment) {
			t.Errorf("export should write %q, got:\n%s", fragment, stdout)
		}
	}

	// Every uplink the router has, not only the one under test: a slot left out
	// of the file reads as a slot the Controller does not have. And each is
	// written the way it actually connects — or, for a way of connecting unifig
	// does not model, as the slot alone.
	written := exportedYAML(t, res.Stdout)
	for _, held := range r.slotNames(t) {
		i := slices.IndexFunc(written.WAN, func(e exportedWANSlot) bool { return e.Slot == held })
		if i < 0 {
			t.Errorf("export left out the %s slot, which the Controller has:\n%s", held, stdout)
			continue
		}
		if want := modelledWANType(r.slot(t, held)); written.WAN[i].Type != want {
			t.Errorf("export wrote the %s slot as type %q, want %q — the way the Controller says it connects",
				held, written.WAN[i].Type, want)
		}
	}

	// The password left as a reference, and stderr naming the variable that
	// reference needs — asked of the export rather than assumed, so the naming
	// rule lives in one place.
	variable := redactedPasswordFor(t, written, slot)
	if !strings.Contains(string(res.Stderr), "export "+variable+"=") {
		t.Errorf("export should say to set %s, got: %s", variable, res.Stderr)
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
	pppoeSlot(t, r)

	exported := testRig.runUnifig(t, []string{"export"}, r.env())
	if exported.ExitCode != 0 {
		t.Fatalf("export exited %d\nstderr: %s", exported.ExitCode, exported.Stderr)
	}

	// The secrets export redacted have to come back from the environment, which
	// is the workflow the file it wrote describes.
	res := planExportedConfig(t, r, exported.Stdout)
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
	// Any slot would do, and this is the one the recording is certain to hold:
	// static addressing is a way of connecting, not a slot only a second uplink
	// can be on.
	slot := r.aSlot(t)
	r.seedSlot(t, slot, map[string]any{
		"wan_type": "static", "wan_ip": "203.0.113.9",
		"wan_netmask": "255.255.255.0", "wan_gateway": "203.0.113.1",
		"wan_username": "", "x_wan_password": "",
	})

	exported := testRig.runUnifig(t, []string{"export"}, r.env())
	if exported.ExitCode != 0 {
		t.Fatalf("export exited %d — one slot it cannot describe should not stop it\nstderr: %s",
			exported.ExitCode, exported.Stderr)
	}
	stdout := string(exported.Stdout)
	if !strings.Contains(stdout, "slot: "+slot) {
		t.Errorf("export should still say the slot exists, got:\n%s", stdout)
	}
	if strings.Contains(stdout, "static") {
		t.Errorf("export wrote a connection type the schema does not accept:\n%s", stdout)
	}
	if !strings.Contains(string(exported.Stderr), slot) {
		t.Errorf("export should say which slot it could only name, got: %s", exported.Stderr)
	}

	// And what it wrote is still config that plans clean: a slot unifig manages
	// nothing about is not a change.
	if res := planExportedConfig(t, r, exported.Stdout); res.ExitCode != exitNoChanges {
		t.Fatalf("plan of the exported config exited %d, want %d\nplan:\n%s",
			res.ExitCode, exitNoChanges, res.Stdout)
	}
}

// Which slots a router has is the router's answer, not unifig's. A gateway with
// an uplink beyond the ones the recording holds has to export and plan like any
// other, or the brownfield path would end at "unifig wrote you a file it will
// not read back".
func TestASlotUnifigHasNeverHeardOfStillRoundTrips(t *testing.T) {
	r := startReplay(t)
	extra := r.absentSlot(t)
	r.addSlot(t, extra, map[string]any{"wan_type": "dhcp"})

	exported := testRig.runUnifig(t, []string{"export"}, r.env())
	if exported.ExitCode != 0 {
		t.Fatalf("export exited %d\nstderr: %s", exported.ExitCode, exported.Stderr)
	}
	if !strings.Contains(string(exported.Stdout), "slot: "+extra) {
		t.Errorf("export left out an uplink the router has:\n%s", exported.Stdout)
	}

	res := planExportedConfig(t, r, exported.Stdout)
	if res.ExitCode != exitNoChanges {
		t.Fatalf("plan of the exported config exited %d, want %d — unifig will not read back what it wrote\nexported:\n%s\nstderr: %s",
			res.ExitCode, exitNoChanges, exported.Stdout, res.Stderr)
	}
}

// modelledWANType is the `type:` an exported slot should carry, given what the
// Controller holds for it: the connection type itself, or nothing at all where
// the Controller's answer is one unifig's config cannot state.
//
// The three types are spelled out here rather than imported from the engine,
// for the same reason rig.managedNetworkNames spells out the network purposes:
// a test that asked the code under test what it models could not catch the code
// modelling the wrong thing.
func modelledWANType(live map[string]any) string {
	switch wanType, _ := live["wan_type"].(string); wanType {
	case "dhcp", "pppoe", "disabled":
		return wanType
	default:
		return ""
	}
}

// redactedPasswordFor is the variable export wrote in place of a slot's PPPoE
// password — and a failure if it wrote anything else, since a password left in
// the clear is what redaction exists to prevent.
func redactedPasswordFor(t *testing.T, exported exportedConfig, slot string) string {
	t.Helper()
	for _, entry := range exported.WAN {
		if entry.Slot != slot {
			continue
		}
		name, ok := envReference(entry.Password)
		if !ok {
			t.Fatalf("export wrote the %s slot's password as %q rather than as a ${VAR} reference",
				slot, entry.Password)
		}
		return name
	}
	t.Fatalf("export wrote no %s slot: %+v", slot, exported.WAN)
	return ""
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
