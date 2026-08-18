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
func TestPlanShowsAZoneToCreateAndApplyMakesIt(t *testing.T) {
	r := startReplay(t)
	lan := r.aNetwork(t)

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
	lan := r.aNetwork(t)
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

// unzonedNetwork creates a network through unifig and answers with its name and
// the `networks:` stanza every later config in the test has to carry — a zone may
// only name a network the same file defines. A network the Controller has just
// created is in no zone, which is where these tests start.
//
// The recording's own LAN cannot stand in for it. `Default` sits in the built-in
// `Internal` zone (`e2e/testdata/udr/README.md`), which is the zone the
// Controller falls back to — so a test moving it out of somewhere could not tell
// "the zone it came from" and "the zone the Controller sends it to" apart, and
// would pass on either.
func unzonedNetwork(t *testing.T, r *replay) (name, networks string) {
	t.Helper()
	name = "Probe"
	networks = fmt.Sprintf(`networks:
  - name: %s
    vlan: %d
    subnet: 10.77.0.1/24
`, name, r.unusedVLAN(t))
	applyFirewall(t, r, networks)
	return name, networks
}

// placedNetwork is the same network, put in one zone on the way.
func placedNetwork(t *testing.T, r *replay, zone string) (name, networks string) {
	t.Helper()
	name, networks = unzonedNetwork(t, r)
	applyFirewall(t, r, networks+fmt.Sprintf(`zones:
  - name: %s
    networks:
      - %s
`, zone, name))
	return name, networks
}

// A network belongs to exactly one firewall zone, and the Controller keeps it
// that way itself: putting a network in a second zone takes it out of the first,
// in the same request, without unifig asking for it. unifig sends one PUT to one
// zone and two zones change — so a plan naming only the zone that gains the
// network is not a statement about what will happen, which is the standard
// ADR-0014 sets and ADR-0020 applies here.
//
// These tests assert what the plan *says*, and they have to. The stand-in stores
// whatever it is handed, so it will not evict the network from the other zone on
// its own; a test reading the membership back afterwards would be reading
// unifig's own model rather than the Controller's behaviour (#32, #30).
func TestPlanNamesTheZoneANetworkLeavesToJoinAnother(t *testing.T) {
	r := startReplay(t)
	probe, networks := placedNetwork(t, r, "Alpha")
	r.seedZone(t, "Beta", nil, nil)

	res := planFirewall(t, r, networks+fmt.Sprintf(`zones:
  - name: Beta
    networks:
      - %s
`, probe))

	if res.ExitCode != exitChangesPending {
		t.Fatalf("plan exited %d, want %d\nplan:\n%s", res.ExitCode, exitChangesPending, res.Stdout)
	}
	stdout := string(res.Stdout)
	if !strings.Contains(stdout, `~ zone "Beta"`) {
		t.Fatalf("plan does not update the zone gaining the network:\n%s", stdout)
	}
	if !strings.Contains(stdout, `"Alpha"`) {
		t.Errorf("the plan puts %q in Beta without naming Alpha, the zone it leaves:\n%s", probe, stdout)
	}
}

// The same on a zone being created. A zone born holding a network empties
// whichever zone held it, and the operator reading `+ zone` has even less reason
// to expect that than the one reading `~ zone`.
func TestPlanNamesTheZoneANetworkLeavesWhenTheZoneItJoinsIsCreated(t *testing.T) {
	r := startReplay(t)
	probe, networks := placedNetwork(t, r, "Alpha")

	res := planFirewall(t, r, networks+fmt.Sprintf(`zones:
  - name: Gamma
    networks:
      - %s
`, probe))

	if res.ExitCode != exitChangesPending {
		t.Fatalf("plan exited %d, want %d\nplan:\n%s", res.ExitCode, exitChangesPending, res.Stdout)
	}
	stdout := string(res.Stdout)
	if !strings.Contains(stdout, `+ zone "Gamma"`) {
		t.Fatalf("plan does not create the zone gaining the network:\n%s", stdout)
	}
	if !strings.Contains(stdout, `"Alpha"`) {
		t.Errorf("the plan creates Gamma holding %q without naming Alpha, the zone it leaves:\n%s", probe, stdout)
	}
}

// Taking a network out of a zone does not leave it unzoned: the Controller
// reassigns it to the zone it keys `internal`. A plan that showed the membership
// going to nothing would be telling the operator their network ends up outside
// every zone, which is the one thing that cannot happen.
func TestPlanSaysWhereTheControllerPutsANetworkTakenOutOfAZone(t *testing.T) {
	r := startReplay(t)
	internal := r.internalZone(t)
	probe, networks := placedNetwork(t, r, "Alpha")

	res := planFirewall(t, r, networks+`zones:
  - name: Alpha
    networks: []
`)

	if res.ExitCode != exitChangesPending {
		t.Fatalf("plan exited %d, want %d\nplan:\n%s", res.ExitCode, exitChangesPending, res.Stdout)
	}
	stdout := string(res.Stdout)
	if !strings.Contains(stdout, probe) {
		t.Fatalf("plan does not name the network leaving the zone:\n%s", stdout)
	}
	if !strings.Contains(stdout, internal) {
		t.Errorf("the plan empties Alpha without saying the Controller will put %q in %q:\n%s",
			probe, internal, stdout)
	}
}

// Which zone that is comes from the Controller's own key rather than from the
// name "Internal", for the reason the gateway's does (ADR-0005, ADR-0018): a
// firmware presenting it under another name would have unifig telling operators
// their network lands somewhere it does not.
func TestTheZoneANetworkIsReassignedToIsFoundByTheControllersKey(t *testing.T) {
	r := startReplay(t)
	r.renameZoneKeyed(t, "internal", "Hausnetz")
	_, networks := placedNetwork(t, r, "Alpha")

	res := planFirewall(t, r, networks+`zones:
  - name: Alpha
    networks: []
`)

	if !strings.Contains(string(res.Stdout), "Hausnetz") {
		t.Errorf("plan does not name the renamed zone the Controller keys `internal`, so unifig found it by its name:\n%s",
			res.Stdout)
	}
}

// The reassignment only happens when nothing else takes the network, so a plan
// that moves it must not claim it. Saying both would be a plan contradicting
// itself two lines apart, which is the shape ADR-0014 already refused once.
func TestAPlanThatRehomesANetworkItselfDoesNotSayTheControllerWill(t *testing.T) {
	r := startReplay(t)
	internal := r.internalZone(t)
	probe, networks := placedNetwork(t, r, "Alpha")
	r.seedZone(t, "Beta", nil, nil)

	res := planFirewall(t, r, networks+fmt.Sprintf(`zones:
  - name: Alpha
    networks: []
  - name: Beta
    networks:
      - %s
`, probe))

	if res.ExitCode != exitChangesPending {
		t.Fatalf("plan exited %d, want %d\nplan:\n%s", res.ExitCode, exitChangesPending, res.Stdout)
	}
	stdout := string(res.Stdout)
	if strings.Contains(stdout, internal) {
		t.Errorf("the plan says the Controller will put %q in %q, and this same plan puts it in Beta:\n%s",
			probe, internal, stdout)
	}
	if !strings.Contains(stdout, `"Beta"`) {
		t.Errorf("the plan empties Alpha without saying where %q goes:\n%s", probe, stdout)
	}
}

// A network in no zone displaces nothing, and the plan must not invent a zone
// for it to have come from. Every zone the site has is checked rather than a
// guess at which one would have been named wrongly.
func TestPlanNamesNoZoneWhenTheNetworkItPlacesWasInNone(t *testing.T) {
	r := startReplay(t)
	probe, networks := unzonedNetwork(t, r)

	res := planFirewall(t, r, networks+fmt.Sprintf(`zones:
  - name: Alpha
    networks:
      - %s
`, probe))

	stdout := string(res.Stdout)
	if !strings.Contains(stdout, `+ zone "Alpha"`) {
		t.Fatalf("plan does not create the zone:\n%s", stdout)
	}
	for _, zone := range r.liveZones(t) {
		name, _ := zone["name"].(string)
		if strings.Contains(stdout, name) {
			t.Errorf("the plan names the zone %q, and the network it places was in no zone at all:\n%s", name, stdout)
		}
	}
}

// A zone that is deleted lets go of its networks as surely as one that is
// emptied, and the plan owes the operator the same sentence. What it does not owe
// them is a zone name: where the Controller puts a network whose zone was deleted
// has never been measured, and the write probe deleted only zones that held
// nothing (ADR-0020). So the plan says what the invariant entails — the networks
// survive and end up somewhere — and stops there rather than guessing `Internal`.
func TestPlanSaysADeletedZonesNetworksAreNotDeletedWithIt(t *testing.T) {
	r := startReplay(t)
	probe, networks := placedNetwork(t, r, "Doomed")

	res := planFirewall(t, r, networks+`zones:
  - name: Keeper
`, "--prune")

	stdout := string(res.Stdout)
	if !strings.Contains(stdout, `- zone "Doomed"`) {
		t.Fatalf("the prune under test is not in the plan:\n%s", stdout)
	}
	if !strings.Contains(stdout, "not deleted with") {
		t.Errorf("the plan deletes a zone holding %q without saying the network survives it:\n%s", probe, stdout)
	}
	// Naming a zone as the destination would be asserting a guess, and the
	// recording's own built-ins are what such a guess would reach for. The zone
	// being deleted is not one of them: it is the change's own subject.
	for _, zone := range r.liveZones(t) {
		name, _ := zone["name"].(string)
		if name == "Doomed" {
			continue
		}
		if strings.Contains(stdout, fmt.Sprintf("%q", name)) {
			t.Errorf("the plan names the zone %q as where %q ends up, and no one has measured that:\n%s", name, probe, stdout)
		}
	}
}

// The prose plan's notes reach `plan --json` too, as a list rather than a single
// string: one membership change can displace a network, rehome another and hold
// a member unifig cannot name, and a pipeline reading one of those three would
// be reading less than the operator does.
func TestPlanJSONCarriesEveryNoteOnAField(t *testing.T) {
	r := startReplay(t)
	internal := r.internalZone(t)
	probe, networks := placedNetwork(t, r, "Alpha")

	res := planFirewall(t, r, networks+`zones:
  - name: Alpha
    networks: []
`, "--json")

	var out struct {
		Changes []struct {
			Kind   string `json:"kind"`
			Name   string `json:"name"`
			Fields []struct {
				Name  string   `json:"name"`
				Notes []string `json:"notes"`
			} `json:"fields"`
		} `json:"changes"`
	}
	if err := json.Unmarshal(res.Stdout, &out); err != nil {
		t.Fatalf("plan --json is not valid JSON: %v\nstdout: %s", err, res.Stdout)
	}

	var notes []string
	for _, change := range out.Changes {
		if change.Kind != "zone" || change.Name != "Alpha" {
			continue
		}
		for _, field := range change.Fields {
			if field.Name == "networks" {
				notes = append(notes, field.Notes...)
			}
		}
	}
	if len(notes) == 0 {
		t.Fatalf("the zone's networks field carries no notes:\n%s", res.Stdout)
	}
	if !slices.ContainsFunc(notes, func(note string) bool {
		return strings.Contains(note, probe) && strings.Contains(note, internal)
	}) {
		t.Errorf("no note says the Controller will put %q in %q; notes are %q", probe, internal, notes)
	}
}

// The built-in External zone holds the WAN, which is not a network unifig
// manages and has no name the config can use. Stating the LANs in such a zone
// must therefore not detach the uplink from it — the membership is owned per
// member, which is ADR-0004 one level in.
//
// Provenance: this ran against hardware on 18 August 2026, not only against the
// stand-in. A membership PUT to a real built-in zone holding a network unifig
// cannot name returned 200, and an independent read-back found the stated member
// present and the unnameable one still beside it. See
// docs/adr/0019-a-zone-refuses-unifigs-payload-not-the-operators-change.md.
// The same session found that the PUT unifig itself built was refused for the
// three zones marked attr_no_edit — not by this rule, but because go-unifi
// serialises that read-only field back into the request (issue #27). External
// is one of the three, so this test used to pass on a payload no Controller
// would have taken, the stand-in storing whatever it was handed. The stand-in
// refuses that payload now, and unifig no longer builds one.
func TestStatingAZonesNetworksLeavesAMemberUnifigCannotNameAlone(t *testing.T) {
	r := startReplay(t)
	lan := r.aNetwork(t)

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

// A zone the Controller marks `attr_no_edit` takes a membership change like any
// other. The marker reserves nothing: a PUT built by hand carrying the same
// three fields unifig sends for an unmarked zone was accepted by a real UDR on
// `Vpn`, and a read-back found the new member really there (ADR-0019).
//
// What refused every such change was unifig's own payload. go-unifi models the
// marker with `omitempty`, so it went back out on exactly the zones carrying
// it, and the Controller's write DTO answers a field it has not heard of with a
// 400 — which made a serialisation bug look like a rule about which zones the
// Controller lets anyone edit (#27).
func TestApplyChangesTheMembershipOfAZoneTheControllerMarksNoEdit(t *testing.T) {
	r := startReplay(t)
	marked := r.markedZone(t)
	lan := r.aNetwork(t)

	before := r.zoneMembers(t, marked)
	if slices.Contains(before, lan) {
		t.Fatalf("the recording already has %q in the %q zone, so this test would change nothing", lan, marked)
	}

	body := fmt.Sprintf(`networks:
  - name: %q
zones:
  - name: %s
    networks:
      - %q
`, lan, marked, lan)

	res := planFirewall(t, r, body)
	if res.ExitCode != exitChangesPending {
		t.Fatalf("plan exited %d, want %d — a marked zone is one unifig plans an update for like any other\nplan:\n%s",
			res.ExitCode, exitChangesPending, res.Stdout)
	}

	// applyFirewall fails the test on a non-zero exit, which is what a refused
	// payload produces: `update zone "Vpn": Server error (400) ... Unrecognized
	// field "attr_no_edit"`.
	applyFirewall(t, r, body)

	if after := r.zoneMembers(t, marked); !slices.Contains(after, lan) {
		t.Errorf("the %q zone holds %v after the apply, and the config asked for %q to be in it", marked, after, lan)
	}
}

// The assertion the round-trip above cannot make. The stand-in stores whatever
// it is handed, so an apply that reads back correctly says nothing about what
// was in the request — which is how a zone unifig could not write shipped with
// a passing test (ADR-0014, ADR-0019).
//
// The rule is stated over the whole `attr_*` family rather than over the one
// field that has been seen to bite, and the zone is seeded carrying all
// four markers so that stating it is not stating nothing. `attr_no_delete`,
// `attr_hidden` and `attr_hidden_id` are the same shape with the same
// `omitempty`, and the first firmware to put one of them on a zone reproduces
// #27 exactly. Nothing here claims the Controller would refuse them — no one
// has sent one to find out, and the stand-in refuses only what a UDR was
// measured refusing. What is being stated is unifig's own rule: a marker the
// Controller sends is not a field unifig sends back.
func TestTheZoneUnifigWritesBackCarriesNoneOfTheControllersReadOnlyMarkers(t *testing.T) {
	r := startReplay(t)
	lan := r.aNetwork(t)
	r.seedZone(t, "Marked", nil, map[string]any{
		"attr_hidden":    true,
		"attr_hidden_id": "6613a1f0c4b2d90a5e1f7001",
		"attr_no_delete": true,
		"attr_no_edit":   true,
	})

	applyFirewall(t, r, fmt.Sprintf(`networks:
  - name: %q
zones:
  - name: Marked
    networks:
      - %q
`, lan, lan))

	writes := r.zoneWrites(t)
	if len(writes) != 1 {
		t.Fatalf("unifig made %d writes to the zone collection, want the one update this config asks for: %v",
			len(writes), writes)
	}
	sent := writes[0]

	for field := range sent {
		if strings.HasPrefix(field, "attr_") {
			t.Errorf("unifig sent the read-only marker %q back to the Controller: %v", field, sent)
		}
	}
	// And it still carries the shape a real UDR accepted, so that "no markers"
	// cannot be met by sending nothing worth sending.
	for _, needed := range []string{"_id", "name", "network_ids"} {
		if _, carried := sent[needed]; !carried {
			t.Errorf("the update carries no %q: %v", needed, sent)
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
	lan := r.aNetwork(t)
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

	lan := r.aNetwork(t)
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
	lan := r.aNetwork(t)
	r.seedZone(t, "Bystander", []string{lan}, nil)
	r.seedPolicy(t, "Bystander Policy", "ALLOW", "Bystander", "External", nil)

	res := applyFirewall(t, r, "wlans: []\n", "--prune")

	if strings.Contains(string(res.Stdout), "Bystander") {
		t.Errorf("apply --prune has an opinion about a section the config does not have:\n%s", res.Stdout)
	}
	r.zoneNamed(t, "Bystander")
	r.policyNamed(t, "Bystander Policy")
}

// The firewall's half of issue #22, and the same shape as the networks': a file
// with `zones:` and no `firewall-policies:` key puts zones at stake and no
// policy, so prune can reach a zone that a policy it may not touch still
// governs. A policy has to have a zone at either end, so that deletion is one
// unifig can tell will not happen, and a plan is a statement about what will
// (ADR-0014).
//
// What this states is unifig's own promise rather than the Controller's refusal.
// The networks half witnesses the refusal for real — the dockerized Controller
// answers `api.err.ResourceReferredBy` — and nothing here can: the stand-in
// serves a recording, and teaching it to refuse would be writing a fixture that
// asserts a guess about somebody else's product (ADR-0013).
func TestPruneLeavesAZoneAPolicyStillGovernsAlone(t *testing.T) {
	r := startReplay(t)
	lan := r.aNetwork(t)
	r.seedZone(t, "Held Zone", []string{lan}, nil)
	r.seedPolicy(t, "Held Policy", "ALLOW", "Held Zone", "External", nil)

	// A second zone the config does not name, with no policy on either end of it,
	// so the prune under test really runs and really deletes something: the
	// held-back zone is then spared on purpose rather than because nothing
	// happened.
	r.seedZone(t, "Zone Bystander", nil, nil)

	body := `zones:
  - name: Keeper
`
	res := applyFirewall(t, r, body, "--prune")
	stdout := string(res.Stdout)

	if !strings.Contains(stdout, `- zone "Zone Bystander" deleted`) {
		t.Fatalf("the prune under test did not happen:\n%s", stdout)
	}
	if strings.Contains(stdout, `- zone "Held Zone"`) {
		t.Errorf("prune proposed deleting a zone a policy still governs:\n%s", stdout)
	}
	// And says so, rather than reading as a site with nothing more to prune
	// (ADR-0005).
	for _, fragment := range []string{`"Held Zone" will not be deleted`, `"Held Policy"`} {
		if !strings.Contains(stdout, fragment) {
			t.Errorf("the plan should say why it kept the zone, looking for %q:\n%s", fragment, stdout)
		}
	}
	r.zoneNamed(t, "Held Zone")     // fails the test if the apply deleted it
	r.policyNamed(t, "Held Policy") // or the policy on it
}

// Where the rule above stops, and the measurement that decided it. A policy the
// Controller generates for a pair of zones is exempt from prune on its marker
// (ADR-0005), so it survives however the file is written — `firewall-policies:
// []` puts every policy the file can reach at stake and cannot reach that one.
// By the rule above it would therefore hold its zone back, and it did, which made
// `--prune` unable to remove a custom zone on any migrated router: the Controller
// generates policies for the pairs of every zone that holds a member, so every
// zone an operator makes is born held.
//
// Hardware settled it. A custom zone made the Controller generate eighteen
// predefined policies of its own; deleting the zone answered `204` and the
// Controller reclaimed all eighteen itself, so the deletion unifig declined was
// one nobody had ever seen refused (ADR-0019, issue #28). The hold-back is now
// what issue #22 asked for: a policy an operator wrote holds its zone back, and a
// policy the Controller generated does not.
//
// The seeded policy is a fixture, with its limit stated rather than glossed. The
// recording's own predefined policies all govern built-in zones, which prune
// never proposes anyway, so the arrangement this test needs — a deletable zone
// with a generated policy on it — exists only on a Controller that generates
// them. What the fixture asserts is a measurement now (ADR-0019) rather than the
// guess it would have been; what it cannot show is the Controller's reclaim,
// which lives in that ADR's prose.
func TestTheControllersOwnPolicyDoesNotHoldItsZoneBack(t *testing.T) {
	r := startReplay(t)
	lan := r.aNetwork(t)
	r.seedZone(t, "Generated Against", []string{lan}, nil)
	r.seedPolicy(t, "Block All Traffic", "BLOCK", "Generated Against", "External",
		map[string]any{"predefined": true})
	policies := len(r.livePolicies(t))

	res := applyFirewall(t, r, `zones:
  - name: Keeper
firewall-policies: []
`, "--prune")
	stdout := string(res.Stdout)

	if !strings.Contains(stdout, `- zone "Generated Against" deleted`) {
		t.Errorf("prune left a zone only the Controller's own policy governs:\n%s", stdout)
	}
	if strings.Contains(stdout, `"Generated Against" will not be deleted`) {
		t.Errorf("the plan held the zone back on a policy the Controller deletes along with it:\n%s", stdout)
	}
	for _, zone := range r.liveZones(t) {
		if zone["name"] == "Generated Against" {
			t.Errorf("apply --prune did not carry out the deletion it proposed: %v", zone)
		}
	}
	// The policy is still exempt: what changed is whether it holds a zone back,
	// not whether prune may delete it (ADR-0005). The stand-in keeps whatever it
	// is handed, so this counts what unifig chose to delete rather than what a
	// Controller would have reclaimed.
	if left := len(r.livePolicies(t)); left != policies {
		t.Errorf("prune deleted %d of the Controller's own policies", policies-left)
	}
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
	lan := r.aNetwork(t)
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
	lan := r.aNetwork(t)
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
	lan := r.aNetwork(t)
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

// The Risky half of the firewall, and the line it draws (ADR-0018).
//
// A Firewall Policy is Risky when it would newly block traffic to the zone the
// Controller answers in, because that is the change an operator cannot undo over
// the network: the fix is in a UI they can no longer reach. Every other firewall
// change — including one that takes the site's internet away — leaves the
// Controller reachable and the fix one field away, which is ADR-0012's test
// applied to a second area rather than a new rule.
//
// These tests name Internal and External as the existing ones do, but never the
// gateway: that one is asked for by the Controller's own key, so nothing here
// asserts what Ubiquiti calls it.

// riskOfBlockingTheGateway is the part of the risk sentence that belongs to it
// alone. The caveat unifig carries when it cannot find the gateway says "the
// Controller answers in" and "managed over" too, so a test matching either of
// those cannot tell a marked change from an unmarked one in a plan that gave up
// — which is a test that passes whether the lookup works or not (ADR-0013).
const riskOfBlockingTheGateway = "blocking traffic to it can cut the path"

func TestPlanMarksAPolicyThatWouldBlockTheGatewayAsRisky(t *testing.T) {
	r := startReplay(t)
	gateway := r.gatewayZone(t)
	r.seedPolicy(t, "Management", "ALLOW", "Internal", gateway, nil)

	res := planFirewall(t, r, fmt.Sprintf(`firewall-policies:
  - name: Management
    action: block
    source: Internal
    destination: %s
`, gateway))

	if res.ExitCode != exitChangesPending {
		t.Fatalf("plan exited %d, want %d\nstderr: %s", res.ExitCode, exitChangesPending, res.Stderr)
	}
	// The plan is where an operator finds out this one is dangerous, before
	// anyone asks them to approve anything.
	stdout := string(res.Stdout)
	for _, fragment := range []string{`~ firewall-policy "Management"`, "! ", riskOfBlockingTheGateway} {
		if !strings.Contains(stdout, fragment) {
			t.Errorf("plan should say what blocking the gateway risks, looking for %q:\n%s", fragment, stdout)
		}
	}
}

// The mechanism, stated on its own: unifig finds the gateway by the key the
// Controller stores beside the name, so a zone that is the gateway under another
// name is still the gateway. Hard-coding the string "Gateway" would pass every
// other test in this file and fail this one — which is the whole argument of
// issue #23 restated one Resource along.
func TestTheGatewayIsFoundByTheControllersKeyRatherThanItsName(t *testing.T) {
	r := startReplay(t)
	r.renameGateway(t, "Router Services")

	res := planFirewall(t, r, `firewall-policies:
  - name: Lock it down
    action: block
    source: Internal
    destination: Router Services
`)

	if !strings.Contains(string(res.Stdout), riskOfBlockingTheGateway) {
		t.Errorf("a renamed gateway is still the gateway, and blocking it is still Risky:\n%s", res.Stdout)
	}
}

// The deliberate no. Blocking the internet is the change this issue was raised
// about, and it is not Risky: the uplink stays up, the Controller stays
// reachable on its LAN address, and the operator can undo it in a UI they can
// still get to. Marking it would put a confirmation in front of the ordinary
// "stop this VLAN reaching the internet" edit, which is how a prompt stops being
// read (ADR-0012).
func TestBlockingTheInternetIsNotARiskyChange(t *testing.T) {
	r := startReplay(t)

	res := planFirewall(t, r, `firewall-policies:
  - name: No internet for the IoT VLAN
    action: block
    source: Internal
    destination: External
`)

	if res.ExitCode != exitChangesPending {
		t.Fatalf("plan exited %d, want %d\nstderr: %s", res.ExitCode, exitChangesPending, res.Stderr)
	}
	stdout := string(res.Stdout)
	if strings.Contains(stdout, "! ") || strings.Contains(stdout, riskOfBlockingTheGateway) {
		t.Errorf("a policy blocking the internet was marked Risky:\n%s", stdout)
	}
}

// A path already closed cannot be closed again. block -> reject changes what the
// Controller sends back to a blocked packet and nothing about reachability, so
// the confirmation would be one an operator learns to click through.
func TestReplacingOneBlockingVerdictWithAnotherIsNotRisky(t *testing.T) {
	r := startReplay(t)
	gateway := r.gatewayZone(t)
	r.seedPolicy(t, "Already shut", "BLOCK", "Internal", gateway, nil)

	res := planFirewall(t, r, fmt.Sprintf(`firewall-policies:
  - name: Already shut
    action: reject
    source: Internal
    destination: %s
`, gateway))

	if res.ExitCode != exitChangesPending {
		t.Fatalf("plan exited %d, want %d\nstderr: %s", res.ExitCode, exitChangesPending, res.Stderr)
	}
	if strings.Contains(string(res.Stdout), riskOfBlockingTheGateway) {
		t.Errorf("a policy that was already blocking was marked Risky:\n%s", res.Stdout)
	}
}

// The other direction is the operator getting their management path back, and
// nothing about it is worth a warning.
func TestOpeningTheGatewayAgainIsNotRisky(t *testing.T) {
	r := startReplay(t)
	gateway := r.gatewayZone(t)
	r.seedPolicy(t, "Locked out", "BLOCK", "Internal", gateway, nil)

	res := planFirewall(t, r, fmt.Sprintf(`firewall-policies:
  - name: Locked out
    action: allow
    source: Internal
    destination: %s
`, gateway))

	if strings.Contains(string(res.Stdout), riskOfBlockingTheGateway) {
		t.Errorf("opening the management path was marked Risky:\n%s", res.Stdout)
	}
}

// A policy created blocking the gateway is Risky for the same reason one turned
// that way is, and more directly: the Controller's own allow on that pair sits
// at the lowest precedence there is, so a new rule over it takes effect.
func TestCreatingAPolicyThatBlocksTheGatewayIsRisky(t *testing.T) {
	r := startReplay(t)
	gateway := r.gatewayZone(t)

	res := planFirewall(t, r, fmt.Sprintf(`firewall-policies:
  - name: Shut the door
    action: reject
    source: Internal
    destination: %s
`, gateway))

	stdout := string(res.Stdout)
	if !strings.Contains(stdout, `+ firewall-policy "Shut the door"`) || !strings.Contains(stdout, riskOfBlockingTheGateway) {
		t.Errorf("a created policy blocking the gateway should be Risky:\n%s", stdout)
	}
}

// A pipeline gating on the changes that can lock an operator out needs to see
// which ones those are without keeping its own list of dangerous kinds — the
// same promise the WAN suite makes, now that a second kind can carry a risk.
func TestPlanJSONMarksAGatewayBlockingPolicyAsRisky(t *testing.T) {
	r := startReplay(t)
	gateway := r.gatewayZone(t)
	r.seedPolicy(t, "Management", "ALLOW", "Internal", gateway, nil)

	res := planFirewall(t, r, fmt.Sprintf(`firewall-policies:
  - name: Management
    action: block
    source: Internal
    destination: %s
  - name: No internet
    action: block
    source: Internal
    destination: External
`, gateway), "--json")

	if res.ExitCode != exitChangesPending {
		t.Fatalf("plan --json exited %d, want %d\nstderr: %s", res.ExitCode, exitChangesPending, res.Stderr)
	}

	var out struct {
		Changes []struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
			Risk string `json:"risk"`
		} `json:"changes"`
	}
	if err := json.Unmarshal(res.Stdout, &out); err != nil {
		t.Fatalf("plan --json is not valid JSON: %v\nstdout: %s", err, res.Stdout)
	}

	risks := map[string]string{}
	for _, change := range out.Changes {
		risks[change.Name] = change.Risk
	}
	if risks["Management"] == "" {
		t.Errorf("the policy blocking the gateway carries no risk in the JSON: %+v", out.Changes)
	}
	if risks["No internet"] != "" {
		t.Errorf("the policy blocking the internet carries a risk it should not: %q", risks["No internet"])
	}
}

// The Risky-change contract, for the second kind that can carry one: an apply
// the operator already approved wholesale still stops at this change and asks
// about that one on its own (ADR-0009).
func TestApplyAsksAboutAGatewayBlockingPolicyEvenWhenItIsAutoApproved(t *testing.T) {
	r := startReplay(t)
	gateway := r.gatewayZone(t)
	r.seedPolicy(t, "Management", "ALLOW", "Internal", gateway, nil)
	path := configFile(t, fmt.Sprintf(`firewall-policies:
  - name: Management
    action: block
    source: Internal
    destination: %s
`, gateway))

	res := testRig.runUnifigWithInput(t, []string{"apply", "--auto-approve", path}, r.env(), "y\n")
	t.Logf("unifig apply --auto-approve -> exit %d\n%s\n%s", res.ExitCode, res.Stdout, res.Stderr)

	if res.ExitCode != 0 {
		t.Fatalf("apply exited %d\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}
	stdout := string(res.Stdout)
	if !strings.Contains(stdout, "Risky change") || !strings.Contains(stdout, "[y/N]") {
		t.Errorf("--auto-approve should still ask about the gateway change, got:\n%s", stdout)
	}
	if action := r.policyNamed(t, "Management")["action"]; action != "BLOCK" {
		t.Errorf("the operator said yes and the policy did not change: %#v", action)
	}
}

// Refusing one is not cancelling the apply: the question was about that policy,
// and the rest of the file was still asked for. Nothing is hard-blocked either —
// the same run with --allow-risky applies it.
func TestARefusedGatewayPolicyIsSkippedAndTheRestIsApplied(t *testing.T) {
	r := startReplay(t)
	gateway := r.gatewayZone(t)
	r.seedPolicy(t, "Management", "ALLOW", "Internal", gateway, nil)
	// The rest of the file is a firewall change that is not the management path,
	// so that "the rest was still applied" has something to be true of.
	path := configFile(t, fmt.Sprintf(`firewall-policies:
  - name: Management
    action: block
    source: Internal
    destination: %s
  - name: No internet
    action: block
    source: Internal
    destination: External
`, gateway))

	res := testRig.runUnifigWithInput(t, []string{"apply", "--auto-approve", path}, r.env(), "n\n")
	t.Logf("unifig apply --auto-approve -> exit %d\n%s\n%s", res.ExitCode, res.Stdout, res.Stderr)

	if res.ExitCode != 0 {
		t.Fatalf("refusing a Risky change should not be a failure: exit %d\nstdout: %s\nstderr: %s",
			res.ExitCode, res.Stdout, res.Stderr)
	}
	stdout := string(res.Stdout)
	for _, fragment := range []string{`+ firewall-policy "No internet" created`, `"Management"`, "--allow-risky"} {
		if !strings.Contains(stdout, fragment) {
			t.Errorf("apply should report %q, got:\n%s", fragment, stdout)
		}
	}
	if action := r.policyNamed(t, "Management")["action"]; action != "ALLOW" {
		t.Errorf("the operator said no and the policy changed anyway: %#v", action)
	}

	applyFirewall(t, r, fmt.Sprintf(`firewall-policies:
  - name: Management
    action: block
    source: Internal
    destination: %s
`, gateway), "--allow-risky")
	if action := r.policyNamed(t, "Management")["action"]; action != "BLOCK" {
		t.Errorf("--allow-risky did not apply the change: %#v", action)
	}
}

// An apply left running unattended has nobody to answer, and EOF is not a yes.
func TestAGatewayPolicyWithNoOneToAskIsLeftUnapplied(t *testing.T) {
	r := startReplay(t)
	gateway := r.gatewayZone(t)
	r.seedPolicy(t, "Management", "ALLOW", "Internal", gateway, nil)
	path := configFile(t, fmt.Sprintf(`firewall-policies:
  - name: Management
    action: block
    source: Internal
    destination: %s
`, gateway))

	res := testRig.runUnifig(t, []string{"apply", "--auto-approve", path}, r.env())
	t.Logf("unifig apply --auto-approve -> exit %d\n%s\n%s", res.ExitCode, res.Stdout, res.Stderr)

	if action := r.policyNamed(t, "Management")["action"]; action != "ALLOW" {
		t.Errorf("the management path was blocked with nobody there to approve it: %#v", action)
	}
	if !strings.Contains(string(res.Stdout), "--allow-risky") {
		t.Errorf("apply should say how to approve it in advance, got:\n%s", res.Stdout)
	}
}

// Being Risky is what puts a change last, rather than which kind it is. A
// firewall policy sits mid-table because it must follow the zones it names, so
// without this an apply would cut the management path and then try to do the
// rest of its work down a connection that no longer exists.
func TestARiskyPolicyIsAppliedAfterTheSafeWork(t *testing.T) {
	r := startReplay(t)
	gateway := r.gatewayZone(t)
	// Both are updates to a firewall policy, so action and kind are equal and
	// the only thing left to order them by is which one is Risky. They are named
	// so that the Risky one sorts first alphabetically: without the risk
	// criterion this plan comes out in exactly the wrong order, which is what
	// makes the assertion below worth making.
	r.seedPolicy(t, "Aaa management", "ALLOW", "Internal", gateway, nil)
	r.seedPolicy(t, "Zzz no internet", "ALLOW", "Internal", "External", nil)

	res := applyFirewall(t, r, fmt.Sprintf(`firewall-policies:
  - name: Aaa management
    action: block
    source: Internal
    destination: %s
  - name: Zzz no internet
    action: block
    source: Internal
    destination: External
`, gateway), "--allow-risky")

	stdout := string(res.Stdout)
	risky := strings.Index(stdout, `"Aaa management"`)
	safe := strings.Index(stdout, `"Zzz no internet"`)
	if risky < 0 || safe < 0 {
		t.Fatalf("apply did not report both changes:\n%s", stdout)
	}
	if risky < safe {
		t.Errorf("the Risky change was applied before the safe one:\n%s", stdout)
	}
}

// A Controller that answers about its zones and says nothing about a gateway is
// one unifig cannot run this check against. Skipping quietly is the failure that
// matters: an operator would read a plan with no `!` on it as a plan that risks
// nothing (issue #23's lesson, one Resource along).
func TestPlanSaysWhenItCouldNotTellWhetherAPolicyBlocksTheGateway(t *testing.T) {
	r := startReplay(t)
	gateway := r.gatewayZone(t)
	r.hideTheGateway(t)

	res := planFirewall(t, r, fmt.Sprintf(`firewall-policies:
  - name: Management
    action: block
    source: Internal
    destination: %s
`, gateway))

	stdout := string(res.Stdout)
	for _, fragment := range []string{"firewall-policy:", "no firewall policy is marked", "could not read"} {
		if !strings.Contains(stdout, fragment) {
			t.Errorf("the plan should say it could not run the check, looking for %q:\n%s", fragment, stdout)
		}
	}
	// A caveat and a risk line both lead with `!`, so the test has to say which
	// one it wants: the change itself must not be marked, because unifig does
	// not know that it blocks anything.
	if strings.Contains(stdout, riskOfBlockingTheGateway) {
		t.Errorf("the change was marked Risky though the gateway could not be read:\n%s", stdout)
	}
}

// And it is only said when there was a question to answer. A firewall plan that
// blocks nothing had nothing to check, and a caveat on every run is one an
// operator reads past by the third.
func TestAPlanThatBlocksNothingSaysNothingAboutTheGateway(t *testing.T) {
	r := startReplay(t)
	r.hideTheGateway(t)

	res := planFirewall(t, r, `firewall-policies:
  - name: Let them through
    action: allow
    source: Internal
    destination: External
`)

	if strings.Contains(string(res.Stdout), "no firewall policy is marked") {
		t.Errorf("a plan with nothing blocking in it carried the gateway caveat:\n%s", res.Stdout)
	}
}
