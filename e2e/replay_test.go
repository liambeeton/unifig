package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// The replay stand-in is how the WAN slots get tested at the same seam as
// everything else.
//
// The dockerized Controller substitutes for a UDR everywhere it can, and WAN is
// where it cannot: with no gateway attached it has no WAN entries at all, so
// there is nothing to match, nothing to update, and nothing a seeded LAN could
// honestly stand in for. What takes its place is not a mock of unifig's client
// but a Controller at the same seam — recorded responses served over HTTPS,
// behind the same base URL, through the same API-key header, to the same real
// unifig binary. Nothing in unifig knows the difference, which is the whole
// point: swap the recording for a real UDR's and the same tests still describe
// the same behaviour.
//
// It holds the recorded networkconf in memory and updates its copy on a PUT,
// the way the Controller updates its own — replaying a fixed answer would make
// "apply converged, and the next plan is empty" impossible to state, and that
// sentence is the contract this suite exists to check.

// Recorded endpoints. Everything unifig asks a Controller for while planning,
// applying or exporting is one of these; anything else arriving here is a
// change in what unifig does, and the stand-in says so rather than 404ing
// quietly.
//
// The settings pair is not symmetrical, and that is the Internal API rather
// than a simplification here: reading one setting means asking for all of them,
// while writing one names the key in the path.
// The firewall pair sits under a different tree from everything else — the
// Controller's v2 API — and answers with a bare JSON array rather than the
// {meta, data} envelope the rest of the Internal API wraps everything in. Both
// differences are the Controller's, not a simplification here, and the stand-in
// reproduces them exactly: a fixture that wrapped a zone list in an envelope
// would be a fixture unifig's own client cannot read.
const (
	sysinfoPath     = "/proxy/network/api/s/default/stat/sysinfo"
	networkconfPath = "/proxy/network/api/s/default/rest/networkconf"
	wlanconfPath    = "/proxy/network/api/s/default/rest/wlanconf"
	portforwardPath = "/proxy/network/api/s/default/rest/portforward"
	userPath        = "/proxy/network/api/s/default/rest/user"
	settingPath     = "/proxy/network/api/s/default/get/setting"
	setDoHPath      = "/proxy/network/api/s/default/set/setting/doh"
	zonePath        = "/proxy/network/v2/api/site/default/firewall/zone"
	policyPath      = "/proxy/network/v2/api/site/default/firewall-policies"
	reorderPath     = policyPath + "/batch-reorder"
)

// dohKey is the Controller's own name for the Encrypted DNS setting — how it is
// picked out of the settings collection, in the recording and in unifig alike.
const dohKey = "doh"

// replayAPIKey is the key the stand-in accepts, standing in for a UDR API key
// exactly as the rig's proxy does.
const replayAPIKey = "unifig-e2e-replay-api-key"

// replay is a recorded Controller: the UniFi OS base URL, the API-key gate, and
// the recorded state, mutable where the Controller's own is.
type replay struct {
	t   *testing.T
	url string

	mu sync.Mutex
	// entries is the networkconf collection as the Controller holds it: the
	// recording to begin with, and whatever unifig has written since.
	entries []map[string]any
	// settings is the same for the settings collection, which unifig reads
	// whole and writes one key of.
	settings []map[string]any
	// zones and policies are the zone-based firewall, and the first collections
	// here that unifig creates and deletes rather than only updating — so the
	// stand-in keeps them the way the Controller does, with an ID handed out on
	// create and the entry gone on delete.
	zones    []map[string]any
	policies []map[string]any
	// issued counts the IDs this stand-in has handed out, so that two objects
	// created in one apply cannot collide.
	issued int
	// reordered counts the batch-reorder requests unifig has made, so a test can
	// assert that a plan promising no placement change made none. The bodies
	// themselves are not in `written`, which holds the two object collections.
	reordered []map[string]any
	// written is every body unifig has sent to one of the v2 collections, in
	// the order it sent them and whatever the stand-in then did with it.
	//
	// It is here because the stored object cannot answer for the request: this
	// stand-in keeps whatever it is handed, so a test that applies and reads
	// back passes whatever the payload contained. That is how a zone unifig
	// could not actually write shipped with a passing test — the payload
	// carried a field the Controller refuses, and nothing here could refuse it
	// (ADR-0014, ADR-0019).
	written []sentRequest
	// wlans, forwards and sysinfo are served exactly as recorded. Nothing in the
	// Settings tests writes to any of them, and a recording that could not be
	// trusted to come back unchanged would be a poor witness for the one that
	// does.
	//
	// The first three are recorded empty (ADR-0011): the dockerized Controller
	// covers WLANs, port forwards and DHCP reservations, so a recording keeps
	// none of them and those endpoints answer here only because export and prune
	// ask them.
	wlans    json.RawMessage
	forwards json.RawMessage
	clients  json.RawMessage
	sysinfo  json.RawMessage
}

// sentRequest is one write unifig made to a v2 collection: where it went and
// the body it carried.
type sentRequest struct {
	path string
	body map[string]any
}

// writeSemantics is what a collection's PUT does with a field the body leaves
// out. It is per endpoint rather than per API version, which is the whole of why
// it is a parameter here and not a rule: both of the v2 collections this
// stand-in serves were asked the question on the same live migrated UDR a day
// apart, and they gave opposite answers. The policy endpoint replaces
// (ADR-0021), the zone endpoint merges (ADR-0024), and neither was inferable
// from the other — the guess that they matched is the one the zone's comment
// below used to carry.
type writeSemantics int

const (
	replaces writeSemantics = iota
	merges
)

// writeContract is one v2 collection's write endpoint as this stand-in models
// it: what it refuses, and what it does with a field the body leaves out.
//
// The three travel together because they are one endpoint's answer, and every
// one of them was measured on the same router in the same session as the others
// — so a collection served without one of them is a collection this stand-in
// has an unstated opinion about. They used to be three parameters, which meant
// each call site passed a nil for the part of its endpoint nobody had asked
// about yet, and "nil" read as "nothing to say" whether that was measured or
// merely untried.
type writeContract struct {
	// refuses names the fields this endpoint answers 400 to by name, so a
	// payload the Controller would not parse is not one this stand-in quietly
	// stores. Empty is a claim: see the policy collection, where it is a reading
	// (issue #37) rather than an absence.
	refuses []string
	// refusesBody is the same cover for a refusal about a combination the
	// endpoint understands rather than a field it has never heard of. It guards
	// the create and the update alike, because the endpoint that refused issue
	// #36's create refused issue #37's update on the same body.
	refusesBody func(r *replay, sent map[string]any) string
	// semantics is what a PUT here does with a field the body leaves out.
	semantics writeSemantics
	// companions is whether this endpoint generates a companion return rule for
	// a policy that asks for one, which only the firewall-policy collection
	// does. It is modelled because it was measured on both verbs and in both
	// directions (issue #40, ADR-0026) — and because without it an apply that
	// asks for a companion is not idempotent here: the next plan would still see
	// one missing, which is a real failure this stand-in would otherwise hide.
	companions bool
	// notFoundCode is what this collection answers when the id in the path is
	// not one it resolves, and empty for a collection nobody has asked. See
	// writeContract.unresolvable.
	notFoundCode string
	// assignsIndex is whether this collection decides for itself where a created
	// object sits in evaluation order, **overruling whatever the body said**.
	//
	// Measured on the live UDR on 4 September 2026, on a throwaway `Dmz -> Dmz`
	// policy deleted afterwards: a POST naming `index: 40000` answered 201 and
	// stored 10000, and a PUT of that same object naming 40000 answered 200 and
	// stored 10000. So `index` is the Controller's to assign on both verbs, and a
	// stand-in that stored what it was handed would pass a unifig that thought
	// otherwise — which is exactly what happened, and what this now stops
	// (ADR-0019, ADR-0033).
	assignsIndex bool
}

// The two contracts, one per collection. Neither was inferred from the other:
// the zone's semantics were measured a day after the policy's and came back the
// opposite way (ADR-0021, ADR-0024).
var (
	zoneWriteContract = writeContract{
		refuses:   refusedByZoneWrite,
		semantics: merges,
	}
	policyWriteContract = writeContract{
		notFoundCode: "api.err.FirewallPolicyNotFound",
		refusesBody:  refusedByPolicyWrite,
		semantics:    replaces,
		companions:   true,
		assignsIndex: true,
	}
)

// reconcileCompanion brings the companion return rule into line with the policy
// just written, which is what the Controller was measured doing on both verbs.
//
// Issue #40's probe, on the live migrated UDR on 19 August 2026, moved one
// variable at a time on a throwaway `Dmz` -> `Dmz` policy with the verdict held
// at `ALLOW` throughout: creating it with the flag took the site 86 -> 88,
// clearing the flag took it 88 -> 87 and the companion was the one missing, and
// setting the flag again took it 87 -> 88 with the companion back. So the flag
// drives the companion on an update exactly as it does on a create, in both
// directions, and this models that rather than the create alone.
//
// The companion is shaped the way one was read off the router: named after its
// parent, `RESPOND_ONLY`, `predefined` — so unifig's prune spares it (ADR-0005)
// and it holds no zone back (ADR-0019) — and carrying `origin_id` back to the
// policy that caused it. Its ends are the reverse pair, which is where the
// twelve `Allow Return Traffic` policies a migrated router ships sit.
//
// Its `_id` is a composite rather than a document handle, which is the same
// session's reading: ADR-0026 recorded `<source zone id><destination zone
// id>30000` off the companion of a *custom* parent, which is what says the
// composite scheme belongs to generated policies rather than to shipped ones.
// This stand-in asserted a handle here until issue #45, which was a fixture
// stating a guess hardware had already refuted (ADR-0019).
//
// The zone ids are the companion's own ends rather than its parent's, and the
// reading cannot separate the two: the policy it was read from ran `Dmz` ->
// `Dmz`, so the pair and its reverse are the same two ids. The companion's own
// is the reading that makes the id a description of where *this* policy sits,
// which is what a composite id is (ADR-0027).
func (r *replay) reconcileCompanion(held *[]map[string]any, parent map[string]any) {
	name, _ := parent["name"].(string)
	requested, _ := parent["create_allow_respond"].(bool)
	action, _ := parent["action"].(string)
	if !requested || action != "ALLOW" {
		r.dropCompanion(held, name)
		return
	}

	companion := name + " (Return)"
	for _, entry := range *held {
		if entry["name"] == companion {
			return
		}
	}
	source, _ := parent["source"].(map[string]any)
	destination, _ := parent["destination"].(map[string]any)
	from, _ := destination["zone_id"].(string)
	to, _ := source["zone_id"].(string)
	*held = append(*held, map[string]any{
		// companionIndex is the index the composite ends in, taken from the
		// same reading rather than invented alongside it.
		"_id":                   compositePolicyID(from, to, companionIndex),
		"name":                  companion,
		"action":                "ALLOW",
		"enabled":               true,
		"predefined":            true,
		"connection_state_type": "RESPOND_ONLY",
		"origin_type":           "custom_firewall_rule",
		"origin_id":             parent["_id"],
		"create_allow_respond":  false,
		// The reverse pair, which is the end the reply arrives on.
		"source":      destination,
		"destination": source,
		"site_id":     "6613a1f0c4b2d90a5e1f0000",
	})
}

// companionIndex is the index the companion's composite `_id` ends in, read off
// the router in ADR-0026's write session.
const companionIndex = 30000

// dropCompanion removes the companion of the named policy, if it has one. It is
// how the Controller was measured answering both a cleared request and a deleted
// parent, which is one behaviour reached two ways.
func (r *replay) dropCompanion(held *[]map[string]any, parent string) {
	companion := parent + " (Return)"
	for i, entry := range *held {
		if entry["name"] == companion {
			*held = append((*held)[:i], (*held)[i+1:]...)
			return
		}
	}
}

// refused is every way this contract's endpoint has been measured answering
// 400, asked once so the create and the update cannot drift apart — which is not
// hypothetical tidiness: the refusal issue #36 measured on a create is the one
// issue #37 met on an update, and a stand-in that guarded only the verb it was
// first written for is the stand-in that missed it.
//
// The two shapes are both here rather than one covering the other: a field the
// DTO has never heard of (the zone's, ADR-0019) and a combination it understands
// and rejects (the policy's, ADR-0022). It reports whether it answered, so a
// caller stops.
func (c writeContract) refused(r *replay, w http.ResponseWriter, sent map[string]any) bool {
	if r.unrecognisedField(w, sent, c.refuses) {
		return true
	}
	if c.refusesBody == nil {
		return false
	}
	if refusal := c.refusesBody(r, sent); refusal != "" {
		r.refuse(w, refusal)
		return true
	}
	return false
}

// refusedByZoneWrite names the fields the Controller's zone write endpoint has
// been seen to refuse. Its DTO rejects any field it has not heard of, with a
// 400 naming the first one it reaches, and this is the list a real UDR was
// measured refusing one field at a time (ADR-0019, ADR-0024).
//
// It grew from two to six when issue #38 asked the endpoint the question
// directly instead of inferring it. Each of these was PUT on its own, on top of
// the three-field body unifig already sends, to a throwaway custom zone on the
// live migrated UDR on 19 August 2026, and each came back
//
//	400: JSON parse error: Unrecognized field "<name>"
//	     (class com.ubnt.g.c.t.AWSXjrFfvsFZsv), not marked as ignorable
//
// So this DTO takes `_id`, `name` and `network_ids` and nothing else — every
// other field a zone GET returns is refused by name, whatever its value.
// `attr_no_edit` was refused sent as `false`, which is the value `omitempty`
// hides today, so the field is refused rather than the value.
//
// `site_id` is the one on this list that is not in a zone's read shape at all —
// no GET on the live router returns it, and neither does the recording — and it
// is here because go-unifi models it as `json:"site_id,omitempty"`. That is the
// exact shape of the defect ADR-0019 was written about: a field that escapes
// only because the value happens to be empty, on a library struct unifig writes
// whole. A firmware that starts answering with it would put it on the wire and
// 400 every zone update, which is why `writableZone` clears it rather than
// trusting the read to keep being silent.
//
// The three sibling markers are still deliberately absent. Nobody has sent
// `attr_hidden`, `attr_hidden_id` or `attr_no_delete` to this endpoint, and
// ADR-0014's objection to a fixture that asserts a guess still stands — the rule
// that unifig sends no marker back is asserted against the request instead.
var refusedByZoneWrite = []string{
	"attr_no_edit",
	"cloud_template",
	"default_zone",
	"external_id",
	"site_id",
	"zone_key",
}

// refusedByPolicyWrite is the one refusal the policy write endpoint has been
// measured making, on both of the writes that can reach it, and it is not shaped
// like the zone's. The zone endpoint refuses a field it has never heard of, so
// naming the field is enough; this one refuses a *combination* it understands
// perfectly well — asking it to generate the companion return rule for a policy
// that blocks, which there would be no traffic to return.
//
// Measured on the live migrated UDR on 18 August 2026 (issue #36, ADR-0022). An
// apply creating a `block` policy with `create_allow_respond: true` came back
//
//	400: Firewall policy create respond traffic not allowed
//
// and applied none of its two changes. unifig had just started sending the field
// true on every create, so this refused every block and reject policy it could
// have made — which the stand-in of the day could not notice, because it stores
// whatever it is handed. That is the whole reason this is here: ADR-0019 said
// the cover for a payload the Controller would not take has to be the request
// rather than the round-trip, and a stand-in that accepts what hardware refuses
// is a fixture asserting the wrong guess.
//
// Only the measured half is encoded. `block` is what was sent and refused;
// `reject` was never reached, because the apply stopped at the first failure.
// The predicate therefore turns on the Controller's own spelling of the verdict
// being anything but `ALLOW`, which is the shape of the rule the message states
// rather than the list of verdicts anybody watched it apply.
//
// **The second condition is the destination**, and it was measured a week later
// on the same live migrated UDR, off a half-applied firewall rather than a
// probe: an operator's `apply` created three blocking policies and then stopped
// on the first `allow` it came to, `Cyberdelia -> External`, with this identical
// message. Two probes on `Dmz -> External` — a zone holding no networks — closed
// it: the same body carrying `create_allow_respond: true` is refused `400`, and
// carrying it `false` is accepted `201` with no companion generated. So an allow
// into the internet takes the policy and declines the request, and the reverse
// pair is not what decides — a third probe, `Internal -> Dmz`, was accepted and
// generated a companion onto a pair that already carried a `RESPOND_ONLY` rule,
// which is what refuted the reading that the refusal was about the reverse pair
// being occupied (ADR-0030).
//
// The zone is found by the Controller's own `zone_key`, not by the name
// "External", for the reason zoneKeyed gives.
//
// It covers the update as well as the create, and that is a second measurement
// rather than a symmetry. ADR-0022 had only ever seen the update carry the pair
// the Controller accepts — a policy created `block`, so carrying the flag false,
// updated to `allow` — and wrote down that "the update path neither refuses nor
// generates". Issue #37's probe ran the mirror on the live migrated UDR on 19
// August 2026: a policy created `allow`, so carrying it true, updated to `block`
// puts the stored true back beside `BLOCK` and is refused with this same
// message. The endpoint is one endpoint and the rule is about the body, not
// about which verb carried it.
func refusedByPolicyWrite(r *replay, sent map[string]any) string {
	respond, _ := sent["create_allow_respond"].(bool)
	if !respond {
		return ""
	}
	action, _ := sent["action"].(string)
	if action != "ALLOW" {
		return refusedRespondTraffic
	}
	if r.zoneKeyOfLocked(zoneIDOf(sent, "destination")) == "external" {
		return refusedRespondTraffic
	}
	return ""
}

// refusedRespondTraffic is the Controller's own wording, and it is one string
// because it was one string on the wire: the two refusals above are the same
// message, which is what says the endpoint is applying one rule about the
// request rather than two rules about two situations.
const refusedRespondTraffic = "Firewall policy create respond traffic not allowed"

// zoneIDOf reads one end's zone id out of a policy body.
func zoneIDOf(sent map[string]any, end string) string {
	side, _ := sent[end].(map[string]any)
	id, _ := side["zone_id"].(string)
	return id
}

// zoneKeyOfLocked is the Controller's own key for the zone with this id, or
// empty for a zone of the operator's own — which carries no key at all, exactly
// as the live router answers.
//
// Locked, because the only caller is refusedByPolicyWrite and collectionV2 holds
// r.mu for the whole of a write. Taking it again here would deadlock, and taking
// it before the write would be reading zones the same request may be about.
func (r *replay) zoneKeyOfLocked(id string) string {
	for _, zone := range r.zones {
		if zone["_id"] == id {
			key, _ := zone["zone_key"].(string)
			return key
		}
	}
	return ""
}

// startReplay serves the recording at its own base URL for the duration of one
// test. Each test gets its own, so one test's apply cannot become another's
// starting state — the isolation the shared dockerized Controller has to buy
// with per-test names and cleanups.
func startReplay(t *testing.T) *replay {
	t.Helper()

	r := &replay{t: t}
	r.entries = recordedEntries(t, "networkconf.json")
	r.settings = recordedEntries(t, "setting.json")
	r.wlans = recordedBody(t, "wlanconf.json")
	r.forwards = recordedBody(t, "portforward.json")
	r.clients = recordedBody(t, "user.json")
	r.sysinfo = recordedBody(t, "sysinfo.json")
	r.zones = recordedList(t, "firewallzone.json")
	r.policies = recordedList(t, "firewallpolicy.json")

	server := httptest.NewTLSServer(http.HandlerFunc(r.serve))
	t.Cleanup(server.Close)
	r.url = server.URL
	return r
}

// env is the connection config that points unifig at this stand-in instead of
// at the dockerized Controller — the only difference between a WAN test and
// every other test in this suite.
func (r *replay) env() map[string]string {
	return map[string]string{"UNIFIG_HOST": r.url, "UNIFIG_API_KEY": replayAPIKey}
}

func (r *replay) serve(w http.ResponseWriter, req *http.Request) {
	// The SDK's new-style API detection probe: a 200 here is what tells it to
	// talk to /proxy/network, as it does to a UDR.
	if req.URL.Path == "/" {
		w.WriteHeader(http.StatusOK)
		return
	}
	if req.Header.Get("X-Api-Key") != replayAPIKey {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":401,"message":"Unauthorized"}}`))
		return
	}

	switch {
	case req.Method == http.MethodGet && req.URL.Path == sysinfoPath:
		r.write(w, r.sysinfo)
	case req.Method == http.MethodGet && req.URL.Path == wlanconfPath:
		r.write(w, r.wlans)
	case req.Method == http.MethodGet && req.URL.Path == portforwardPath:
		r.write(w, r.forwards)
	case req.Method == http.MethodGet && req.URL.Path == userPath:
		r.write(w, r.clients)
	case req.Method == http.MethodGet && req.URL.Path == networkconfPath:
		r.write(w, r.collection())
	case req.Method == http.MethodPut && strings.HasPrefix(req.URL.Path, networkconfPath+"/"):
		r.update(w, req, strings.TrimPrefix(req.URL.Path, networkconfPath+"/"))
	case req.Method == http.MethodPost && req.URL.Path == networkconfPath:
		r.create(w, req)
	case req.Method == http.MethodGet && req.URL.Path == settingPath:
		r.write(w, r.recordedSettings())
	case req.Method == http.MethodPut && req.URL.Path == setDoHPath:
		r.setDoH(w, req)
	case req.URL.Path == zonePath || strings.HasPrefix(req.URL.Path, zonePath+"/"):
		r.collectionV2(w, req, zonePath, &r.zones, zoneWriteContract)
	case req.Method == http.MethodPut && req.URL.Path == reorderPath:
		r.reorder(w, req)
	case req.URL.Path == policyPath || strings.HasPrefix(req.URL.Path, policyPath+"/"):
		// No field is refused here, and that empty list is now a reading rather
		// than an absence. Issue #37 put a policy back to the live migrated UDR
		// carrying every field this DTO had never been watched accept —
		// `origin_id`, `origin_type`, `icmp_typename`, `icmp_v6_typename`,
		// `hits`, `last_hit` — and got 200. It refuses none of them; it stores
		// four of them nowhere, which is a thing a round-trip test would see and
		// not a payload this stand-in has to cover for. The zone's list is
		// populated because that DTO really does refuse what it has not heard of
		// (ADR-0019), and the difference between the two collections is measured
		// on both sides now rather than assumed on either.
		//
		// What this endpoint refuses is a combination, and that is the predicate
		// beside the list.
		r.collectionV2(w, req, policyPath, &r.policies, policyWriteContract)
	default:
		r.t.Errorf("unifig asked the Controller for something the recording does not have: %s %s",
			req.Method, req.URL.Path)
		http.NotFound(w, req)
	}
}

// update stores what unifig sent, the way the Controller does: the fields the
// body carries are written onto the entry, and every field it leaves out is
// left exactly as it was.
//
// It merges because that is what was measured. A v1 PUT carrying an ID and one
// field changes that field and keeps the rest — asked of a real dockerized
// Controller by TestAV1PutOnANetworkKeepsTheFieldsTheBodyLeavesOut, and read
// back off all four collections unifig updates by the apply-side tests beside
// it (ADR-0023). It used to replace, which was a fixture asserting a guess —
// the thing ADR-0014 objects to — and one that cost nothing only while unifig
// happened to send the whole object back anyway.
//
// The WAN slots are the rows this could not be asked about directly, because a
// container has no WAN entries at all (ADR-0008). What stands in for that is
// not a guess either: a slot is a networkconf entry, so the endpoint measured
// above is this endpoint, and the reading across is between two rows of one
// collection rather than between two endpoints. The v2 firewall tree in this
// same file replaces, and that difference is measured too (ADR-0021) — which is
// why neither is inferred from the other.
func (r *replay) update(w http.ResponseWriter, req *http.Request, id string) {
	var sent map[string]any
	if err := json.NewDecoder(req.Body).Decode(&sent); err != nil {
		r.t.Errorf("unifig sent a networkconf update that is not JSON: %v", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for i, entry := range r.entries {
		if entry["_id"] != id {
			continue
		}
		merged := maps.Clone(entry)
		maps.Copy(merged, sent)
		merged["_id"] = id
		r.entries[i] = merged
		r.write(w, data(merged))
		return
	}

	r.t.Errorf("unifig updated a networkconf entry the Controller does not have: %s", id)
	http.Error(w, "not found", http.StatusNotFound)
}

// create adds a networkconf entry the way the Controller does, handing out an
// ID as it goes.
//
// The WAN suite never reaches this — a slot is a Setting and nothing creates one
// — and it is here for the firewall suite, where a zone holding a network the
// same file declares is the case worth testing: the network's ID does not exist
// when the plan is made, so the zone's write has to read it at the moment it
// runs. Without a create here there would be no new ID to read.
func (r *replay) create(w http.ResponseWriter, req *http.Request) {
	var sent map[string]any
	if err := json.NewDecoder(req.Body).Decode(&sent); err != nil {
		r.t.Errorf("unifig sent a networkconf create that is not JSON: %v", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.issued++
	sent["_id"] = fmt.Sprintf("6613a1f0c4b2d90a5e1f6%03d", r.issued)
	sent["site_id"] = "6613a1f0c4b2d90a5e1f0000"
	r.entries = append(r.entries, sent)
	r.write(w, data(sent))
}

func (r *replay) collection() json.RawMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return data(r.entries...)
}

func (r *replay) recordedSettings() json.RawMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return data(r.settings...)
}

// setDoH stores the Encrypted DNS setting unifig sent, the way the Controller
// does: the setting is replaced by the body rather than merged with it, which
// is what makes "unifig writes the whole setting back, and only its own fields
// have changed" a thing this suite can check.
//
// Replacing is also the assumption underneath clearing a list. `servers: []`
// reaches the Controller as a request with no custom_servers in it at all, and
// only a Controller that replaces the document reads that as "none". That is
// stated here because it is the one thing about this endpoint the recording
// cannot answer (ADR-0012).
func (r *replay) setDoH(w http.ResponseWriter, req *http.Request) {
	var sent map[string]any
	if err := json.NewDecoder(req.Body).Decode(&sent); err != nil {
		r.t.Errorf("unifig sent an Encrypted DNS update that is not JSON: %v", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for i, setting := range r.settings {
		if setting["key"] != dohKey {
			continue
		}
		sent["key"] = dohKey
		r.settings[i] = sent
		r.write(w, data(sent))
		return
	}

	r.t.Errorf("unifig wrote an Encrypted DNS setting to a Controller that does not have one")
	http.Error(w, "not found", http.StatusNotFound)
}

// collectionV2 serves one of the Controller's v2 collections — the firewall
// zones or the firewall policies — with the whole lifecycle unifig uses on them:
// list, create, update, delete.
//
// It is the first stand-in here that creates and deletes rather than only
// updating, because zones and policies are the first Resources tested this way
// rather than Settings. That difference is the whole point of these tests: an ID
// is handed out on create, and it has to be one a policy created later in the
// same apply can be pointed at, which is exactly what unifig's dependency
// ordering exists to arrange.
//
// Responses are bare, with no {meta, data} envelope, because that is what the
// Controller's v2 API answers with.
//
// contract is what this collection's write endpoint refuses and what its PUT
// does with a field the body leaves out — see writeContract, and the two values
// beside it. Every write is kept whether it was refused or not, because what a
// test about the request needs is what unifig sent rather than what survived.
func (r *replay) collectionV2(
	w http.ResponseWriter,
	req *http.Request,
	base string,
	held *[]map[string]any,
	contract writeContract,
) {
	id := strings.TrimPrefix(strings.TrimPrefix(req.URL.Path, base), "/")

	r.mu.Lock()
	defer r.mu.Unlock()

	switch {
	case req.Method == http.MethodGet && id == "":
		r.writeJSON(w, *held)

	case req.Method == http.MethodPost && id == "":
		var sent map[string]any
		if !r.decode(w, req, &sent, base) {
			return
		}
		// Cloned, because the stand-in stamps `_id` and `site_id` into `sent`
		// below: a record that aliased it would be the stored object wearing the
		// request's name, and the fields it gained would be unfalsifiable.
		r.written = append(r.written, sentRequest{path: req.URL.Path, body: maps.Clone(sent)})
		if contract.refused(r, w, sent) {
			return
		}
		r.issued++
		sent["_id"] = fmt.Sprintf("6613a1f0c4b2d90a5e1f9%03d", r.issued)
		sent["site_id"] = "6613a1f0c4b2d90a5e1f0000"
		if contract.assignsIndex {
			sent["index"] = r.nextStoredIndexLocked(sent)
		}
		*held = append(*held, sent)
		if contract.companions {
			r.reconcileCompanion(held, sent)
		}
		r.writeJSON(w, sent)

	case req.Method == http.MethodPut && id != "":
		var sent map[string]any
		if !r.decode(w, req, &sent, base) {
			return
		}
		// Cloned, because the stand-in stamps `_id` and `site_id` into `sent`
		// below: a record that aliased it would be the stored object wearing the
		// request's name, and the fields it gained would be unfalsifiable.
		r.written = append(r.written, sentRequest{path: req.URL.Path, body: maps.Clone(sent)})
		if contract.unresolvable(r, w, id) {
			return
		}
		if contract.refused(r, w, sent) {
			return
		}
		for i, entry := range *held {
			if entry["_id"] != id {
				continue
			}
			// Replace or merge as the endpoint itself was measured behaving.
			// Both answers are measurements now, taken a day apart on the same
			// live migrated UDR, and they disagree: an apply that changed one
			// policy's verdict reverted the ICMP type an operator had narrowed
			// it to in the same request (ADR-0021, issue #35), while a mutating
			// PUT to a custom zone left the `external_id` it did not carry
			// exactly where it was (ADR-0024, issue #38).
			//
			// The zone's half used to be a deliberate over-strictness, on the
			// argument that a stand-in which merged would store what unifig
			// failed to send and read it back as a success. That argument does
			// not survive the measurement, and it was pointing at the wrong
			// risk: what keeps a zone honest is refusedByZoneWrite, which now
			// answers 400 to every field this DTO does not take, so there is no
			// payload a merge here could quietly launder.
			sent["_id"] = id
			if contract.semantics == replaces {
				(*held)[i] = sent
			} else {
				for field, value := range sent {
					(*held)[i][field] = value
				}
			}
			stored := (*held)[i]
			if contract.companions {
				r.reconcileCompanion(held, stored)
			}
			r.writeJSON(w, stored)
			return
		}
		r.t.Errorf("unifig updated something at %s the Controller does not have: %s", base, id)
		http.NotFound(w, req)

	case req.Method == http.MethodDelete && id != "":
		if contract.unresolvable(r, w, id) {
			return
		}
		for i, entry := range *held {
			if entry["_id"] != id {
				continue
			}
			name, _ := entry["name"].(string)
			*held = append((*held)[:i], (*held)[i+1:]...)
			// The Controller reclaims the companion with its parent, measured by
			// deleting one and watching the site return to its baseline id for
			// id (ADR-0022). Without this a prune would leave orphans no test
			// could see and no unifig could clean up.
			if contract.companions {
				r.dropCompanion(held, name)
			}
			r.writeJSON(w, map[string]any{})
			return
		}
		r.t.Errorf("unifig deleted something at %s the Controller does not have: %s", base, id)
		http.NotFound(w, req)

	default:
		r.t.Errorf("unifig made a request the recording does not have: %s %s", req.Method, req.URL.Path)
		http.NotFound(w, req)
	}
}

// unresolvable answers the way the Controller does when the id in the path is
// not one it can resolve — and reports whether it answered, so a caller stops.
//
// The Controller addresses an object it stores by a document handle: twenty-four
// hex characters. It also *lists* objects it never stored, whose `_id` is a
// description of where they came from rather than a handle — a policy it
// generates for a pair of zones is `source zone + destination zone + index`
// concatenated — and it resolves none of those. Measured on the live migrated
// UDR on 19 August 2026: eighty-six of eighty-six shipped policies carried such
// an id and every one answered 404 `api.err.FirewallPolicyNotFound` on GET and
// on PUT (ADR-0027, issue #41).
//
// **This is the half of that measurement a recording could not carry.** The
// scrub used to map every `_id` through a twenty-four character placeholder, so
// the recording held no composite id at all and the stand-in had never been
// handed one. Every policy in the suite was addressable and every update passed
// — ADR-0019's rule (a stand-in that accepts what hardware refuses is a fixture
// asserting the wrong guess) arriving through the recording rather than through
// the stand-in.
//
// It runs **before** the body is looked at, which is also measured rather than
// assumed. The same probe sent one body the Controller refuses to three ids: a
// real handle answered 400 naming the refusal, an absent handle answered 404, and
// a composite answered 404. So the lookup happens first, and a 404 here is the id
// rather than the payload.
//
// **It hangs off the contract because the answer is one endpoint's.** The code
// and the message are the firewall policy collection's own, measured there; the
// zone collection shares this function and has never been asked the question, so
// its contract names no code and this does nothing for it. A stand-in answering a
// zone with `api.err.FirewallPolicyNotFound` would be inventing a reading, which
// is the defect ADR-0019 is about, in miniature and on the error path.
//
// **GET and PUT were measured; DELETE is inferred**, and the inference is named
// rather than hidden: the three verbs share one `{id}` path segment, the 404 body
// is a lookup failure by its own wording, and the calibration showed the lookup
// runs before anything else. Nobody sent a DELETE to a composite id, because the
// success branch of that request would have deleted one of the Controller's own
// policies. Prune spares a generated policy twice over — on its `predefined`
// marker and on the id itself (ADR-0028) — so unifig has no path to that request,
// and this is what says so out loud when one of the two stops holding.
//
// It fails the test as well as answering, because unifig must never send one:
// the plan holds back a change to a Generated Policy rather than promising it
// (ADR-0027). A PUT arriving here is that hold-back having regressed, and the
// point of the stand-in is that it says so out loud.
func (c writeContract) unresolvable(r *replay, w http.ResponseWriter, id string) bool {
	if c.notFoundCode == "" || isDocumentHandle(id) {
		return false
	}
	r.t.Errorf("unifig wrote to %q, which is not an id the Controller resolves: "+
		"a policy it generates for a pair of zones has no document handle and cannot be written to", id)
	body, err := json.Marshal(map[string]any{
		"code":      c.notFoundCode,
		"details":   map[string]any{"_id": id},
		"errorCode": 404,
		"message":   "Firewall policy not found",
	})
	if err != nil {
		r.t.Errorf("encoding the Controller's 404: %v", err)
		return true
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	if _, err := w.Write(body); err != nil {
		r.t.Errorf("writing the Controller's 404: %v", err)
	}
	return true
}

// isDocumentHandle is the shape of an id the Controller resolves, stated here
// rather than imported because the stand-in is a Controller rather than a reader
// of unifig's opinions about one.
func isDocumentHandle(id string) bool {
	if len(id) != 24 {
		return false
	}
	for _, c := range id {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

// unrecognisedField answers the way the Controller does when a body carries a
// field its write DTO has never heard of: a 400 whose message names the field,
// which is what unifig then puts in front of the operator. It reports whether
// it answered.
//
// The message is the one a real UDR returned, obfuscated class name and all
// (ADR-0019). Nothing asserts on the text — what it is here for is that a
// refused write fails an apply exactly as it fails one in the field, rather
// than being stored and read back as a success.
func (r *replay) unrecognisedField(w http.ResponseWriter, sent map[string]any, refuses []string) bool {
	for _, field := range refuses {
		if _, carried := sent[field]; !carried {
			continue
		}
		body, err := json.Marshal(map[string]any{"message": fmt.Sprintf(
			"JSON parse error: Unrecognized field %q (class com.ubnt.g.c.t.AWSXjrFfvsFZsv), not marked as ignorable",
			field)})
		if err != nil {
			r.t.Errorf("encoding the Controller's refusal: %v", err)
			return true
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		if _, err := w.Write(body); err != nil {
			r.t.Errorf("writing the Controller's refusal: %v", err)
		}
		return true
	}
	return false
}

// refuse answers a write the way the Controller answers one it will not make: a
// 400 carrying the Controller's own message, which is what an operator sees.
func (r *replay) refuse(w http.ResponseWriter, message string) {
	body, err := json.Marshal(map[string]any{"message": message})
	if err != nil {
		r.t.Errorf("encoding the Controller's refusal: %v", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	if _, err := w.Write(body); err != nil {
		r.t.Errorf("writing the Controller's refusal: %v", err)
	}
}

func (r *replay) decode(w http.ResponseWriter, req *http.Request, into any, base string) bool {
	if err := json.NewDecoder(req.Body).Decode(into); err != nil {
		r.t.Errorf("unifig sent a %s request body that is not JSON: %v", base, err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return false
	}
	return true
}

func (r *replay) writeJSON(w http.ResponseWriter, body any) {
	encoded, err := json.Marshal(body)
	if err != nil {
		r.t.Errorf("encoding a recorded response: %v", err)
		return
	}
	r.write(w, encoded)
}

func (r *replay) write(w http.ResponseWriter, body json.RawMessage) {
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(body); err != nil {
		r.t.Errorf("writing the recorded response: %v", err)
	}
}

// seedVersion makes the recorded Controller answer that it runs another
// version, which is how a test reaches a Controller the compatibility matrix
// does not carry. There is no other way to get one: the matrix is the set of
// versions CI can boot, so an untested version is by definition not among them.
func (r *replay) seedVersion(t *testing.T, version string) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()

	answered := systemInformation(t, r.sysinfo)
	answered["version"] = version

	encoded, err := json.Marshal(map[string]any{
		"meta": map[string]any{"rc": "ok"},
		"data": []map[string]any{answered},
	})
	if err != nil {
		t.Fatalf("re-encoding the recorded system information: %v", err)
	}
	r.sysinfo = encoded
}

// systemInformation is the one entry stat/sysinfo answers with. The caller
// holds the lock.
func systemInformation(t *testing.T, recorded json.RawMessage) map[string]any {
	t.Helper()
	var answered struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(recorded, &answered); err != nil {
		t.Fatalf("reading the recorded system information: %v", err)
	}
	if len(answered.Data) == 0 {
		t.Fatal("the recording holds no system information, so it does not say which Controller it came from")
	}
	return answered.Data[0]
}

// slotNames names the uplinks the recording holds, in the order the Controller
// returns them — the names, where slot returns the entry occupying one.
//
// This is what keeps a WAN test from being about which router the recording
// came from. Which slots exist is the router's answer — one gateway has WAN and
// WAN2, another has WAN and a cellular backup — so a test that named one would
// fail on a re-recording for a reason that has nothing to do with unifig. It
// asks instead, and seeds what it needs onto whatever came back.
func (r *replay) slotNames(t *testing.T) []string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()

	var slots []string
	for _, entry := range r.entries {
		if entry["purpose"] != "wan" {
			continue
		}
		if slot, ok := entry["wan_networkgroup"].(string); ok && slot != "" {
			slots = append(slots, slot)
		}
	}
	return slots
}

// aSlot is the first uplink the recording holds — the one a test seeds when
// what it states is true of any slot, which is most of them.
//
// Which uplink it turns out to be does not matter: these tests are about what
// unifig does to a slot, not about which slot it is. What matters is that a
// recording from any router holds one, so nothing here has to know whether this
// router's second uplink is WAN2, a cellular backup, or absent. A recording
// with no WAN entry at all is one this suite can say nothing with, and it says
// so here rather than four assertions later.
func (r *replay) aSlot(t *testing.T) string {
	t.Helper()
	slots := r.slotNames(t)
	if len(slots) == 0 {
		t.Fatalf("the recording holds no WAN slots, so there is no uplink to test against")
	}
	return slots[0]
}

// absentSlot names a slot the recording does not hold — what a test needs in
// order to state that unifig refuses to create one.
//
// The candidates mirror the slot pattern in schema/unifig.schema.json, so what
// comes back is a name a config file can legally carry — a slot outside that
// pattern would fail validation and this test would never reach the answer it
// is about. Which of them is free is checked against the recording rather than
// assumed: a recording that already has WAN2 would make a hardcoded WAN2 a test
// about nothing. If the schema ever narrows, this list goes stale loudly, as a
// config the validator rejects.
func (r *replay) absentSlot(t *testing.T) string {
	t.Helper()

	held := r.slotNames(t)
	var candidates []string
	for n := 2; n <= 9; n++ {
		candidates = append(candidates, fmt.Sprintf("WAN%d", n))
	}
	candidates = append(candidates, "WAN_LTE_FAILOVER")
	for _, candidate := range candidates {
		if !slices.Contains(held, candidate) {
			return candidate
		}
	}

	t.Fatalf("the recording holds every slot the schema allows (%v), so none is left to be absent", held)
	return ""
}

// slotPassword is the PPPoE password the recording holds for a slot — what a
// test setting the environment for an exported config has to put back, without
// spelling out a value only the committed recording supplies.
func (r *replay) slotPassword(t *testing.T, slot string) string {
	t.Helper()
	password, _ := r.slot(t, slot)["x_wan_password"].(string)
	return password
}

// stampFor is the DNS stamp the Controller holds for a custom resolver — what a
// test setting the environment for an exported config has to put back, without
// spelling out a value only the Controller supplies.
func (r *replay) stampFor(t *testing.T, name string) string {
	t.Helper()
	for _, server := range r.dohServers(t) {
		if server["server_name"] == name {
			stamp, _ := server["sdns_stamp"].(string)
			return stamp
		}
	}
	t.Fatalf("the Controller holds no custom DNS server named %q", name)
	return ""
}

// wlanPassphrase is the same thing for a WLAN. An export redacts every secret
// it writes, WLAN passphrases included, so a WAN test that plans an exported
// config needs these whether or not it is about WLANs.
func (r *replay) wlanPassphrase(t *testing.T, name string) string {
	t.Helper()
	for _, wlan := range entries(t, r.wlans) {
		if wlan["name"] == name {
			passphrase, _ := wlan["x_passphrase"].(string)
			return passphrase
		}
	}
	t.Fatalf("the recording has no WLAN named %q", name)
	return ""
}

// managedNetworkNames names the networks unifig manages, as opposed to the WAN
// slots that share the collection with them — what a WAN test needs in order to
// write a config that leaves the networks entirely settled, so that a prune has
// nothing at stake but the uplinks.
//
// It is rig.managedNetworkNames against the recording instead of against the
// live Controller, down to the name, and the purposes are spelled out here for
// the same reason that one spells them out: the stand-in describes the
// Controller rather than agreeing with the code under test by construction.
func (r *replay) managedNetworkNames(t *testing.T) []string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()

	var names []string
	for _, entry := range r.entries {
		switch entry["purpose"] {
		case "corporate", "guest", "vlan-only":
		default:
			continue
		}
		if name, ok := entry["name"].(string); ok && name != "" {
			names = append(names, name)
		}
	}
	return names
}

// aNetwork is one of them — what a WAN test needs when it has to put a change
// that is not the uplink in the same file.
func (r *replay) aNetwork(t *testing.T) string {
	t.Helper()
	names := r.managedNetworkNames(t)
	if len(names) == 0 {
		t.Fatalf("the recording holds no network unifig manages, so there is nothing in it to change but the uplinks")
	}
	return names[0]
}

// unusedVLAN is a tag no network in the recording is on, so that a config
// putting a LAN on it is a change whatever the recording holds. Where it starts
// counting is arbitrary; that it counts past what is taken is not.
func (r *replay) unusedVLAN(t *testing.T) int {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()

	taken := map[int]bool{}
	for _, entry := range r.entries {
		if vlan, ok := entry["vlan"].(float64); ok {
			taken[int(vlan)] = true
		}
	}
	// Any free tag will do, so this counts up from clear of the low numbers a
	// home router hands out, and stops at the largest the schema accepts.
	for vlan := 40; vlan <= 4094; vlan++ {
		if !taken[vlan] {
			return vlan
		}
	}

	t.Fatalf("the recording holds a network on every VLAN tag the schema allows")
	return 0
}

// slot is the WAN entry occupying a slot, as the stand-in holds it now — how a
// WAN test checks what an apply actually wrote, the way rig.liveNetwork does
// against the real Controller.
func (r *replay) slot(t *testing.T, slot string) map[string]any {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, entry := range r.entries {
		if entry["purpose"] == "wan" && entry["wan_networkgroup"] == slot {
			return entry
		}
	}
	t.Fatalf("the recording has no %s slot", slot)
	return nil
}

// asStored is a test's fields as a Controller would be holding them: the map
// round-tripped through JSON, so a number seeded as a Go `int` is read back as
// the `float64` any JSON decoder produces.
//
// The rig gets this for nothing, because its seeds go to a real Controller over
// HTTP and come back decoded. The stand-in's seeds are handed to it in memory,
// so without this a test could read back a type no Controller can send — and
// pass or fail on the difference between two ways of writing 911.
func asStored(t *testing.T, fields map[string]any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("seeding the stand-in with %v: %v", fields, err)
	}
	var stored map[string]any
	if err := json.Unmarshal(encoded, &stored); err != nil {
		t.Fatalf("seeding the stand-in with %v: %v", fields, err)
	}
	return stored
}

// seedSlot sets fields on a WAN slot without going through unifig — the
// stand-in's version of the rig seeding the Controller through its own API, and
// the reason these tests do not depend on which values the recording happens to
// hold.
func (r *replay) seedSlot(t *testing.T, slot string, fields map[string]any) {
	t.Helper()
	entry := r.slot(t, slot)
	stored := asStored(t, fields)

	r.mu.Lock()
	defer r.mu.Unlock()
	maps.Copy(entry, stored)
}

// addSlot puts another uplink on the Controller, which is how a test states
// that a router may have slots unifig has never heard of — the recording is one
// router's answer, not the set of routers that exist.
func (r *replay) addSlot(t *testing.T, slot string, fields map[string]any) {
	t.Helper()
	stored := asStored(t, fields)

	r.mu.Lock()
	defer r.mu.Unlock()

	entry := map[string]any{
		"_id":              fmt.Sprintf("6613a1f0c4b2d90a5e1f9%03d", len(r.entries)),
		"name":             slot,
		"purpose":          "wan",
		"enabled":          true,
		"wan_networkgroup": slot,
	}
	maps.Copy(entry, stored)
	r.entries = append(r.entries, entry)
}

// doh is the Encrypted DNS setting as the stand-in holds it now — how a DNS
// test checks what an apply actually wrote, the way replay.slot does for an
// uplink.
func (r *replay) doh(t *testing.T) map[string]any {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, setting := range r.settings {
		if setting["key"] == dohKey {
			return setting
		}
	}
	t.Fatalf("the recording holds no Encrypted DNS setting")
	return nil
}

// dohServers is the custom resolvers on it, in the order the Controller holds
// them.
func (r *replay) dohServers(t *testing.T) []map[string]any {
	t.Helper()
	raw, _ := r.doh(t)["custom_servers"].([]any)

	servers := make([]map[string]any, 0, len(raw))
	for _, server := range raw {
		entry, ok := server.(map[string]any)
		if !ok {
			t.Fatalf("the Controller holds a custom DNS server that is not an object: %T", server)
		}
		servers = append(servers, entry)
	}
	return servers
}

// seedDoH puts the Encrypted DNS setting into the state a test starts from,
// without going through unifig — the stand-in's version of the rig seeding the
// Controller through its own API.
//
// Every DNS test seeds, for the same reason every WAN test does: what the
// recording happens to hold is one router's answer, and a test that depended on
// it would fail on a re-recording for a reason that has nothing to do with
// unifig.
func (r *replay) seedDoH(t *testing.T, fields map[string]any) {
	t.Helper()
	setting := r.doh(t)

	r.mu.Lock()
	defer r.mu.Unlock()
	for name, value := range fields {
		setting[name] = value
	}
}

// withoutEncryptedDNS takes the setting off the Controller altogether, which is
// how a test states what unifig does against a Network version that predates
// encrypted DNS: it cannot create a Setting, so it says so.
func (r *replay) withoutEncryptedDNS(t *testing.T) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()

	r.settings = slices.DeleteFunc(r.settings, func(setting map[string]any) bool {
		return setting["key"] == dohKey
	})
}

// liveZones is the zones as the stand-in holds them now — how a firewall test
// checks what an apply actually did, the way replay.slot does for an uplink.
func (r *replay) liveZones(t *testing.T) []map[string]any {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.zones)
}

func (r *replay) livePolicies(t *testing.T) []map[string]any {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.policies)
}

// zoneNamed is one zone by the name unifig matches it by, and fails the test
// when the Controller does not hold exactly one — the stand-in's counterpart of
// rig.liveWLAN.
func (r *replay) zoneNamed(t *testing.T, name string) map[string]any {
	t.Helper()
	var found []map[string]any
	for _, zone := range r.liveZones(t) {
		if zone["name"] == name {
			found = append(found, zone)
		}
	}
	if len(found) != 1 {
		t.Fatalf("the Controller has %d zones named %q, want exactly 1", len(found), name)
	}
	return found[0]
}

func (r *replay) policyNamed(t *testing.T, name string) map[string]any {
	t.Helper()
	var found []map[string]any
	for _, policy := range r.livePolicies(t) {
		if policy["name"] == name {
			found = append(found, policy)
		}
	}
	if len(found) != 1 {
		t.Fatalf("the Controller has %d firewall policies named %q, want exactly 1", len(found), name)
	}
	return found[0]
}

// hasPolicyNamed is whether the site holds a policy by this name at all — the
// question policyNamed cannot answer, because it fails the test when the answer
// is none. What it is for is the companion return rule, which a test asks after
// rather than before.
func (r *replay) hasPolicyNamed(t *testing.T, name string) bool {
	t.Helper()
	for _, policy := range r.livePolicies(t) {
		if policy["name"] == name {
			return true
		}
	}
	return false
}

// zoneMembers names the networks a zone holds, translated out of the Controller
// IDs it stores them as.
//
// A member the recording has no network for comes back as its raw ID rather than
// being dropped, because the tests that use this are about what unifig leaves
// alone: a zone whose WAN membership silently vanished from this list would look
// exactly like one unifig correctly preserved.
func (r *replay) zoneMembers(t *testing.T, name string) []string {
	t.Helper()
	ids, _ := r.zoneNamed(t, name)["network_ids"].([]any)

	r.mu.Lock()
	defer r.mu.Unlock()
	names := make([]string, 0, len(ids))
	for _, raw := range ids {
		id, _ := raw.(string)
		named := id
		for _, entry := range r.entries {
			if entry["_id"] == id {
				if name, ok := entry["name"].(string); ok {
					named = name
				}
			}
		}
		names = append(names, named)
	}
	slices.Sort(names)
	return names
}

// zoneWrites is every body unifig has sent to the zone collection, and
// policyWrites the same for the policies — the requests themselves rather than
// what the stand-in stored, which is the only way to state anything about a
// field the Controller would have refused.
func (r *replay) zoneWrites(t *testing.T) []map[string]any {
	t.Helper()
	return r.writesTo(zonePath)
}

// reorderWrites is every batch-reorder body unifig has sent. It is separate from
// policyWrites because a reorder is not a write to the policy collection: it
// carries ids rather than an object, and it goes to its own endpoint.
func (r *replay) reorderWrites(t *testing.T) []map[string]any {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.reordered)
}

func (r *replay) policyWrites(t *testing.T) []map[string]any {
	t.Helper()
	return r.writesTo(policyPath)
}

// onlyZoneWrite and onlyPolicyWrite are the one body unifig sent to each
// collection, for the request-shape tests whose config asks for exactly one
// change to one Resource.
//
// The count is checked rather than the last write taken, because these tests are
// about what unifig put on the wire: a second write nobody expected is a
// different apply than the one being described, and reading field names out of
// whichever body happened to be last would report on it as though it were.
// theOnlyWriteTo is where that check lives, so the two cannot drift into
// disagreeing about what "only" means.
func (r *replay) onlyZoneWrite(t *testing.T) map[string]any {
	t.Helper()
	return theOnlyWriteTo(t, r.zoneWrites(t), "zone")
}

func (r *replay) onlyPolicyWrite(t *testing.T) map[string]any {
	t.Helper()
	return theOnlyWriteTo(t, r.policyWrites(t), "policy")
}

func theOnlyWriteTo(t *testing.T, writes []map[string]any, collection string) map[string]any {
	t.Helper()
	if len(writes) != 1 {
		t.Fatalf("unifig made %d writes to the %s collection, want the one this config asks for: %v",
			len(writes), collection, writes)
	}
	return writes[0]
}

func (r *replay) writesTo(base string) []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()

	var bodies []map[string]any
	for _, write := range r.written {
		if strings.HasPrefix(write.path, base) {
			bodies = append(bodies, write.body)
		}
	}
	return bodies
}

// markedZone names a zone the Controller marks `attr_no_edit` — the marker it
// puts on some of its own zones and refuses to be told about (ADR-0019).
//
// Which zone it turns out to be does not matter, and is asked rather than named
// for the reason gatewayZone is: a test naming `Vpn` would fail on a recording
// from a router that marks a different three, for a reason that has nothing to
// do with unifig.
func (r *replay) markedZone(t *testing.T) string {
	t.Helper()
	for _, zone := range r.liveZones(t) {
		if marked, _ := zone["attr_no_edit"].(bool); marked {
			name, _ := zone["name"].(string)
			return name
		}
	}
	t.Fatalf("the recording marks no zone attr_no_edit, so there is no marked zone here to write to")
	return ""
}

// seedZone puts a zone on the Controller without going through unifig — the
// stand-in's version of the rig seeding through the Controller's own API.
//
// The networks it names are taken out of whichever zone held them, because a
// network belongs to exactly one zone and the Controller keeps it that way
// itself (ADR-0020). A seed that skipped that would start its test from a site
// no Controller could be in, which is the state this recording used to ship
// (issue #32).
//
// That is not the stand-in reproducing the eviction, and the difference matters:
// a write arriving here is still stored exactly as unifig sent it, so a test
// that applies a membership change and reads it back still learns nothing about
// what the Controller would have done to the other zone. What a plan *says*
// about that is asserted on the plan.
func (r *replay) seedZone(t *testing.T, name string, networks []string, fields map[string]any) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()

	ids := make([]string, 0, len(networks))
	for _, wanted := range networks {
		for _, entry := range r.entries {
			if entry["name"] == wanted {
				id, _ := entry["_id"].(string)
				ids = append(ids, id)
			}
		}
	}
	for _, zone := range r.zones {
		held, _ := zone["network_ids"].([]any)
		kept := make([]any, 0, len(held))
		for _, raw := range held {
			id, _ := raw.(string)
			if !slices.Contains(ids, id) {
				kept = append(kept, raw)
			}
		}
		zone["network_ids"] = kept
	}

	r.issued++
	zone := map[string]any{
		"_id":         fmt.Sprintf("6613a1f0c4b2d90a5e1f8%03d", r.issued),
		"name":        name,
		"network_ids": ids,
		"site_id":     "6613a1f0c4b2d90a5e1f0000",
	}
	for field, value := range fields {
		zone[field] = value
	}
	r.zones = append(r.zones, zone)
}

// zoneKeyed is the name the recording gives the zone carrying one of the
// Controller's own stable keys, found the way unifig finds it: by the key rather
// than by the name. A test that hard-coded "Gateway" or "Internal" would be
// asserting a guess about someone else's product, which is the mistake ADR-0013
// was written about.
func (r *replay) zoneKeyed(t *testing.T, key string) string {
	t.Helper()
	for _, zone := range r.liveZones(t) {
		if zone["zone_key"] == key {
			name, _ := zone["name"].(string)
			return name
		}
	}
	t.Fatalf("the recording holds no zone the Controller keys %q", key)
	return ""
}

// gatewayZone is the zone the Controller answers in — where a policy blocking
// traffic can cut the path the site is managed over (ADR-0018).
func (r *replay) gatewayZone(t *testing.T) string {
	t.Helper()
	return r.zoneKeyed(t, "gateway")
}

// internalZone is the zone the Controller puts a network in when nothing else
// holds it. A network belongs to exactly one zone, so taking one out of a zone
// does not leave it in none — and which zone it lands in is the Controller's
// answer rather than a name unifig keeps (ADR-0020).
func (r *replay) internalZone(t *testing.T) string {
	t.Helper()
	return r.zoneKeyed(t, "internal")
}

// externalZone is the zone the Controller stands the internet up as. A policy
// allowing traffic into it is one the Controller will not generate a companion
// return rule for (ADR-0030).
func (r *replay) externalZone(t *testing.T) string {
	t.Helper()
	return r.zoneKeyed(t, "external")
}

// renameZoneKeyed gives a keyed zone a different name, leaving the key that says
// what it is untouched. It is how a test asks whether unifig found the zone by
// the key or by the name, and there is no other way to tell the two apart.
func (r *replay) renameZoneKeyed(t *testing.T, key, name string) {
	t.Helper()
	was := r.zoneKeyed(t, key)
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, zone := range r.zones {
		if zone["name"] == was {
			zone["name"] = name
			return
		}
	}
}

func (r *replay) renameGateway(t *testing.T, name string) {
	t.Helper()
	r.renameZoneKeyed(t, "gateway", name)
}

// hideTheGateway drops the key that says which zone the Controller answers in,
// leaving a response unifig can read perfectly well and find no gateway in.
//
// That is a different failure from a response it cannot read at all, and the two
// have to be reachable separately: one is a Controller that answered a question
// unifig did not understand, and this one is a Controller that answered it and
// said nothing about a gateway.
func (r *replay) hideTheGateway(t *testing.T) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, zone := range r.zones {
		delete(zone, "zone_key")
	}
}

// seedPolicy is the same for a firewall policy, stated in the zone names a test
// reads rather than in the IDs the Controller stores.
func (r *replay) seedPolicy(t *testing.T, name, action, source, destination string, fields map[string]any) {
	t.Helper()
	zones := map[string]string{}
	for _, zone := range r.liveZones(t) {
		id, _ := zone["_id"].(string)
		named, _ := zone["name"].(string)
		zones[named] = id
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.issued++
	policy := map[string]any{
		"_id":      fmt.Sprintf("6613a1f0c4b2d90a5e1f7%03d", r.issued),
		"name":     name,
		"action":   action,
		"enabled":  true,
		"protocol": "all",
		// The recording's shape: all eighty-six policies a migrated router
		// ships carry `{mode: ALWAYS}` and no `time_all_day` at all. A seed that
		// added the key would be a fixture stating something no policy anyone
		// has read says, and it is exactly the key a Go bool invents.
		"schedule": map[string]any{"mode": "ALWAYS"},
		// True on every policy of both sites anyone has read, whatever its
		// verdict: all eighty-six in the recording, of which thirty-four block.
		// Those were two readings that disagreed on the count until the recording
		// was refreshed for issue #41; they are now one site said twice.
		// The Controller stores the request made at creation rather than a
		// property of the policy, which is what a blocking policy carrying it
		// says (ADR-0022). A seed without it would be a policy neither site has,
		// and the one shape that hides the refusal issue #37 measured on the
		// update path.
		"create_allow_respond": true,
		"source":               map[string]any{"zone_id": zones[source], "matching_target": "ANY"},
		"destination":          map[string]any{"zone_id": zones[destination], "matching_target": "ANY"},
		"site_id":              "6613a1f0c4b2d90a5e1f0000",
	}
	for field, value := range fields {
		// A nil says the Controller sent no such field at all, which is a thing
		// a seed has to be able to state: the recording's policies carry no
		// `time_all_day` and no `description`, and a field's absence is half of
		// what ADR-0021 is about — unifig may not invent one any more than it
		// may drop one.
		if value == nil {
			delete(policy, field)
			continue
		}
		policy[field] = value
	}
	r.policies = append(r.policies, policy)
}

// seedGeneratedPolicy seeds a policy of the Controller's own: one it computes for
// a pair of zones rather than storing, carrying the composite `_id` that shape
// really has and no document handle anywhere on it.
//
// It is a seed rather than something read out of the recording, for the reason
// seedPolicy is: a test that says what unifig does about a Generated Policy
// should state the policy it is about, not depend on the recording happening to
// hold one of the right shape on the right pair. The recording carries eighty-odd
// of them and every one is somebody else's subject.
//
// It is seedPolicy with the `_id` replaced, because that is the only difference
// that matters: everything else about a generated policy is a policy.
func (r *replay) seedGeneratedPolicy(t *testing.T, name, action, source, destination string, index int) {
	t.Helper()
	r.seedPolicy(t, name, action, source, destination, map[string]any{
		// The whole of what makes it generated: not a document handle but a
		// description of where the policy came from.
		"_id":   r.generatedPolicyID(t, source, destination, index),
		"index": index,
		// Along for the ride because the Controller sends both on every one of
		// the eighty-six a migrated router holds, and because prune's exemptions
		// read the first. Neither is what makes it unwritable.
		"predefined": true,
		"origin_id":  fmt.Sprintf("6613a1f0c4b2d90a5e1f8%03d", len(r.livePolicies(t))),
	})
}

// seedStoredPolicy seeds the other half of the clash issue #46 measured: a
// policy of the operator's own, on a pair the Controller already generates one
// for, carrying everything the Controller was watched giving it.
//
// **Every field here is a reading rather than a choice (ADR-0019).** A
// hand-built POST against the live migrated UDR on 20 August 2026 put a policy
// carrying a Generated Policy's own name and pair onto the site, and the
// Controller answered `201` and kept both objects side by side. What it gave the
// new one was `predefined: false`, an ordinary document handle — which seedPolicy
// already issues — and `index: 10000`, which unifig never asked for and which is
// below the whole `30000` band the shipped specific policies sit in, let alone
// the `2147483647` its generated twin carries.
//
// The index is what the precedence in ADR-0029 rests on, so a seed leaving it out
// would be a fixture asserting the clash without the fact that resolves it.
// Nothing reads the field — unifig matches on the id shape, not on the index, for
// the reason that ADR gives — which is exactly why it has to be stated here: the
// measurement is the evidence, and a fixture is where this repository keeps one.
func (r *replay) seedStoredPolicy(t *testing.T, name, action, source, destination string) {
	t.Helper()
	r.seedPolicy(t, name, action, source, destination, map[string]any{
		"index":      storedPolicyIndex,
		"predefined": false,
	})
}

// seedCompanion puts a Return Rule on a pair without going through unifig: the
// companion of an allow somebody already made, which is what makes a block on
// that pair the bug issue #54 measured rather than an ordinary policy.
//
// It carries the composite `_id` of a Generated Policy and the index the
// Controller was measured generating one at, because that is what a companion is
// on every router anyone has read (ADR-0026, ADR-0027).
func (r *replay) seedCompanion(t *testing.T, parent, source, destination string) {
	t.Helper()
	r.seedPolicy(t, parent+" (Return)", "ALLOW", source, destination, map[string]any{
		"_id":                   r.generatedPolicyID(t, source, destination, companionIndex),
		"index":                 companionIndex,
		"predefined":            true,
		"connection_state_type": "RESPOND_ONLY",
	})
}

// nextStoredIndexLocked is the index the Controller gives a policy it has just
// been asked to create, which is the Controller's decision rather than the
// body's.
//
// A create lands **before** the tier the Controller generates its own policies
// into, and successive creates on one pair count up from there: measured on the
// live UDR on 4 September 2026, where two throwaway policies on `Dmz -> Dmz` came
// back at 10000 and 10001. That second value matters more than it looks — it says
// two policies unifig created on one pair were never tied, which an earlier
// reading of this had claimed they were.
//
// It is per pair, like every index the Controller assigns: the composite `_id` of
// a generated policy is the two zone ids and the index run together (ADR-0027),
// and every pair in the recording starts again at 30000.
func (r *replay) nextStoredIndexLocked(sent map[string]any) int {
	source, destination := zoneEndsOf(sent)
	next := storedPolicyIndex
	for _, held := range r.policies {
		if predefined, _ := held["predefined"].(bool); predefined {
			continue
		}
		heldSource, heldDestination := zoneEndsOf(held)
		if heldSource != source || heldDestination != destination {
			continue
		}
		if index, ok := numberIn(held, "index"); ok && index >= next && index < companionIndex {
			next = index + 1
		}
	}
	return next
}

// reorder is `PUT .../firewall-policies/batch-reorder`, the only way a client can
// say where a stored policy sits — measured on the live UDR on 4 September 2026,
// in the session that established `index` is not writable on either verb.
//
// What it was measured doing, on a throwaway pair restored afterwards:
//
//	after_predefined_ids   ->  40000
//	before_predefined_ids  ->  10000, then 10001, …
//
// **And it refuses a partial list.** Naming one of two stored policies on a pair
// answered `400 api.err.ShouldIncludeFirewallPolicyInBatchUpdate`; naming both
// answered 200. That refusal is modelled rather than skipped, because it is the
// whole reason unifig has to read the pair before it can move one policy — a
// stand-in that accepted a partial list would pass a unifig that sent one, and
// the operator would find out when their own unmanaged policy moved (ADR-0019).
func (r *replay) reorder(w http.ResponseWriter, req *http.Request) {
	// Pointers rather than plain slices, because `null` and `[]` are different
	// requests here and only one of them works. A body whose list is `null`
	// answered 500 on the live UDR — a Tomcat error page with no message in it —
	// and a stand-in decoding both into a nil slice would accept the payload that
	// failed the operator's own apply (ADR-0019, ADR-0033).
	var sent struct {
		SourceZoneID        string    `json:"source_zone_id"`
		DestinationZoneID   string    `json:"destination_zone_id"`
		AfterPredefinedIDs  *[]string `json:"after_predefined_ids"`
		BeforePredefinedIDs *[]string `json:"before_predefined_ids"`
	}
	if !r.decode(w, req, &sent, reorderPath) {
		return
	}
	if sent.AfterPredefinedIDs == nil || sent.BeforePredefinedIDs == nil {
		r.t.Errorf("unifig sent a reorder with a null list, which the Controller answers 500 to: %+v", sent)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	after, before := *sent.AfterPredefinedIDs, *sent.BeforePredefinedIDs

	r.mu.Lock()
	defer r.mu.Unlock()
	r.reordered = append(r.reordered, map[string]any{
		"source_zone_id":        sent.SourceZoneID,
		"destination_zone_id":   sent.DestinationZoneID,
		"after_predefined_ids":  after,
		"before_predefined_ids": before,
	})

	named := make(map[string]bool, len(after)+len(before))
	for _, id := range append(append([]string{}, after...), before...) {
		named[id] = true
	}
	for _, held := range r.policies {
		if predefined, _ := held["predefined"].(bool); predefined {
			continue
		}
		source, destination := zoneEndsOf(held)
		if source != sent.SourceZoneID || destination != sent.DestinationZoneID {
			continue
		}
		if id, _ := held["_id"].(string); !named[id] {
			r.refuseCode(w, "api.err.ShouldIncludeFirewallPolicyInBatchUpdate",
				"Should include firewall policy in batch update")
			return
		}
	}

	reordered := make([]map[string]any, 0, len(named))
	for i, id := range before {
		if held := r.storedPolicyLocked(id); held != nil {
			held["index"] = storedPolicyIndex + i
			reordered = append(reordered, held)
		}
	}
	for _, id := range after {
		if held := r.storedPolicyLocked(id); held != nil {
			held["index"] = afterPredefinedIndex
			reordered = append(reordered, held)
		}
	}
	r.writeJSON(w, reordered)
}

// afterPredefinedIndex is what the Controller assigns a policy named in
// `after_predefined_ids`, read off the router in the same session.
const afterPredefinedIndex = 40000

func (r *replay) storedPolicyLocked(id string) map[string]any {
	for _, held := range r.policies {
		if heldID, _ := held["_id"].(string); heldID == id {
			return held
		}
	}
	return nil
}

// zoneEndsOf is the pair a policy governs, as the two zone ids the Controller
// stores rather than the names unifig knows them by.
func zoneEndsOf(policy map[string]any) (source, destination string) {
	end := func(key string) string {
		side, _ := policy[key].(map[string]any)
		id, _ := side["zone_id"].(string)
		return id
	}
	return end("source"), end("destination")
}

// numberIn reads a JSON number out of a decoded body, which is a float64
// whatever the Controller meant by it.
func numberIn(policy map[string]any, key string) (int, bool) {
	switch value := policy[key].(type) {
	case float64:
		return int(value), true
	case int:
		return value, true
	}
	return 0, false
}

// refuseCode answers the way the Controller answers a request it will not make
// for a reason it has a code for, which is what refuse does without one.
func (r *replay) refuseCode(w http.ResponseWriter, code, message string) {
	body, err := json.Marshal(map[string]any{
		"code": code, "details": map[string]any{}, "errorCode": 400, "message": message,
	})
	if err != nil {
		r.t.Errorf("encoding the Controller's refusal: %v", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	if _, err := w.Write(body); err != nil {
		r.t.Errorf("writing the Controller's refusal: %v", err)
	}
}

// storedPolicyIndex is the index the Controller assigned the created policy,
// unasked, in issue #46's probe — and the same value all nine stored policies on
// the live site carried in issue #54's reading.
const storedPolicyIndex = 10000

// seedUnmarkedGeneratedPolicy is that policy with the marker taken off: the
// composite `_id` of one the Controller computes, and no `predefined` on it.
//
// **It is a firmware nobody has met, and the fixture says so rather than
// implying otherwise.** On every policy anyone has measured the two fields agree
// — all eighty-six a migrated router ships are `predefined: true` and carry a
// composite id, and the one custom policy ADR-0027's probe created was
// `predefined: false` with a document handle — so a seed carrying both says
// nothing about which field a caller read. This is the only arrangement in which
// it can be told, which is what a test of prune's `_id` clause needs and the
// whole of why this exists (ADR-0028).
//
// Nothing else about it is invented. It carries no `origin_id`, because the one
// generated policy shape anyone has read carries one and this is not that shape;
// a seed guessing at what an unmet firmware would send beside the id would be
// stating more than the test is about (ADR-0019).
func (r *replay) seedUnmarkedGeneratedPolicy(t *testing.T, name, action, source, destination string, index int) {
	t.Helper()
	r.seedPolicy(t, name, action, source, destination, map[string]any{
		"_id":   r.generatedPolicyID(t, source, destination, index),
		"index": index,
	})
}

// seedUnmarkedReturnRule is the companion with everything but its own field
// taken off it: `RESPOND_ONLY`, an ordinary document handle, and no `predefined`.
//
// **It is a firmware nobody has met, and the fixture says so rather than
// implying otherwise.** Every companion anyone has measured carries a composite
// `_id`, which is the thing this one drops: the twelve `Allow Return Traffic` a
// migrated router ships carry one and the `predefined` marker with it, and the
// one ADR-0026's write session watched the Controller generate for a *custom*
// parent was recorded on its id — the marker on that one was not read, which is
// why this says the id rather than both. So a companion of any shape anyone has
// seen is spared by `generated` before `returnRule` is ever reached, and a clause
// that had been deleted would look exactly like one that works. This is the only
// arrangement in which `returnRule` can be told from the two clauses beside it,
// which is what a test of prune's companion clause needs (ADR-0019, ADR-0028).
//
// Its verdict is `ALLOW` rather than a parameter, because that is the whole of
// when the Controller makes one: a companion answers for a policy created
// allowing, and the request is refused beside any verdict that closes a path
// (ADR-0022).
//
// Nothing else about it is invented. It keeps seedPolicy's `create_allow_respond:
// true`, which is what all twelve in the recording carry, and it takes no
// `origin_id`, for the reason seedUnmarkedGeneratedPolicy takes none: the
// companion shapes anyone has read carry one and this is not one of those shapes.
func (r *replay) seedUnmarkedReturnRule(t *testing.T, name, source, destination string) {
	t.Helper()
	r.seedPolicy(t, name, "ALLOW", source, destination, map[string]any{
		"connection_state_type": "RESPOND_ONLY",
	})
}

// unnameableZone is a zone id no zone on this site carries. A policy with it on
// one end is one unifig has no name to write there, which is the whole of what
// makes a policy indescribable: a policy is its name, its verdict and its pair
// of zones, and there is no partial way to write one down.
//
// It is an id rather than a zone a test deletes, because that is the state the
// projection actually meets — the zones are read once, and an end naming
// something that is not among them has no name however it got that way.
const unnameableZone = "6613a1f0c4b2d90a5e1fdead"

// seedPolicyOnAZoneItCannotName seeds a policy with that id on one end.
//
// `fields` is seedPolicy's, so the same fixture states both policies a test
// about the two notices needs: hand it a composite `_id` and the policy is one
// the Controller generated on an end unifig cannot name, which is the case where
// the notices could disagree about whose fault the omission is.
func (r *replay) seedPolicyOnAZoneItCannotName(t *testing.T, name string, fields map[string]any) {
	t.Helper()
	all := map[string]any{
		"destination": map[string]any{"zone_id": unnameableZone, "matching_target": "ANY"},
	}
	maps.Copy(all, fields)
	r.seedPolicy(t, name, "ALLOW", "Internal", "Internal", all)
}

// generatedPolicyID is the id a Generated Policy carries, asked in the zone
// names a test reads: the source zone id, the destination zone id and the index
// run together — a description of where the policy came from rather than a
// handle into anything (ADR-0027).
//
// It is a function of its own so that both seeds state the shape once. Its zone
// reads happen before seedPolicy is called rather than inside it, because that
// takes the same lock they do — which holds while it is an argument expression,
// evaluated before the call it sits in.
func (r *replay) generatedPolicyID(t *testing.T, source, destination string, index int) string {
	t.Helper()
	from, _ := r.zoneNamed(t, source)["_id"].(string)
	to, _ := r.zoneNamed(t, destination)["_id"].(string)
	return compositePolicyID(from, to, index)
}

// policiesOnKey is every policy the stand-in holds under one policy key — a name
// together with the pair of zones it governs (ADR-0001) — which is what a test
// asks when it wants to know what an apply left on a key rather than under a
// name. The recording ships nineteen policies called `Allow All Traffic`, so a
// lookup by name alone says almost nothing about which of them was meant.
//
// It hands back the policies rather than a count, because the tests that need it
// need the `_id`: on a key a stored policy shares with a generated one, which of
// the two is left is the whole question, and the id shape is what tells them
// apart (ADR-0029).
func (r *replay) policiesOnKey(t *testing.T, name, source, destination string) []map[string]any {
	t.Helper()
	from, _ := r.zoneNamed(t, source)["_id"].(string)
	to, _ := r.zoneNamed(t, destination)["_id"].(string)

	var found []map[string]any
	for _, policy := range r.livePolicies(t) {
		ends, _ := policy["source"].(map[string]any)
		other, _ := policy["destination"].(map[string]any)
		if policy["name"] == name && ends["zone_id"] == from && other["zone_id"] == to {
			found = append(found, policy)
		}
	}
	return found
}

// compositePolicyID is that shape spelled once, for the callers that already
// hold the ids: the stand-in generating a companion, which is inside the lock
// the zone reads take, and a test whose end is an id no zone carries.
func compositePolicyID(source, destination string, index int) string {
	return fmt.Sprintf("%s%s%d", source, destination, index)
}

// zoneEnds names the zones a policy governs, translated out of the IDs it stores
// them as — what a test checks an apply against.
func (r *replay) zoneEnds(t *testing.T, name string) (source, destination string) {
	t.Helper()
	policy := r.policyNamed(t, name)

	names := map[string]string{}
	for _, zone := range r.liveZones(t) {
		id, _ := zone["_id"].(string)
		named, _ := zone["name"].(string)
		names[id] = named
	}

	end := func(field string) string {
		side, _ := policy[field].(map[string]any)
		id, _ := side["zone_id"].(string)
		return names[id]
	}
	return end("source"), end("destination")
}

// recordedList reads a recorded v2 response, which is a bare array rather than
// the envelope the rest of the Internal API answers with.
func recordedList(t *testing.T, name string) []map[string]any {
	t.Helper()
	var list []map[string]any
	if err := json.Unmarshal(recordedBody(t, name), &list); err != nil {
		t.Fatalf("reading the recorded %s: %v", name, err)
	}
	return list
}

// customServer is one entry of the Controller's custom_servers, in the shape
// the recording holds them, for a test seeding the state it needs.
func customServer(name, stamp string, enabled bool) map[string]any {
	return map[string]any{"server_name": name, "sdns_stamp": stamp, "enabled": enabled}
}

// data wraps entries in the envelope every Internal API response carries.
func data(entries ...map[string]any) json.RawMessage {
	body, err := json.Marshal(map[string]any{
		"meta": map[string]any{"rc": "ok"},
		"data": entries,
	})
	if err != nil {
		panic(fmt.Sprintf("marshaling a recorded response: %v", err))
	}
	return body
}

func recordedBody(t *testing.T, name string) json.RawMessage {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "udr", name))
	if err != nil {
		t.Fatalf("reading the recording: %v", err)
	}
	return json.RawMessage(bytes.TrimSpace(body))
}

func recordedEntries(t *testing.T, name string) []map[string]any {
	t.Helper()
	return entries(t, recordedBody(t, name))
}

// entries unwraps a recorded response into the objects it carries, so that a
// test can ask what the recording holds rather than assuming.
func entries(t *testing.T, body json.RawMessage) []map[string]any {
	t.Helper()

	var recorded struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &recorded); err != nil {
		t.Fatalf("reading a recorded response: %v", err)
	}
	return recorded.Data
}
