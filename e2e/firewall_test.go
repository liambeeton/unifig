package e2e

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// Zones and Firewall Policies are the zone-based firewall, and these tests state
// the two things that make it different from everything reconciled so far: the
// Controller ships its own instances of a Resource type for the first time, and
// a reference runs two deep — a policy names a zone, which names a network — so
// a file declaring all three has to apply in one pass.
//
// They run against the recorded stand-in rather than the dockerized Controller,
// and that is not a preference. A gateway-less Network application has no
// zone-based firewall at all: on the pinned image both collections are empty on
// every site, `described-features` never mentions ZONE_BASED_FIREWALL, and a
// zone create is refused outright with api.err.CouldNotFindHotspotFirewallZone.
// There is nothing to match, nothing to update, and no honest way to seed one —
// which is ADR-0008's WAN reasoning arriving at a second area. See ADR-0013.
//
// As in the WAN and DNS suites, nothing here depends on what the recording
// happens to hold beyond the built-in zones every zone-based firewall has: the
// tests seed what they need and ask the stand-in what resulted.

// planFirewall and applyFirewall run a verb against the stand-in with a config
// body written to a temporary file — the firewall suite's counterpart of the
// plan/apply helpers the dockerized tests use.
func planFirewall(t *testing.T, r *replay, body string, args ...string) result {
	t.Helper()
	return planEnv(t, r.env(), append(args, configFile(t, body))...)
}

func applyFirewall(t *testing.T, r *replay, body string, args ...string) result {
	t.Helper()
	return applyEnv(t, r.env(), append(args, configFile(t, body))...)
}

// aLAN is the network the recording holds for zones to be about — asked for
// rather than named, for the same reason the WAN suite asks which uplinks exist.
func aLAN(t *testing.T, r *replay) string {
	t.Helper()
	return r.aNetwork(t)
}

func TestPlanShowsAZoneToCreateAndApplyMakesIt(t *testing.T) {
	r := startReplay(t)
	lan := aLAN(t, r)

	body := fmt.Sprintf(`networks:
  - name: %q
zones:
  - name: Untrusted
    networks:
      - %q
`, lan, lan)

	res := planFirewall(t, r, body)
	if res.ExitCode != exitChangesPending {
		t.Fatalf("plan exited %d, want %d\nstderr: %s", res.ExitCode, exitChangesPending, res.Stderr)
	}
	stdout := string(res.Stdout)
	for _, fragment := range []string{`+ zone "Untrusted"`, "networks", lan, "1 to create"} {
		if !strings.Contains(stdout, fragment) {
			t.Errorf("plan should mention %q, got:\n%s", fragment, stdout)
		}
	}

	applyFirewall(t, r, body)

	if got := r.zoneMembers(t, "Untrusted"); !slices.Equal(got, []string{lan}) {
		t.Errorf("the created zone holds %v, want just %q", got, lan)
	}
	assertNoChangesPendingEnv(t, r.env(), configFile(t, body))
}

// A zone unifig creates with nothing in it is a zone no traffic is ever in,
// which the config does not say and the plan therefore does.
func TestPlanWarnsThatAZoneWithNoNetworksHoldsNothing(t *testing.T) {
	r := startReplay(t)

	res := planFirewall(t, r, `zones:
  - name: Empty
    networks: []
`)

	if res.ExitCode != exitChangesPending {
		t.Fatalf("plan exited %d, want %d\nstderr: %s", res.ExitCode, exitChangesPending, res.Stderr)
	}
	if !strings.Contains(string(res.Stdout), "no traffic") {
		t.Errorf("plan should warn that this zone holds nothing, got:\n%s", res.Stdout)
	}
}

// ADR-0004: a zone that states no membership manages none. This is how an
// operator names a built-in zone in order to write policies about it without
// taking over what is in it — and the built-in zones are where the interesting
// policies point.
func TestAZoneWithNoNetworksKeyLeavesItsMembershipAlone(t *testing.T) {
	r := startReplay(t)
	before := r.zoneMembers(t, "Internal")

	res := planFirewall(t, r, `zones:
  - name: Internal
`)

	if res.ExitCode != exitNoChanges {
		t.Fatalf("plan exited %d, want %d — it is proposing to change a membership the config never stated\nplan:\n%s",
			res.ExitCode, exitNoChanges, res.Stdout)
	}
	if got := r.zoneMembers(t, "Internal"); !slices.Equal(got, before) {
		t.Errorf("the zone's membership changed from %v to %v", before, got)
	}
}

// The other half of that rule: stating the key states the whole list, so a
// network dropped from it is one the next apply takes out of the zone — and the
// plan says so before it does.
func TestStatingAZonesNetworksSaysWhichOneLeavesIt(t *testing.T) {
	r := startReplay(t)
	lan := aLAN(t, r)
	r.seedZone(t, "Segmented", []string{lan}, nil)

	body := `zones:
  - name: Segmented
    networks: []
`
	res := planFirewall(t, r, body)
	if res.ExitCode != exitChangesPending {
		t.Fatalf("plan exited %d, want %d\nplan:\n%s", res.ExitCode, exitChangesPending, res.Stdout)
	}
	if !strings.Contains(string(res.Stdout), lan) {
		t.Errorf("plan should name the network leaving the zone, got:\n%s", res.Stdout)
	}

	applyFirewall(t, r, body)

	if got := r.zoneMembers(t, "Segmented"); len(got) != 0 {
		t.Errorf("the zone still holds %v after being stated as holding none", got)
	}
}

// The built-in External zone holds the WAN, which is not a network unifig
// manages and has no name the config can use. Stating the LANs in such a zone
// must therefore not detach the uplink from it — the membership is owned per
// member, which is ADR-0004 one level in.
func TestStatingAZonesNetworksLeavesAMemberUnifigCannotNameAlone(t *testing.T) {
	r := startReplay(t)
	lan := aLAN(t, r)

	before := r.zoneMembers(t, "External")
	if len(before) == 0 {
		t.Fatalf("the recording's External zone holds nothing, so this test would prove nothing")
	}

	body := fmt.Sprintf(`networks:
  - name: %q
zones:
  - name: External
    networks:
      - %q
`, lan, lan)

	res := planFirewall(t, r, body)
	if res.ExitCode != exitChangesPending {
		t.Fatalf("plan exited %d, want %d\nplan:\n%s", res.ExitCode, exitChangesPending, res.Stdout)
	}
	// An operator reading a membership that is only part of the truth has to be
	// told so, or the plan looks like it is emptying the zone.
	if !strings.Contains(string(res.Stdout), "not one of this site's LANs") {
		t.Errorf("plan should say the zone holds something unifig does not name, got:\n%s", res.Stdout)
	}

	applyFirewall(t, r, body)

	after := r.zoneMembers(t, "External")
	if !slices.Contains(after, lan) {
		t.Errorf("the network the config named is not in the zone: %v", after)
	}
	for _, was := range before {
		if was == lan {
			continue
		}
		if !slices.Contains(after, was) {
			t.Fatalf("apply took %q out of the External zone; the config could not name it and did not ask", was)
		}
	}
}

func TestApplyCreatesAPolicyBetweenTwoZonesAndTheNextPlanIsEmpty(t *testing.T) {
	r := startReplay(t)

	body := `firewall-policies:
  - name: Internal to External
    action: allow
    source: Internal
    destination: External
  - name: Block guests out
    action: block
    source: Internal
    destination: External
`
	res := applyFirewall(t, r, body)
	if !strings.Contains(string(res.Stdout), `+ firewall-policy "Block guests out" created`) {
		t.Errorf("apply should report what it created, got:\n%s", res.Stdout)
	}

	source, destination := r.zoneEnds(t, "Block guests out")
	if source != "Internal" || destination != "External" {
		t.Errorf("the policy governs %q -> %q, want Internal -> External", source, destination)
	}
	if action := r.policyNamed(t, "Block guests out")["action"]; action != "BLOCK" {
		t.Errorf("action = %#v, want the Controller's own spelling of block", action)
	}

	assertNoChangesPendingEnv(t, r.env(), configFile(t, body))
}

// The one-pass promise, two references deep: a network, a zone holding it and a
// policy governing that zone all apply in a single run, because each write reads
// the IDs it needs at the moment it runs rather than the moment it was planned.
func TestApplyCreatesANetworkAZoneAndAPolicyInOnePass(t *testing.T) {
	r := startReplay(t)
	vlan := r.unusedVLAN(t)

	body := fmt.Sprintf(`networks:
  - name: Cameras
    vlan: %d
    subnet: 10.99.0.1/24
zones:
  - name: Camera Zone
    networks:
      - Cameras
firewall-policies:
  - name: Cameras out
    action: block
    source: Camera Zone
    destination: External
`, vlan)

	res := applyFirewall(t, r, body)

	stdout := string(res.Stdout)
	at := map[string]int{}
	for _, fragment := range []string{
		`network "Cameras" created`,
		`zone "Camera Zone" created`,
		`firewall-policy "Cameras out" created`,
	} {
		if at[fragment] = strings.Index(stdout, fragment); at[fragment] < 0 {
			t.Fatalf("apply did not report %q:\n%s", fragment, stdout)
		}
	}
	if at[`network "Cameras" created`] > at[`zone "Camera Zone" created`] {
		t.Errorf("apply created the zone before the network it holds:\n%s", stdout)
	}
	if at[`zone "Camera Zone" created`] > at[`firewall-policy "Cameras out" created`] {
		t.Errorf("apply created the policy before the zone it governs:\n%s", stdout)
	}

	if got := r.zoneMembers(t, "Camera Zone"); !slices.Equal(got, []string{"Cameras"}) {
		t.Errorf("the zone holds %v, want the network created alongside it", got)
	}
	source, _ := r.zoneEnds(t, "Cameras out")
	if source != "Camera Zone" {
		t.Errorf("the policy's source is %q, want the zone created alongside it", source)
	}

	assertNoChangesPendingEnv(t, r.env(), configFile(t, body))
}

// Building goes from the ground up and dismantling goes the other way. The plan
// prints the order apply will use, so an operator sees it before agreeing to it.
func TestPlanOrdersPoliciesAfterZonesAndDeletionsTheOtherWayAround(t *testing.T) {
	r := startReplay(t)
	lan := aLAN(t, r)
	r.seedZone(t, "Old Zone", []string{lan}, nil)
	r.seedPolicy(t, "Old Policy", "ALLOW", "Old Zone", "External", nil)

	body := fmt.Sprintf(`networks:
  - name: %q
zones:
  - name: New Zone
    networks:
      - %q
firewall-policies:
  - name: New Policy
    action: allow
    source: New Zone
    destination: External
`, lan, lan)

	res := planFirewall(t, r, body, "--prune")
	if res.ExitCode != exitChangesPending {
		t.Fatalf("plan --prune exited %d, want %d\nstderr: %s", res.ExitCode, exitChangesPending, res.Stderr)
	}

	stdout := string(res.Stdout)
	at := map[string]int{}
	for _, fragment := range []string{
		`+ zone "New Zone"`,
		`+ firewall-policy "New Policy"`,
		`- firewall-policy "Old Policy"`,
		`- zone "Old Zone"`,
	} {
		if at[fragment] = strings.Index(stdout, fragment); at[fragment] < 0 {
			t.Fatalf("plan does not mention %q:\n%s", fragment, stdout)
		}
	}
	if at[`+ zone "New Zone"`] > at[`+ firewall-policy "New Policy"`] {
		t.Errorf("plan creates the policy before the zone it governs:\n%s", stdout)
	}
	if at[`- firewall-policy "Old Policy"`] > at[`- zone "Old Zone"`] {
		t.Errorf("plan deletes the zone before the policy on it:\n%s", stdout)
	}
}

// ADR-0005, and the case it matters most in: the Controller owns its built-in
// zones and says so on the object. An operator whose file has never mentioned
// External must not lose the zone that stands for the internet by pruning.
func TestPruneNeverDeletesTheControllersBuiltInZones(t *testing.T) {
	r := startReplay(t)

	// unifig reads the Controller's own marker rather than keeping a list of
	// built-in names, so the test states that first. If this stops holding, the
	// reason the rest of the test passes has changed.
	//
	// The marker is `default_zone`, which is the one a zone carries. It is not
	// `attr_no_delete` — that is how a *network* says the same thing, and this
	// test asserted it against a hand-written fixture that had been written to
	// satisfy it, until a recording from migrated hardware showed no zone has the
	// field at all (issue #23).
	//
	// Which built-ins there are is read from the recording rather than listed
	// here, for the same reason unifig does not list them: the set is Ubiquiti's
	// to change. The hand-written fixture guessed five and a real UDR ships six,
	// spelling two of them differently.
	var builtIns []string
	for _, zone := range r.liveZones(t) {
		if zone["default_zone"] != true {
			t.Fatalf("the recording's %v zone is not marked as the Controller's own; unifig's exemption reads that marker", zone["name"])
		}
		name, _ := zone["name"].(string)
		builtIns = append(builtIns, name)
	}
	if len(builtIns) == 0 {
		t.Fatal("the recording holds no built-in zones, so this test would prove nothing")
	}

	lan := aLAN(t, r)
	r.seedZone(t, "Prunable", []string{lan}, nil)

	// A config that has never heard of the built-ins, pruned.
	body := `zones:
  - name: Keeper
`
	res := planFirewall(t, r, body, "--prune")
	if !strings.Contains(string(res.Stdout), `- zone "Prunable"`) {
		t.Fatalf("the prune under test is not in the plan:\n%s", res.Stdout)
	}
	for _, builtIn := range builtIns {
		if strings.Contains(string(res.Stdout), fmt.Sprintf(`zone %q`, builtIn)) {
			t.Fatalf("plan --prune proposes deleting the built-in %s zone:\n%s", builtIn, res.Stdout)
		}
	}

	applyFirewall(t, r, body, "--prune")

	for _, builtIn := range builtIns {
		r.zoneNamed(t, builtIn) // fails the test if it is gone or duplicated
	}
	for _, zone := range r.liveZones(t) {
		if zone["name"] == "Prunable" {
			t.Fatalf("the prune under test did not happen: %v", zone)
		}
	}
}

// The Controller generates a default policy for a pair of zones and marks it
// predefined. That is the same statement as attr_no_delete — the object is the
// Controller's own — so prune leaves it alone too.
func TestPruneLeavesTheControllersOwnPoliciesAlone(t *testing.T) {
	r := startReplay(t)
	r.seedPolicy(t, "Mine", "ALLOW", "Internal", "External", nil)

	predefined := 0
	for _, policy := range r.livePolicies(t) {
		if policy["predefined"] == true {
			predefined++
		}
	}
	if predefined == 0 {
		t.Fatalf("the recording holds no predefined policy, so this test would prove nothing")
	}

	res := applyFirewall(t, r, "firewall-policies: []\n", "--prune")

	if !strings.Contains(string(res.Stdout), `- firewall-policy "Mine" deleted`) {
		t.Fatalf("the prune under test did not happen:\n%s", res.Stdout)
	}
	left := 0
	for _, policy := range r.livePolicies(t) {
		if policy["predefined"] == true {
			left++
		}
		if policy["name"] == "Mine" {
			t.Errorf("apply --prune left the policy the config does not name: %v", policy)
		}
	}
	if left != predefined {
		t.Errorf("prune deleted %d of the Controller's own policies", predefined-left)
	}
}

// ADR-0006. A file with no `zones:` key says nothing about zones, so a prune it
// takes part in has no business deleting one.
func TestPruneLeavesTheFirewallAloneWhenTheFileDoesNotMentionIt(t *testing.T) {
	r := startReplay(t)
	lan := aLAN(t, r)
	r.seedZone(t, "Bystander", []string{lan}, nil)
	r.seedPolicy(t, "Bystander Policy", "ALLOW", "Bystander", "External", nil)

	res := applyFirewall(t, r, "wlans: []\n", "--prune")

	if strings.Contains(string(res.Stdout), "Bystander") {
		t.Errorf("apply --prune has an opinion about a section the config does not have:\n%s", res.Stdout)
	}
	r.zoneNamed(t, "Bystander")
	r.policyNamed(t, "Bystander Policy")
}

// A policy names its two ends, and either may be a zone only the Controller has.
// One that is neither on the Controller nor in the file is an operator's typo,
// and unifig says so while reading — listing the zones the site really has,
// because the likeliest cause is a misspelled built-in.
func TestAPolicyNamingAZoneThatExistsNowhereIsRefusedWithTheZonesThatDo(t *testing.T) {
	r := startReplay(t)

	res := planFirewall(t, r, `firewall-policies:
  - name: Typo
    action: allow
    source: Internal
    destination: Exteral
`)

	if res.ExitCode != exitError {
		t.Fatalf("plan exited %d, want %d — it accepted a policy with an end that does not exist\nstdout: %s",
			res.ExitCode, exitError, res.Stdout)
	}
	stderr := string(res.Stderr)
	for _, fragment := range []string{`"Exteral"`, `"External"`, `"Internal"`} {
		if !strings.Contains(stderr, fragment) {
			t.Errorf("stderr should mention %q, got: %s", fragment, stderr)
		}
	}
}

// ADR-0001 matches Resources by name and calls a duplicate a hard error rather
// than a guess.
// ADR-0005's addition, and the reason it was added: where unifig cannot
// establish which zones are the Controller's own, prune leaves every zone alone
// rather than treating them all as fair game. "Cannot tell" has to mean "do not
// delete" for the one change that can sever a site from the internet (issue #23).
//
// The marker is read off the zone endpoint separately from the list, so a
// firmware that changed its type would leave the list readable and the marker
// not — which is what this seeds. A zone whose `default_zone` is a string is
// enough: the ownership read decodes the whole answer or none of it.
func TestPruneLeavesEveryZoneAloneWhenItCannotTellWhichAreTheControllersOwn(t *testing.T) {
	r := startReplay(t)
	lan := aLAN(t, r)
	r.seedZone(t, "Prunable", []string{lan}, map[string]any{"default_zone": "not a bool"})

	// A config that names none of the live zones, pruned. Every one of them is a
	// candidate for deletion, and none may be proposed.
	res := planFirewall(t, r, `zones:
  - name: Keeper
`, "--prune")

	if strings.Contains(string(res.Stdout), `- zone "`) {
		t.Errorf("prune proposes deleting a zone though it could not tell which are built-in:\n%s", res.Stdout)
	}
	for _, zone := range r.liveZones(t) {
		name, _ := zone["name"].(string)
		if strings.Contains(string(res.Stdout), fmt.Sprintf(`zone %q`, name)) {
			t.Errorf("plan --prune proposes deleting the %s zone:\n%s", name, res.Stdout)
		}
	}

	// Skipping quietly is the other half of the failure. An operator who asked
	// for a prune and got none has to be told, or the plan reads as a site with
	// nothing to prune.
	stdout := string(res.Stdout)
	for _, fragment := range []string{"zone", "no zone will be deleted", "could not read"} {
		if !strings.Contains(stdout, fragment) {
			t.Errorf("the plan should say why it pruned no zones, looking for %q:\n%s", fragment, stdout)
		}
	}
}

// The same silence, in the case that hides it best: a plan with nothing else in
// it. "No changes" and "no changes I was willing to make" are different
// statements and must not print the same (issue #23).
func TestAnEmptyPlanStillSaysWhyItPrunedNoZones(t *testing.T) {
	r := startReplay(t)
	r.seedZone(t, "Unreadable", nil, map[string]any{"default_zone": "not a bool"})

	// Names every zone the recording holds, so nothing is left to prune and the
	// plan is empty but for the caveat.
	body := "zones:\n"
	for _, zone := range r.liveZones(t) {
		name, _ := zone["name"].(string)
		body += fmt.Sprintf("  - name: %s\n", name)
	}

	res := planFirewall(t, r, body, "--prune")

	stdout := string(res.Stdout)
	if !strings.Contains(stdout, "No changes") {
		t.Fatalf("expected an otherwise-empty plan, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "no zone will be deleted") {
		t.Errorf("an empty plan dropped the reason it pruned nothing:\n%s", stdout)
	}
}

// A policy's key is its name and the pair of zones it governs, so a name on its
// own repeating is ordinary — the Controller's own predefined set does it
// nineteen times over (ADR-0001, issue #24). What has no answer is two policies
// alike in all three, and that is still refused rather than guessed at.
func TestPlanRefusesToGuessBetweenTwoPoliciesSharingANameAndBothEnds(t *testing.T) {
	r := startReplay(t)
	r.seedPolicy(t, "Twice Over", "ALLOW", "Internal", "External", nil)
	r.seedPolicy(t, "Twice Over", "BLOCK", "Internal", "External", nil)

	res := planFirewall(t, r, `firewall-policies:
  - name: Twice Over
    action: allow
    source: Internal
    destination: External
`)

	if res.ExitCode != exitError {
		t.Fatalf("plan exited %d, want %d — it picked one of two policies alike in name and both ends\nstdout: %s",
			res.ExitCode, exitError, res.Stdout)
	}
	// The pair is named, because the name alone does not identify which policies
	// the operator has to go and look at.
	stderr := string(res.Stderr)
	for _, fragment := range []string{"Twice Over", "Internal", "External", "UI"} {
		if !strings.Contains(stderr, fragment) {
			t.Errorf("stderr should mention %q, got: %s", fragment, stderr)
		}
	}
}

// The same name on two policies whose zones unifig cannot name is not that
// ambiguity. Their keys collapse to empty ends, so counting them as duplicates
// reports a clash between a pair of nothings — `2 matching "X" ( to )` — about
// policies that were never matchable in the first place. They belong to export's
// "could not describe" list rather than to a refusal (issue #24).
func TestTwoPoliciesOnZonesUnifigCannotNameAreNotADuplicate(t *testing.T) {
	r := startReplay(t)
	r.seedPolicy(t, "Adrift", "ALLOW", "Nowhere", "External", nil)
	r.seedPolicy(t, "Adrift", "BLOCK", "Nowhere Else", "External", nil)

	res := planFirewall(t, r, `firewall-policies:
  - name: Kept
    action: allow
    source: Internal
    destination: External
`)

	if res.ExitCode == exitError {
		t.Fatalf("plan exited %d — it refused a site over policies it cannot match at all\nstderr: %s",
			res.ExitCode, res.Stderr)
	}
	if got := string(res.Stderr); strings.Contains(got, "( to )") {
		t.Errorf("the refusal names a pair of nothings, so it is about unmatchable policies: %s", got)
	}
}

func TestPlanRefusesToGuessBetweenTwoZonesSharingAName(t *testing.T) {
	r := startReplay(t)
	lan := aLAN(t, r)
	r.seedZone(t, "Twice", []string{lan}, nil)
	r.seedZone(t, "Twice", nil, nil)

	res := planFirewall(t, r, `zones:
  - name: Twice
`)

	if res.ExitCode != exitError {
		t.Fatalf("plan exited %d, want %d — it picked one of two zones named the same\nstdout: %s",
			res.ExitCode, exitError, res.Stdout)
	}
	for _, fragment := range []string{"Twice", "UI"} {
		if !strings.Contains(string(res.Stderr), fragment) {
			t.Errorf("stderr should mention %q, got: %s", fragment, res.Stderr)
		}
	}
}

func TestPlanJSONNamesTheFirewallKinds(t *testing.T) {
	r := startReplay(t)

	res := planFirewall(t, r, `zones:
  - name: JSON Zone
    networks: []
firewall-policies:
  - name: JSON Policy
    action: reject
    source: Internal
    destination: External
`, "--json")

	if res.ExitCode != exitChangesPending {
		t.Fatalf("plan --json exited %d, want %d\nstderr: %s", res.ExitCode, exitChangesPending, res.Stderr)
	}

	var out struct {
		Changes []struct {
			Action string `json:"action"`
			Kind   string `json:"kind"`
			Name   string `json:"name"`
			Fields []struct {
				Name string `json:"name"`
				To   any    `json:"to"`
			} `json:"fields"`
		} `json:"changes"`
	}
	if err := json.Unmarshal(res.Stdout, &out); err != nil {
		t.Fatalf("plan --json is not valid JSON: %v\nstdout: %s", err, res.Stdout)
	}

	kinds := map[string]string{}
	for _, change := range out.Changes {
		kinds[change.Name] = change.Kind
	}
	if kinds["JSON Zone"] != "zone" {
		t.Errorf("the zone's kind is %q, want %q", kinds["JSON Zone"], "zone")
	}
	if kinds["JSON Policy"] != "firewall-policy" {
		t.Errorf("the policy's kind is %q, want %q", kinds["JSON Policy"], "firewall-policy")
	}

	// The verdict reaches the JSON in the config's own vocabulary rather than
	// the Controller's, so a pipeline reads back what the file says.
	for _, change := range out.Changes {
		if change.Name != "JSON Policy" {
			continue
		}
		for _, field := range change.Fields {
			if field.Name == "action" && field.To != "reject" {
				t.Errorf("action field = %#v, want %q", field.To, "reject")
			}
		}
	}
}

// The brownfield path: a Controller with a zone-based firewall exports as a file
// that describes it, and that file plans clean.
func TestExportWritesTheFirewallAndItPlansClean(t *testing.T) {
	r := startReplay(t)
	lan := aLAN(t, r)
	r.seedZone(t, "Exported Zone", []string{lan}, nil)
	r.seedPolicy(t, "Exported Policy", "BLOCK", "Exported Zone", "External", nil)

	exported := testRig.runUnifig(t, []string{"export"}, r.env())
	if exported.ExitCode != 0 {
		t.Fatalf("unifig export exited %d\nstderr: %s", exported.ExitCode, exported.Stderr)
	}

	cfg := exportedYAML(t, exported.Stdout)
	var zone *exportedZone
	for i, z := range cfg.Zones {
		if z.Name == "Exported Zone" {
			zone = &cfg.Zones[i]
		}
	}
	if zone == nil {
		t.Fatalf("export left out the seeded zone:\n%s", exported.Stdout)
	}
	if zone.Networks == nil || !slices.Equal(*zone.Networks, []string{lan}) {
		t.Errorf("the exported zone holds %v, want %q", zone.Networks, lan)
	}

	var found bool
	for _, policy := range cfg.FirewallPolicies {
		if policy.Name != "Exported Policy" {
			continue
		}
		found = true
		if policy.Action != "block" || policy.Source != "Exported Zone" || policy.Destination != "External" {
			t.Errorf("the exported policy is %+v, want block from Exported Zone to External", policy)
		}
	}
	if !found {
		t.Fatalf("export left out the seeded policy:\n%s", exported.Stdout)
	}

	if res := planExportedConfig(t, r, exported.Stdout); res.ExitCode != exitNoChanges {
		t.Fatalf("plan of a freshly exported config exited %d, want %d\nexported:\n%s\nplan:\n%s",
			res.ExitCode, exitNoChanges, exported.Stdout, res.Stdout)
	}
}

// A zone holding something the config cannot name is written with the part it
// can — and export says so, because a file that came back short says why.
func TestExportSaysWhichZonesItCouldOnlyDescribeInPart(t *testing.T) {
	r := startReplay(t)

	exported := testRig.runUnifig(t, []string{"export"}, r.env())
	if exported.ExitCode != 0 {
		t.Fatalf("unifig export exited %d\nstderr: %s", exported.ExitCode, exported.Stderr)
	}

	if !strings.Contains(string(exported.Stderr), "External") {
		t.Errorf("export should say the External zone holds something it cannot name, got: %s", exported.Stderr)
	}
	if !strings.Contains(string(exported.Stderr), "leaves the rest of the membership") {
		t.Errorf("export should say what it does about the part it cannot name, got: %s", exported.Stderr)
	}
}
