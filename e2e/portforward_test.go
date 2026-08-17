package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Port forwards are the section that says what the network answers to from the
// internet, so these tests state what an operator would want to be able to trust
// about it: the file is the record of every forward, a create says out loud what
// it is opening, and a forward unifig cannot describe is one it never touches.
//
// As everywhere in this suite, the assertions are on what a shell would see or
// on what the Controller itself reports afterwards.

// managedPortForward writes a config file and deletes the named forwards from
// the Controller when the test ends — managedNetwork's counterpart.
func managedPortForward(t *testing.T, body string, names ...string) string {
	t.Helper()
	cleanupPortForwards(t, names...)
	return configFile(t, body)
}

func cleanupPortForwards(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		t.Cleanup(func() { testRig.deletePortForwardsNamed(t, name) })
	}
}

// seedPortForwardTo puts a forward on the Controller through its own API, in the
// shape its UI creates: enabled, on the primary uplink, open to anywhere.
func seedPortForwardTo(t *testing.T, name, port, forwardIP, forwardPort, protocol string) {
	t.Helper()
	testRig.seedPortForward(t, map[string]any{
		"name": name, "enabled": true, "pfwd_interface": "wan",
		"dst_port": port, "fwd": forwardIP, "fwd_port": forwardPort,
		"proto": protocol, "src": "any", "destination_ip": "any",
	})
	cleanupPortForwards(t, name)
}

// livePortForwardEntriesExcept is liveWLANEntriesExcept for the other Resource
// with a prune of its own: every forward the Controller holds right now, apart
// from the named ones, written as config entries.
//
// A forward unifig cannot describe is skipped, which mirrors what unifig itself
// does with the same forward and is deliberate for the same reason: the config a
// prune test writes has to name everything unifig would otherwise delete, so an
// entry unifig could not have exported must not appear in it.
//
// `source` is left off every entry. It is the one optional field, so omitting it
// manages nothing about where traffic may come from — which keeps these configs
// to "match this one, change nothing about it", as the network entries are.
func livePortForwardEntriesExcept(t *testing.T, excluded ...string) string {
	t.Helper()

	var entries strings.Builder
	for _, forward := range testRig.portForwards(t) {
		name, _ := forward["name"].(string)
		if slices.Contains(excluded, name) {
			continue
		}
		port, single := singlePortValue(forward["dst_port"])
		forwardPort, singleForward := singlePortValue(forward["fwd_port"])
		address, _ := forward["fwd"].(string)
		protocol, _ := forward["proto"].(string)
		if !single || !singleForward || address == "" ||
			!slices.Contains([]string{"tcp", "udp", "tcp_udp"}, protocol) {
			continue
		}
		fmt.Fprintf(&entries,
			"  - name: %q\n    port: %d\n    forward-ip: %q\n    forward-port: %d\n    protocol: %q\n",
			name, port, address, forwardPort, protocol)
	}
	return entries.String()
}

// portForwardSection wraps those entries in the section header they belong to,
// writing the `port-forwards: []` form when there are none — the difference
// between a file that says nothing about forwards and one that says there should
// be none (ADR-0006).
func portForwardSection(entries string) string {
	if entries == "" {
		return "port-forwards: []\n"
	}
	return "port-forwards:\n" + entries
}

// singlePortValue reads a Controller port field as the single port unifig
// models. The Controller stores one as text; the rig accepts a number too rather
// than depending on which, since what a test needs from it is the same either
// way.
func singlePortValue(value any) (int, bool) {
	switch port := value.(type) {
	case string:
		parsed, err := strconv.Atoi(port)
		return parsed, err == nil && parsed >= 1 && parsed <= 65535
	case float64:
		return int(port), port >= 1 && port <= 65535
	default:
		return 0, false
	}
}

func TestPlanShowsAPortForwardToCreateAndExitsWithChangesPending(t *testing.T) {
	path := managedPortForward(t, `port-forwards:
  - name: Forward Plan Create
    port: 8443
    forward-ip: 10.20.0.10
    forward-port: 8123
    protocol: tcp
    source: 203.0.113.0/24
`, "Forward Plan Create")

	res := plan(t, path)

	if res.ExitCode != exitChangesPending {
		t.Fatalf("plan exited %d, want %d\nstderr: %s", res.ExitCode, exitChangesPending, res.Stderr)
	}
	stdout := string(res.Stdout)
	for _, fragment := range []string{
		`+ port-forward "Forward Plan Create"`, "8443", "10.20.0.10", "8123", "tcp", "203.0.113.0/24", "1 to create",
	} {
		if !strings.Contains(stdout, fragment) {
			t.Errorf("plan output should mention %q, got:\n%s", fragment, stdout)
		}
	}
}

// A forward unifig creates from a config that states no source is open to the
// whole internet, which is a consequence the config does not state — so the plan
// states it, before anyone is asked to approve anything.
func TestPlanWarnsThatAPortForwardWithNoSourceIsOpenToTheInternet(t *testing.T) {
	path := managedPortForward(t, `port-forwards:
  - name: Forward Open
    port: 8444
    forward-ip: 10.20.0.10
    forward-port: 8123
    protocol: tcp
`, "Forward Open")

	res := plan(t, path)

	if res.ExitCode != exitChangesPending {
		t.Fatalf("plan exited %d, want %d\nstderr: %s", res.ExitCode, exitChangesPending, res.Stderr)
	}
	// The value as well as the sentence: `any` is what the apply will write, and
	// a plan that printed only prose about it would be doing more than it said —
	// and would leave a pipeline reading the JSON nothing to gate on.
	for _, fragment := range []string{"source:", "any", "anywhere on the internet"} {
		if !strings.Contains(string(res.Stdout), fragment) {
			t.Errorf("plan should warn that this forward is open to anyone, looking for %q, got:\n%s",
				fragment, res.Stdout)
		}
	}
	if strings.Contains(string(res.Stdout), "source:       (none)") {
		t.Errorf("plan says the source is nothing, while the apply behind it writes %q:\n%s",
			"any", res.Stdout)
	}

	apply(t, path)

	if live := testRig.livePortForward(t, "Forward Open"); live["src"] != "any" {
		t.Errorf("src = %#v, want %q — the plan said anywhere", live["src"], "any")
	}
	assertNoChangesPending(t, path)
}

func TestApplyCreatesAPortForwardAndTheNextPlanIsEmpty(t *testing.T) {
	path := managedPortForward(t, `port-forwards:
  - name: Forward Apply Create
    port: 8445
    forward-ip: 10.20.0.10
    forward-port: 8123
    protocol: tcp_udp
    source: 203.0.113.4
`, "Forward Apply Create")

	res := apply(t, path)
	if !strings.Contains(string(res.Stdout), `+ port-forward "Forward Apply Create" created`) {
		t.Errorf("apply should report what it created, got:\n%s", res.Stdout)
	}

	live := testRig.livePortForward(t, "Forward Apply Create")
	for field, want := range map[string]any{
		"dst_port": "8445",
		"fwd":      "10.20.0.10",
		"fwd_port": "8123",
		"proto":    "tcp_udp",
		"src":      "203.0.113.4",
	} {
		if live[field] != want {
			t.Errorf("%s = %#v, want %#v", field, live[field], want)
		}
	}
	// The Controller will not forward anything for an entry that is off or bound
	// to no uplink, so a created forward has to arrive usable rather than as a row
	// in the Controller's database.
	if live["enabled"] != true {
		t.Errorf("enabled = %#v, so this forward would pass no traffic at all", live["enabled"])
	}
	if live["pfwd_interface"] != "wan" {
		t.Errorf("pfwd_interface = %#v, want the primary uplink", live["pfwd_interface"])
	}

	assertNoChangesPending(t, path)
}

func TestApplyUpdatesAPortForwardAndTheNextPlanIsEmpty(t *testing.T) {
	seedPortForwardTo(t, "Forward Apply Update", "8446", "10.20.0.10", "8123", "tcp")

	path := managedPortForward(t, `port-forwards:
  - name: Forward Apply Update
    port: 8446
    forward-ip: 10.20.0.11
    forward-port: 9123
    protocol: tcp
    source: 203.0.113.0/24
`)

	planned := plan(t, path)
	stdout := string(planned.Stdout)
	for _, fragment := range []string{
		`~ port-forward "Forward Apply Update"`,
		"10.20.0.10 -> 10.20.0.11",
		"8123 -> 9123",
		"any -> 203.0.113.0/24",
	} {
		if !strings.Contains(stdout, fragment) {
			t.Errorf("plan should show both ends of %q, got:\n%s", fragment, stdout)
		}
	}
	// The port agrees, so it is not a change and has no business in the plan.
	if strings.Contains(stdout, "8446") {
		t.Errorf("plan should list only the fields that differ, got:\n%s", stdout)
	}

	apply(t, path)

	live := testRig.livePortForward(t, "Forward Apply Update")
	for field, want := range map[string]any{
		"fwd": "10.20.0.11", "fwd_port": "9123", "src": "203.0.113.0/24", "dst_port": "8446",
	} {
		if live[field] != want {
			t.Errorf("%s = %#v after apply, want %#v", field, live[field], want)
		}
	}

	assertNoChangesPending(t, path)
}

// unifig owns the fields its config models and nothing else. A forward carries
// more than a mapping — which uplink it listens on, whether it logs, whether it
// is on at all — and an operator who set those in the Controller's UI must not
// lose them because they moved a service to another host in YAML.
func TestApplyLeavesPortForwardSettingsUnifigDoesNotModelAlone(t *testing.T) {
	testRig.seedPortForward(t, map[string]any{
		"name": "Forward Preserve", "enabled": false, "pfwd_interface": "wan",
		"dst_port": "8447", "fwd": "10.20.0.10", "fwd_port": "8123",
		"proto": "tcp", "src": "any", "destination_ip": "any", "log": true,
	})
	cleanupPortForwards(t, "Forward Preserve")

	path := managedPortForward(t, `port-forwards:
  - name: Forward Preserve
    port: 8447
    forward-ip: 10.20.0.11
    forward-port: 8123
    protocol: tcp
`)

	apply(t, path)

	live := testRig.livePortForward(t, "Forward Preserve")
	if live["fwd"] != "10.20.0.11" {
		t.Fatalf("the change under test did not happen: fwd = %#v", live["fwd"])
	}
	for field, want := range map[string]any{
		"enabled": false,
		"log":     true,
		"src":     "any",
	} {
		if live[field] != want {
			t.Errorf("%s = %#v after apply, want %#v — unifig does not model this field and must not touch it",
				field, live[field], want)
		}
	}
}

// A forward the config does not mention is not unifig's business, which is the
// same promise every other section makes: name one forward in the file and the
// rest are not at stake.
func TestApplyWithoutPruneLeavesUnlistedPortForwardsAlone(t *testing.T) {
	seedPortForwardTo(t, "Forward Untouched", "8448", "10.20.0.10", "8123", "tcp")

	path := managedPortForward(t, `port-forwards:
  - name: Forward Listed
    port: 8449
    forward-ip: 10.20.0.10
    forward-port: 8124
    protocol: tcp
`, "Forward Listed")

	res := apply(t, path)

	if strings.Contains(string(res.Stdout), "Forward Untouched") {
		t.Errorf("apply should say nothing about a forward the config does not list, got:\n%s", res.Stdout)
	}
	if found := testRig.portForwardsNamed(t, "Forward Untouched"); len(found) != 1 {
		t.Fatalf("apply deleted an unlisted forward nobody asked it to delete: %v", found)
	}
}

func TestApplyWithPruneDeletesPortForwardsTheConfigDoesNotName(t *testing.T) {
	seedPortForwardTo(t, "Forward Prune Applied", "8450", "10.20.0.10", "8123", "tcp")

	path := managedPortForward(t,
		portForwardSection(livePortForwardEntriesExcept(t, "Forward Prune Applied")))

	res := apply(t, "--prune", path)

	stdout := string(res.Stdout)
	if !strings.Contains(stdout, `- port-forward "Forward Prune Applied" deleted`) {
		t.Errorf("apply --prune should report what it deleted, got:\n%s", stdout)
	}
	// An operator about to close a service needs to see which one it was, and the
	// mapping says that where the name may not.
	for _, fragment := range []string{"8450", "10.20.0.10", "8123"} {
		if !strings.Contains(stdout, fragment) {
			t.Errorf("the deletion should show what the forward did, looking for %q:\n%s", fragment, stdout)
		}
	}
	if found := testRig.portForwardsNamed(t, "Forward Prune Applied"); len(found) != 0 {
		t.Fatalf("apply --prune left a forward the config does not name: %v", found)
	}

	assertNoChangesPending(t, "--prune", path)
}

// `port-forwards: []` is a statement, and it says the network should expose
// nothing.
func TestAnEmptyPortForwardSectionPutsEveryPortForwardAtStake(t *testing.T) {
	seedPortForwardTo(t, "Forward Prune Emptied", "8451", "10.20.0.10", "8123", "tcp")

	path := managedPortForward(t, "port-forwards: []\n")

	res := apply(t, "--prune", path)

	if !strings.Contains(string(res.Stdout), `- port-forward "Forward Prune Emptied" deleted`) {
		t.Errorf("apply --prune should report what it deleted, got:\n%s", res.Stdout)
	}
	if found := testRig.portForwardsNamed(t, "Forward Prune Emptied"); len(found) != 0 {
		t.Fatalf("an empty port-forwards section left a forward behind: %v", found)
	}
}

// ADR-0006, for this section: a file with no `port-forwards:` key says nothing
// about forwards, so a prune it takes part in has no business closing one.
func TestPruneLeavesAPortForwardSectionTheConfigDoesNotHaveAlone(t *testing.T) {
	seedPortForwardTo(t, "Forward Out Of Scope", "8452", "10.20.0.10", "8123", "tcp")
	testRig.seedNetwork(t, map[string]any{
		"name": "Forward Prune Bystander", "purpose": "corporate", "enabled": true,
		"vlan_enabled": true, "vlan": 171, "ip_subnet": "10.171.0.1/24",
	})
	cleanupNetworks(t, "Forward Prune Bystander")

	// Networks only, and one of them missing — so the prune under test really
	// runs and really deletes something, and the forward is spared on purpose
	// rather than because nothing happened.
	path := configFile(t, liveNetworksExcept(t, "Forward Prune Bystander"))

	res := apply(t, "--prune", path)

	if found := testRig.networksNamed(t, "Forward Prune Bystander"); len(found) != 0 {
		t.Fatalf("the prune under test did not happen: %v", found)
	}
	if strings.Contains(string(res.Stdout), "Forward Out Of Scope") {
		t.Errorf("apply --prune has an opinion about a section the config does not have:\n%s", res.Stdout)
	}
	if found := testRig.portForwardsNamed(t, "Forward Out Of Scope"); len(found) != 1 {
		t.Fatalf("prune deleted a forward from a config with no port-forwards section: %v", found)
	}
}

// The brownfield path for this section: what export writes describes the
// forwards exactly, so an operator's very first plan against a Controller they
// have not touched is empty.
func TestExportWritesThePortForwardsAndTheyPlanClean(t *testing.T) {
	seedPortForwardTo(t, "Forward Export", "8453", "10.20.0.10", "8123", "udp")
	testRig.seedPortForward(t, map[string]any{
		"name": "Forward Export Restricted", "enabled": true, "pfwd_interface": "wan",
		"dst_port": "8454", "fwd": "10.20.0.11", "fwd_port": "22",
		"proto": "tcp", "src": "203.0.113.0/24", "destination_ip": "any",
	})
	cleanupPortForwards(t, "Forward Export Restricted")

	exported := testRig.runUnifig(t, []string{"export"}, nil)
	if exported.ExitCode != 0 {
		t.Fatalf("unifig export exited %d\nstderr: %s", exported.ExitCode, exported.Stderr)
	}

	byName := map[string]exportedPortForward{}
	for _, forward := range exportedYAML(t, exported.Stdout).PortForwards {
		byName[forward.Name] = forward
	}
	open, ok := byName["Forward Export"]
	if !ok {
		t.Fatalf("export left out the seeded forward entirely:\n%s", exported.Stdout)
	}
	if open.Port != 8453 || open.ForwardIP != "10.20.0.10" || open.ForwardPort != 8123 || open.Protocol != "udp" {
		t.Errorf("exported %+v, want the mapping the Controller holds", open)
	}
	// The source is written rather than left to a default, because a file that
	// described a forward open to the whole internet by saying nothing about it
	// would be describing the exposure by omitting it.
	if open.Source != "any" {
		t.Errorf("source = %q, want %q — the file has to say who can reach this", open.Source, "any")
	}
	if restricted := byName["Forward Export Restricted"]; restricted.Source != "203.0.113.0/24" {
		t.Errorf("source = %q, want the restriction the Controller holds", restricted.Source)
	}

	path := filepath.Join(t.TempDir(), "unifig.yaml")
	if err := os.WriteFile(path, exported.Stdout, 0o600); err != nil {
		t.Fatalf("writing exported config: %v", err)
	}
	res := planEnv(t, exportedWLANSecretEnv(t, exported.Stdout), path)
	if res.ExitCode != exitNoChanges {
		t.Fatalf("plan of a freshly exported config exited %d, want %d\nexported:\n%s\nplan:\n%s",
			res.ExitCode, exitNoChanges, exported.Stdout, res.Stdout)
	}
}

// unifig models a single port at each end of a forward, and the Controller will
// hold a range or a list. Such a forward is out of unifig's reach entirely: not
// exported, and above all not pruned.
//
// The last of those is the one that would hurt, and it is the same failure a
// WLAN bound to something unnameable would have: a forward export left out of
// the file is a forward the file does not name, and if prune could still see it,
// the brownfield path would be "adopt your Controller, then close the port the
// adoption could not describe".
//
// Sparing it quietly would be the other half of the failure (ADR-0005): an
// operator who asked for a prune and got all but one of it has to be told which
// one, and why it stayed.
func TestAPortForwardUnifigCannotDescribeIsOutOfItsReach(t *testing.T) {
	// A range on one end has to be a range on the other: the Controller refuses
	// `27015-27020 -> 27015` outright (api.err.IncorrectMultiportFwdPort), which
	// is the shape a real multiport forward has anyway.
	seedPortForwardTo(t, "Forward Ranged", "27015-27020", "10.20.0.10", "27015-27020", "udp")

	// Something for the prune to actually delete, so this is a test about what a
	// working prune spares rather than about a prune that did nothing.
	seedPortForwardTo(t, "Forward Ranged Bystander", "8455", "10.20.0.10", "8123", "tcp")

	exported := testRig.runUnifig(t, []string{"export"}, nil)
	if exported.ExitCode != 0 {
		t.Fatalf("export exited %d — one forward it cannot describe should not stop it\nstderr: %s",
			exported.ExitCode, exported.Stderr)
	}
	for _, forward := range exportedYAML(t, exported.Stdout).PortForwards {
		if forward.Name == "Forward Ranged" {
			t.Errorf("export wrote a forward whose port it cannot state, as %+v:\n%s", forward, exported.Stdout)
		}
	}
	// Left out, but not left unsaid: export is the adoption path, and an operator
	// told the file describes their Controller should not discover otherwise later.
	if !strings.Contains(string(exported.Stderr), "Forward Ranged") {
		t.Errorf("export should say which forward it left out, got: %s", exported.Stderr)
	}

	path := managedPortForward(t,
		portForwardSection(livePortForwardEntriesExcept(t, "Forward Ranged Bystander")))
	res := apply(t, "--prune", path)

	if found := testRig.portForwardsNamed(t, "Forward Ranged Bystander"); len(found) != 0 {
		t.Fatalf("the prune under test did not happen: %v", found)
	}
	stdout := string(res.Stdout)
	if strings.Contains(stdout, `- port-forward "Forward Ranged"`) {
		t.Errorf("apply --prune proposed deleting a forward unifig cannot describe:\n%s", stdout)
	}
	for _, fragment := range []string{`"Forward Ranged" will not be deleted`, "range or a list"} {
		if !strings.Contains(stdout, fragment) {
			t.Errorf("the plan should say why it kept the forward, looking for %q:\n%s", fragment, stdout)
		}
	}
	if found := testRig.portForwardsNamed(t, "Forward Ranged"); len(found) != 1 {
		t.Fatalf("prune deleted a forward unifig could not have exported: %v", found)
	}
}

// The other side of the same forward: unifig cannot *export* one whose ports are
// a range, but it can still manage one the file names — that is the difference
// between what the config could not state and what the operator did state.
//
// What the plan has to get right is which half is changing. The port is, and it
// is shown as the range it really is rather than as a `(none)` claiming the
// Controller held nothing there; the address is not, and has no business in the
// plan at all.
func TestPlanNarrowsARangedPortForwardTheConfigNames(t *testing.T) {
	seedPortForwardTo(t, "Forward Narrowed", "27015-27020", "10.20.0.10", "27015-27020", "udp")

	path := managedPortForward(t, `port-forwards:
  - name: Forward Narrowed
    port: 27015
    forward-ip: 10.20.0.10
    forward-port: 27015
    protocol: udp
`)

	res := plan(t, path)

	if res.ExitCode != exitChangesPending {
		t.Fatalf("plan exited %d, want %d\nstderr: %s", res.ExitCode, exitChangesPending, res.Stderr)
	}
	stdout := string(res.Stdout)
	for _, fragment := range []string{"port:", "27015-27020 -> 27015"} {
		if !strings.Contains(stdout, fragment) {
			t.Errorf("plan should show the range the forward is narrowing from, looking for %q, got:\n%s",
				fragment, stdout)
		}
	}
	if strings.Contains(stdout, "forward-ip") {
		t.Errorf("plan proposes changing an address that agrees with the Controller:\n%s", stdout)
	}

	apply(t, path)

	live := testRig.livePortForward(t, "Forward Narrowed")
	if live["dst_port"] != "27015" || live["fwd_port"] != "27015" {
		t.Errorf("ports = %#v -> %#v after apply, want the single ports the config named",
			live["dst_port"], live["fwd_port"])
	}

	assertNoChangesPending(t, path)
}

// ADR-0001 matches Resources by name and calls a duplicate a hard error rather
// than a guess. The Controller does permit two forwards to share a name, so this
// is a state unifig can really meet.
func TestPlanRefusesToGuessBetweenTwoPortForwardsSharingAName(t *testing.T) {
	seedPortForwardTo(t, "Forward Twice", "8456", "10.20.0.10", "8123", "tcp")
	// seedPortForward clears same-named entries first, so the twin goes in
	// through the Controller's own API directly.
	testRig.addPortForward(t, map[string]any{
		"name": "Forward Twice", "enabled": true, "pfwd_interface": "wan",
		"dst_port": "8457", "fwd": "10.20.0.11", "fwd_port": "8124",
		"proto": "tcp", "src": "any", "destination_ip": "any",
	})

	path := managedPortForward(t, `port-forwards:
  - name: Forward Twice
    port: 8456
    forward-ip: 10.20.0.10
    forward-port: 8123
    protocol: tcp
`)

	res := plan(t, path)

	if res.ExitCode != exitError {
		t.Fatalf("plan exited %d, want %d — it picked one of two forwards named the same\nstdout: %s",
			res.ExitCode, exitError, res.Stdout)
	}
	stderr := string(res.Stderr)
	for _, fragment := range []string{"Forward Twice", "UI"} {
		if !strings.Contains(stderr, fragment) {
			t.Errorf("stderr should mention %q, got: %s", fragment, stderr)
		}
	}
}
