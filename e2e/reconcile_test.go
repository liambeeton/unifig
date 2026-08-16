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

// These tests state the reconcile contract the way an operator would: given
// this YAML and this live Controller, plan says exactly this, apply converges,
// and the next plan is empty. Everything goes through the real binary and a
// real Controller, and every assertion is either on what a shell would see —
// stdout, stderr, exit code — or on what the Controller itself reports
// afterwards.

// Plan's exit codes are a machine interface, so they get names rather than
// bare integers at the call sites that assert on them.
const (
	exitNoChanges      = 0
	exitError          = 1
	exitChangesPending = 2
)

// managedNetwork writes a config file describing one network, and deletes that
// network from the Controller when the test ends. Tests use distinct names so
// that one test's leftovers cannot become another's live state.
func managedNetwork(t *testing.T, body string, names ...string) string {
	t.Helper()
	for _, name := range names {
		t.Cleanup(func() { testRig.deleteNetworksNamed(t, name) })
	}

	path := filepath.Join(t.TempDir(), "unifig.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return path
}

// liveNetworksExcept is a config body naming every network unifig manages on
// the Controller right now, apart from the named ones.
//
// Prune's targets are decided by what is live rather than by what the test
// wrote, so a prune test that listed only its own networks would also be
// proposing to delete whatever else the Controller happened to ship with, and
// its assertions would be about that too. Naming the rest leaves exactly the
// exclusions at stake. They are named and no more — `- name: X` is a complete
// entry meaning "match this one, manage nothing about it" — so nothing here
// proposes an update either.
func liveNetworksExcept(t *testing.T, excluded ...string) string {
	t.Helper()

	var b strings.Builder
	b.WriteString("networks:\n")
	for _, name := range testRig.managedNetworkNames(t) {
		if !slices.Contains(excluded, name) {
			fmt.Fprintf(&b, "  - name: %q\n", name)
		}
	}
	return b.String()
}

func plan(t *testing.T, args ...string) result {
	t.Helper()
	res := testRig.runUnifig(t, append([]string{"plan"}, args...), nil)
	t.Logf("unifig plan %v -> exit %d\n%s", args, res.ExitCode, res.Stdout)
	return res
}

// apply runs an apply the operator has already approved. Confirmation is a
// thing to test on its own, not a thing to work around in every other test.
func apply(t *testing.T, args ...string) result {
	t.Helper()
	res := testRig.runUnifig(t, append([]string{"apply", "--auto-approve"}, args...), nil)
	t.Logf("unifig apply %v -> exit %d\n%s\n%s", args, res.ExitCode, res.Stdout, res.Stderr)
	if res.ExitCode != 0 {
		t.Fatalf("unifig apply exited %d\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}
	return res
}

func TestPlanShowsANetworkToCreateAndExitsWithChangesPending(t *testing.T) {
	path := managedNetwork(t, `networks:
  - name: Plan Create
    vlan: 120
    subnet: 10.120.0.1/24
`, "Plan Create")

	res := plan(t, path)

	if res.ExitCode != exitChangesPending {
		t.Fatalf("plan exited %d, want %d\nstderr: %s", res.ExitCode, exitChangesPending, res.Stderr)
	}
	stdout := string(res.Stdout)
	for _, fragment := range []string{`+ network "Plan Create"`, "120", "10.120.0.1/24", "1 to create"} {
		if !strings.Contains(stdout, fragment) {
			t.Errorf("plan output should mention %q, got:\n%s", fragment, stdout)
		}
	}
}

func TestPlanShowsBothEndsOfAFieldItWouldChange(t *testing.T) {
	testRig.seedNetwork(t, map[string]any{
		"name": "Plan Update", "purpose": "corporate", "enabled": true,
		"vlan_enabled": true, "vlan": 121, "ip_subnet": "10.121.0.1/24",
	})
	path := managedNetwork(t, `networks:
  - name: Plan Update
    vlan: 121
    subnet: 10.221.0.1/24
`, "Plan Update")

	res := plan(t, path)

	if res.ExitCode != exitChangesPending {
		t.Fatalf("plan exited %d, want %d\nstderr: %s", res.ExitCode, exitChangesPending, res.Stderr)
	}
	stdout := string(res.Stdout)
	for _, fragment := range []string{`~ network "Plan Update"`, "10.121.0.1/24 -> 10.221.0.1/24", "1 to update"} {
		if !strings.Contains(stdout, fragment) {
			t.Errorf("plan output should mention %q, got:\n%s", fragment, stdout)
		}
	}
	// The VLAN agrees, so it is not a change and has no business in the plan.
	if strings.Contains(stdout, "vlan") {
		t.Errorf("plan should list only the fields that differ, got:\n%s", stdout)
	}
}

// A plan is grouped by what it does and alphabetical within a group, whatever
// order the file happened to list things in. Two runs against the same
// Controller and config print byte-identical plans, which is what lets a
// pipeline diff one against another.
func TestPlanGroupsCreatesBeforeUpdatesRegardlessOfFileOrder(t *testing.T) {
	testRig.seedNetwork(t, map[string]any{
		"name": "Plan Mixed Existing", "purpose": "corporate", "enabled": true,
		"vlan_enabled": true, "vlan": 125, "ip_subnet": "10.125.0.1/24",
	})
	path := managedNetwork(t, `networks:
  - name: Plan Mixed Existing
    vlan: 125
    subnet: 10.225.0.1/24
  - name: Plan Mixed Zulu
    vlan: 127
  - name: Plan Mixed Alpha
    vlan: 126
`, "Plan Mixed Existing", "Plan Mixed Zulu", "Plan Mixed Alpha")

	res := plan(t, path)

	if res.ExitCode != exitChangesPending {
		t.Fatalf("plan exited %d, want %d\nstderr: %s", res.ExitCode, exitChangesPending, res.Stderr)
	}
	stdout := string(res.Stdout)
	if !strings.Contains(stdout, "Plan: 2 to create, 1 to update.") {
		t.Errorf("plan should summarise both kinds of change, got:\n%s", stdout)
	}

	order := []string{"Plan Mixed Alpha", "Plan Mixed Zulu", "Plan Mixed Existing"}
	at := make([]int, len(order))
	for i, name := range order {
		if at[i] = strings.Index(stdout, name); at[i] < 0 {
			t.Fatalf("plan does not mention %q:\n%s", name, stdout)
		}
	}
	if at[0] >= at[1] || at[1] >= at[2] {
		t.Errorf("plan should list creates alphabetically and then updates, got:\n%s", stdout)
	}
}

// A network the config does not mention is not unifig's business. This is the
// promise that makes adopting unifig on a configured Controller safe: name one
// network in the file and the rest are not at stake.
func TestPlanLeavesNetworksTheConfigDoesNotMentionAlone(t *testing.T) {
	testRig.seedNetwork(t, map[string]any{
		"name": "Plan Unmanaged", "purpose": "corporate", "enabled": true,
		"vlan_enabled": true, "vlan": 122, "ip_subnet": "10.122.0.1/24",
	})
	t.Cleanup(func() { testRig.deleteNetworksNamed(t, "Plan Unmanaged") })

	path := managedNetwork(t, `networks:
  - name: Default
    subnet: 192.168.1.1/24
`)

	res := plan(t, path)

	if res.ExitCode != exitNoChanges {
		t.Fatalf("plan exited %d, want %d\nstdout: %s\nstderr: %s",
			res.ExitCode, exitNoChanges, res.Stdout, res.Stderr)
	}
	if strings.Contains(string(res.Stdout), "Plan Unmanaged") {
		t.Errorf("plan should say nothing about a network the config does not list, got:\n%s", res.Stdout)
	}
}

func TestPlanJSONDescribesTheSameChangesMachineReadably(t *testing.T) {
	path := managedNetwork(t, `networks:
  - name: Plan JSON
    vlan: 123
    subnet: 10.123.0.1/24
`, "Plan JSON")

	res := plan(t, "--json", path)

	if res.ExitCode != exitChangesPending {
		t.Fatalf("plan --json exited %d, want %d\nstderr: %s", res.ExitCode, exitChangesPending, res.Stderr)
	}

	var out struct {
		Changes []struct {
			Action   string `json:"action"`
			Resource string `json:"resource"`
			Name     string `json:"name"`
			Fields   []struct {
				Name string `json:"name"`
				From any    `json:"from"`
				To   any    `json:"to"`
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
	if change.Action != "create" || change.Resource != "network" || change.Name != "Plan JSON" {
		t.Errorf("change = %+v, want a create of network %q", change, "Plan JSON")
	}

	// Values keep the config's own types, so a consumer reads a VLAN as a
	// number rather than having to parse it back out of a string.
	fields := map[string]any{}
	for _, f := range change.Fields {
		fields[f.Name] = f.To
	}
	if vlan, ok := fields["vlan"].(float64); !ok || vlan != 123 {
		t.Errorf("vlan field = %#v, want the number 123", fields["vlan"])
	}
	if fields["subnet"] != "10.123.0.1/24" {
		t.Errorf("subnet field = %#v, want %q", fields["subnet"], "10.123.0.1/24")
	}
}

func TestPlanJSONOnAMatchingControllerIsAnEmptyChangeList(t *testing.T) {
	path := managedNetwork(t, `networks:
  - name: Default
    subnet: 192.168.1.1/24
`)

	res := plan(t, "--json", path)

	if res.ExitCode != exitNoChanges {
		t.Fatalf("plan --json exited %d, want %d\nstdout: %s\nstderr: %s",
			res.ExitCode, exitNoChanges, res.Stdout, res.Stderr)
	}
	// An empty list rather than a null, so a consumer can count without
	// special-casing the quiet case.
	if got := strings.Join(strings.Fields(string(res.Stdout)), ""); got != `{"changes":[]}` {
		t.Errorf("empty plan JSON = %s, want an empty changes array", res.Stdout)
	}
}

// A field the config leaves out is a field unifig does not manage (ADR-0004).
// The schema requires only a name, so `- name: X` is a whole valid config —
// and it has to mean "I am not managing anything about X yet", because the
// schema gives an operator no way to say "X should have no subnet" and unifig
// must never delete what could not have been asked for.
//
// This is also the "never required" half of ADR-0001: a network can be managed
// with nothing but its natural key.
func TestPlanTreatsAnOmittedFieldAsUnmanagedNotAsRemoval(t *testing.T) {
	testRig.seedNetwork(t, map[string]any{
		"name": "Plan Omitted", "purpose": "corporate", "enabled": true,
		"vlan_enabled": true, "vlan": 128, "ip_subnet": "10.128.0.1/24",
	})
	t.Cleanup(func() { testRig.deleteNetworksNamed(t, "Plan Omitted") })

	for _, config := range []struct{ what, body string }{
		{"nothing but a name", "networks:\n  - name: Plan Omitted\n"},
		{"no vlan", "networks:\n  - name: Plan Omitted\n    subnet: 10.128.0.1/24\n"},
		{"no subnet", "networks:\n  - name: Plan Omitted\n    vlan: 128\n"},
	} {
		t.Run(config.what, func(t *testing.T) {
			res := plan(t, managedNetwork(t, config.body))
			if res.ExitCode != exitNoChanges {
				t.Errorf("a config with %s exited %d, want %d — it is proposing to clear a field it never mentioned\nplan:\n%s",
					config.what, res.ExitCode, exitNoChanges, res.Stdout)
			}
		})
	}
}

// The other half of the same rule, at the far end: what apply writes is what
// plan showed, so a field the config omits survives on the Controller.
func TestApplyLeavesAnOmittedFieldOnTheController(t *testing.T) {
	testRig.seedNetwork(t, map[string]any{
		"name": "Apply Omitted", "purpose": "corporate", "enabled": true,
		"vlan_enabled": true, "vlan": 139, "ip_subnet": "10.139.0.1/24",
	})
	// The subnet is managed and changes; the VLAN is not mentioned at all.
	path := managedNetwork(t, `networks:
  - name: Apply Omitted
    subnet: 10.239.0.1/24
`, "Apply Omitted")

	apply(t, path)

	live := testRig.liveNetwork(t, "Apply Omitted")
	if live["ip_subnet"] != "10.239.0.1/24" {
		t.Fatalf("the change under test did not happen: subnet = %#v", live["ip_subnet"])
	}
	if vlan, ok := live["vlan"].(float64); !ok || vlan != 139 {
		t.Errorf("vlan = %#v after apply, want the untouched 139 — the config never mentioned it", live["vlan"])
	}
	if live["vlan_enabled"] != true {
		t.Errorf("vlan_enabled = %#v after apply, want it left on", live["vlan_enabled"])
	}
}

// ADR-0001 matches Resources by name and calls a duplicate a hard error rather
// than a guess. The Controller does permit two networks to share a name, so
// this is a state unifig can really meet.
func TestPlanRefusesToGuessBetweenTwoNetworksSharingAName(t *testing.T) {
	testRig.seedNetwork(t, map[string]any{
		"name": "Plan Twice", "purpose": "corporate", "enabled": true,
		"vlan_enabled": true, "vlan": 129, "ip_subnet": "10.129.0.1/24",
	})
	// seedNetwork clears same-named entries first, so the second one is added
	// through the Controller's own API directly.
	testRig.addNetwork(t, map[string]any{
		"name": "Plan Twice", "purpose": "corporate", "enabled": true,
		"vlan_enabled": true, "vlan": 229, "ip_subnet": "10.229.0.1/24",
	})
	t.Cleanup(func() { testRig.deleteNetworksNamed(t, "Plan Twice") })

	path := managedNetwork(t, `networks:
  - name: Plan Twice
    vlan: 129
    subnet: 10.129.0.1/24
`)

	res := plan(t, path)

	if res.ExitCode != exitError {
		t.Fatalf("plan exited %d, want %d — it picked one of two networks named the same",
			res.ExitCode, exitError)
	}
	if !strings.Contains(string(res.Stderr), "Plan Twice") {
		t.Errorf("stderr should name the ambiguous network, got: %s", res.Stderr)
	}
}

// Every duplicate at once rather than the first one found. Resolving them is a
// trip to the Controller's UI, and an operator should have to make it once:
// a run that reported one duplicate, was fixed, and then reported the next
// would be spending the operator's afternoon one name at a time.
func TestPlanNamesEveryDuplicatedNetworkNameAtOnce(t *testing.T) {
	for _, twin := range []struct {
		name string
		vlan int
	}{{"Plan Twice Alpha", 146}, {"Plan Twice Zulu", 147}} {
		testRig.seedNetwork(t, map[string]any{
			"name": twin.name, "purpose": "corporate", "enabled": true,
			"vlan_enabled": true, "vlan": twin.vlan,
			"ip_subnet": fmt.Sprintf("10.%d.0.1/24", twin.vlan),
		})
		// seedNetwork clears same-named entries first, so the twin goes in
		// through the Controller's own API directly.
		testRig.addNetwork(t, map[string]any{
			"name": twin.name, "purpose": "corporate", "enabled": true,
			"vlan_enabled": true, "vlan": twin.vlan + 100,
			"ip_subnet": fmt.Sprintf("10.%d.0.1/24", twin.vlan+100),
		})
		t.Cleanup(func() { testRig.deleteNetworksNamed(t, twin.name) })
	}

	path := managedNetwork(t, `networks:
  - name: Default
    subnet: 192.168.1.1/24
`)

	res := plan(t, path)

	if res.ExitCode != exitError {
		t.Fatalf("plan exited %d, want %d — it picked between networks sharing a name\nstdout: %s",
			res.ExitCode, exitError, res.Stdout)
	}
	stderr := string(res.Stderr)
	for _, fragment := range []string{"Plan Twice Alpha", "Plan Twice Zulu", "UI"} {
		if !strings.Contains(stderr, fragment) {
			t.Errorf("stderr should mention %q, so every duplicate is fixed in one trip to the UI, got: %s",
				fragment, stderr)
		}
	}
}

// ADR-0001: Controller IDs never appear in, and are never required in, the
// config file. TestPlanTreatsAnOmittedFieldAsUnmanagedNotAsRemoval covers the
// "never required" half; this asserts the other, that the ID the Controller
// does hold never leaks back out through the plan.
func TestPlanNeverExposesControllerIDs(t *testing.T) {
	testRig.seedNetwork(t, map[string]any{
		"name": "Plan No IDs", "purpose": "corporate", "enabled": true,
		"vlan_enabled": true, "vlan": 124, "ip_subnet": "10.124.0.1/24",
	})
	path := managedNetwork(t, `networks:
  - name: Plan No IDs
    vlan: 124
    subnet: 10.224.0.1/24
`, "Plan No IDs")

	live := testRig.liveNetwork(t, "Plan No IDs")
	id, _ := live["_id"].(string)
	if id == "" {
		t.Fatalf("the seeded network has no Controller ID to look for: %v", live)
	}

	for _, args := range [][]string{{path}, {"--json", path}} {
		res := plan(t, args...)
		if strings.Contains(string(res.Stdout), id) {
			t.Errorf("plan %v leaked the Controller ID %q:\n%s", args, id, res.Stdout)
		}
		if strings.Contains(string(res.Stdout), "_id") {
			t.Errorf("plan %v mentions Controller IDs at all:\n%s", args, res.Stdout)
		}
	}
}

func TestApplyCreatesANetworkAndTheNextPlanIsEmpty(t *testing.T) {
	path := managedNetwork(t, `networks:
  - name: Apply Create
    vlan: 130
    subnet: 10.130.0.1/24
`, "Apply Create")

	res := apply(t, path)
	if !strings.Contains(string(res.Stdout), `+ network "Apply Create" created`) {
		t.Errorf("apply should report what it created, got:\n%s", res.Stdout)
	}

	live := testRig.liveNetwork(t, "Apply Create")
	if live["ip_subnet"] != "10.130.0.1/24" {
		t.Errorf("created network subnet = %#v, want %q", live["ip_subnet"], "10.130.0.1/24")
	}
	if vlan, ok := live["vlan"].(float64); !ok || vlan != 130 {
		t.Errorf("created network vlan = %#v, want 130", live["vlan"])
	}
	if live["vlan_enabled"] != true {
		t.Errorf("created network has vlan_enabled = %#v, so the VLAN tag would not be used", live["vlan_enabled"])
	}

	assertNoChangesPending(t, path)
}

// A network unifig creates has to be a usable LAN, not just a row in the
// Controller's database. The config models three fields; the rest of what
// makes a network work has to come from somewhere, and it comes from the
// Controller's own defaults for a new LAN.
func TestApplyCreatesANetworkThatIsActuallyUsable(t *testing.T) {
	path := managedNetwork(t, `networks:
  - name: Apply Usable
    vlan: 131
    subnet: 10.131.0.1/24
`, "Apply Usable")

	apply(t, path)

	live := testRig.liveNetwork(t, "Apply Usable")
	if live["is_nat"] != true {
		t.Errorf("created network has NAT off (is_nat = %#v), so its clients could not reach the internet", live["is_nat"])
	}
	if live["dhcpd_enabled"] != true {
		t.Errorf("created network has no DHCP server (dhcpd_enabled = %#v), so its clients would get no address", live["dhcpd_enabled"])
	}
	// The Controller's own convention: the sixth address through the last
	// usable one, as its built-in Default network is configured.
	if live["dhcpd_start"] != "10.131.0.6" || live["dhcpd_stop"] != "10.131.0.254" {
		t.Errorf("DHCP pool = %#v..%#v, want 10.131.0.6..10.131.0.254", live["dhcpd_start"], live["dhcpd_stop"])
	}
}

func TestApplyUpdatesANetworkAndTheNextPlanIsEmpty(t *testing.T) {
	testRig.seedNetwork(t, map[string]any{
		"name": "Apply Update", "purpose": "corporate", "enabled": true,
		"vlan_enabled": true, "vlan": 132, "ip_subnet": "10.132.0.1/24",
	})
	path := managedNetwork(t, `networks:
  - name: Apply Update
    vlan: 232
    subnet: 10.232.0.1/24
`, "Apply Update")

	res := apply(t, path)
	if !strings.Contains(string(res.Stdout), `~ network "Apply Update" updated`) {
		t.Errorf("apply should report what it updated, got:\n%s", res.Stdout)
	}

	live := testRig.liveNetwork(t, "Apply Update")
	if live["ip_subnet"] != "10.232.0.1/24" {
		t.Errorf("updated network subnet = %#v, want %q", live["ip_subnet"], "10.232.0.1/24")
	}
	if vlan, ok := live["vlan"].(float64); !ok || vlan != 232 {
		t.Errorf("updated network vlan = %#v, want 232", live["vlan"])
	}

	assertNoChangesPending(t, path)
}

// unifig manages the fields its config models and nothing else. An operator
// who set a DHCP pool, a DNS server and a domain name in the Controller's UI
// must not lose them because they later changed a VLAN ID in YAML — the config
// file is not a claim about the fields it does not mention.
func TestApplyLeavesControllerSettingsUnifigDoesNotModelAlone(t *testing.T) {
	testRig.seedNetwork(t, map[string]any{
		"name": "Apply Preserve", "purpose": "corporate", "enabled": true,
		"vlan_enabled": true, "vlan": 133, "ip_subnet": "10.133.0.1/24",
		"dhcpd_enabled": true, "dhcpd_start": "10.133.0.100", "dhcpd_stop": "10.133.0.199",
		"dhcpd_dns_enabled": true, "dhcpd_dns_1": "9.9.9.9",
		"domain_name": "chosen.by.hand",
	})
	path := managedNetwork(t, `networks:
  - name: Apply Preserve
    vlan: 233
    subnet: 10.133.0.1/24
`, "Apply Preserve")

	apply(t, path)

	live := testRig.liveNetwork(t, "Apply Preserve")
	if vlan, ok := live["vlan"].(float64); !ok || vlan != 233 {
		t.Fatalf("the change under test did not happen: vlan = %#v", live["vlan"])
	}
	for field, want := range map[string]any{
		"dhcpd_start":   "10.133.0.100",
		"dhcpd_stop":    "10.133.0.199",
		"dhcpd_dns_1":   "9.9.9.9",
		"domain_name":   "chosen.by.hand",
		"dhcpd_enabled": true,
	} {
		if live[field] != want {
			t.Errorf("%s = %#v after apply, want %#v — unifig does not model this field and must not touch it",
				field, live[field], want)
		}
	}
}

// The one field unifig writes without modelling, and only when leaving it
// alone is not an option: a DHCP pool cannot stay in a subnet the network no
// longer has, and the Controller rejects the whole update if it tries.
func TestApplyMovesADHCPPoolItsSubnetLeftBehind(t *testing.T) {
	testRig.seedNetwork(t, map[string]any{
		"name": "Apply Restranded", "purpose": "corporate", "enabled": true,
		"vlan_enabled": true, "vlan": 137, "ip_subnet": "10.137.0.1/24",
		"dhcpd_enabled": true, "dhcpd_start": "10.137.0.100", "dhcpd_stop": "10.137.0.199",
	})
	path := managedNetwork(t, `networks:
  - name: Apply Restranded
    vlan: 137
    subnet: 10.237.0.1/24
`, "Apply Restranded")

	// The plan says so before the apply does it. A plan that quietly did more
	// than it printed would not be a plan.
	planned := plan(t, path)
	if !strings.Contains(string(planned.Stdout), "DHCP pool") {
		t.Errorf("plan should warn that the DHCP pool has to move, got:\n%s", planned.Stdout)
	}

	apply(t, path)

	live := testRig.liveNetwork(t, "Apply Restranded")
	if live["dhcpd_start"] != "10.237.0.6" || live["dhcpd_stop"] != "10.237.0.254" {
		t.Errorf("DHCP pool = %#v..%#v after the subnet moved, want 10.237.0.6..10.237.0.254",
			live["dhcpd_start"], live["dhcpd_stop"])
	}

	assertNoChangesPending(t, path)
}

// A pool the operator narrowed by hand still fits the new subnet, so it stays
// exactly as they set it. Only a pool that cannot survive is rebuilt.
func TestApplyKeepsADHCPPoolThatStillFits(t *testing.T) {
	testRig.seedNetwork(t, map[string]any{
		"name": "Apply Refits", "purpose": "corporate", "enabled": true,
		"vlan_enabled": true, "vlan": 138, "ip_subnet": "10.138.0.1/24",
		"dhcpd_enabled": true, "dhcpd_start": "10.138.0.100", "dhcpd_stop": "10.138.0.199",
	})
	// Same address space, wider prefix: the pool is still inside it.
	path := managedNetwork(t, `networks:
  - name: Apply Refits
    vlan: 138
    subnet: 10.138.0.1/23
`, "Apply Refits")

	apply(t, path)

	live := testRig.liveNetwork(t, "Apply Refits")
	if live["dhcpd_start"] != "10.138.0.100" || live["dhcpd_stop"] != "10.138.0.199" {
		t.Errorf("DHCP pool = %#v..%#v, want the operator's own 10.138.0.100..10.138.0.199",
			live["dhcpd_start"], live["dhcpd_stop"])
	}
}

func TestApplyOnAMatchingControllerChangesNothing(t *testing.T) {
	path := managedNetwork(t, `networks:
  - name: Default
    subnet: 192.168.1.1/24
`)

	res := apply(t, path)

	if !strings.Contains(string(res.Stdout), "No changes") {
		t.Errorf("apply should say there was nothing to do, got:\n%s", res.Stdout)
	}
	// Nothing was applied, so nothing should claim to have been.
	if strings.Contains(string(res.Stdout), "Applied") {
		t.Errorf("apply reported applying something when there was nothing to do:\n%s", res.Stdout)
	}
}

// Confirmation is the safety contract: no change without an explicit yes.
func TestApplyWithoutApprovalAsksAndChangesNothingWhenRefused(t *testing.T) {
	path := managedNetwork(t, `networks:
  - name: Apply Refused
    vlan: 134
    subnet: 10.134.0.1/24
`, "Apply Refused")

	res := testRig.runUnifigWithInput(t, []string{"apply", path}, nil, "n\n")

	if res.ExitCode != exitError {
		t.Errorf("a refused apply exited %d, want %d — it did not do what was asked", res.ExitCode, exitError)
	}
	if !strings.Contains(string(res.Stdout), "[y/N]") {
		t.Errorf("apply should have asked before changing anything, got:\n%s", res.Stdout)
	}
	if found := testRig.networksNamed(t, "Apply Refused"); len(found) != 0 {
		t.Errorf("a refused apply created the network anyway: %v", found)
	}
}

// An apply running unattended without --auto-approve gets EOF where the
// operator's answer should be. That is a no: the alternative is a tool that
// changes a Controller because nobody was there to object.
func TestApplyWithNoOneToAskChangesNothing(t *testing.T) {
	path := managedNetwork(t, `networks:
  - name: Apply Unattended
    vlan: 135
    subnet: 10.135.0.1/24
`, "Apply Unattended")

	res := testRig.runUnifigWithInput(t, []string{"apply", path}, nil, "")

	if res.ExitCode != exitError {
		t.Errorf("an unanswered apply exited %d, want %d", res.ExitCode, exitError)
	}
	if found := testRig.networksNamed(t, "Apply Unattended"); len(found) != 0 {
		t.Errorf("an unanswered apply created the network anyway: %v", found)
	}
}

func TestApplyProceedsWhenTheOperatorAgrees(t *testing.T) {
	path := managedNetwork(t, `networks:
  - name: Apply Approved
    vlan: 136
    subnet: 10.136.0.1/24
`, "Apply Approved")

	res := testRig.runUnifigWithInput(t, []string{"apply", path}, nil, "yes\n")

	if res.ExitCode != 0 {
		t.Fatalf("apply exited %d\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}
	if found := testRig.networksNamed(t, "Apply Approved"); len(found) != 1 {
		t.Errorf("an approved apply did not create the network: %v", found)
	}
}

// Prune is the destructive half of reconcile, and its default is the whole
// point. TestPlanLeavesNetworksTheConfigDoesNotMentionAlone says the plan is
// silent about an unlisted network; this says the apply behind it does nothing
// to one either, which is where the difference would actually cost something.
func TestApplyWithoutPruneLeavesUnlistedNetworksAlone(t *testing.T) {
	testRig.seedNetwork(t, map[string]any{
		"name": "Prune Untouched", "purpose": "corporate", "enabled": true,
		"vlan_enabled": true, "vlan": 141, "ip_subnet": "10.141.0.1/24",
	})
	t.Cleanup(func() { testRig.deleteNetworksNamed(t, "Prune Untouched") })

	path := managedNetwork(t, `networks:
  - name: Prune Listed
    vlan: 142
    subnet: 10.142.0.1/24
`, "Prune Listed")

	res := apply(t, path)

	if strings.Contains(string(res.Stdout), "Prune Untouched") {
		t.Errorf("apply should say nothing about a network the config does not list, got:\n%s", res.Stdout)
	}
	if found := testRig.networksNamed(t, "Prune Untouched"); len(found) != 1 {
		t.Fatalf("apply deleted an unlisted network nobody asked it to delete: %v", found)
	}
}

// The deletions are in the plan, and the plan comes first: an operator sees
// every network they are about to lose — and what is in it — before they are
// asked to approve anything. Computing that plan still mutates nothing, however
// destructive the changes it describes.
func TestPlanWithPruneShowsWhatItWouldDeleteAndDeletesNothing(t *testing.T) {
	testRig.seedNetwork(t, map[string]any{
		"name": "Prune Planned", "purpose": "corporate", "enabled": true,
		"vlan_enabled": true, "vlan": 143, "ip_subnet": "10.143.0.1/24",
	})
	t.Cleanup(func() { testRig.deleteNetworksNamed(t, "Prune Planned") })

	path := managedNetwork(t, liveNetworksExcept(t, "Prune Planned"))

	res := plan(t, "--prune", path)

	if res.ExitCode != exitChangesPending {
		t.Fatalf("plan --prune exited %d, want %d\nstdout: %s\nstderr: %s",
			res.ExitCode, exitChangesPending, res.Stdout, res.Stderr)
	}
	stdout := string(res.Stdout)
	for _, fragment := range []string{`- network "Prune Planned"`, "10.143.0.1/24", "1 to delete"} {
		if !strings.Contains(stdout, fragment) {
			t.Errorf("plan --prune should show %q — an operator about to lose a network needs to see what was in it, got:\n%s",
				fragment, stdout)
		}
	}
	if found := testRig.networksNamed(t, "Prune Planned"); len(found) != 1 {
		t.Errorf("plan deleted something; plan is read-only: %v", found)
	}
}

// A pipeline gating on destructive change reads the plan rather than the prose,
// so a deletion has to be as legible to a machine as a create is. The values go
// on the from side with nothing opposite them: a delete is not a move to a new
// value, and `"to": null` would read as a field being cleared on a network that
// survives.
func TestPlanJSONDescribesADeletionAsALossNotAMove(t *testing.T) {
	testRig.seedNetwork(t, map[string]any{
		"name": "Prune JSON", "purpose": "corporate", "enabled": true,
		"vlan_enabled": true, "vlan": 151, "ip_subnet": "10.151.0.1/24",
	})
	t.Cleanup(func() { testRig.deleteNetworksNamed(t, "Prune JSON") })

	path := managedNetwork(t, liveNetworksExcept(t, "Prune JSON"))

	res := plan(t, "--json", "--prune", path)

	if res.ExitCode != exitChangesPending {
		t.Fatalf("plan --json --prune exited %d, want %d\nstderr: %s",
			res.ExitCode, exitChangesPending, res.Stderr)
	}

	var out struct {
		Changes []struct {
			Action   string `json:"action"`
			Resource string `json:"resource"`
			Name     string `json:"name"`
			Fields   []struct {
				Name string `json:"name"`
				From any    `json:"from"`
				To   any    `json:"to"`
			} `json:"fields"`
		} `json:"changes"`
	}
	if err := json.Unmarshal(res.Stdout, &out); err != nil {
		t.Fatalf("plan --json --prune is not valid JSON: %v\nstdout: %s", err, res.Stdout)
	}
	if len(out.Changes) != 1 {
		t.Fatalf("plan reported %d changes, want 1\nstdout: %s", len(out.Changes), res.Stdout)
	}

	change := out.Changes[0]
	if change.Action != "delete" || change.Resource != "network" || change.Name != "Prune JSON" {
		t.Fatalf("change = %+v, want a delete of network %q", change, "Prune JSON")
	}
	for _, field := range change.Fields {
		if field.To != nil {
			t.Errorf("%s field has to = %#v; a delete moves nothing to a new value", field.Name, field.To)
		}
	}
	fields := map[string]any{}
	for _, field := range change.Fields {
		fields[field.Name] = field.From
	}
	if vlan, ok := fields["vlan"].(float64); !ok || vlan != 151 {
		t.Errorf("vlan field = %#v, want the number 151 on the from side", fields["vlan"])
	}
	if fields["subnet"] != "10.151.0.1/24" {
		t.Errorf("subnet field = %#v, want %q on the from side", fields["subnet"], "10.151.0.1/24")
	}
}

func TestApplyWithPruneDeletesNetworksTheConfigDoesNotName(t *testing.T) {
	testRig.seedNetwork(t, map[string]any{
		"name": "Prune Applied", "purpose": "corporate", "enabled": true,
		"vlan_enabled": true, "vlan": 144, "ip_subnet": "10.144.0.1/24",
	})
	t.Cleanup(func() { testRig.deleteNetworksNamed(t, "Prune Applied") })

	path := managedNetwork(t, liveNetworksExcept(t, "Prune Applied"))

	res := apply(t, "--prune", path)

	if !strings.Contains(string(res.Stdout), `- network "Prune Applied" deleted`) {
		t.Errorf("apply --prune should report what it deleted, got:\n%s", res.Stdout)
	}
	if found := testRig.networksNamed(t, "Prune Applied"); len(found) != 0 {
		t.Fatalf("apply --prune left a network the config does not name: %v", found)
	}

	assertNoChangesPending(t, "--prune", path)
}

// Deletions join the other changes rather than replacing them, and they come
// last — which is the order apply executes in, and matters because apply stops
// at the first failure. This asserts the order the plan prints; that the plan
// and apply cannot disagree about it is structural, since both read the same
// sorted Changes.
func TestPlanWithPruneListsDeletionsAfterTheChangesThatBuildThings(t *testing.T) {
	testRig.seedNetwork(t, map[string]any{
		"name": "Prune Ordered Old", "purpose": "corporate", "enabled": true,
		"vlan_enabled": true, "vlan": 148, "ip_subnet": "10.148.0.1/24",
	})
	t.Cleanup(func() { testRig.deleteNetworksNamed(t, "Prune Ordered Old") })

	path := managedNetwork(t, liveNetworksExcept(t, "Prune Ordered Old")+`  - name: Prune Ordered New
    vlan: 149
    subnet: 10.149.0.1/24
`, "Prune Ordered New")

	res := plan(t, "--prune", path)

	if res.ExitCode != exitChangesPending {
		t.Fatalf("plan --prune exited %d, want %d\nstderr: %s", res.ExitCode, exitChangesPending, res.Stderr)
	}
	stdout := string(res.Stdout)
	if !strings.Contains(stdout, "Plan: 1 to create, 1 to delete.") {
		t.Errorf("plan should summarise both kinds of change, got:\n%s", stdout)
	}
	created, deleted := strings.Index(stdout, "Prune Ordered New"), strings.Index(stdout, "Prune Ordered Old")
	if created < 0 || deleted < 0 {
		t.Fatalf("plan does not mention both networks:\n%s", stdout)
	}
	if created > deleted {
		t.Errorf("plan lists the deletion before the create; deletions come last, got:\n%s", stdout)
	}
}

// The Controller owns some objects outright, and says so on the object itself:
// the built-in Default network carries attr_no_delete. Prune respects that
// whether or not the config names it — an operator whose file has never
// mentioned Default must not lose their LAN by pruning.
func TestPruneNeverDeletesTheControllersBuiltInNetwork(t *testing.T) {
	// unifig reads the Controller's own marker rather than keeping a list of
	// built-in names, so the test states that first. If this stops holding, the
	// reason the rest of the test passes has changed, and asserting it here
	// fails before anything destructive is asked of a real Controller.
	live := testRig.liveNetwork(t, "Default")
	if live["attr_no_delete"] != true {
		t.Fatalf("the Controller no longer marks Default undeletable (attr_no_delete = %#v); unifig's exemption reads that marker",
			live["attr_no_delete"])
	}

	// A config that has never heard of Default, pruned. The network it does add
	// is what makes the plan worth reading: prune ran, produced changes, and
	// Default is not among them.
	path := managedNetwork(t, liveNetworksExcept(t, "Default")+`  - name: Prune Exempt
    vlan: 145
    subnet: 10.145.0.1/24
`, "Prune Exempt")

	// Plan before apply, and fatal rather than error on what it says. A
	// regression in the exemption is caught here, while nothing has happened
	// yet, instead of by an apply that takes the Controller's LAN with it and
	// leaves every later test running against a site without one.
	planned := plan(t, "--prune", path)
	if !strings.Contains(string(planned.Stdout), `+ network "Prune Exempt"`) {
		t.Fatalf("the plan under test is not the one intended:\n%s", planned.Stdout)
	}
	if strings.Contains(string(planned.Stdout), `network "Default"`) {
		t.Fatalf("plan --prune proposes deleting the built-in Default network:\n%s", planned.Stdout)
	}

	apply(t, "--prune", path)

	if found := testRig.networksNamed(t, "Default"); len(found) != 1 {
		t.Fatalf("prune deleted the built-in Default network: %v", found)
	}
}

// Prune only ever deletes Resources of the types unifig manages. WAN slots are
// Settings — fixed slots that are updated, never created or deleted — and they
// live in the very same networkconf collection as the LANs, so "of a managed
// type" is the difference between pruning a spare VLAN and taking the site off
// the internet.
func TestPruneLeavesTypesUnifigDoesNotManageAlone(t *testing.T) {
	// A WAN slot, in the same networkconf collection as the LANs and named by
	// no config anywhere — which is exactly the shape prune deletes things for.
	testRig.seedNetwork(t, map[string]any{
		"name": "Prune WAN Slot", "purpose": "wan", "enabled": true,
		"wan_networkgroup": "WAN2", "wan_type": "dhcp",
	})
	t.Cleanup(func() { testRig.deleteNetworksNamed(t, "Prune WAN Slot") })

	// Something for the prune to actually delete, so this is a test about what
	// a working prune spares rather than about a prune that did nothing.
	testRig.seedNetwork(t, map[string]any{
		"name": "Prune Bystander", "purpose": "corporate", "enabled": true,
		"vlan_enabled": true, "vlan": 150, "ip_subnet": "10.150.0.1/24",
	})
	t.Cleanup(func() { testRig.deleteNetworksNamed(t, "Prune Bystander") })

	path := managedNetwork(t, liveNetworksExcept(t, "Prune Bystander"))

	res := apply(t, "--prune", path)

	if found := testRig.networksNamed(t, "Prune Bystander"); len(found) != 0 {
		t.Fatalf("the prune under test did not happen: %v", found)
	}
	if strings.Contains(string(res.Stdout), "Prune WAN Slot") {
		t.Errorf("apply --prune has an opinion about a WAN slot:\n%s", res.Stdout)
	}
	if found := testRig.networksNamed(t, "Prune WAN Slot"); len(found) != 1 {
		t.Fatalf("prune deleted a WAN slot — that is the site's internet connection: %v", found)
	}
}

// The brownfield path end to end: adopt a configured Controller with export,
// and the file that comes out describes it exactly. Anything less means an
// operator's very first plan shows changes against a Controller they have not
// touched.
func TestExportedConfigPlansClean(t *testing.T) {
	testRig.seedNetwork(t, map[string]any{
		"name": "Export Round Trip", "purpose": "corporate", "enabled": true,
		"vlan_enabled": true, "vlan": 140, "ip_subnet": "10.140.0.1/24",
	})
	t.Cleanup(func() { testRig.deleteNetworksNamed(t, "Export Round Trip") })

	exported := testRig.runUnifig(t, []string{"export"}, nil)
	if exported.ExitCode != 0 {
		t.Fatalf("unifig export exited %d\nstderr: %s", exported.ExitCode, exported.Stderr)
	}

	path := filepath.Join(t.TempDir(), "unifig.yaml")
	if err := os.WriteFile(path, exported.Stdout, 0o600); err != nil {
		t.Fatalf("writing exported config: %v", err)
	}

	res := plan(t, path)
	if res.ExitCode != exitNoChanges {
		t.Fatalf("plan of a freshly exported config exited %d, want %d\nexported:\n%s\nplan:\n%s",
			res.ExitCode, exitNoChanges, exported.Stdout, res.Stdout)
	}
}

func TestPlanWithABadAPIKeyIsAnErrorNotChangesPending(t *testing.T) {
	path := managedNetwork(t, `networks:
  - name: Default
    subnet: 192.168.1.1/24
`)

	res := testRig.runUnifig(t, []string{"plan", path}, map[string]string{"UNIFIG_API_KEY": "not-the-rigs-key"})

	if res.ExitCode != exitError {
		t.Errorf("exit code = %d, want %d", res.ExitCode, exitError)
	}
	stderr := strings.ToLower(string(res.Stderr))
	if !strings.Contains(stderr, "401") && !strings.Contains(stderr, "unauthorized") {
		t.Errorf("stderr should report the auth failure, got: %s", res.Stderr)
	}
}

// assertNoChangesPending is the idempotence check: reconciling twice does the
// work once. It is asserted at the process boundary — a fresh unifig process
// reading the Controller back through the whole stack — because that is the
// only way it proves anything about what was actually written. It is given the
// same arguments the apply had, since an apply's idempotence is only a claim
// about the reconcile that was actually asked for.
func assertNoChangesPending(t *testing.T, args ...string) {
	t.Helper()
	res := plan(t, args...)
	if res.ExitCode != exitNoChanges {
		t.Errorf("re-planning after apply exited %d, want %d — apply is not idempotent\nplan:\n%s",
			res.ExitCode, exitNoChanges, res.Stdout)
	}
}
