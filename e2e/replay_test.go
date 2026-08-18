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

// refusedByZoneWrite names the fields the Controller's zone write endpoint has
// been seen to refuse. Its DTO rejects any field it has not heard of, with a
// 400 naming the first one it reaches, and these are the two a real UDR was
// measured refusing (ADR-0019): `attr_no_edit`, which the Controller puts on
// three of its own zones and go-unifi models, and `cloud_template`, which it
// puts on all six and go-unifi does not — so no payload can carry the second
// today. It is here because what makes a field refusable is the Controller's
// DTO rather than the library's struct, and the day go-unifi models one more of
// them is the day that stops being true.
//
// Only what was measured is here, and the sibling markers are deliberately not
// — ADR-0014's objection to a fixture that asserts a guess still stands. What
// dissolved is only that this refusal is no longer one: it was reproduced on
// hardware, and it belongs to the request unifig builds rather than to any rule
// about which zones may be edited. That a payload carries no sibling marker
// either is unifig's own rule, and is asserted against the request rather than
// asserted here as the Controller's.
var refusedByZoneWrite = []string{"attr_no_edit", "cloud_template"}

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
		r.collectionV2(w, req, zonePath, &r.zones, refusedByZoneWrite)
	case req.URL.Path == policyPath || strings.HasPrefix(req.URL.Path, policyPath+"/"):
		// Nothing is refused here: no policy in the recording carries a marker
		// of this kind, and no write to one has been seen refused. A list
		// invented for symmetry would be this stand-in asserting a guess about
		// a DTO nobody has read.
		r.collectionV2(w, req, policyPath, &r.policies, nil)
	default:
		r.t.Errorf("unifig asked the Controller for something the recording does not have: %s %s",
			req.Method, req.URL.Path)
		http.NotFound(w, req)
	}
}

// update stores what unifig sent, the way the Controller does: the entry is
// replaced by the body rather than merged with it, which is what makes "unifig
// writes the whole object back" a thing this suite can actually check.
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
		sent["_id"] = id
		r.entries[i] = sent
		r.write(w, data(sent))
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
// refuses names the fields this collection's write endpoint answers 400 to, so
// that a payload the Controller would not parse is not one this stand-in
// quietly stores (see refusedByZoneWrite). Every write is kept whether it was
// refused or not, because what a test about the request needs is what unifig
// sent rather than what survived.
func (r *replay) collectionV2(w http.ResponseWriter, req *http.Request, base string, held *[]map[string]any, refuses []string) {
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
		if r.unrecognisedField(w, sent, refuses) {
			return
		}
		r.issued++
		sent["_id"] = fmt.Sprintf("6613a1f0c4b2d90a5e1f9%03d", r.issued)
		sent["site_id"] = "6613a1f0c4b2d90a5e1f0000"
		*held = append(*held, sent)
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
		if r.unrecognisedField(w, sent, refuses) {
			return
		}
		for i, entry := range *held {
			if entry["_id"] != id {
				continue
			}
			// Replaced rather than merged, the way the Controller replaces its
			// own — which is what makes "unifig writes the whole object back"
			// something this suite can actually check.
			sent["_id"] = id
			(*held)[i] = sent
			r.writeJSON(w, sent)
			return
		}
		r.t.Errorf("unifig updated something at %s the Controller does not have: %s", base, id)
		http.NotFound(w, req)

	case req.Method == http.MethodDelete && id != "":
		for i, entry := range *held {
			if entry["_id"] != id {
				continue
			}
			*held = append((*held)[:i], (*held)[i+1:]...)
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

// seedSlot sets fields on a WAN slot without going through unifig — the
// stand-in's version of the rig seeding the Controller through its own API, and
// the reason these tests do not depend on which values the recording happens to
// hold.
func (r *replay) seedSlot(t *testing.T, slot string, fields map[string]any) {
	t.Helper()
	entry := r.slot(t, slot)

	r.mu.Lock()
	defer r.mu.Unlock()
	for name, value := range fields {
		entry[name] = value
	}
}

// addSlot puts another uplink on the Controller, which is how a test states
// that a router may have slots unifig has never heard of — the recording is one
// router's answer, not the set of routers that exist.
func (r *replay) addSlot(t *testing.T, slot string, fields map[string]any) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()

	entry := map[string]any{
		"_id":              fmt.Sprintf("6613a1f0c4b2d90a5e1f9%03d", len(r.entries)),
		"name":             slot,
		"purpose":          "wan",
		"enabled":          true,
		"wan_networkgroup": slot,
	}
	for name, value := range fields {
		entry[name] = value
	}
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

// zoneWrites is every body unifig has sent to the zone collection — the
// requests themselves rather than what the stand-in stored, which is the only
// way to state anything about a field the Controller would have refused.
func (r *replay) zoneWrites(t *testing.T) []map[string]any {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()

	var bodies []map[string]any
	for _, write := range r.written {
		if strings.HasPrefix(write.path, zonePath) {
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

// gatewayZone is the name the recording gives the zone the Controller answers
// in, found the way unifig finds it: by the Controller's own `zone_key` rather
// than by the name. A test that hard-coded "Gateway" would be asserting a guess
// about someone else's product, which is the mistake ADR-0013 was written about.
func (r *replay) gatewayZone(t *testing.T) string {
	t.Helper()
	for _, zone := range r.liveZones(t) {
		if zone["zone_key"] == "gateway" {
			name, _ := zone["name"].(string)
			return name
		}
	}
	t.Fatalf("the recording holds no zone the Controller marks as its gateway")
	return ""
}

// renameGateway gives the gateway zone a different name, leaving the key that
// says what it is untouched. It is how a test asks whether unifig found the zone
// by the key or by the name, and there is no other way to tell the two apart.
func (r *replay) renameGateway(t *testing.T, name string) {
	t.Helper()
	was := r.gatewayZone(t)
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, zone := range r.zones {
		if zone["name"] == was {
			zone["name"] = name
			return
		}
	}
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
		"_id":         fmt.Sprintf("6613a1f0c4b2d90a5e1f7%03d", r.issued),
		"name":        name,
		"action":      action,
		"enabled":     true,
		"protocol":    "all",
		"schedule":    map[string]any{"mode": "ALWAYS", "time_all_day": true},
		"source":      map[string]any{"zone_id": zones[source], "matching_target": "ANY"},
		"destination": map[string]any{"zone_id": zones[destination], "matching_target": "ANY"},
		"site_id":     "6613a1f0c4b2d90a5e1f0000",
	}
	for field, value := range fields {
		policy[field] = value
	}
	r.policies = append(r.policies, policy)
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
