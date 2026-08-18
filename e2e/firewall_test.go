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

// The same assertion for a policy, made the same way and for the same reason:
// the stand-in stores whatever it is handed, so an apply that reads back
// correctly says nothing about what the request carried (ADR-0014, ADR-0019).
//
// What differs is the evidence behind it, and the test should not be read as
// claiming otherwise. A real UDR was measured refusing `attr_no_edit` on a zone;
// nobody has ever sent one to the policy endpoint, and no policy has been seen
// carrying a marker at all — none of the eighty-three a migrated router ships
// has an `attr_*` field. What is stated here is unifig's own rule rather than
// the Controller's: a marker the Controller sends is not a field unifig sends
// back, on every object unifig writes whole (issue #34).
//
// The positive half is taken from the domain rather than from a measured
// minimal shape, which exists for the zone and not for this endpoint. A policy's
// key is its name together with the pair of zones it governs, and the config
// states one field beyond that key — so the key and the verdict are what an
// update has to carry for "no markers" to be worth asserting.
func TestThePolicyUnifigWritesBackCarriesNoneOfTheControllersReadOnlyMarkers(t *testing.T) {
	r := startReplay(t)
	r.seedPolicy(t, "Marked", "ALLOW", "Internal", "External", map[string]any{
		"attr_hidden":    true,
		"attr_hidden_id": "6613a1f0c4b2d90a5e1f7002",
		"attr_no_delete": true,
		"attr_no_edit":   true,
		// A fifth that go-unifi does not model, invented here on purpose and
		// named so it cannot be mistaken for a field anybody has read. It is
		// what tells the rule apart from its old implementation: clearing the
		// four the library models let `omitempty` drop them, and dropped this
		// one by never having heard of it. Merging into the object the
		// Controller sent puts it back unless the rule is kept by name
		// (issue #35).
		"attr_invented_by_this_test": true,
	})

	applyFirewall(t, r, `firewall-policies:
  - name: Marked
    action: block
    source: Internal
    destination: External
`)

	sent := r.onlyPolicyWrite(t)

	for field := range sent {
		if strings.HasPrefix(field, "attr_") {
			t.Errorf("unifig sent the read-only marker %q back to the Controller: %v", field, sent)
		}
	}
	for _, needed := range []string{"_id", "name", "action"} {
		if _, carried := sent[needed]; !carried {
			t.Errorf("the update carries no %q: %v", needed, sent)
		}
	}
	// Both ends, because a policy's key is the pair as much as the name: an
	// update that lost one would be a write to a different policy.
	for _, end := range []string{"source", "destination"} {
		zone, _ := sent[end].(map[string]any)
		if id, _ := zone["zone_id"].(string); id == "" {
			t.Errorf("the update carries no zone at its %s end: %v", end, sent)
		}
	}
}

// The defect issue #35 measured on hardware, in the one form a stand-in can
// hold: a policy an operator narrowed in the Controller's UI, and an apply that
// means to change the verdict and nothing else.
//
// A v2 PUT replaces the object rather than merging into it. That was the guess
// this stand-in encoded and it is now a measurement: on 18 August 2026 unifig
// changed one live policy's `action` on a migrated UDR and reverted its
// `icmp_typename` from `ECHO_REQUEST` to `ANY` in the same request, silently
// (ADR-0021). So a field missing from the body is a field the operator loses,
// and what the body carries is the whole of what survives.
//
// The four fields seeded here are the losses that probe found, by the two
// mechanisms it found them by, and every one of them is invisible in the plan:
//
//   - `origin_id`, `origin_type` and `icmp_typename` are gone at unmarshal,
//     because go-unifi v2.3.0 does not model them. The last is the one the probe
//     watched revert — the ICMP matching a narrowed policy is narrowed by — and
//     the first two are a back-reference to whatever made the policy, read off
//     `origin_type`'s own value of `network_config` rather than off anything
//     Ubiquiti documents. Severing that is a sharper thing than a narrowing an
//     operator can redo, which is why they are in here beside it.
//   - `description` is gone for the other reason: go-unifi does model it, and
//     `omitempty` elides an empty string. The loss was never confined to the
//     fields the library has never heard of, and a fix scoped that way would
//     have left this one behind.
//
// The mirror image is asserted by absence. The recording's policies carry a
// schedule of `{mode: ALWAYS}` and no `time_all_day` at all, while a Go bool
// serialises as `false` whether the Controller sent one or not — and a field
// unifig invents is as much a change the operator never asked for as a field it
// drops.
func TestUpdatingAPolicySendsBackEveryFieldTheControllerSent(t *testing.T) {
	r := startReplay(t)
	sentByTheController := map[string]any{
		"origin_id":     "6613a1f0c4b2d90a5e1f7101",
		"origin_type":   "network_config",
		"icmp_typename": "ECHO_REQUEST",
		"description":   "",
	}
	r.seedPolicy(t, "Narrowed", "ALLOW", "Internal", "External", sentByTheController)

	applyFirewall(t, r, `firewall-policies:
  - name: Narrowed
    action: block
    source: Internal
    destination: External
`)

	sent := r.onlyPolicyWrite(t)

	for field, want := range sentByTheController {
		got, carried := sent[field]
		if !carried {
			t.Errorf("the update dropped %q, which the Controller sent and a PUT replaces: %v", field, sent)
			continue
		}
		if got != want {
			t.Errorf("the update sent %q as %v, want %v — the operator set it and unifig does not model it", field, got, want)
		}
	}
	schedule, _ := sent["schedule"].(map[string]any)
	if _, invented := schedule["time_all_day"]; invented {
		t.Errorf("the update tells the Controller a schedule field it never sent: %v", schedule)
	}

	// The change the operator did ask for still happens, and what the Controller
	// holds afterwards is the whole policy rather than the part unifig can name.
	if sent["action"] != "BLOCK" {
		t.Errorf("the update sent action %v, want the BLOCK the config asks for", sent["action"])
	}
	if typename := r.policyNamed(t, "Narrowed")["icmp_typename"]; typename != "ECHO_REQUEST" {
		t.Errorf("the policy matches ICMP %v after the apply, and the operator had narrowed it to ECHO_REQUEST", typename)
	}
}

// The create-path counterpart, and the one field where unifig was measured
// disagreeing with the Controller about a policy it makes.
//
// `newFirewallPolicy` sets the fields a policy needs to govern traffic at all,
// because a bare struct would be disabled, on no schedule and matching nothing.
// `create_allow_respond` is modelled by go-unifi v2.3.0 as a plain `bool` with
// no `omitempty`, so it goes on the wire on every create whether unifig has an
// opinion about it or not — and unifig had none, which made the Go zero value
// the opinion. On the live migrated UDR on 18 August 2026 all eighty-six
// policies the Controller ships carried `true` and the one unifig created
// carried `false`, with the Controller's UI showing that policy's return-traffic
// toggle off beside its own policies' on (issue #36). This test counts
// eighty-three of them, because that is how many of the eighty-six the recording
// holds.
//
// What the field does to traffic is **not** what this asserts, and nothing in
// this repository has measured it: no one has sent a conversation through a
// unifig-created allow policy and watched what came back. The rule being stated
// is the narrower one the evidence supports — a policy unifig creates agrees
// with the Controller's own about return traffic — which is why the value is
// read off the recording rather than written here as `true`. A recording from a
// router whose own policies said otherwise would move this test's expectation
// with it, the way markedZone and gatewayZone are asked rather than named.
//
// It is asserted on the request rather than the round-trip for the reason the
// marker tests are: the stand-in stores whatever it is handed, so reading the
// policy back afterwards would pass just as well if unifig had sent nothing at
// all (ADR-0014, ADR-0019).
func TestCreatingAnAllowPolicyAsksTheControllerForTheReturnRule(t *testing.T) {
	r := startReplay(t)

	// Read before the apply, so what is being compared against is the
	// Controller's own policies rather than the one this test is about to make.
	const field = "create_allow_respond"
	own := r.livePolicies(t)
	if len(own) == 0 {
		t.Fatalf("the recording holds no policy of the Controller's own to agree with about %q", field)
	}
	want, carried := own[0][field]
	if !carried {
		t.Fatalf("the recording's policies carry no %q, so there is nothing here for unifig to agree or disagree with", field)
	}
	for _, policy := range own {
		if policy[field] != want {
			// Fatal rather than skipped, the way markedZone fails when the
			// recording cannot answer it. A recording whose own policies
			// disagreed would retire the only assertion on unifig's create body
			// while `CreateAllowRespond` stayed set in the code, and a pin that
			// quietly stops pinning is the thing ADR-0014 objects to.
			t.Fatalf("the recording's %d policies disagree among themselves about %q, so there is no value of the Controller's to match: re-read #36 against that router before trusting the one unifig sends",
				len(own), field)
		}
	}

	applyFirewall(t, r, `firewall-policies:
  - name: Let them answer
    action: allow
    source: Internal
    destination: External
`)

	sent := r.onlyPolicyWrite(t)

	got, carried := sent[field]
	if !carried {
		t.Fatalf("the create carries no %q, and go-unifi sends it without omitempty on every create: %v", field, sent)
	}
	if got != want {
		t.Errorf("unifig creates a policy with %q = %v, and all %d of the Controller's own carry %v",
			field, got, len(own), want)
	}
}

// The other half of the same rule, and the half that cost an apply on hardware.
//
// `create_allow_respond` asks the Controller to generate the companion return
// rule. On a policy that blocks there is no traffic to return, and the
// Controller does not treat the request as meaningless — it refuses it:
//
//	400: Firewall policy create respond traffic not allowed
//
// Measured on the live migrated UDR on 18 August 2026, on an apply that had two
// policies to create and made neither (issue #36, ADR-0022). unifig had been
// sending the field true on every create for exactly as long as it took to run
// this probe, which is to say it could not create a `block` or a `reject` policy
// at all in that window.
//
// This asserts the request rather than the refusal, for ADR-0019's reason, and
// the stand-in asserts the refusal separately in refusedByPolicyCreate — so a
// unifig that sent the pair again would fail here on the body it built and there
// on the answer it got, which is the pairing the marker tests use.
func TestCreatingABlockingPolicyDoesNotAskForAReturnRule(t *testing.T) {
	// Both verdicts that close a path, because the config models three and the
	// one measured refused is only the first of them.
	for _, verdict := range []string{"block", "reject"} {
		t.Run(verdict, func(t *testing.T) {
			r := startReplay(t)
			applyFirewall(t, r, fmt.Sprintf(`firewall-policies:
  - name: Shut %s
    action: %s
    source: Internal
    destination: External
`, verdict, verdict))

			sent := r.onlyPolicyWrite(t)
			respond, carried := sent["create_allow_respond"]
			if !carried {
				t.Fatalf("the create carries no %q at all, and go-unifi sends it without omitempty: %v",
					"create_allow_respond", sent)
			}
			if respond != false {
				t.Errorf("unifig asked the Controller to make a return rule for a policy that %ss, and the Controller refuses that pair 400: %v",
					verdict, sent)
			}
		})
	}
}

// The plan says what the apply will do, and creating one allow policy makes two
// policies (ADR-0022).
//
// Asking the Controller for the return rule is a request it acts on: measured on
// the live migrated UDR on 18 August 2026, one allow policy created with
// `create_allow_respond` true took the site from 86 policies to 88, the second
// named after the first and reclaimed with it. That is the shape ADR-0014 holds
// a plan to — "a plan is a statement about what will happen" — and the shape
// issue #32 fixed for a zone's membership, where one PUT moved two zones and the
// plan named one.
//
// The note is on the verdict rather than on the policy, because the verdict is
// what decides it: the Controller refuses the same request on a policy that
// blocks, so a create that says `block` makes exactly one policy and says so by
// carrying no note at all.
func TestPlanSaysTheControllerWillMakeTheReturnRuleForAnAllowPolicy(t *testing.T) {
	r := startReplay(t)

	res := planFirewall(t, r, `firewall-policies:
  - name: Let them answer
    action: allow
    source: Internal
    destination: External
`)
	stdout := string(res.Stdout)
	for _, fragment := range []string{`+ firewall-policy "Let them answer"`, "Let them answer (Return)", "reply"} {
		if !strings.Contains(stdout, fragment) {
			t.Errorf("the plan does not say the Controller will make the return rule (%q missing):\n%s", fragment, stdout)
		}
	}
}

// The other half, and the reason the note is computed rather than printed on
// every create: there is no second policy to announce when the Controller
// refuses to make one.
func TestPlanSaysNothingAboutAReturnRuleForAPolicyThatBlocks(t *testing.T) {
	r := startReplay(t)

	res := planFirewall(t, r, `firewall-policies:
  - name: Shut it
    action: block
    source: Internal
    destination: External
`)
	if stdout := string(res.Stdout); strings.Contains(stdout, "Return") {
		t.Errorf("the plan promises a return rule for a policy that blocks, which the Controller refuses to make:\n%s", stdout)
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
