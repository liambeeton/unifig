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

	sent := r.onlyZoneWrite(t)

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

// The whole of what a zone update puts on the wire, asserted as an exact set
// rather than as a list of things that must not be in it.
//
// The negative tests beside this one — no `attr_*`, none of the fields the
// Controller sent — each name what they are looking for, so each is blind to the
// field nobody thought to name. That is not hypothetical: `site_id` was on the
// wire for the whole of this repository's history and no test saw it. go-unifi
// models it as `json:"site_id,omitempty"` and no zone GET has ever returned one,
// so the value stayed empty and `omitempty` kept it home — the same accident as
// `attr_no_edit`, which went out exactly when it was true, with the sign
// flipped. Issue #38 sent one and the Controller answered
// `400 Unrecognized field "site_id"` like all the rest (ADR-0024).
//
// So the cover is the one ADR-0019 said it had to be — an assertion about the
// request, because a stand-in that stores what it is handed reads any payload
// back as a success — and it is stated as an exact set rather than as a list of
// names.
//
// That last part is what this test is for, and it is worth being exact about,
// because `site_id` itself is no longer the field it would catch: it is on
// refusedByZoneWrite now, so the stand-in answers 400 and the apply fails before
// these assertions run. What an exact set catches is the field nobody has
// thought of — the three `attr_*` siblings deliberately left off that list
// because no hardware has refused them, and whatever the next go-unifi models
// without `omitempty`. Every other assertion here has to be told a name first.
// This one does not, which is the only reason it is a separate test and not
// three more lines in the one below.
//
// The three are the measured accepted set rather than a preference: `_id`,
// `name` and `network_ids` are what this DTO takes, and every other field a
// zone GET returns is a 400.
func TestTheZoneUnifigWritesBackCarriesOnlyTheThreeFieldsItsDTOTakes(t *testing.T) {
	r := startReplay(t)
	lan := r.aNetwork(t)
	// Seeded with the read shape of a live custom zone. `seedZone` stamps a
	// `site_id` of its own besides, which is the field this test exists for:
	// no Controller has been seen sending one, and unifig sent it anyway.
	r.seedZone(t, "Whole", nil, map[string]any{
		"external_id":    "28a563df-4bdc-4f9f-b795-76b4ea54dbf1",
		"zone_key":       nil,
		"default_zone":   false,
		"cloud_template": nil,
	})

	applyFirewall(t, r, fmt.Sprintf(`networks:
  - name: %q
zones:
  - name: Whole
    networks:
      - %q
`, lan, lan))

	sent := r.onlyZoneWrite(t)
	takenByTheDTO := []string{"_id", "name", "network_ids"}
	for _, field := range takenByTheDTO {
		if _, carried := sent[field]; !carried {
			t.Errorf("the update carries no %q, which this DTO needs: %v", field, sent)
		}
	}
	for field := range sent {
		if !slices.Contains(takenByTheDTO, field) {
			t.Errorf("the update carries %q, which this DTO answers 400 to: %v", field, sent)
		}
	}
}

// What the Controller sent a zone with survives an apply, and unifig sends none
// of it back — which on this endpoint is not a contradiction but the mechanism.
//
// The policy endpoint replaces, so carrying the operator's fields back is the
// only way to keep them — ADR-0021, and
// TestUpdatingAPolicySendsBackEveryFieldTheControllerSent below.
// Reading the same rule onto a zone would have been the
// obvious move and would have broken every zone update there is: this DTO takes
// `_id`, `name` and `network_ids` and answers 400 to every other field a zone
// GET returns, so the merge that saved the policies is not a request a zone can
// even make. It does not have to. Issue #38 put a mutating PUT of exactly this
// three-field shape to a throwaway custom zone on the live migrated UDR on 19
// August 2026, and the `external_id` the body did not carry — a UUID on a custom
// zone, which the Controller cannot regenerate the way it can a built-in's
// `zone_key` — was still there on an independent read afterwards. **A v2 zone
// PUT merges** (ADR-0024).
//
// Both halves are asserted, and they are not the same kind of claim. The
// request loop is about unifig: it may not send these, and a body carrying one
// is a 400. The read-back is about the **stand-in** — with those fields refused,
// unifig cannot send them, so nothing here could make them vanish except the
// fixture deciding they do. That is exactly the assertion worth having: this
// stand-in replaced on zones until ADR-0024, on a guess its own comment owned
// up to, and this is what fails if anyone flips it back.
func TestUpdatingAZoneLeavesTheFieldsTheControllerSentAlone(t *testing.T) {
	r := startReplay(t)
	lan := r.aNetwork(t)
	// The read shape of a custom zone as the live router answers with it: a
	// `zone_key` and a `default_zone` the Controller could regenerate, and an
	// `external_id` it could not, which is why #38 needed a custom zone to be
	// able to tell a merge from a replace at all.
	sentByTheController := map[string]any{
		"external_id":    "28a563df-4bdc-4f9f-b795-76b4ea54dbf1",
		"cloud_template": nil,
		"zone_key":       nil,
		"default_zone":   false,
	}
	r.seedZone(t, "Probe", nil, sentByTheController)

	applyFirewall(t, r, fmt.Sprintf(`networks:
  - name: %q
zones:
  - name: Probe
    networks:
      - %q
`, lan, lan))

	sent := r.onlyZoneWrite(t)
	for field := range sentByTheController {
		if _, carried := sent[field]; carried {
			t.Errorf("the update carries %q, which this endpoint answers 400 to: %v", field, sent)
		}
	}

	held := r.zoneNamed(t, "Probe")
	for field, want := range sentByTheController {
		got, kept := held[field]
		if !kept {
			t.Errorf("the zone lost %q over an apply that could not have sent it: %v", field, held)
			continue
		}
		if got != want {
			t.Errorf("the zone holds %q as %v after the apply, and the Controller had it as %v", field, got, want)
		}
	}

	// And the change the operator did ask for still happened, so "nothing was
	// lost" cannot be met by an update that did nothing.
	if members, _ := held["network_ids"].([]any); len(members) != 1 {
		t.Errorf("the zone holds %d networks after an apply that puts one in it: %v", len(members), held)
	}
}

// The same assertion for a policy, made the same way and for the same reason:
// the stand-in stores whatever it is handed, so an apply that reads back
// correctly says nothing about what the request carried (ADR-0014, ADR-0019).
//
// What differs is the evidence behind it, and the test should not be read as
// claiming otherwise. A real UDR was measured refusing `attr_no_edit` on a zone;
// nobody has ever sent one to the policy endpoint, and no policy has been seen
// carrying a marker at all — none of the eighty-six a migrated router ships
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
// The **policy** endpoint replaces rather than merging into the object. That was
// the guess this stand-in encoded and it is now a measurement: on 18 August 2026 unifig
// changed one live policy's `action` on a migrated UDR and reverted its
// `icmp_typename` from `ECHO_REQUEST` to `ANY` in the same request, silently
// (ADR-0021). So a field missing from the body is a field the operator loses,
// and what the body carries is the whole of what survives. It is the endpoint
// that behaves this way rather than the API version: the zone collection beside
// it was measured merging (ADR-0024).
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
// toggle off beside its own policies' on (issue #36). This test counts all
// eighty-six: the recording used to hold eighty-three of them, and refreshing it
// for issue #41 closed the gap, so the number here and the number on the router
// are the same number now.
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
    destination: Dmz
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
// the stand-in asserts the refusal separately in refusedByPolicyWrite — so a
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

// The second condition on the request, and the one an operator found the hard
// way rather than a probe.
//
// ADR-0022 had the rule as "ask on an allow and not otherwise", measured against
// a `Dmz -> Dmz` probe. That generalised from the one pair it ran on. An
// operator applying a twelve-policy firewall got three blocking policies created
// and then, on the first `allow` the apply came to, the identical 400 the
// blocking creates had been refused with:
//
//	unifig: create firewall-policy "Cyberdelia to internet": Server error (400)
//	for POST .../firewall-policies: Firewall policy create respond traffic not
//	allowed
//
// Three probes on the live migrated UDR on 25 August 2026 closed it, and they
// are why this is a rule about the destination rather than about anything else
// the failing policy happened to be (ADR-0030):
//
//   - `Dmz -> External`, `ALLOW`, the flag true: refused 400. So it is the
//     destination and not the source, the zone's age or its membership.
//   - `Dmz -> External`, `ALLOW`, the flag false: accepted 201, and the site
//     gained one policy rather than two. So the Controller takes the allow
//     perfectly well and simply generates nothing — which is what makes not
//     asking a fix rather than a different failure.
//   - `Internal -> Dmz`, `ALLOW`, the flag true: accepted 201, and the site
//     gained two onto a pair whose reverse already held a `RESPOND_ONLY` rule.
//     That refuted the reading that the refusal was about the reverse pair being
//     occupied, which fitted every observation until it was tested.
//
// Asserted on the request rather than the round-trip, for ADR-0019's reason and
// the one ADR-0022 gave when this same test was got wrong the first time: a
// stand-in that stores what it is handed would pass a body hardware refuses. The
// stand-in refuses it too, in refusedByPolicyWrite, so a unifig that asked again
// fails here on the body it built and there on the answer it got.
func TestCreatingAnAllowPolicyIntoExternalDoesNotAskForTheReturnRule(t *testing.T) {
	r := startReplay(t)
	external := r.externalZone(t)

	applyFirewall(t, r, fmt.Sprintf(`firewall-policies:
  - name: Out to the internet
    action: allow
    source: %s
    destination: %s
`, r.internalZone(t), external))

	sent := r.onlyPolicyWrite(t)
	if respond := sent["create_allow_respond"]; respond != false {
		t.Errorf("unifig asked the Controller to make a return rule for a policy into %q, and the Controller refuses that 400: %v",
			external, sent)
	}
	if r.hasPolicyNamed(t, "Out to the internet (Return)") {
		t.Errorf("something generated a companion for a policy into %q, which hardware was measured not doing", external)
	}
}

// The update half, on the same rule. It matters separately because the two paths
// reach the field by different routes — a create builds a struct, an update
// merges into the object the Controller sent (ADR-0021) — and ADR-0022's first
// reading of this endpoint was wrong precisely because it checked one and
// generalised to both.
func TestUpdatingAPolicyToAllowIntoExternalDoesNotAskForTheReturnRule(t *testing.T) {
	r := startReplay(t)
	external := r.externalZone(t)
	internal := r.internalZone(t)
	r.seedPolicy(t, "Was Shut", "BLOCK", internal, external, map[string]any{
		"create_allow_respond": false,
	})

	applyFirewall(t, r, fmt.Sprintf(`firewall-policies:
  - name: Was Shut
    action: allow
    source: %s
    destination: %s
`, internal, external))

	sent := r.onlyPolicyWrite(t)
	if respond := sent["create_allow_respond"]; respond != false {
		t.Errorf("the update asked for a return rule on a policy into %q, which the Controller refuses on a PUT exactly as on a POST: %v",
			external, sent)
	}
}

// ADR-0014's standard, applied to the companion: a plan is a statement about
// what will happen. A policy into External never gets a companion, so a plan
// that promises one is promising an object that is never going to appear —
// which is the same defect the return-rule note was added to fix, pointing the
// other way.
func TestPlanPromisesNoReturnRuleForAPolicyIntoExternal(t *testing.T) {
	r := startReplay(t)
	external := r.externalZone(t)

	res := planFirewall(t, r, fmt.Sprintf(`firewall-policies:
  - name: Out to the internet
    action: allow
    source: %s
    destination: %s
`, r.internalZone(t), external))

	stdout := string(res.Stdout)
	if !strings.Contains(stdout, `+ firewall-policy "Out to the internet"`) {
		t.Fatalf("the plan does not hold the policy this is about:\n%s", stdout)
	}
	if strings.Contains(stdout, "(Return)") || strings.Contains(stdout, "reply traffic") {
		t.Errorf("the plan promises a return rule for a policy into %q, and the Controller will not make one:\n%s",
			external, stdout)
	}
}

// The arrangement the bug was reported in, end to end: a firewall holding both
// verdicts, where the policies that block sort before the one that allows.
//
// It is a whole-apply test rather than another request-shape one because the
// failure was a whole-apply failure. Changes run in the order the plan printed
// them and apply stops at the first that fails, so what an operator got was a
// half-written firewall — the blocking policies created, the allow refused, and
// every policy after it untouched. The two assertions that matter are that the
// apply finishes and that the next plan is empty; the second is what says the
// firewall the file describes is the firewall the Controller now holds, rather
// than one that merely stopped erroring.
func TestApplyingAFirewallThatAllowsIntoExternalAppliesAllOfIt(t *testing.T) {
	r := startReplay(t)
	internal, external := r.internalZone(t), r.externalZone(t)

	body := fmt.Sprintf(`firewall-policies:
  - name: A blocked path
    action: block
    source: %s
    destination: Dmz
  - name: B out to the internet
    action: allow
    source: %s
    destination: %s
`, internal, internal, external)

	res := applyFirewall(t, r, body, "--prune")
	if res.ExitCode != 0 {
		t.Fatalf("apply exited %d, and every policy in this file is one the Controller takes\nstdout:\n%s\nstderr:\n%s",
			res.ExitCode, res.Stdout, res.Stderr)
	}
	for _, name := range []string{"A blocked path", "B out to the internet"} {
		if !r.hasPolicyNamed(t, name) {
			t.Errorf("apply did not create %q:\n%s", name, res.Stdout)
		}
	}

	if next := planFirewall(t, r, body); next.ExitCode != exitNoChanges {
		t.Errorf("the plan after the apply exited %d, want %d — the firewall does not match the file it was applied from:\n%s",
			next.ExitCode, exitNoChanges, next.Stdout)
	}
}

// The same refusal on the update path, where it breaks not an unlucky policy
// but every one of them.
//
// ADR-0022 measured a policy created `block` and updated to `allow` — the body
// carried `create_allow_respond: false` alongside `ALLOW`, which the Controller
// took, and the ADR wrote down that "the update path neither refuses nor
// generates". That was the half of the mirror the probe happened to run. The
// other half was measured on the live migrated UDR on 19 August 2026 (issue
// #37): a policy created `allow` and updated to `block` sends the stored `true`
// back alongside `BLOCK`, and the Controller answers
//
//	400: Firewall policy create respond traffic not allowed
//
// which is the same refusal `refusedByPolicyWrite` was written for, reached
// through the update rather than the create. It is not a narrow case. Every one
// of the 86 policies the migrated router holds carries the flag true — the 34
// that already block included — so under ADR-0021's merge, *every* allow -> block
// update fails, and it fails in the direction an operator tightens a firewall.
//
// The fix is the rule the create already keeps, applied to the object the merge
// puts back: the request goes out true only on a verdict that leaves a path
// open. What it deliberately does not do is set the flag true when the verdict
// becomes `allow`. Whether unifig should own the flag that way — so a companion
// follows the config rather than the policy's history — is issue #40's question,
// and answering it here would be answering it by accident.
func TestUpdatingAPolicyToAVerdictThatClosesAPathDoesNotAskForTheReturnRule(t *testing.T) {
	// Both verdicts that close a path, for the reason the create's twin gives:
	// only `block` reached the wire on hardware, and the rule the Controller
	// states is about the verdict rather than about that one word.
	for _, verdict := range []string{"block", "reject"} {
		t.Run(verdict, func(t *testing.T) {
			r := startReplay(t)
			// The shape every policy the Controller ships has, which is what
			// seedPolicy gives a policy unless a test says otherwise.
			r.seedPolicy(t, "Was Open", "ALLOW", "Internal", "External", nil)

			applyFirewall(t, r, fmt.Sprintf(`firewall-policies:
  - name: Was Open
    action: %s
    source: Internal
    destination: External
`, verdict))

			sent := r.onlyPolicyWrite(t)
			respond, carried := sent["create_allow_respond"]
			if !carried {
				t.Fatalf("the update carries no %q at all, and the policy it merged into had one: %v",
					"create_allow_respond", sent)
			}
			if respond != false {
				t.Errorf("unifig put the stored request for a return rule back on a policy that now %ss, and the Controller refuses that pair 400: %v",
					verdict, sent)
			}
		})
	}
}

// The other side of the same edit, and it reversed when the reading arrived.
//
// unifig used to send the stored flag back untouched on a verdict that opens a
// path, because setting it was a write nobody had watched a Controller answer.
// Issue #40's probe watched it, on the live migrated UDR on 19 August 2026, with
// the verdict held at `ALLOW` so that only the flag moved: false -> true took the
// site 87 -> 88 and the companion came back. So the flag drives the companion on
// an update as it does on a create, and unifig owns it (ADR-0026).
//
// What that buys is the whole of issue #40: a policy's companion follows the
// config rather than the policy's history, so the same file gives two operators
// the same firewall.
func TestUpdatingAPolicyToAnOpenVerdictAsksForTheReturnRule(t *testing.T) {
	r := startReplay(t)
	// A policy created blocking, which is how the Controller records one that
	// never asked for a companion (ADR-0022) — issue #40's second table row.
	r.seedPolicy(t, "Was Shut", "BLOCK", "Internal", "Dmz", map[string]any{
		"create_allow_respond": false,
	})

	applyFirewall(t, r, `firewall-policies:
  - name: Was Shut
    action: allow
    source: Internal
    destination: Dmz
`)

	sent := r.onlyPolicyWrite(t)
	if respond := sent["create_allow_respond"]; respond != true {
		t.Errorf("unifig left %q at %v on a policy the config says allows, so its companion would follow the policy's history rather than the file: %v",
			"create_allow_respond", respond, sent)
	}
}

// A field unifig owns is one it states, and that is not the defect ADR-0021
// named.
//
// This used to assert the opposite: that an update writes no `create_allow_respond`
// onto a policy the Controller sent none for. That was right while unifig only
// ever cleared the flag — writing a Go zero onto a field nobody had set is the
// `schedule.time_all_day` defect, and the narrowest legal body was the one that
// touched nothing it did not have to.
//
// Owning the field changes what the rule is about. ADR-0021 is about fields
// unifig does **not** model: those must survive an update untouched, because a
// value unifig invents for one is a value the operator never chose. This one
// unifig now models — it is the verdict, restated as the request the Controller
// acts on — and a modelled field that went out only sometimes would be a policy
// whose companion depended on what the Controller happened to be holding, which
// is the whole of what issue #40 was filed about.
//
// Every policy either site holds carries the key, so this seeds a shape neither
// has, and pins what unifig puts on the wire rather than anything about the
// Controller.
func TestUpdatingAPolicyStatesTheReturnRuleRequestEvenWhereTheControllerSentNone(t *testing.T) {
	r := startReplay(t)
	r.seedPolicy(t, "No Such Field", "ALLOW", "Internal", "External", map[string]any{
		"create_allow_respond": nil,
	})

	applyFirewall(t, r, `firewall-policies:
  - name: No Such Field
    action: block
    source: Internal
    destination: External
`)

	sent := r.onlyPolicyWrite(t)
	if respond, carried := sent["create_allow_respond"]; !carried || respond != false {
		t.Errorf("unifig did not state %q on a policy it now owns the field of (carried=%v, value=%v): %v",
			"create_allow_respond", carried, respond, sent)
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
    destination: Dmz
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

// The update path's half of the same sentence, and the silence issue #37 left
// behind: an operator approving a one-word verdict change was deleting a second
// policy with no warning at all.
//
// It is a field rather than a note, because under ownership the return rule is a
// thing that can differ on its own — see the test below, where it is the only
// difference there is.
func TestPlanSaysTheReturnRuleGoesWhenAPolicyStopsAllowing(t *testing.T) {
	for _, verdict := range []string{"block", "reject"} {
		t.Run(verdict, func(t *testing.T) {
			r := startReplay(t)
			// A policy unifig created allowing: the request, and the companion
			// the Controller generated from it.
			r.seedPolicy(t, "Was Open", "ALLOW", "Internal", "External", nil)
			r.seedPolicy(t, "Was Open (Return)", "ALLOW", "External", "Internal", map[string]any{
				"predefined":            true,
				"connection_state_type": "RESPOND_ONLY",
			})

			res := planFirewall(t, r, fmt.Sprintf(`firewall-policies:
  - name: Was Open
    action: %s
    source: Internal
    destination: External
`, verdict))
			stdout := string(res.Stdout)
			for _, fragment := range []string{`~ firewall-policy "Was Open"`, "return-rule", `"Was Open (Return)"`} {
				if !strings.Contains(stdout, fragment) {
					t.Errorf("the plan does not say the return rule goes (%q missing):\n%s", fragment, stdout)
				}
			}
		})
	}
}

// The row issue #40 was filed for, and it is now a change rather than a warning:
// a policy created blocking and later allowed gains the companion it would have
// had if the config had built it from nothing.
func TestPlanSaysTheReturnRuleArrivesWhenAPolicyStartsAllowing(t *testing.T) {
	r := startReplay(t)
	r.seedPolicy(t, "Was Shut", "BLOCK", "Internal", "Dmz", map[string]any{
		"create_allow_respond": false,
	})

	res := planFirewall(t, r, `firewall-policies:
  - name: Was Shut
    action: allow
    source: Internal
    destination: Dmz
`)
	stdout := string(res.Stdout)
	for _, fragment := range []string{`~ firewall-policy "Was Shut"`, "return-rule", `"Was Shut (Return)"`} {
		if !strings.Contains(stdout, fragment) {
			t.Errorf("the plan does not say the return rule arrives (%q missing):\n%s", fragment, stdout)
		}
	}
}

// The return rule on its own, with no verdict change behind it — the case a note
// could not have carried, because a note needs a field that is already moving.
//
// This is the state an operator is left in by a unifig older than #36: a policy
// created allowing with the request false, so allowing traffic with no companion
// while the config says exactly what the companion-carrying one says. Every
// modelled field agrees, and issue #40's complaint was precisely that the plan is
// clean here. It is not clean any more, and applying it converges the two.
func TestPlanSaysTheReturnRuleIsMissingWhenNothingElseDiffers(t *testing.T) {
	r := startReplay(t)
	r.seedPolicy(t, "Open No Companion", "ALLOW", "Internal", "Dmz", map[string]any{
		"create_allow_respond": false,
	})

	res := planFirewall(t, r, `firewall-policies:
  - name: Open No Companion
    action: allow
    source: Internal
    destination: Dmz
`)
	if res.ExitCode != exitChangesPending {
		t.Fatalf("plan exited %d, want %d — a policy allowing without its companion is a change:\n%s",
			res.ExitCode, exitChangesPending, res.Stdout)
	}
	stdout := string(res.Stdout)
	for _, fragment := range []string{`~ firewall-policy "Open No Companion"`, "return-rule", `"Open No Companion (Return)"`} {
		if !strings.Contains(stdout, fragment) {
			t.Errorf("the plan does not say the return rule is missing (%q missing):\n%s", fragment, stdout)
		}
	}
	if strings.Contains(stdout, "action:") {
		t.Errorf("the plan reports a verdict change where only the return rule differs:\n%s", stdout)
	}
}

// The whole of issue #40, stated as the thing an operator would notice: one
// config file, two histories, one firewall.
//
// A policy created allowing and a policy created blocking and then allowed used
// to end up different — one with a companion, one without — and nothing in the
// config or the plan said so. Both are applied here from the same text, and both
// finish holding the companion.
func TestTheSameConfigGivesTheSameFirewallWhateverThePolicysHistory(t *testing.T) {
	const body = `firewall-policies:
  - name: Let them answer
    action: allow
    source: Internal
    destination: Dmz
`
	companion := "Let them answer (Return)"

	t.Run("created allowing", func(t *testing.T) {
		r := startReplay(t)
		applyFirewall(t, r, body)
		if !r.hasPolicyNamed(t, companion) {
			t.Errorf("a policy created allowing has no %q", companion)
		}
	})

	t.Run("created blocking, then allowed", func(t *testing.T) {
		r := startReplay(t)
		r.seedPolicy(t, "Let them answer", "BLOCK", "Internal", "Dmz", map[string]any{
			"create_allow_respond": false,
		})
		applyFirewall(t, r, body)
		if !r.hasPolicyNamed(t, companion) {
			t.Errorf("a policy created blocking and later allowed has no %q, so the firewall still depends on its history",
				companion)
		}
	})
}

// And the plan is empty afterwards, which is what says the two paths really did
// converge rather than the apply having something left to do.
func TestApplyingAnAllowedPolicyTwiceIsAnEmptySecondPlan(t *testing.T) {
	r := startReplay(t)
	r.seedPolicy(t, "Was Shut", "BLOCK", "Internal", "External", map[string]any{
		"create_allow_respond": false,
	})

	body := `firewall-policies:
  - name: Was Shut
    action: allow
    source: Internal
    destination: External
`
	applyFirewall(t, r, body)
	res := planFirewall(t, r, body)
	if res.ExitCode != exitNoChanges {
		t.Errorf("the second plan is not empty, so the apply did not converge:\n%s", res.Stdout)
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

// The second half of that exemption, and a different claim from the first. A
// deletion needs an id to send exactly as an update does, so a policy the
// Controller computes for a pair of zones is one prune has nothing to address —
// and a plan is a statement about what will happen (ADR-0014, ADR-0028).
//
// **The seed is the only arrangement in which this can be told apart from the
// marker**, which is the whole of why seedUnmarkedGeneratedPolicy exists and
// where the measurement behind it is written down. On real hardware the clause is
// inert and the test above already covers those eighty-six; what is seeded here
// is the disagreement, and it is a firmware nobody has met.
//
// It matters because the file is about to stop covering that case. An exported
// file names all eighty-six, so `named[key]` spares them whatever the markers say
// — and export will leave a Generated Policy out (ADR-0028, issue #45, still open
// as this is written). A file unifig wrote must not be a file prune deletes from,
// so the clause lands before the export that needs it.
//
// Two things fail here if the clause goes: the plan proposes the deletion, and
// the stand-in refuses the DELETE the way the Controller does — 404 on an id that
// was never a handle.
func TestPruneSparesAPolicyItHasNoIdToDelete(t *testing.T) {
	r := startReplay(t)
	r.seedUnmarkedGeneratedPolicy(t, "Computed", "ALLOW", "Dmz", "Dmz", 2147483647)
	// A policy prune really does delete, so that the one under test is spared on
	// purpose rather than because the prune never ran.
	r.seedPolicy(t, "Mine", "ALLOW", "Internal", "External", nil)

	res := applyFirewall(t, r, "firewall-policies: []\n", "--prune")

	stdout := string(res.Stdout)
	if !strings.Contains(stdout, `- firewall-policy "Mine" deleted`) {
		t.Fatalf("the prune under test did not happen:\n%s", stdout)
	}
	if strings.Contains(stdout, `firewall-policy "Computed"`) {
		t.Errorf("prune proposed deleting a policy the Controller has no id to delete:\n%s", stdout)
	}
	r.policyNamed(t, "Computed") // fails the test if the apply deleted it anyway
}

// The third clause, and a third claim: a Return Rule is not a Resource at all.
// unifig never creates, names or deletes one — the config states its arrival and
// departure as a field of its parent's change (ADR-0026) — so prune has no more
// business deleting one than export has writing one.
//
// **This is the arrangement the exported file used to cover.** Export wrote the
// companion until issue #45 — `Allow Return Traffic` twelve times over on a
// migrated router, and `"<name> (Return)"` beside any allow policy of the
// operator's own — so `named[key]` spared it whatever its markers said. That
// backstop is gone, and the file below is the one export now writes: the parent,
// and nothing about the companion. A file unifig wrote must not be a file prune
// deletes from (ADR-0028).
//
// **The seed is the only arrangement that can tell this clause from the two
// beside it**, which is why seedUnmarkedReturnRule exists and where the
// measurement behind it is written down. Every companion anyone has read carries
// a composite id — the twelve a migrated router ships carry the `predefined`
// marker too — so `generated` alone would spare every one of them before this
// clause was reached; the one seeded here is unmarked with a document handle, and
// it is a firmware nobody has met.
//
// What the deletion would do is worse than the 404 a Generated Policy answers.
// The handle is real, so the DELETE lands — and the parent still carries
// `create_allow_respond: true`, so the next plan proposes putting the companion
// back as a `return-rule` field change (ADR-0026). Prune deletes it, apply
// restores it, and neither plan says the two are about the same object.
func TestPruneSparesTheReturnRuleExportStoppedWriting(t *testing.T) {
	r := startReplay(t)
	r.seedPolicy(t, "Was Open", "ALLOW", "Internal", "Dmz", nil)
	r.seedUnmarkedReturnRule(t, "Was Open (Return)", "Dmz", "Internal")
	// A policy prune really does delete, so that the companion is spared on
	// purpose rather than because the prune never ran.
	r.seedPolicy(t, "Mine", "ALLOW", "Internal", "Dmz", nil)

	res := applyFirewall(t, r, `firewall-policies:
  - name: Was Open
    action: allow
    source: Internal
    destination: Dmz
`, "--prune")

	stdout := string(res.Stdout)
	if !strings.Contains(stdout, `- firewall-policy "Mine" deleted`) {
		t.Fatalf("the prune under test did not happen:\n%s", stdout)
	}
	if strings.Contains(stdout, `firewall-policy "Was Open (Return)"`) {
		t.Errorf("prune proposed deleting the companion the file states as its parent's verdict:\n%s", stdout)
	}
	r.policyNamed(t, "Was Open (Return)") // fails the test if the apply deleted it anyway
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
// the operator **stored** alike in all three, and that is still refused rather
// than guessed at: both can be written to, so unifig would be picking which one
// the file meant.
//
// The precedence in ADR-0029 does not reach this clash and the count does not
// move for it. What the message gains is the half issue #46 found missing — it
// says which end of a clash the operator can act on. Here that is both ends, and
// it is the sentence's other clash that needs it: a policy the Controller
// generates has no id, so telling an operator to go and remove one is telling
// them to do something the UI cannot do.
func TestPlanRefusesToGuessBetweenTwoStoredPoliciesSharingANameAndBothEnds(t *testing.T) {
	r := startReplay(t)
	r.seedStoredPolicy(t, "Twice Over", "ALLOW", "Internal", "External")
	r.seedStoredPolicy(t, "Twice Over", "BLOCK", "Internal", "External")

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
	stderr := string(res.Stderr)
	for _, fragment := range []string{
		// The pair is named, because the name alone does not identify which
		// policies the operator has to go and look at. Both are theirs, and the
		// count is of the clash rather than of the key: nothing here is shadowed.
		`2 of your own matching "Twice Over" (Internal to External)`,
		"rename or remove the extras of your own in the Controller's UI",
		// And the half that is new, which is what the operator needs on the
		// clash this one is not.
		"it can be neither renamed nor deleted",
		"takes precedence over it rather than clashing with it",
	} {
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

	exported := exportFirewall(t, r)
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

// exportFirewall runs an export against the stand-in and fails the test if it
// did not finish, handing back both streams. Which one a test reads is the point
// of the test: what export does about a Generated Policy is split across them —
// the file says nothing and stderr says why — so some of these assert on the
// file, some on the notice, and the first on both.
func exportFirewall(t *testing.T, r *replay) result {
	t.Helper()
	exported := testRig.runUnifig(t, []string{"export"}, r.env())
	if exported.ExitCode != 0 {
		t.Fatalf("unifig export exited %d\nstderr: %s", exported.ExitCode, exported.Stderr)
	}
	return exported
}

// The file a migrated router adopts as, which is the whole of ADR-0028: every
// one of its policies is one the Controller computes rather than stores, so the
// section is not short — it is not there.
//
// The count is asked of the recording rather than written down as 86, for the
// reason gatewayZone is asked rather than named: a refreshed recording is not a
// reason for this test to fail. What it asserts is the two halves agreeing —
// nothing survived, and the notice accounts for everything that did not.
func TestExportLeavesOutThePoliciesTheControllerGeneratesAndSaysHowMany(t *testing.T) {
	r := startReplay(t)
	live := r.livePolicies(t)

	exported := exportFirewall(t, r)

	// The key goes rather than going empty. Nil and empty are two different
	// statements in this file — an absent section is unmanaged, an empty one
	// says there should be none and prune acts on it (ADR-0006) — so `[]` here
	// would be unifig asserting a claim the operator never made.
	if stdout := string(exported.Stdout); strings.Contains(stdout, "firewall-policies") {
		t.Errorf("export wrote a firewall-policies key on a site whose every policy is the Controller's own:\n%s", stdout)
	}
	if policies := exportedYAML(t, exported.Stdout).FirewallPolicies; policies != nil {
		t.Errorf("the exported config carries %d firewall policies, want none at all", len(policies))
	}

	stderr := string(exported.Stderr)
	for _, fragment := range []string{
		fmt.Sprintf("Left out %d firewall policies the Controller generates rather than stores", len(live)),
		// The reason, which no other notice carries: this is the one whose
		// subject an operator can act on.
		"no id to write to",
		"`--prune` will not delete them",
		// And the way out, rename and all (issue #43).
		"a policy of your own on the same pair under a name of your own",
		"lowest precedence",
	} {
		if !strings.Contains(stderr, fragment) {
			t.Errorf("export should say why the policies went and what to do about it, looking for %q:\n%s", fragment, stderr)
		}
	}
}

// The notice goes to stderr and nowhere else, because `unifig export >
// unifig.yaml` has to leave stdout carrying nothing but YAML.
func TestTheGeneratedPolicyNoticeStaysOffStdout(t *testing.T) {
	r := startReplay(t)

	exported := exportFirewall(t, r)

	if stdout := string(exported.Stdout); strings.Contains(stdout, "Left out") {
		t.Errorf("the notice reached stdout, so the file is not YAML:\n%s", stdout)
	}
}

// A stored policy of the operator's own is written exactly as it always was.
// The exclusion is about what the Controller generates, and a file that lost the
// operator's own policies along with them would be a worse adoption path than
// the one ADR-0028 replaced.
func TestExportStillWritesAPolicyTheControllerReallyStores(t *testing.T) {
	r := startReplay(t)
	r.seedPolicy(t, "Mine", "BLOCK", "Internal", "External", nil)

	cfg := exportedYAML(t, exportFirewall(t, r).Stdout)

	if len(cfg.FirewallPolicies) != 1 {
		t.Fatalf("export wrote %+v, want the operator's own policy and nothing else", cfg.FirewallPolicies)
	}
	if want := (exportedFirewallPolicy{
		Name: "Mine", Action: "block", Source: "Internal", Destination: "External",
	}); !cfg.FirewallPolicies[0].same(want) {
		t.Errorf("the exported policy is %+v, want %+v", cfg.FirewallPolicies[0], want)
	}
}

// The second exclusion, and its own reason: a Return Rule is not a Resource.
// unifig never creates, names or deletes one — the config states its arrival and
// departure as a field of its parent's change (ADR-0026) — so an entry of its
// own would be a second, competing statement about the same object.
//
// It is a live trap rather than noise. Flip the parent to `block`, the Controller
// reclaims the companion, and the next plan proposes creating `X (Return)` —
// which would generate `X (Return) (Return)`.
func TestExportWritesAPolicyAndNotTheReturnRuleBesideIt(t *testing.T) {
	r := startReplay(t)
	const body = `firewall-policies:
  - name: Let them answer
    action: allow
    source: Internal
    destination: Dmz
`
	// Every policy the recording holds is one the Controller generates, so this
	// is what the notice should still say after unifig has added a policy of the
	// operator's own and the Controller has answered with a companion.
	recorded := len(r.livePolicies(t))

	// Applied rather than seeded, so the companion is the one the stand-in
	// generates on the terms hardware was measured on (ADR-0026) rather than one
	// this test drew itself.
	applyFirewall(t, r, body)
	const companion = "Let them answer (Return)"
	if !r.hasPolicyNamed(t, companion) {
		t.Fatalf("the Controller generated no %q, so there is nothing here to leave out", companion)
	}

	exported := exportFirewall(t, r)
	cfg := exportedYAML(t, exported.Stdout)

	var parent, written bool
	for _, policy := range cfg.FirewallPolicies {
		switch policy.Name {
		case "Let them answer":
			parent = true
		case companion:
			written = true
		}
	}
	if !parent {
		t.Errorf("export left out the policy the operator wrote:\n%s", exported.Stdout)
	}
	if written {
		t.Errorf("export wrote %q, which unifig can neither create nor delete:\n%s", companion, exported.Stdout)
	}

	// And the file it wrote is a file that plans clean, which is what says the
	// omission is not a difference the next run would try to close.
	if res := planExportedConfig(t, r, exported.Stdout); res.ExitCode != exitNoChanges {
		t.Fatalf("plan of a freshly exported config exited %d, want %d\nexported:\n%s\nplan:\n%s",
			res.ExitCode, exitNoChanges, exported.Stdout, res.Stdout)
	}

	// The companion is not in the count, and this is the case that says so: it
	// carries the composite id of a generated policy, exactly as the one ADR-0026
	// read off the router did, so a count asked of the id alone would take the
	// recording's total up by one here. What keeps it out is the parent sitting in
	// the file above it — the file accounts for this policy, which is the opposite
	// of the notice's subject.
	if want := fmt.Sprintf("Left out %d firewall policies the Controller generates", recorded); !strings.Contains(string(exported.Stderr), want) {
		t.Errorf("the notice counts a companion whose parent is in the file, looking for %q:\n%s", want, exported.Stderr)
	}
}

// The exclusion rides `connection_state_type` rather than the id shape, and this
// is the arrangement in which the two can be told apart: a companion carrying a
// document handle, which only the first test excludes.
//
// Every companion anyone has read carries the composite id of a generated policy
// instead — the twelve a migrated router ships, and the one ADR-0026 watched the
// Controller make from a custom parent — so this is a shape hardware has not
// shown and the fixture says so rather than implying otherwise. It is here
// because what disqualifies a companion is that it is not a Resource, which is
// true whichever way its id falls, and a test resting on the id would be
// asserting the wrong reason.
//
// It is not in the count either, and here that holds twice over: its parent is
// in the file, and it has an id, so "the Controller generates rather than
// stores" would not be a true sentence about it.
func TestExportLeavesOutAReturnRuleThatCarriesADocumentHandle(t *testing.T) {
	r := startReplay(t)
	const companion = "Was Open (Return)"
	recorded := len(r.livePolicies(t))
	r.seedPolicy(t, "Was Open", "ALLOW", "Internal", "External", nil)
	r.seedPolicy(t, companion, "ALLOW", "External", "Internal", map[string]any{
		"predefined":            true,
		"connection_state_type": "RESPOND_ONLY",
	})

	exported := exportFirewall(t, r)
	cfg := exportedYAML(t, exported.Stdout)

	for _, policy := range cfg.FirewallPolicies {
		if policy.Name == companion {
			t.Errorf("export wrote the return rule of a policy it also wrote:\n%s", exported.Stdout)
		}
	}
	if want := fmt.Sprintf("Left out %d firewall policies the Controller generates", recorded); !strings.Contains(string(exported.Stderr), want) {
		t.Errorf("the notice should count only the Controller's own, looking for %q:\n%s", want, exported.Stderr)
	}
}

// And the twelve a migrated router ships are counted, which is the other side of
// the same rule rather than an exception to it. They are companions — every one
// is `RESPOND_ONLY` — but the policies they answer for are the Controller's own
// and went out of the file with the rest, so nothing in the file accounts for
// them and the notice has to.
//
// It is the assertion that keeps the recording's count at eighty-six rather than
// seventy-four, stated on its own so that a change to the counting rule cannot
// quietly take twelve policies out of the number an operator reads.
func TestTheReturnRulesAMigratedRouterShipsAreCounted(t *testing.T) {
	r := startReplay(t)
	var companions int
	for _, policy := range r.livePolicies(t) {
		if policy["connection_state_type"] == "RESPOND_ONLY" {
			companions++
		}
	}
	if companions == 0 {
		t.Fatal("the recording holds no return rules, so there is nothing here to count")
	}

	stderr := string(exportFirewall(t, r).Stderr)

	if want := fmt.Sprintf("Left out %d firewall policies the Controller generates", len(r.livePolicies(t))); !strings.Contains(stderr, want) {
		t.Errorf("the notice leaves the %d shipped return rules out of the count, looking for %q:\n%s",
			companions, want, stderr)
	}
}

// The two notices meet on one policy each, on the same unnameable end, so that
// the only thing separating them is which of them unifig put the policy in.
//
// A Generated Policy is left out for having no id to write to, whether or not
// its zones have names — so counting it is the true statement and calling it
// indescribable would blame the wrong thing.
func TestAGeneratedPolicyOnAnUnnameableZoneIsCountedRatherThanCalledIndescribable(t *testing.T) {
	r := startReplay(t)
	internal, _ := r.zoneNamed(t, "Internal")["_id"].(string)
	live := len(r.livePolicies(t))

	r.seedPolicyOnAZoneItCannotName(t, "Stored And Unwordable", nil)
	r.seedPolicyOnAZoneItCannotName(t, "Generated And Unwordable", map[string]any{
		"_id":        compositePolicyID(internal, unnameableZone, 3),
		"index":      3,
		"predefined": true,
	})

	stderr := string(exportFirewall(t, r).Stderr)

	if !strings.Contains(stderr, `"Stored And Unwordable"`) {
		t.Errorf("export should name the stored policy it could not word:\n%s", stderr)
	}
	if strings.Contains(stderr, `"Generated And Unwordable"`) {
		t.Errorf("export blamed the zones for a policy it could not have written to anyway:\n%s", stderr)
	}
	// The recording's own plus the generated one this test added; the stored one
	// is the only policy on the site the count leaves out.
	if want := fmt.Sprintf("Left out %d firewall policies the Controller generates", live+1); !strings.Contains(stderr, want) {
		t.Errorf("the notice should count the generated policy on the unnameable zone, looking for %q:\n%s", want, stderr)
	}
}

// A zone holding something the config cannot name is written with the part it
// can — and export says so, because a file that came back short says why.
func TestExportSaysWhichZonesItCouldOnlyDescribeInPart(t *testing.T) {
	r := startReplay(t)

	exported := exportFirewall(t, r)

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

// A Generated Policy is one the Controller computes for a pair of zones rather
// than storing, and unifig cannot write to it: its `_id` is the two zone ids and
// the index run together, which the write endpoint answers 404 to (ADR-0027,
// issue #41).
//
// This is the case the whole issue is about, and `unifig.yaml` in this repo is
// the reason it matters: it names nineteen `Allow All Traffic` policies and every
// one of them is generated. Changing the verdict of one — the single edit the
// config models — used to plan cleanly and then fail on apply. A plan is a
// statement about what will happen (ADR-0014), so the change is not planned at
// all, and the operator is told why rather than told nothing.
func TestPlanWillNotPromiseAChangeToAPolicyTheControllerGenerates(t *testing.T) {
	r := startReplay(t)
	r.seedGeneratedPolicy(t, "Allow All Traffic", "ALLOW", "Dmz", "Dmz", 2147483647)

	res := planFirewall(t, r, `firewall-policies:
  - name: Allow All Traffic
    action: block
    source: Dmz
    destination: Dmz
`)

	if res.ExitCode != exitNoChanges {
		t.Fatalf("plan exited %d, want %d — there is nothing unifig can do here\nstdout: %s",
			res.ExitCode, exitNoChanges, res.Stdout)
	}
	stdout := string(res.Stdout)
	if strings.Contains(stdout, `~ firewall-policy "Allow All Traffic"`) {
		t.Errorf("the plan promised a change to a policy the Controller generates:\n%s", stdout)
	}
	// Named by its whole key, because a migrated router ships nineteen of that
	// name and a sentence about one has to say which (ADR-0001).
	//
	// The way out names the rename, and says what the operator's policy wins
	// against. Without the first half the advice loops back into this same caveat
	// — an entry under this policy's name on this policy's pair has this policy's
	// key — and without the second an operator has no reason to believe a policy
	// of their own beats the Controller's (issue #43).
	for _, fragment := range []string{
		`"Allow All Traffic" (Dmz to Dmz)`,
		"will not be changed",
		"no endpoint can edit it",
		"the lowest precedence there is",
		"under a name of your own, takes precedence over it",
	} {
		if !strings.Contains(stdout, fragment) {
			t.Errorf("the plan does not say why the change cannot be made (%q missing):\n%s", fragment, stdout)
		}
	}
}

// The caveat is said about a change, not about a policy. Every one of the
// nineteen `Allow All Traffic` policies a migrated router ships is generated, so
// a caveat per generated policy would put nineteen lines under every firewall
// plan and an operator would read past all of them by the third run — the same
// argument unreadableGateway is gated on.
func TestAPolicyTheControllerGeneratesAndTheFileAgreesWithIsQuiet(t *testing.T) {
	r := startReplay(t)
	r.seedGeneratedPolicy(t, "Allow All Traffic", "ALLOW", "Dmz", "Dmz", 2147483647)

	res := planFirewall(t, r, `firewall-policies:
  - name: Allow All Traffic
    action: allow
    source: Dmz
    destination: Dmz
`)

	if res.ExitCode != exitNoChanges {
		t.Fatalf("plan exited %d, want %d\nstdout: %s", res.ExitCode, exitNoChanges, res.Stdout)
	}
	if strings.Contains(string(res.Stdout), "will not be changed") {
		t.Errorf("a policy the file agrees with was reported as one unifig could not change:\n%s", res.Stdout)
	}
}

// The plan's promise is only worth what apply does, so this is the same sentence
// checked at the seam where it used to break: nothing is written at all.
//
// Without the hold-back the stand-in fails this loudly of its own accord — a PUT
// to a composite id is a request the Controller answers 404 to, and it says so
// (see unresolvable). That is the guard, and this is the statement.
func TestApplyWritesNothingForAPolicyTheControllerGenerates(t *testing.T) {
	r := startReplay(t)
	r.seedGeneratedPolicy(t, "Allow All Traffic", "ALLOW", "Dmz", "Dmz", 2147483647)

	res := applyFirewall(t, r, `firewall-policies:
  - name: Allow All Traffic
    action: block
    source: Dmz
    destination: Dmz
`, "--auto-approve")

	if res.ExitCode != 0 {
		t.Fatalf("apply exited %d, want 0\nstderr: %s", res.ExitCode, res.Stderr)
	}
	if writes := r.policyWrites(t); len(writes) != 0 {
		t.Errorf("apply made %d write(s) to a policy the Controller generates, want none: %v", len(writes), writes)
	}
	// Found by its whole key rather than by its name: the recording ships
	// nineteen more of that name, which is the whole reason a policy's key is not
	// its name.
	for _, policy := range r.policiesOnKey(t, "Allow All Traffic", "Dmz", "Dmz") {
		if policy["action"] != "ALLOW" {
			t.Errorf("the Controller's own policy is %v, want it left exactly as it was", policy["action"])
		}
	}
}

// Issue #41's last box, and the one that reaches back into ADR-0018.
//
// The Risky mark exists for the change that can lock an operator out, and the
// example the ADR led with was the Controller's own `Allow All Traffic` from
// Internal to Gateway turned to block: "a predefined policy is matchable and
// updatable like any other". The second half of that is false. Marking the
// change would stop an operator to confirm something that then cannot happen,
// which is worse than not marking it — a confirmation for a non-event is how a
// prompt stops being read (ADR-0012).
//
// So the mark goes with the change. What does not go is the warning: the caveat
// tells the operator to write their own policy on the pair instead, and on this
// pair that is precisely the create ADR-0018 marks Risky. Advice to go and do the
// dangerous thing, with the danger left out, would be a worse outcome than the
// mark that was removed — so the words travel from one to the other.
func TestAGeneratedPolicyBlockingTheGatewayWarnsWithoutMarkingAChangeRisky(t *testing.T) {
	r := startReplay(t)
	gateway := r.gatewayZone(t)
	r.seedGeneratedPolicy(t, "Allow All Traffic", "ALLOW", "Dmz", gateway, 2147483647)

	res := planFirewall(t, r, fmt.Sprintf(`firewall-policies:
  - name: Allow All Traffic
    action: block
    source: Dmz
    destination: %s
`, gateway))

	// No change at all, so nothing for apply to stop and ask about.
	if res.ExitCode != exitNoChanges {
		t.Fatalf("plan exited %d, want %d\nstdout: %s", res.ExitCode, exitNoChanges, res.Stdout)
	}
	stdout := string(res.Stdout)
	if strings.Contains(stdout, `~ firewall-policy "Allow All Traffic"`) {
		t.Errorf("a change that cannot be applied was planned:\n%s", stdout)
	}
	// The warning survives the mark, in the sentence that suggests the create it
	// is about.
	for _, fragment := range []string{
		"will not be changed",
		"under a name of your own, takes precedence over it",
		riskOfBlockingTheGateway,
	} {
		if !strings.Contains(stdout, fragment) {
			t.Errorf("the caveat should carry the way out and what it costs (%q missing):\n%s", fragment, stdout)
		}
	}
}

// The same policy on a pair that is not the gateway's gets the way out and no
// warning, because there is nothing there to warn about. This is what says the
// warning is computed from the change rather than pasted onto every caveat —
// ADR-0012's rule that a warning on everything is a warning read past.
func TestTheWayOutOfAnUnwritablePolicyCarriesNoWarningWhereThereIsNoDanger(t *testing.T) {
	r := startReplay(t)
	r.seedGeneratedPolicy(t, "Allow All Traffic", "ALLOW", "Dmz", "Dmz", 2147483647)

	res := planFirewall(t, r, `firewall-policies:
  - name: Allow All Traffic
    action: block
    source: Dmz
    destination: Dmz
`)

	stdout := string(res.Stdout)
	if !strings.Contains(stdout, "under a name of your own, takes precedence over it") {
		t.Errorf("the caveat should still say the way out:\n%s", stdout)
	}
	if strings.Contains(stdout, riskOfBlockingTheGateway) {
		t.Errorf("a policy nowhere near the gateway was warned about:\n%s", stdout)
	}
}

// Advice is only worth what following it does, and followed literally this
// advice used to arrive back where it started. Keep the name, keep the pair,
// change the verdict — and the entry has the generated policy's own key, name
// together with pair (ADR-0001), so planFirewallPolicies matches it, finds the
// same difference and says the same caveat again. unifig would never create
// anything, and nothing in the sentence said why (issue #43).
//
// The rename is what makes the way out a way out: a name of the operator's own
// is a key of their own, there is nothing to match, and the plan creates. That
// create is a policy unifig owns and can change afterwards, which is the whole
// of what the caveat promises.
func TestTheWayOutOfAnUnwritablePolicyIsAPlanThatCreates(t *testing.T) {
	r := startReplay(t)
	r.seedGeneratedPolicy(t, "Allow All Traffic", "ALLOW", "Dmz", "Dmz", 2147483647)

	res := planFirewall(t, r, `firewall-policies:
  - name: Block the Dmz
    action: block
    source: Dmz
    destination: Dmz
`)

	stdout := string(res.Stdout)
	if !strings.Contains(stdout, `+ firewall-policy "Block the Dmz"`) {
		t.Errorf("a policy of the operator's own on the pair should be a create:\n%s", stdout)
	}
	// And it is a create rather than a create with a hedge on it: the policy the
	// operator was told to write is not one unifig then declines to write.
	if strings.Contains(stdout, "will not be changed") {
		t.Errorf("the way out was itself caveated:\n%s", stdout)
	}
}

// The other side of that reconciliation, and the reason the mark still earns its
// place: the lockout is still one line of config away, as a create rather than an
// edit. The Controller's own allow on that pair sits at `index: 2147483647`, the
// lowest precedence there is, so a policy written over it takes effect
// (ADR-0018). This is the same rule as
// TestCreatingAPolicyThatBlocksTheGatewayIsRisky, stated where the generated
// policy it has to out-rank is actually present.
func TestCreatingAPolicyOverAGeneratedOneThatBlocksTheGatewayIsStillRisky(t *testing.T) {
	r := startReplay(t)
	gateway := r.gatewayZone(t)
	r.seedGeneratedPolicy(t, "Allow All Traffic", "ALLOW", "Dmz", gateway, 2147483647)

	res := planFirewall(t, r, fmt.Sprintf(`firewall-policies:
  - name: Lock the gateway
    action: block
    source: Dmz
    destination: %s
`, gateway))

	stdout := string(res.Stdout)
	if !strings.Contains(stdout, `+ firewall-policy "Lock the gateway"`) ||
		!strings.Contains(stdout, riskOfBlockingTheGateway) {
		t.Errorf("a created policy blocking the gateway is still the change that can lock an operator out:\n%s", stdout)
	}
}

// A pipeline reads the caveats out of the JSON rather than out of the prose, and
// this is the first caveat about a change an operator explicitly asked for — the
// others are about deletions unifig proposed itself.
func TestPlanJSONCarriesTheCaveatAboutAPolicyItCannotChange(t *testing.T) {
	r := startReplay(t)
	r.seedGeneratedPolicy(t, "Allow All Traffic", "ALLOW", "Dmz", "Dmz", 2147483647)

	res := planFirewall(t, r, `firewall-policies:
  - name: Allow All Traffic
    action: block
    source: Dmz
    destination: Dmz
`, "--json")

	var plan struct {
		Changes []struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"changes"`
		Caveats []struct {
			Kind   string `json:"kind"`
			Reason string `json:"reason"`
		} `json:"caveats"`
	}
	if err := json.Unmarshal(res.Stdout, &plan); err != nil {
		t.Fatalf("decoding the plan JSON: %v\n%s", err, res.Stdout)
	}
	if len(plan.Changes) != 0 {
		t.Errorf("plan --json carries %d change(s) for a policy that cannot be written, want none", len(plan.Changes))
	}
	if len(plan.Caveats) != 1 {
		t.Fatalf("plan --json carries %d caveat(s), want exactly 1: %+v", len(plan.Caveats), plan.Caveats)
	}
	if plan.Caveats[0].Kind != "firewall-policy" {
		t.Errorf("the caveat is about kind %q, want firewall-policy", plan.Caveats[0].Kind)
	}
	if !strings.Contains(plan.Caveats[0].Reason, `"Allow All Traffic" (Dmz to Dmz)`) {
		t.Errorf("the caveat does not name the policy it is about: %q", plan.Caveats[0].Reason)
	}
}

// An empty plan that is quiet because there was nothing to do and an empty plan
// that is quiet because unifig could not do it are different states, and the
// headline used to claim the first for both. Here the Controller emphatically
// does not match the config — the file says `block` and the site says `allow` —
// and the only reason nothing is planned is that the policy cannot be addressed.
func TestAnEmptyPlanWithACaveatDoesNotClaimTheControllerMatches(t *testing.T) {
	r := startReplay(t)
	r.seedGeneratedPolicy(t, "Allow All Traffic", "ALLOW", "Dmz", "Dmz", 2147483647)

	res := planFirewall(t, r, `firewall-policies:
  - name: Allow All Traffic
    action: block
    source: Dmz
    destination: Dmz
`)

	stdout := string(res.Stdout)
	if !strings.Contains(stdout, "No changes") {
		t.Errorf("the plan should still say there is nothing it will do:\n%s", stdout)
	}
	if strings.Contains(stdout, "already matches the config") {
		t.Errorf("the plan claims the Controller matches a config it disagrees with:\n%s", stdout)
	}
}

// The other side of it: a plan with nothing to say keeps the sentence, because
// there the claim is true and it is the whole of what an operator wants to read.
func TestAnEmptyPlanWithNothingToSayStillSaysTheControllerMatches(t *testing.T) {
	r := startReplay(t)

	res := planFirewall(t, r, `firewall-policies:
  - name: Allow All Traffic
    action: allow
    source: Internal
    destination: External
`)

	if !strings.Contains(string(res.Stdout), "already matches the config") {
		t.Errorf("a plan with nothing to say should say so plainly:\n%s", res.Stdout)
	}
}

// The clash issue #46 went and measured: a policy of the operator's own sharing
// all three parts of its key with one the Controller generates for the pair.
//
// unifig used to refuse the whole site for it — export outright, and plan the
// moment the file had a `firewall-policies:` section at all — which is a refusal
// over a state ADR-0027's own way out invites an operator to create. The
// Controller has answered the question the refusal says has no answer: it took
// the create at `201`, kept both objects, and put the stored one at `index:
// 10000` against the generated one's `2147483647` (ADR-0029).
//
// So the match resolves to the stored policy, and it resolves to the one thing
// on the pair unifig could write to in the first place.
func TestAStoredPolicyTakesPrecedenceOverTheGeneratedOneSharingItsKey(t *testing.T) {
	r := startReplay(t)
	r.seedGeneratedPolicy(t, "Allow All Traffic", "ALLOW", "Dmz", "Dmz", 2147483647)
	r.seedStoredPolicy(t, "Allow All Traffic", "ALLOW", "Dmz", "Dmz")

	res := planFirewall(t, r, `firewall-policies:
  - name: Allow All Traffic
    action: block
    source: Dmz
    destination: Dmz
`)

	if res.ExitCode != exitChangesPending {
		t.Fatalf("plan exited %d, want %d — the site was refused over a clash the Controller has resolved\nstderr: %s",
			res.ExitCode, exitChangesPending, res.Stderr)
	}
	stdout := string(res.Stdout)
	if !strings.Contains(stdout, `~ firewall-policy "Allow All Traffic"`) {
		t.Errorf("the plan does not change the policy the operator can change:\n%s", stdout)
	}
	// The generated one is shadowed rather than matched, so nothing here is the
	// caveat about a policy with no id to write to.
	if strings.Contains(stdout, "will not be changed") {
		t.Errorf("the plan matched the generated policy over the operator's own:\n%s", stdout)
	}
}

// The plan's promise is only worth what apply does, and this is the seam the
// precedence has to survive: the write goes to the document handle of the stored
// policy, and the generated one is left exactly as it was.
//
// The stand-in is the guard on the first half rather than an assertion here — a
// PUT to a composite id is answered 404 and fails the test out loud (see
// unresolvable) — so what this states is the second: unifig changed the
// operator's policy and nothing else on the pair.
func TestApplyWritesToTheStoredPolicyRatherThanTheGeneratedOneSharingItsKey(t *testing.T) {
	r := startReplay(t)
	r.seedGeneratedPolicy(t, "Allow All Traffic", "ALLOW", "Dmz", "Dmz", 2147483647)
	r.seedStoredPolicy(t, "Allow All Traffic", "ALLOW", "Dmz", "Dmz")

	res := applyFirewall(t, r, `firewall-policies:
  - name: Allow All Traffic
    action: block
    source: Dmz
    destination: Dmz
`, "--auto-approve")

	if res.ExitCode != 0 {
		t.Fatalf("apply exited %d, want 0\nstderr: %s", res.ExitCode, res.Stderr)
	}

	// Both objects are still there — the Controller keeps them side by side, and
	// an apply that reduced two policies to one would be unifig having deleted
	// something nobody asked it to.
	verdicts := map[string]string{}
	for _, policy := range r.policiesOnKey(t, "Allow All Traffic", "Dmz", "Dmz") {
		id, _ := policy["_id"].(string)
		action, _ := policy["action"].(string)
		verdicts[id] = action
	}
	dmz, _ := r.zoneNamed(t, "Dmz")["_id"].(string)
	generated := compositePolicyID(dmz, dmz, 2147483647)
	if verdicts[generated] != "ALLOW" {
		t.Errorf("the Controller's own policy is %q, want it left exactly as it was", verdicts[generated])
	}
	stored := 0
	for id, action := range verdicts {
		if !isDocumentHandle(id) {
			continue
		}
		stored++
		if action != "BLOCK" {
			t.Errorf("the operator's own policy is %q, want the verdict the config states", action)
		}
	}
	if stored != 1 {
		t.Errorf("the site holds %d stored policies on the pair, want the one the operator created", stored)
	}
}

// The other half of #46's injury, and the louder one: `unifig export` exited 1
// with nothing on stdout, so a site holding this clash could not be adopted at
// all.
//
// What export writes is unchanged by the precedence — the stored policy goes in
// because a plan can act on it, the generated one stays out because no plan ever
// could (ADR-0028) — and the file it hands over now plans clean, which is the
// whole of the site no longer being refused.
func TestExportWritesTheStoredPolicyAndStillLeavesOutTheGeneratedOneItShadows(t *testing.T) {
	r := startReplay(t)
	r.seedGeneratedPolicy(t, "Allow All Traffic", "ALLOW", "Dmz", "Dmz", 2147483647)
	r.seedStoredPolicy(t, "Allow All Traffic", "ALLOW", "Dmz", "Dmz")
	live := r.livePolicies(t)

	exported := exportFirewall(t, r)

	cfg := exportedYAML(t, exported.Stdout)
	if len(cfg.FirewallPolicies) != 1 {
		t.Fatalf("export wrote %+v, want the operator's own policy and nothing else", cfg.FirewallPolicies)
	}
	if want := (exportedFirewallPolicy{
		Name: "Allow All Traffic", Action: "allow", Source: "Dmz", Destination: "Dmz",
	}); !cfg.FirewallPolicies[0].same(want) {
		t.Errorf("the exported policy is %+v, want %+v", cfg.FirewallPolicies[0], want)
	}
	// The shadowed policy is left out and counted like every other one the
	// Controller generates. Being shadowed is not being described: nothing in
	// the file is a statement about it, and the entry beside it is a statement
	// about the operator's own policy that happens to share its key.
	if want := fmt.Sprintf("Left out %d firewall policies the Controller generates rather than stores", len(live)-1); !strings.Contains(string(exported.Stderr), want) {
		t.Errorf("the notice should still speak for the shadowed policy, looking for %q:\n%s", want, exported.Stderr)
	}

	if res := planExportedConfig(t, r, exported.Stdout); res.ExitCode != exitNoChanges {
		t.Fatalf("plan of a freshly exported config exited %d, want %d\nexported:\n%s\nplan:\n%s",
			res.ExitCode, exitNoChanges, exported.Stdout, res.Stdout)
	}
}

// Prune's exemptions are untouched by the precedence, and they have to be said
// on the pair where the two policies sit together: matching resolves to the
// stored one, and that is not a statement about which of them prune may delete.
//
// The stored policy is the operator's own and absent from the file, so it goes.
// The generated one is spared twice over — on its marker (ADR-0005) and on
// having no id to send a deletion to (ADR-0028) — and sharing a key with a
// policy prune just deleted changes neither.
func TestPruneSparesTheGeneratedPolicyWhoseKeyTheStoredOneTook(t *testing.T) {
	r := startReplay(t)
	r.seedGeneratedPolicy(t, "Allow All Traffic", "ALLOW", "Dmz", "Dmz", 2147483647)
	r.seedStoredPolicy(t, "Allow All Traffic", "ALLOW", "Dmz", "Dmz")

	before := len(r.livePolicies(t))

	res := applyFirewall(t, r, "firewall-policies: []\n", "--prune")

	if !strings.Contains(string(res.Stdout), `- firewall-policy "Allow All Traffic" deleted`) {
		t.Fatalf("the prune under test did not happen:\n%s", res.Stdout)
	}

	// Which of the two went is asked of the key rather than of the name, because
	// nineteen policies on this site are called `Allow All Traffic` and a line
	// saying one of them was deleted does not say which. What has to be left
	// standing on the key is the one with no document handle.
	var onTheKey []string
	for _, policy := range r.policiesOnKey(t, "Allow All Traffic", "Dmz", "Dmz") {
		id, _ := policy["_id"].(string)
		onTheKey = append(onTheKey, id)
	}
	if len(onTheKey) != 1 || isDocumentHandle(onTheKey[0]) {
		t.Errorf("the key is left holding %v, want only the policy the Controller generates for the pair", onTheKey)
	}
	// And exactly one policy left this site, so the prune took the operator's own
	// and nothing beside it.
	if after := len(r.livePolicies(t)); after != before-1 {
		t.Errorf("the site went from %d policies to %d, want the one stored policy gone and nothing else",
			before, after)
	}
}

// The boundary of the precedence, said where it can be told: what outranks a
// generated policy is a policy unifig could write to, not "any other policy".
//
// **Two generated policies sharing a key is a firmware nobody has met**, and the
// fixture says so rather than implying otherwise. The eighty-six a migrated
// router ships hold no such pair — 86 policies under 86 distinct keys, which is
// why export against #46's restored baseline is healthy — so this is seeded, and
// seeded to pin a boundary rather than to describe a router. That is
// seedUnmarkedGeneratedPolicy's arrangement rather than a new liberty with
// ADR-0019: without it, "a stored policy outranks a generated one" and
// "generated policies are not counted" look identical, and the second silently
// picks between two objects unifig cannot write to.
//
// **Refusing it is the behaviour that was already there**, kept rather than
// chosen: nothing has answered this clash either. There is no stored policy to
// resolve to and no way to choose between two ends with no id, which is the
// guess `policiesByKey` refuses on the operator's behalf.
//
// So the way out has to be in the message, and it is a real one: writing a
// policy of their own on that name and pair takes precedence over both.
func TestTwoGeneratedPoliciesSharingAKeyAreStillRefused(t *testing.T) {
	r := startReplay(t)
	r.seedGeneratedPolicy(t, "Computed Twice", "ALLOW", "Dmz", "Dmz", 30000)
	r.seedGeneratedPolicy(t, "Computed Twice", "BLOCK", "Dmz", "Dmz", 2147483647)

	res := planFirewall(t, r, `firewall-policies:
  - name: Computed Twice
    action: allow
    source: Dmz
    destination: Dmz
`)

	if res.ExitCode != exitError {
		t.Fatalf("plan exited %d, want %d — it picked one of two policies it can write to neither of\nstdout: %s",
			res.ExitCode, exitError, res.Stdout)
	}
	stderr := string(res.Stderr)
	// Named as the Controller's, because that is what decides what the operator
	// can do about them — and telling them to remove one would be telling them
	// to do something the UI cannot do (ADR-0027).
	if !strings.Contains(stderr, `2 the Controller generates itself matching "Computed Twice" (Dmz to Dmz)`) {
		t.Errorf("the refusal should say whose the clashing policies are, got: %s", stderr)
	}
	// And the way out, which is the only one this clash has.
	if !strings.Contains(stderr, "takes precedence over it rather than clashing with it") {
		t.Errorf("the refusal should carry the create that resolves it, got: %s", stderr)
	}
}

// The recording itself has to carry the shape the Controller answers with, and
// this is the guard that keeps it there.
//
// Every other test in this file can seed what it needs. This one cannot be
// seeded by definition: its subject is the committed fixture, and the defect it
// exists against is a re-recording that quietly flattens every `_id` back into a
// document handle. That is not hypothetical — it is what `make record-udr` did
// until issue #41, and the cost was that the replay stand-in had never been
// handed a composite, so every policy in the suite was addressable and unifig
// could plan an update it could not apply with the suite green throughout.
//
// It asserts the composite is *consistent* rather than merely long, because
// consistency is what makes the recording a firewall: a policy has to point at
// the zones the recording holds, through its `_id` as well as through its ends
// (ADR-0027).
//
// It reads the policies through the stand-in rather than off disk, which is both
// the file's convention and the sharper question: what the recording holds
// matters because it is what the stand-in serves.
func TestTheRecordedPoliciesCarryTheIdShapeTheControllerReturns(t *testing.T) {
	r := startReplay(t)

	recorded := r.livePolicies(t)
	if len(recorded) == 0 {
		t.Fatal("the recording holds no firewall policies, so it asserts nothing about their shape")
	}

	for _, policy := range recorded {
		name, _ := policy["name"].(string)
		id, _ := policy["_id"].(string)
		source, _ := policy["source"].(map[string]any)
		destination, _ := policy["destination"].(map[string]any)
		index, ok := policy["index"].(float64)
		if !ok {
			t.Errorf("the recorded policy %q carries no index, so it cannot carry the id the Controller builds from one", name)
			continue
		}

		want := fmt.Sprintf("%v%v%d", source["zone_id"], destination["zone_id"], int64(index))
		if id != want {
			t.Errorf("the recorded policy %q has _id %q, want its own zone ids and index run together, %q",
				name, id, want)
		}
		// Said separately from the equality above, because this is the sentence
		// that matters to unifig: a policy the Controller generates has no
		// document handle, and one that appeared to have one would be a policy
		// the suite believed was writable.
		if isDocumentHandle(id) {
			t.Errorf("the recorded policy %q has a document handle for an _id (%q), "+
				"so the recording says the Controller stores a policy it generates", name, id)
		}
	}
}

// ---------------------------------------------------------------------------
// Narrowing — the protocol and destination ports that say which packets between
// a pair of zones a policy governs, out of all of them (ADR-0031, issue #51).
//
// The feature exists because of what a custom zone's path to the Controller is
// made of. Read off the live migrated UDR on 25 August 2026, each custom zone
// reaches Gateway through exactly two Generated Policies — `Allow mDNS` at index
// 30000 and `Allow All Traffic` at 2147483647 — and there is no generated
// `Allow DNS` and no `Allow DHCP` for a custom zone, only for Hotspot. So DHCP,
// DNS and NTP all ride that catch-all, and a policy unifig creates lands at
// index 10000, above every one of them. "Keep the IoT VLAN off the admin page"
// written as `block <zone> -> Gateway` does not keep it off the admin page; it
// keeps it off the network. A narrowing is what makes the rule the operator
// meant writable.

// narrowedTo is a seed's fields for a policy the Controller holds narrowed —
// what an operator gets by setting the ports in the UI. The whole destination
// end has to be handed over because seedPolicy builds it, and the zone id it
// puts there is the part a test cannot invent.
func narrowedTo(t *testing.T, r *replay, zone, protocol, port string) map[string]any {
	t.Helper()
	return map[string]any{
		"protocol": protocol,
		"destination": endOn(t, r, zone, map[string]any{
			"port_matching_type": "SPECIFIC",
			"port":               port,
		}),
	}
}

// The create body, which is the whole of what the probe measured: a POST
// carrying `tcp` with `port_matching_type: SPECIFIC` and `port: "443,80"` was
// answered 201 on the live migrated UDR and read back byte-identical on an
// independent GET (ADR-0031).
//
// The wire form is one comma-separated string rather than a list, which is the
// Controller's shape and not a convenience: its own `Allow Hotspot Portal
// Authentication` carries `"8880,8843"` in that one field.
func TestCreatingAPolicyWithPortsNarrowsItOnTheDestination(t *testing.T) {
	r := startReplay(t)

	applyFirewall(t, r, `firewall-policies:
  - name: Ellingson off the admin UI
    action: block
    source: Internal
    destination: Dmz
    protocol: tcp
    ports: [443, 80]
`)

	sent := r.onlyPolicyWrite(t)
	if sent["protocol"] != "tcp" {
		t.Errorf("the create should carry the stated protocol, got %v: %v", sent["protocol"], sent)
	}
	destination, _ := sent["destination"].(map[string]any)
	if destination["port_matching_type"] != "SPECIFIC" {
		t.Errorf("a policy stating ports should send SPECIFIC port matching, got %v: %v",
			destination["port_matching_type"], sent)
	}
	if destination["port"] != "443,80" {
		t.Errorf("the ports should go on the wire as the Controller's one comma-separated string, got %v: %v",
			destination["port"], sent)
	}
}

// A range is the half of the grammar go-unifi's generated pattern claimed and
// nothing had asked the Controller about. A PUT carrying `"8000-8010"` was
// answered 200 and read back unchanged (ADR-0031), which is why the config takes
// one at all rather than following the port forward's single ports.
func TestAPolicyCanStateAPortRange(t *testing.T) {
	r := startReplay(t)

	applyFirewall(t, r, `firewall-policies:
  - name: Shut the high ports
    action: block
    source: Internal
    destination: Dmz
    protocol: tcp
    ports: ["8000-8010", 22]
`)

	destination, _ := r.onlyPolicyWrite(t)["destination"].(map[string]any)
	if destination["port"] != "8000-8010,22" {
		t.Errorf("a range and a port should join into the Controller's one field, got %v", destination["port"])
	}
}

// The default, and the reason a create can print a protocol for a field the
// config left empty: a create has no live policy to preserve, so what it does
// not state it sets — `all` and ANY, which is what every one of unifig's own
// creates sent before a narrowing was modelled.
func TestCreatingAPolicyWithNoNarrowingSendsAllProtocolsAndAnyPorts(t *testing.T) {
	r := startReplay(t)

	applyFirewall(t, r, `firewall-policies:
  - name: Shut it all
    action: block
    source: Internal
    destination: Dmz
`)

	sent := r.onlyPolicyWrite(t)
	if sent["protocol"] != "all" {
		t.Errorf("a create stating no protocol should send `all`, got %v: %v", sent["protocol"], sent)
	}
	destination, _ := sent["destination"].(map[string]any)
	if destination["port_matching_type"] != "ANY" {
		t.Errorf("a create stating no ports should send ANY port matching, got %v: %v",
			destination["port_matching_type"], sent)
	}
	if port, carried := destination["port"]; carried {
		t.Errorf("a create stating no ports should carry no port at all, got %v", port)
	}
}

// ADR-0004 arriving at the narrowing, and the decision this feature turned on.
//
// A file that says nothing about ports has not asked for every port; it has
// asked for nothing, so a policy someone narrowed in the Controller's UI keeps
// its narrowing. The alternative — the config being the whole statement, as
// ADR-0026 made the Return Rule — would have every entry in every existing file
// start claiming "any port", and the next apply would widen what an operator had
// narrowed by hand.
func TestAPolicyNarrowedInTheControllerKeepsItWhenTheFileStatesNoPorts(t *testing.T) {
	r := startReplay(t)
	r.seedPolicy(t, "Allow DNS out", "ALLOW", "Internal", "Dmz", narrowedTo(t, r, "Dmz", "tcp", "53"))

	res := planFirewall(t, r, `firewall-policies:
  - name: Allow DNS out
    action: allow
    source: Internal
    destination: Dmz
`)

	stdout := string(res.Stdout)
	if !strings.Contains(stdout, "No changes") {
		t.Errorf("a file stating no narrowing manages none, so a narrowed policy is no change:\n%s", stdout)
	}
}

// The way back out, and the reason there is no `ports: any` sentinel.
//
// ADR-0004 has no removal syntax because the schema gives no way to *ask* for a
// network with no VLAN. Here it does: `all` has no ports, so stating it is the
// statement that there are none. The Controller does not enforce that — it was
// measured accepting `all` beside a SPECIFIC port and storing it — so this is
// unifig's rule, and it is what turns a missing syntax into a field an operator
// can widen again.
func TestStatingProtocolAllClearsAPolicysPorts(t *testing.T) {
	r := startReplay(t)
	r.seedPolicy(t, "Allow DNS out", "ALLOW", "Internal", "Dmz", narrowedTo(t, r, "Dmz", "tcp", "53"))

	applyFirewall(t, r, `firewall-policies:
  - name: Allow DNS out
    action: allow
    source: Internal
    destination: Dmz
    protocol: all
`)

	sent := r.onlyPolicyWrite(t)
	if sent["protocol"] != "all" {
		t.Errorf("the update should state the protocol it was given, got %v: %v", sent["protocol"], sent)
	}
	destination, _ := sent["destination"].(map[string]any)
	if destination["port_matching_type"] != "ANY" {
		t.Errorf("clearing a narrowing should put the port matching back to ANY, got %v: %v",
			destination["port_matching_type"], sent)
	}
	// The key is deleted rather than emptied, which is the measurement rather
	// than a preference: under a v2 policy PUT, which replaces, a key absent
	// from the body is a key absent from the object, and that is what the probe
	// read back (ADR-0021, ADR-0031).
	if port, carried := destination["port"]; carried {
		t.Errorf("clearing a narrowing should send no port key at all, got %v: %v", port, sent)
	}
}

// The plan has to show the ports going, because widening a policy is a change an
// operator can make by accident — `protocol: all` looks like it says nothing.
func TestPlanSaysThePortsGoWhenAPolicyIsWidenedAgain(t *testing.T) {
	r := startReplay(t)
	r.seedPolicy(t, "Allow DNS out", "ALLOW", "Internal", "Dmz", narrowedTo(t, r, "Dmz", "tcp", "53"))

	res := planFirewall(t, r, `firewall-policies:
  - name: Allow DNS out
    action: allow
    source: Internal
    destination: Dmz
    protocol: all
`)

	stdout := string(res.Stdout)
	for _, fragment := range []string{"protocol", "tcp", "ports", "53", "(none)"} {
		if !strings.Contains(stdout, fragment) {
			t.Errorf("the plan for a widened policy should carry %q:\n%s", fragment, stdout)
		}
	}
}

// The note this whole feature is arranged around: a block to the Gateway zone
// that states no ports is not the rule the operator asking for it means.
//
// It hedges and stops where ADR-0018 stops — a statement about what the policy
// says, not a verdict on what the rule set will do, because unifig models
// neither `index` nor `enabled` and issue #1 puts lockout analysis out of scope.
func TestPlanSaysAWideBlockToTheGatewayTakesDHCPAndDNSToo(t *testing.T) {
	r := startReplay(t)
	gateway := r.gatewayZone(t)

	res := planFirewall(t, r, fmt.Sprintf(`firewall-policies:
  - name: Ellingson off the router
    action: block
    source: Internal
    destination: %s
`, gateway))

	stdout := string(res.Stdout)
	for _, fragment := range []string{"blocks every service the Controller offers this zone", "DHCP", "DNS"} {
		if !strings.Contains(stdout, fragment) {
			t.Errorf("a wide block to the gateway should say what else it takes, wanted %q:\n%s", fragment, stdout)
		}
	}
}

// The other half, and the half that keeps the note worth reading: a policy that
// names its ports has said what it blocks, so there is nothing left to warn
// about. A note on every gateway policy is one an operator reads past by the
// third run (ADR-0012).
func TestPlanSaysNothingAboutDHCPWhenTheBlockNamesItsPorts(t *testing.T) {
	r := startReplay(t)
	gateway := r.gatewayZone(t)

	res := planFirewall(t, r, fmt.Sprintf(`firewall-policies:
  - name: Ellingson off the admin UI
    action: block
    source: Internal
    destination: %s
    protocol: tcp
    ports: [443, 80]
`, gateway))

	if strings.Contains(string(res.Stdout), "blocks every service") {
		t.Errorf("a narrowed block names what it blocks, so nothing should warn about DHCP:\n%s", res.Stdout)
	}
}

// Q2 of the design, and a deliberate over-warning. The Risky mark asks whether
// the destination is the gateway and whether the verdict is becoming blocking,
// and it does not ask about ports — narrowing it to "only when the port set
// contains the management path" would mean unifig keeping a list of Ubiquiti's
// management ports, which is the construct ADR-0005 and ADR-0018 have each
// already rejected and whose failure mode is silence when a port is added.
func TestANarrowedBlockToTheGatewayIsStillARiskyChange(t *testing.T) {
	r := startReplay(t)
	gateway := r.gatewayZone(t)

	res := planFirewall(t, r, fmt.Sprintf(`firewall-policies:
  - name: Ellingson off the admin UI
    action: block
    source: Internal
    destination: %s
    protocol: tcp
    ports: [443]
`, gateway))

	stdout := string(res.Stdout)
	if !strings.Contains(stdout, "! ") || !strings.Contains(stdout, riskOfBlockingTheGateway) {
		t.Errorf("a policy closing the gateway is Risky however narrow it is:\n%s", stdout)
	}
}

// ---------------------------------------------------------------------------
// A narrowing unifig has no words for (ADR-0032, issue #52).
//
// The other half of ADR-0031. A policy the Controller holds narrowed by
// something the config cannot state — a port group, an inverted match, a source
// port, an application target, a protocol outside the six — is still a name, a
// verdict and a pair of zones, which is what `CONTEXT.md` says export's scope
// is, so export writes it. The entry is true: an entry stating no narrowing
// manages none, and the next plan is silent about it.
//
// What is not true is a file that came back short saying nothing about it, which
// is the promise every other notice on this stderr keeps. So the policy goes in
// the file and the omission goes in a count — deliberately not the
// `WriteIndescribablePortForwards` treatment, which drops the whole object,
// because a forward missing a port has nothing left worth writing and a policy
// missing its narrowing has three of its four fields.

// endOn is one end of a seeded policy: the zone the test names, matching
// everything at it, with whatever narrowing the test is about laid over the top.
//
// The end has to be handed over whole because seedPolicy builds it, and the zone
// id it puts there is the part a test cannot invent. It serves both ends — the
// two are the same shape in the fields these tests set — so a test narrowing a
// source states it the way a test narrowing a destination does.
func endOn(t *testing.T, r *replay, zone string, narrowing map[string]any) map[string]any {
	t.Helper()
	id, _ := r.zoneNamed(t, zone)["_id"].(string)
	if id == "" {
		t.Fatalf("the recording has no zone named %q to put a policy's end on", zone)
	}
	end := map[string]any{"zone_id": id, "matching_target": "ANY"}
	for field, value := range narrowing {
		end[field] = value
	}
	return end
}

// unstatedNarrowing is the notice this section is about, in the singular.
const unstatedNarrowing = "Wrote 1 firewall policy narrowed by something the config cannot state"

// exportedPolicyNamed is the entry export wrote for one policy, and a failure
// where it wrote none: every test here is about a policy that is *in* the file.
func exportedPolicyNamed(t *testing.T, exported []byte, name string) exportedFirewallPolicy {
	t.Helper()
	for _, policy := range exportedYAML(t, exported).FirewallPolicies {
		if policy.Name == name {
			return policy
		}
	}
	t.Fatalf("export left %q out of the file altogether:\n%s", name, exported)
	return exportedFirewallPolicy{}
}

// The shape that matters most, because ADR-0031 is what made it reachable. `all`
// beside a SPECIFIC port is a combination the live migrated UDR was measured
// accepting and storing, and the one thing unifig refuses that the Controller
// takes — so an entry repeating it back is a file `validate` rejects, and export
// writing the policy without its port and saying nothing is a file that claims
// the policy has no ports.
func TestExportWritesAPolicyWhosePortsSitBesideAProtocolWithNoneAndCountsIt(t *testing.T) {
	r := startReplay(t)
	r.seedPolicy(t, "Wide and narrow", "ALLOW", "Internal", "Dmz", map[string]any{
		"protocol":    "all",
		"destination": endOn(t, r, "Dmz", map[string]any{"port_matching_type": "SPECIFIC", "port": "53"}),
	})

	exported := exportFirewall(t, r)

	written := exportedPolicyNamed(t, exported.Stdout, "Wide and narrow")
	if want := (exportedFirewallPolicy{
		Name: "Wide and narrow", Action: "allow", Source: "Internal", Destination: "Dmz",
	}); !written.same(want) {
		t.Errorf("the exported policy is %+v, want %+v — a port beside `all` is a line validate would reject", written, want)
	}
	if !strings.Contains(string(exported.Stderr), unstatedNarrowing) {
		t.Errorf("export said nothing about the port it could not write, looking for %q:\n%s",
			unstatedNarrowing, exported.Stderr)
	}

	// And the file it wrote is a file that plans clean: an entry stating no
	// narrowing manages none, so the port stays where the operator put it.
	if res := planExportedConfig(t, r, exported.Stdout); res.ExitCode != exitNoChanges {
		t.Fatalf("plan of a freshly exported config exited %d, want %d\nexported:\n%s\nplan:\n%s",
			res.ExitCode, exitNoChanges, exported.Stdout, res.Stdout)
	}
}

// A port group: the narrowing is a reference to a Controller object unifig does
// not manage, so there is no name for the file to put in `ports:` — and the
// protocol beside it is still written, because the entry says what it can.
func TestExportWritesAPolicyNarrowedByAPortGroupAndCountsIt(t *testing.T) {
	r := startReplay(t)
	r.seedPolicy(t, "Ports by group", "ALLOW", "Internal", "Dmz", map[string]any{
		"protocol": "tcp",
		"destination": endOn(t, r, "Dmz", map[string]any{
			"port_matching_type": "OBJECT",
			"port_group_id":      "6613a1f0c4b2d90a5e1f7fff",
		}),
	})

	exported := exportFirewall(t, r)

	written := exportedPolicyNamed(t, exported.Stdout, "Ports by group")
	if want := (exportedFirewallPolicy{
		Name: "Ports by group", Action: "allow", Source: "Internal", Destination: "Dmz", Protocol: "tcp",
	}); !written.same(want) {
		t.Errorf("the exported policy is %+v, want %+v — the protocol is describable and the port group is not", written, want)
	}
	if !strings.Contains(string(exported.Stderr), unstatedNarrowing) {
		t.Errorf("export said nothing about the port group, looking for %q:\n%s",
			unstatedNarrowing, exported.Stderr)
	}
}

// An inversion — "every port except these" — which unifig has no way to say and
// which turns the ports beside it into their own opposite. This is the one shape
// where writing the narrowing would be worse than leaving it out: the same
// `ports:` list means the other half of the port space.
func TestExportWritesAPolicyWithAnInvertedPortMatchAndCountsIt(t *testing.T) {
	r := startReplay(t)
	r.seedPolicy(t, "All but DNS", "ALLOW", "Internal", "Dmz", map[string]any{
		"protocol": "tcp",
		"destination": endOn(t, r, "Dmz", map[string]any{
			"port_matching_type":   "SPECIFIC",
			"port":                 "53",
			"match_opposite_ports": true,
		}),
	})

	exported := exportFirewall(t, r)

	written := exportedPolicyNamed(t, exported.Stdout, "All but DNS")
	if len(written.Ports) != 0 {
		t.Errorf("export wrote %v as the policy's ports, which is the set it does not match", written.Ports)
	}
	if !strings.Contains(string(exported.Stderr), unstatedNarrowing) {
		t.Errorf("export said nothing about the inversion, looking for %q:\n%s",
			unstatedNarrowing, exported.Stderr)
	}

	// Dropping the ports is not the same as inventing a difference: the entry
	// states the protocol it can and no ports at all, which manages none, so the
	// inversion stays exactly where the operator put it.
	if res := planExportedConfig(t, r, exported.Stdout); res.ExitCode != exitNoChanges {
		t.Fatalf("plan of a freshly exported config exited %d, want %d\nexported:\n%s\nplan:\n%s",
			res.ExitCode, exitNoChanges, exported.Stdout, res.Stdout)
	}
}

// A destination matched by application rather than by zone and port: a matching
// engine of its own, and nothing the six protocols and a port list can describe.
func TestExportWritesAPolicyMatchedOnAnApplicationAndCountsIt(t *testing.T) {
	r := startReplay(t)
	r.seedPolicy(t, "No streaming", "BLOCK", "Internal", "External", map[string]any{
		"destination": endOn(t, r, "External", map[string]any{
			"matching_target": "APP",
			"app_ids":         []any{4, 7},
		}),
	})

	exported := exportFirewall(t, r)

	exportedPolicyNamed(t, exported.Stdout, "No streaming")
	if !strings.Contains(string(exported.Stderr), unstatedNarrowing) {
		t.Errorf("export said nothing about the application matching, looking for %q:\n%s",
			unstatedNarrowing, exported.Stderr)
	}
}

// The source end, which #51 declined to model: every fixed-source-port rule
// anyone has read is a Generated Policy and on client traffic a source port is
// ephemeral — but a policy the operator narrowed that way is one unifig writes
// down without the half that makes it the rule they wrote.
func TestExportWritesAPolicyNarrowedOnItsSourceAndCountsIt(t *testing.T) {
	r := startReplay(t)
	r.seedPolicy(t, "From one port", "ALLOW", "Internal", "Dmz", map[string]any{
		"protocol": "udp",
		"source": endOn(t, r, "Internal", map[string]any{
			"port_matching_type": "SPECIFIC",
			"port":               "68",
		}),
	})

	exported := exportFirewall(t, r)

	exportedPolicyNamed(t, exported.Stdout, "From one port")
	if !strings.Contains(string(exported.Stderr), unstatedNarrowing) {
		t.Errorf("export said nothing about the source port, looking for %q:\n%s",
			unstatedNarrowing, exported.Stderr)
	}
}

// A protocol outside the six unifig models. The entry states none, which is
// unmanaged and true, and it is the case where the file reads exactly like a
// policy that narrows nothing at all — so the count is the only thing telling
// the two apart.
func TestExportWritesAPolicyOnAnUnmodelledProtocolAndCountsIt(t *testing.T) {
	r := startReplay(t)
	r.seedPolicy(t, "Let the tunnel through", "ALLOW", "Internal", "External", map[string]any{
		"protocol": "esp",
	})

	exported := exportFirewall(t, r)

	written := exportedPolicyNamed(t, exported.Stdout, "Let the tunnel through")
	if written.Protocol != "" {
		t.Errorf("export wrote %q as the protocol, which is not one of the six the file may state", written.Protocol)
	}
	if !strings.Contains(string(exported.Stderr), unstatedNarrowing) {
		t.Errorf("export said nothing about the protocol it could not write, looking for %q:\n%s",
			unstatedNarrowing, exported.Stderr)
	}
}

// The other side of the rule, and the half that keeps the notice worth reading: a
// narrowing export *can* describe is written in full and says nothing on stderr.
// A notice firing on the 126 policies of 135 that carry `all` — or on every
// policy narrowed the way ADR-0031 made writable — is one an operator learns to
// read past (ADR-0012).
func TestExportSaysNothingAboutANarrowingItCanWriteInFull(t *testing.T) {
	r := startReplay(t)
	r.seedPolicy(t, "Allow DNS out", "ALLOW", "Internal", "Dmz", narrowedTo(t, r, "Dmz", "tcp", "53"))
	r.seedPolicy(t, "Wide open", "ALLOW", "Internal", "External", nil)

	exported := exportFirewall(t, r)

	written := exportedPolicyNamed(t, exported.Stdout, "Allow DNS out")
	if want := (exportedFirewallPolicy{
		Name: "Allow DNS out", Action: "allow", Source: "Internal", Destination: "Dmz",
		Protocol: "tcp", Ports: []string{"53"},
	}); !written.same(want) {
		t.Errorf("the exported policy is %+v, want %+v", written, want)
	}
	if strings.Contains(string(exported.Stderr), "narrowed by something the config cannot state") {
		t.Errorf("export counted a narrowing it wrote down in full:\n%s", exported.Stderr)
	}
}

// Two of them read as two, which is what says the count is a count rather than a
// flag — and they are two policies rather than one policy narrowed two ways,
// because the subject is the entry that came back short.
func TestTheNarrowingCountIsOfPoliciesRatherThanOfOmissions(t *testing.T) {
	r := startReplay(t)
	r.seedPolicy(t, "Ports by group", "ALLOW", "Internal", "Dmz", map[string]any{
		"protocol": "tcp",
		"destination": endOn(t, r, "Dmz", map[string]any{
			"port_matching_type": "OBJECT",
			"port_group_id":      "6613a1f0c4b2d90a5e1f7fff",
		}),
	})
	r.seedPolicy(t, "All but DNS", "ALLOW", "Internal", "Dmz", map[string]any{
		"protocol": "tcp",
		"destination": endOn(t, r, "Dmz", map[string]any{
			"port_matching_type":   "SPECIFIC",
			"port":                 "53",
			"match_opposite_ports": true,
			"matching_target":      "WEB",
		}),
	})

	stderr := string(exportFirewall(t, r).Stderr)

	if want := "Wrote 2 firewall policies narrowed by something the config cannot state"; !strings.Contains(stderr, want) {
		t.Errorf("the notice should count both policies, looking for %q:\n%s", want, stderr)
	}
}

// An inversion with nothing under it inverts nothing, and the flags are on the
// wire either way: `match_opposite_ports` and `match_opposite_protocol` are
// plain bools with no `omitempty`, so the Controller sends them on every policy
// it holds — false on all eighty-six in the recording. So each is read beside the
// thing it inverts. A count that read one alone would speak for an omission that
// is not there, on a file that is working, which is the notice ADR-0012 says
// teaches an operator to skip the rest.
//
// The second seed is a shape nobody has read off a router — a policy carrying no
// protocol at all — and it is here rather than implied, because it is the only
// arrangement in which the protocol flag can be told from the protocol beside it.
func TestAnInversionWithNothingToInvertIsNotCounted(t *testing.T) {
	r := startReplay(t)
	r.seedPolicy(t, "Every port already", "ALLOW", "Internal", "Dmz", map[string]any{
		"protocol": "tcp",
		"destination": endOn(t, r, "Dmz", map[string]any{
			"port_matching_type":   "ANY",
			"match_opposite_ports": true,
		}),
	})
	r.seedPolicy(t, "No protocol to invert", "ALLOW", "Internal", "Dmz", map[string]any{
		"protocol":                nil,
		"match_opposite_protocol": true,
	})

	exported := exportFirewall(t, r)

	exportedPolicyNamed(t, exported.Stdout, "Every port already")
	exportedPolicyNamed(t, exported.Stdout, "No protocol to invert")
	if strings.Contains(string(exported.Stderr), "narrowed by something the config cannot state") {
		t.Errorf("export counted a policy whose inversion inverts nothing:\n%s", exported.Stderr)
	}
}

// The two notices do not meet on one policy, and the recording is what says so:
// every one of its eighty-six is the Controller's own, and some of those are
// narrowed by things unifig cannot word. A policy export never wrote is not a
// policy export wrote short — it is counted where it is left out, once.
func TestAGeneratedPolicyNarrowedBeyondTheConfigIsCountedOnlyAsGenerated(t *testing.T) {
	r := startReplay(t)
	// seedGeneratedPolicy with the protocol changed, which is the whole of what
	// this needs: the composite `_id` is what makes a policy generated, and the
	// narrowing is what export would have had no words for had it written one.
	r.seedPolicy(t, "Allow Web", "ALLOW", "Dmz", "External", map[string]any{
		"_id":        r.generatedPolicyID(t, "Dmz", "External", 30000),
		"index":      30000,
		"predefined": true,
		"protocol":   "esp",
	})

	stderr := string(exportFirewall(t, r).Stderr)

	if strings.Contains(stderr, "narrowed by something the config cannot state") {
		t.Errorf("export counted the narrowing of a policy it never wrote:\n%s", stderr)
	}
	if want := fmt.Sprintf("Left out %d firewall policies the Controller generates", len(r.livePolicies(t))); !strings.Contains(stderr, want) {
		t.Errorf("the generated notice should speak for it instead, looking for %q:\n%s", want, stderr)
	}
}

// Where a policy unifig creates sits in the Controller's evaluation order, and
// the whole of why unifig is in the business of saying so.
//
// Measured on the live UDR running 10.6.101 on 3 September 2026 (issue #54). An
// `allow` Gibson -> Ellingson and a `block` Ellingson -> Gibson, both written by
// unifig and both exactly as the file states them, left the pair like this:
//
//	10000        Ellingson -> Gibson     'Ellingson off the Gibson'      BLOCK  ALL
//	10000        Gibson    -> Ellingson  'Gibson to Ellingson'           ALLOW  ALL
//	30000        Ellingson -> Gibson     'Gibson to Ellingson (Return)'  ALLOW  RESPOND_ONLY
//	2147483647   Ellingson -> Gibson     'Block All Traffic'             BLOCK  ALL
//
// The companion is at 30000 and the operator's block is 20000 above it, so the
// reply to a permitted request is dropped before the rule admitting it is
// reached. Confirmed at packet level in the same session by disabling the block
// and re-pinging: replies came back, and stopped again when it was re-enabled.
//
// A policy that blocks therefore has to end up below the companion tier, and the
// only way to put it there is the reorder endpoint — `index` is not writable
// (ADR-0033).
func TestCreatingABlockingPolicyMovesItBelowTheCompanionTier(t *testing.T) {
	// Both verdicts that close a path, because a companion is an `ALLOW` and
	// either of these can stop one being reached.
	for _, verdict := range []string{"block", "reject"} {
		t.Run(verdict, func(t *testing.T) {
			r := startReplay(t)
			// The issue's own shape: an allow one way, a block the other. The
			// allow's companion lands on the block's pair, which is the only
			// place a block above the tier can do any harm.
			applyFirewall(t, r, fmt.Sprintf(`firewall-policies:
  - name: Gibson to Ellingson
    action: allow
    source: Internal
    destination: Dmz
  - name: Somewhere below
    action: %s
    source: Dmz
    destination: Internal
`, verdict))

			policy := r.policyNamed(t, "Somewhere below")
			index, ok := numberIn(policy, "index")
			if !ok {
				t.Fatalf("the created policy holds no index: %v", policy)
			}
			if index <= companionIndex {
				t.Errorf("the policy is at index %d, at or above the companion tier at %d: a policy there drops the reply to traffic an allow unifig made permits",
					index, companionIndex)
			}
		})
	}
}

// The create body says nothing about where the policy goes, because saying so
// does nothing.
//
// Measured on the live UDR on 4 September 2026, on a throwaway `Dmz -> Dmz`
// policy deleted afterwards: a POST naming `index: 40000` answered 201 and stored
// 10000, and a PUT of that same object naming 40000 answered 200 and stored
// 10000. The field is accepted and ignored on both verbs. Sending it anyway would
// be a line of unifig's config on the wire that changes nothing, which is the
// kind of thing that reads as a decision to the next person (ADR-0004).
func TestTheCreateBodyNamesNoIndex(t *testing.T) {
	r := startReplay(t)
	r.seedCompanion(t, "Gibson to Ellingson", "Dmz", "Internal")
	applyFirewall(t, r, `firewall-policies:
  - name: Somewhere below
    action: block
    source: Dmz
    destination: Internal
`)

	if index, carried := r.onlyPolicyWrite(t)["index"]; carried {
		t.Errorf("the create names index %v, and the Controller ignores it on this verb and on the other one", index)
	}
}

// The other half of the rule, and the half that keeps this fix from being issue
// #54 pointing the other way.
//
// The generated tier is not all return rules. `Isolated Networks` is an enabled
// `BLOCK ALL` and it sits at **30000** — the companion's own index — on
// `Internal -> Internal`, `Internal -> Hotspot` and `Internal -> Dmz` in the
// 86-policy recording, with `Block Invalid Traffic`, `Post-Authorization
// Restrictions` and `Block Unauthorized Traffic` in the same band just below.
// An `allow` moved down there would sit under an enabled block and do nothing,
// which is a policy the file states plainly and the firewall does not have.
//
// There is no third position that escapes both, because the companion and
// `Isolated Networks` are at the same index — anything yielding to one yields to
// the other. So an `allow` is left exactly where the Controller puts it, and
// unifig sends no reorder at all.
func TestCreatingAnAllowPolicyLeavesItWhereTheControllerPutIt(t *testing.T) {
	r := startReplay(t)
	// The pair is named rather than arbitrary: this is one of the three the
	// recording holds an enabled generated BLOCK at 30000 on, so it is the pair
	// where getting this wrong costs the operator the policy they wrote.
	applyFirewall(t, r, `firewall-policies:
  - name: Let them through
    action: allow
    source: Internal
    destination: Dmz
`)

	policy := r.policyNamed(t, "Let them through")
	if index, _ := numberIn(policy, "index"); index != storedPolicyIndex {
		t.Errorf("the allow is at index %v, and the Controller generates an enabled BLOCK at %d on this very pair: an allow below one is a policy the file states and the firewall does not have",
			policy["index"], companionIndex)
	}
	if sent := r.reorderWrites(t); len(sent) != 0 {
		t.Errorf("unifig reordered a pair to place an allow, which is a statement about every stored policy on it: %v", sent)
	}
}

// The half that makes this converge, and the case issue #54 was actually
// reported from.
//
// Every site that had this bug when it was found has its policies already
// created, so a fix that only ran on a create would have fixed nobody's firewall
// — including the one it was reported from, whose fourteen stored policies were
// all sitting at 10000. The plan has to see a live policy on the wrong side and
// say so, with nothing else differing.
func TestPlanMovesAnExistingBlockingPolicyBelowTheCompanionTier(t *testing.T) {
	r := startReplay(t)
	r.seedCompanion(t, "Gibson to Ellingson", "Dmz", "Internal")
	r.seedStoredPolicy(t, "Ellingson off the Gibson", "BLOCK", "Dmz", "Internal")

	body := `firewall-policies:
  - name: Ellingson off the Gibson
    action: block
    source: Dmz
    destination: Internal
`
	res := planFirewall(t, r, body)
	if res.ExitCode != exitChangesPending {
		t.Fatalf("plan exited %d, want %d; a policy above the companion tier is a change waiting to be made\nstdout: %s",
			res.ExitCode, exitChangesPending, res.Stdout)
	}
	stdout := string(res.Stdout)
	for _, fragment := range []string{"placement", "after the return rules"} {
		if !strings.Contains(stdout, fragment) {
			t.Errorf("the plan should say the policy is being moved, looking for %q:\n%s", fragment, stdout)
		}
	}

	applyFirewall(t, r, body)
	policy := r.policyNamed(t, "Ellingson off the Gibson")
	if index, _ := numberIn(policy, "index"); index <= companionIndex {
		t.Errorf("the apply left the policy at index %v, at or above the companion tier at %d", policy["index"], companionIndex)
	}
	assertNoChangesPendingEnv(t, r.env(), configFile(t, body))
}

// The reorder names every stored policy on the pair, because the Controller
// refuses a partial list.
//
// Measured on the live UDR on 4 September 2026: naming one of two stored policies
// on a pair answered `400 api.err.ShouldIncludeFirewallPolicyInBatchUpdate`, and
// naming both answered 200. So moving one policy means saying where every one of
// them goes — and a policy the config does not name keeps the side it is on,
// which is what stops an endpoint requiring the whole list from turning into
// unifig managing the whole list (ADR-0004).
func TestAReorderNamesThePairsOtherPoliciesAndLeavesTheUnmanagedOneWhereItIs(t *testing.T) {
	r := startReplay(t)
	r.seedCompanion(t, "Gibson to Ellingson", "Dmz", "Internal")
	r.seedStoredPolicy(t, "Mine to move", "BLOCK", "Dmz", "Internal")
	r.seedStoredPolicy(t, "Not in the file", "BLOCK", "Dmz", "Internal")

	applyFirewall(t, r, `firewall-policies:
  - name: Mine to move
    action: block
    source: Dmz
    destination: Internal
`)

	sent := r.reorderWrites(t)
	if len(sent) != 1 {
		t.Fatalf("unifig made %d reorder requests, want the one this config asks for: %v", len(sent), sent)
	}
	named := len(sent[0]["after_predefined_ids"].([]string)) + len(sent[0]["before_predefined_ids"].([]string))
	if named != 2 {
		t.Errorf("the reorder names %d of the pair's 2 stored policies, and the Controller answers 400 to a partial list: %v",
			named, sent[0])
	}
	if index, _ := numberIn(r.policyNamed(t, "Not in the file"), "index"); index != storedPolicyIndex {
		t.Errorf("the policy the config does not name moved to index %v; unstated is unmanaged, including here", index)
	}
	if index, _ := numberIn(r.policyNamed(t, "Mine to move"), "index"); index <= companionIndex {
		t.Errorf("the policy the config does name did not move: index %v", index)
	}
}

// A plan with nothing to move sends no reorder, because a reorder is a statement
// about every stored policy on a pair and making one nobody asked for is how an
// operator's own ordering quietly stops being theirs.
func TestAPolicyAlreadyBelowTheCompanionTierIsLeftAlone(t *testing.T) {
	r := startReplay(t)
	r.seedCompanion(t, "Gibson to Ellingson", "Dmz", "Internal")
	r.seedPolicy(t, "Already below", "BLOCK", "Dmz", "Internal", map[string]any{
		"index":      afterPredefinedIndex,
		"predefined": false,
	})

	body := `firewall-policies:
  - name: Already below
    action: block
    source: Dmz
    destination: Internal
`
	assertNoChangesPendingEnv(t, r.env(), configFile(t, body))
	if sent := r.reorderWrites(t); len(sent) != 0 {
		t.Errorf("a plan with nothing to move still reordered a pair: %v", sent)
	}
}

// The create says the move is coming, for the reason the return-rule note exists:
// unifig is about to do something the config does not state, and a plan that
// quietly did more than it printed would not be a plan.
func TestPlanSaysABlockingPolicyWillBeMovedBelowTheReturnRules(t *testing.T) {
	r := startReplay(t)
	r.seedCompanion(t, "Gibson to Ellingson", "Dmz", "Internal")
	res := planFirewall(t, r, `firewall-policies:
  - name: Somewhere below
    action: block
    source: Dmz
    destination: Internal
`)

	if !strings.Contains(string(res.Stdout), "move it below") {
		t.Errorf("the plan should say the policy will be moved:\n%s", res.Stdout)
	}
}

// And says nothing of the sort about an allow, which is not moved.
func TestPlanSaysNothingAboutMovingAnAllowPolicy(t *testing.T) {
	r := startReplay(t)
	res := planFirewall(t, r, `firewall-policies:
  - name: Let them through
    action: allow
    source: Internal
    destination: Dmz
`)

	if strings.Contains(string(res.Stdout), "move it below") {
		t.Errorf("the plan promised to move an allow:\n%s", res.Stdout)
	}
}

// A block on a pair with no Return Rule on it is left exactly where it is, and
// this is the test that keeps the fix proportionate to the bug.
//
// A blocking policy only outranks a companion where there is a companion. A block
// on `Internal -> External` has no reply traffic to strand, and moving it would be
// unifig reordering somebody's firewall for no reason — a reorder being a
// statement about every stored policy on the pair. Without this gate a brownfield
// `export` produced a config whose first `plan` proposed moving every blocking
// policy on the site, which is a great deal of change to justify with a bug that
// reaches almost none of them.
func TestABlockingPolicyOnAPairWithNoReturnRuleIsLeftAlone(t *testing.T) {
	r := startReplay(t)
	r.seedStoredPolicy(t, "No internet", "BLOCK", "Internal", "External")

	body := `firewall-policies:
  - name: No internet
    action: block
    source: Internal
    destination: External
`
	assertNoChangesPendingEnv(t, r.env(), configFile(t, body))
	if sent := r.reorderWrites(t); len(sent) != 0 {
		t.Errorf("unifig reordered a pair that carries no return rule: %v", sent)
	}
	if index, _ := numberIn(r.policyNamed(t, "No internet"), "index"); index != storedPolicyIndex {
		t.Errorf("the policy moved to index %d with no companion on its pair to move it for", index)
	}
}

// The companion the config is about to bring into being counts as well as the one
// already there, which is what makes one apply enough.
//
// A block and the allow whose companion it would outrank can be created in either
// order, and the order is whatever their names happened to sort to. Reading the
// config rather than waiting for the Controller to generate the companion is what
// stops "apply, then plan says there is more to do" (ADR-0022, ADR-0033).
func TestABlockIsMovedForACompanionTheSameApplyIsAboutToCreate(t *testing.T) {
	r := startReplay(t)

	// The allow sorts after the block by name, so the companion does not exist
	// at the moment the block is written.
	body := `firewall-policies:
  - name: Block first by name
    action: block
    source: Dmz
    destination: Internal
  - name: Zulu allow sorts last
    action: allow
    source: Internal
    destination: Dmz
`
	applyFirewall(t, r, body)

	if index, _ := numberIn(r.policyNamed(t, "Block first by name"), "index"); index <= companionIndex {
		t.Errorf("the block is at index %d, at or above the companion tier at %d, so the reply to traffic the allow permits is dropped",
			index, companionIndex)
	}
	assertNoChangesPendingEnv(t, r.env(), configFile(t, body))
}
