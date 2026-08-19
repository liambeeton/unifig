package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A DHCP Reservation is the one Resource that is not a Controller object. The
// Controller keeps a record per client it has ever seen, and a reservation is
// the fixed-IP half of that record — so these tests state the two things an
// operator has to be able to trust about that arrangement: the half unifig
// manages really reconciles, and the rest of the record is never collateral.
//
// As everywhere in this suite, the assertions are on what a shell would see or
// on what the Controller itself reports afterwards.

// managedReservation writes a config file and forgets the named clients from the
// Controller when the test ends — managedPortForward's counterpart.
func managedReservation(t *testing.T, body string, macs ...string) string {
	t.Helper()
	cleanupClients(t, macs...)
	return configFile(t, body)
}

// cleanupClients forgets the client records a test created or seeded.
//
// It forgets rather than un-reserves, which is the rig doing something unifig
// deliberately will not: giving up a reservation leaves the record behind
// (ADR-0015), so tidying up through unifig would hand the next test a client
// record it never seeded.
func cleanupClients(t *testing.T, macs ...string) {
	t.Helper()
	for _, mac := range macs {
		t.Cleanup(func() { testRig.forgetClients(t, mac) })
	}
}

// seedReservationFor puts a client record with a fixed address on the
// Controller through its own API, in the shape its UI creates.
func seedReservationFor(t *testing.T, mac, ip string) {
	t.Helper()
	testRig.seedReservation(t, map[string]any{
		"mac": mac, "use_fixedip": true, "fixed_ip": ip,
	})
	cleanupClients(t, mac)
}

func TestPlanShowsADHCPReservationToCreateAndExitsWithChangesPending(t *testing.T) {
	path := managedReservation(t, `dhcp-reservations:
  - mac: "00:11:22:33:44:01"
    ip: 192.168.1.51
`, "00:11:22:33:44:01")

	res := plan(t, path)

	if res.ExitCode != exitChangesPending {
		t.Fatalf("plan exited %d, want %d\nstderr: %s", res.ExitCode, exitChangesPending, res.Stderr)
	}
	stdout := string(res.Stdout)
	for _, fragment := range []string{
		`+ dhcp-reservation "00:11:22:33:44:01"`, "192.168.1.51", "1 to create",
	} {
		if !strings.Contains(stdout, fragment) {
			t.Errorf("plan output should mention %q, got:\n%s", fragment, stdout)
		}
	}
}

func TestApplyCreatesADHCPReservationAndTheNextPlanIsEmpty(t *testing.T) {
	path := managedReservation(t, `dhcp-reservations:
  - mac: "00:11:22:33:44:02"
    ip: 192.168.1.52
`, "00:11:22:33:44:02")

	res := apply(t, path)
	if !strings.Contains(string(res.Stdout), `+ dhcp-reservation "00:11:22:33:44:02" created`) {
		t.Errorf("apply should report what it created, got:\n%s", res.Stdout)
	}

	live := testRig.liveClient(t, "00:11:22:33:44:02")
	if live["use_fixedip"] != true {
		t.Errorf("use_fixedip = %#v, so this client has no reservation at all", live["use_fixedip"])
	}
	if live["fixed_ip"] != "192.168.1.52" {
		t.Errorf("fixed_ip = %#v, want %q", live["fixed_ip"], "192.168.1.52")
	}

	assertNoChangesPending(t, path)
}

// A MAC is the reservation's natural key and the one key in unifig that is not
// case-sensitive, because the Controller lower-cases every MAC it stores. A file
// written in upper case therefore has to match the Controller's own record —
// otherwise every plan would propose creating a reservation that already exists,
// and the apply behind it would be refused as a duplicate MAC.
func TestAReservationMatchesTheControllerWhateverCaseTheMACIsWrittenIn(t *testing.T) {
	seedReservationFor(t, "aa:bb:cc:dd:ee:03", "192.168.1.53")

	path := managedReservation(t, `dhcp-reservations:
  - mac: "AA:BB:CC:DD:EE:03"
    ip: 192.168.1.53
`)

	assertNoChangesPending(t, path)
}

// The other half of that: a reservation unifig creates from a config written in
// upper case is stored in the Controller's own lower case, so the next read
// matches it without anything having to fold case a second time.
func TestAReservationIsWrittenInTheControllersOwnCase(t *testing.T) {
	path := managedReservation(t, `dhcp-reservations:
  - mac: "AA:BB:CC:DD:EE:04"
    ip: 192.168.1.54
`, "AA:BB:CC:DD:EE:04")

	apply(t, path)

	live := testRig.liveClient(t, "aa:bb:cc:dd:ee:04")
	if live["mac"] != "aa:bb:cc:dd:ee:04" {
		t.Errorf("mac = %#v, want the Controller's own lower case", live["mac"])
	}
	assertNoChangesPending(t, path)
}

func TestApplyUpdatesADHCPReservationAndTheNextPlanIsEmpty(t *testing.T) {
	seedReservationFor(t, "00:11:22:33:44:05", "192.168.1.55")

	path := managedReservation(t, `dhcp-reservations:
  - mac: "00:11:22:33:44:05"
    ip: 192.168.1.155
`)

	planned := plan(t, path)
	stdout := string(planned.Stdout)
	for _, fragment := range []string{
		`~ dhcp-reservation "00:11:22:33:44:05"`, "192.168.1.55 -> 192.168.1.155",
	} {
		if !strings.Contains(stdout, fragment) {
			t.Errorf("plan should show both ends of the move, looking for %q, got:\n%s", fragment, stdout)
		}
	}

	apply(t, path)

	if live := testRig.liveClient(t, "00:11:22:33:44:05"); live["fixed_ip"] != "192.168.1.155" {
		t.Errorf("fixed_ip = %#v after apply, want %q", live["fixed_ip"], "192.168.1.155")
	}

	assertNoChangesPending(t, path)
}

// The acceptance criterion this whole type is shaped by. A reservation is two
// fields of a record that is mostly not unifig's: the client's name, the note an
// operator left on it, its user group and whether it is blocked all belong to
// whoever set them in the Controller's UI, and moving a fixed address in YAML
// must not cost any of them.
func TestApplyLeavesTheRestOfTheClientRecordUntouched(t *testing.T) {
	testRig.seedReservation(t, map[string]any{
		"mac": "00:11:22:33:44:06", "name": "Study NAS", "note": "on the shelf",
		"noted": true, "blocked": true, "use_fixedip": true, "fixed_ip": "192.168.1.56",
	})
	cleanupClients(t, "00:11:22:33:44:06")

	path := managedReservation(t, `dhcp-reservations:
  - mac: "00:11:22:33:44:06"
    ip: 192.168.1.156
`)

	apply(t, path)

	live := testRig.liveClient(t, "00:11:22:33:44:06")
	if live["fixed_ip"] != "192.168.1.156" {
		t.Fatalf("the change under test did not happen: fixed_ip = %#v", live["fixed_ip"])
	}
	for field, want := range map[string]any{
		"name":    "Study NAS",
		"note":    "on the shelf",
		"noted":   true,
		"blocked": true,
	} {
		if live[field] != want {
			t.Errorf("%s = %#v after apply, want %#v — unifig does not model this field and must not touch it",
				field, live[field], want)
		}
	}
}

// The same promise as the test above, made about the whole record rather than
// about four fields somebody thought to name — which on this type is the
// promise, because a reservation *is* two fields of somebody else's record
// (ADR-0015). Everything on it that unifig does not manage belongs to whoever
// set it in the UI, and there is no list of what that might be.
//
// The record is seeded with the fields a Controller puts on one and nothing
// else, so what the apply writes shows up against it. Before #39 an update
// wrote five fields at Go's zero onto a record holding thirteen — `network_id`,
// `fixed_ap_enabled` and two `virtual_network_override_*` switches among them —
// none of them unifig's to have an opinion about, and none of them in the plan.
func TestApplyingAReservationChangeWritesNoFieldTheConfigDoesNotState(t *testing.T) {
	testRig.seedReservation(t, map[string]any{
		"mac": "00:11:22:33:44:0e", "name": "Only Stated NAS", "note": "on the shelf",
		"noted": true, "blocked": true, "use_fixedip": true, "fixed_ip": "192.168.1.64",
	})
	cleanupClients(t, "00:11:22:33:44:0e")

	path := configFile(t, `dhcp-reservations:
  - mac: "00:11:22:33:44:0e"
    ip: 192.168.1.164
`)

	before := testRig.liveClient(t, "00:11:22:33:44:0e")

	apply(t, path)

	after := testRig.liveClient(t, "00:11:22:33:44:0e")
	assertOnlyTheseFieldsMoved(t, before, after, "fixed_ip")
}

// A client the Controller already knows but holds no reservation for is a
// *create* of the reservation, because the reservation is what did not exist.
// The record underneath it does, so the write is an edit to that record rather
// than a second one — which the Controller would refuse anyway — and everything
// the operator put on it in the UI survives being given an address.
func TestReservingAnAddressForAKnownClientKeepsTheRecordItAlreadyHad(t *testing.T) {
	testRig.seedReservation(t, map[string]any{
		"mac": "00:11:22:33:44:07", "name": "Known Client", "note": "seen before", "noted": true,
	})
	cleanupClients(t, "00:11:22:33:44:07")

	path := managedReservation(t, `dhcp-reservations:
  - mac: "00:11:22:33:44:07"
    ip: 192.168.1.57
`)

	res := apply(t, path)

	if !strings.Contains(string(res.Stdout), `+ dhcp-reservation "00:11:22:33:44:07" created`) {
		t.Errorf("adding an address to a known client creates a reservation, got:\n%s", res.Stdout)
	}
	live := testRig.liveClient(t, "00:11:22:33:44:07")
	if live["fixed_ip"] != "192.168.1.57" || live["use_fixedip"] != true {
		t.Fatalf("the change under test did not happen: %#v / %#v", live["use_fixedip"], live["fixed_ip"])
	}
	if live["name"] != "Known Client" || live["note"] != "seen before" {
		t.Errorf("name = %#v, note = %#v — reserving an address must not cost the record its own fields",
			live["name"], live["note"])
	}

	assertNoChangesPending(t, path)
}

// A client record with the fixed address switched off is not a Reservation, and
// the address left lying on it describes nothing about how that client is
// addressed today — the same reading unifig gives a passphrase on a WLAN the
// Controller now holds as open. So the config naming it is creating a
// reservation rather than agreeing with one.
func TestAClientWithItsFixedAddressSwitchedOffHasNoReservation(t *testing.T) {
	testRig.seedReservation(t, map[string]any{
		"mac": "00:11:22:33:44:08", "use_fixedip": false, "fixed_ip": "192.168.1.58",
	})
	cleanupClients(t, "00:11:22:33:44:08")

	path := managedReservation(t, `dhcp-reservations:
  - mac: "00:11:22:33:44:08"
    ip: 192.168.1.58
`)

	res := plan(t, path)

	if res.ExitCode != exitChangesPending {
		t.Fatalf("plan exited %d, want %d — a switched-off address is not a reservation\nstdout: %s",
			res.ExitCode, exitChangesPending, res.Stdout)
	}
	if !strings.Contains(string(res.Stdout), `+ dhcp-reservation "00:11:22:33:44:08"`) {
		t.Errorf("plan should create the reservation, got:\n%s", res.Stdout)
	}

	apply(t, path)

	if live := testRig.liveClient(t, "00:11:22:33:44:08"); live["use_fixedip"] != true {
		t.Errorf("use_fixedip = %#v after apply, want the address switched on", live["use_fixedip"])
	}
	assertNoChangesPending(t, path)
}

// A reservation the config does not mention is not unifig's business, which is
// the same promise every other section makes.
func TestApplyWithoutPruneLeavesUnlistedReservationsAlone(t *testing.T) {
	seedReservationFor(t, "00:11:22:33:44:09", "192.168.1.59")

	path := managedReservation(t, `dhcp-reservations:
  - mac: "00:11:22:33:44:10"
    ip: 192.168.1.60
`, "00:11:22:33:44:10")

	res := apply(t, path)

	if strings.Contains(string(res.Stdout), "00:11:22:33:44:09") {
		t.Errorf("apply should say nothing about a reservation the config does not list, got:\n%s", res.Stdout)
	}
	if live := testRig.liveClient(t, "00:11:22:33:44:09"); live["fixed_ip"] != "192.168.1.59" {
		t.Fatalf("apply moved an unlisted reservation nobody asked it to move: %#v", live["fixed_ip"])
	}
}

// liveReservationEntriesExcept is livePortForwardEntriesExcept for the section
// whose live collection is not its own: every reservation the Controller holds
// right now, apart from the named ones, written as config entries.
//
// It walks the client records and keeps the ones with an address switched on,
// which is the same filter unifig applies — the config a prune test writes has to
// name everything unifig would otherwise give up, and a client the Controller
// merely remembers is not one of them.
func liveReservationEntriesExcept(t *testing.T, excluded ...string) string {
	t.Helper()

	var entries strings.Builder
	for _, client := range testRig.clients(t) {
		mac, _ := client["mac"].(string)
		address, _ := client["fixed_ip"].(string)
		if client["use_fixedip"] != true || address == "" {
			continue
		}
		if slices.ContainsFunc(excluded, func(e string) bool { return strings.EqualFold(e, mac) }) {
			continue
		}
		fmt.Fprintf(&entries, "  - mac: %q\n    ip: %q\n", mac, address)
	}
	return entries.String()
}

// reservationSection wraps those entries in the section header they belong to,
// writing the `dhcp-reservations: []` form when there are none — the difference
// between a file that says nothing about reservations and one that says there
// should be none (ADR-0006).
func reservationSection(entries string) string {
	if entries == "" {
		return "dhcp-reservations: []\n"
	}
	return "dhcp-reservations:\n" + entries
}

// The deletion this type has instead of a deletion: prune gives the address up
// and leaves the client record exactly where it was (ADR-0015). An operator
// approving a plan line that reads `- dhcp-reservation` has to be told that,
// because it reads like the device being forgotten and it is not.
func TestApplyWithPruneGivesUpReservationsTheConfigDoesNotNameWithoutForgettingTheClient(t *testing.T) {
	testRig.seedReservation(t, map[string]any{
		"mac": "00:11:22:33:44:11", "name": "Pruned NAS", "note": "keep me", "noted": true,
		"use_fixedip": true, "fixed_ip": "192.168.1.61",
	})
	cleanupClients(t, "00:11:22:33:44:11")

	path := managedReservation(t,
		reservationSection(liveReservationEntriesExcept(t, "00:11:22:33:44:11")))

	res := apply(t, "--prune", path)

	stdout := string(res.Stdout)
	if !strings.Contains(stdout, `- dhcp-reservation "00:11:22:33:44:11" deleted`) {
		t.Errorf("apply --prune should report what it gave up, got:\n%s", stdout)
	}
	// The address, so an operator can tell which reservation this was, and the
	// sentence saying the device keeps everything else.
	for _, fragment := range []string{"192.168.1.61", "Pruned NAS", "record stays"} {
		if !strings.Contains(stdout, fragment) {
			t.Errorf("the deletion should say what it does and does not do, looking for %q:\n%s",
				fragment, stdout)
		}
	}

	live := testRig.liveClient(t, "00:11:22:33:44:11")
	if live["use_fixedip"] != false {
		t.Fatalf("use_fixedip = %#v — the reservation was not given up", live["use_fixedip"])
	}
	if live["name"] != "Pruned NAS" || live["note"] != "keep me" {
		t.Errorf("name = %#v, note = %#v — prune forgot the device instead of the address",
			live["name"], live["note"])
	}

	assertNoChangesPending(t, "--prune", path)
}

// `dhcp-reservations: []` is a statement, and it says no client should have a
// fixed address. It says nothing at all about which clients the Controller knows.
func TestAnEmptyReservationSectionPutsEveryReservationAtStakeAndNoClientRecord(t *testing.T) {
	seedReservationFor(t, "00:11:22:33:44:12", "192.168.1.62")
	testRig.seedReservation(t, map[string]any{"mac": "00:11:22:33:44:13", "name": "Bystander", "noted": true})
	cleanupClients(t, "00:11:22:33:44:13")

	path := managedReservation(t, "dhcp-reservations: []\n")

	res := apply(t, "--prune", path)

	stdout := string(res.Stdout)
	if !strings.Contains(stdout, `- dhcp-reservation "00:11:22:33:44:12" deleted`) {
		t.Errorf("apply --prune should report what it gave up, got:\n%s", stdout)
	}
	if strings.Contains(stdout, "00:11:22:33:44:13") {
		t.Errorf("a client with no reservation is not of a managed type and has no business in a prune:\n%s",
			stdout)
	}
	if live := testRig.liveClient(t, "00:11:22:33:44:13"); live["name"] != "Bystander" {
		t.Fatalf("prune touched a client record that holds no reservation: %#v", live)
	}
}

// ADR-0006, for this section: a file with no `dhcp-reservations:` key says
// nothing about reservations, so a prune it takes part in has no business giving
// one up.
func TestPruneLeavesAReservationSectionTheConfigDoesNotHaveAlone(t *testing.T) {
	seedReservationFor(t, "00:11:22:33:44:14", "192.168.1.64")
	testRig.seedNetwork(t, map[string]any{
		"name": "Reservation Prune Bystander", "purpose": "corporate", "enabled": true,
		"vlan_enabled": true, "vlan": 181, "ip_subnet": "10.181.0.1/24",
	})
	cleanupNetworks(t, "Reservation Prune Bystander")

	// Networks only, and one of them missing — so the prune under test really
	// runs and really deletes something, and the reservation is spared on purpose
	// rather than because nothing happened.
	path := configFile(t, liveNetworksExcept(t, "Reservation Prune Bystander"))

	res := apply(t, "--prune", path)

	if found := testRig.networksNamed(t, "Reservation Prune Bystander"); len(found) != 0 {
		t.Fatalf("the prune under test did not happen: %v", found)
	}
	if strings.Contains(string(res.Stdout), "00:11:22:33:44:14") {
		t.Errorf("apply --prune has an opinion about a section the config does not have:\n%s", res.Stdout)
	}
	if live := testRig.liveClient(t, "00:11:22:33:44:14"); live["use_fixedip"] != true {
		t.Fatalf("prune gave up a reservation from a config with no dhcp-reservations section: %#v", live)
	}
}

// ADR-0014 for the reference the Controller infers rather than stores. A
// reservation names no network, but the Controller refuses to delete a network
// with an address reserved inside its subnet — `FIXED_IP_OVERLAPS_NETWORK_SUBNET`
// — so a plan proposing that deletion would be promising something that cannot
// happen.
func TestPruneWillNotDeleteANetworkWithAnAddressReservedInsideIt(t *testing.T) {
	testRig.seedNetwork(t, map[string]any{
		"name": "Reservation Held", "purpose": "corporate", "enabled": true,
		"vlan_enabled": true, "vlan": 182, "ip_subnet": "10.182.0.1/24",
		"dhcpd_enabled": true, "dhcpd_start": "10.182.0.6", "dhcpd_stop": "10.182.0.254",
	})
	cleanupNetworks(t, "Reservation Held")
	seedReservationFor(t, "00:11:22:33:44:15", "10.182.0.50")

	// Something for the prune to actually delete, so this is a test about what a
	// working prune spares rather than about a prune that did nothing.
	testRig.seedNetwork(t, map[string]any{
		"name": "Reservation Held Bystander", "purpose": "corporate", "enabled": true,
		"vlan_enabled": true, "vlan": 183, "ip_subnet": "10.183.0.1/24",
	})
	cleanupNetworks(t, "Reservation Held Bystander")

	path := configFile(t, liveNetworksExcept(t, "Reservation Held", "Reservation Held Bystander"))

	res := apply(t, "--prune", path)

	if found := testRig.networksNamed(t, "Reservation Held Bystander"); len(found) != 0 {
		t.Fatalf("the prune under test did not happen: %v", found)
	}
	stdout := string(res.Stdout)
	if strings.Contains(stdout, `- network "Reservation Held"`) {
		t.Errorf("apply --prune proposed a deletion the Controller would have refused:\n%s", stdout)
	}
	for _, fragment := range []string{
		`"Reservation Held" will not be deleted`, "00:11:22:33:44:15", "reserving an address inside it",
	} {
		if !strings.Contains(stdout, fragment) {
			t.Errorf("the plan should say why it kept the network, looking for %q:\n%s", fragment, stdout)
		}
	}
	if found := testRig.networksNamed(t, "Reservation Held"); len(found) != 1 {
		t.Fatalf("prune deleted a network the Controller would not have let go of: %v", found)
	}
}

// The brownfield path for this section: what export writes describes the
// reservations exactly, so an operator's very first plan against a Controller
// they have not touched is empty.
func TestExportWritesTheReservationsAndTheyPlanClean(t *testing.T) {
	seedReservationFor(t, "00:11:22:33:44:16", "192.168.1.66")
	// A client the Controller knows and holds no address for. It must not appear
	// in the file at all: export writes the reservations, not the address book.
	testRig.seedReservation(t, map[string]any{"mac": "00:11:22:33:44:17", "name": "Not Reserved", "noted": true})
	cleanupClients(t, "00:11:22:33:44:17")

	exported := testRig.runUnifig(t, []string{"export"}, nil)
	if exported.ExitCode != 0 {
		t.Fatalf("unifig export exited %d\nstderr: %s", exported.ExitCode, exported.Stderr)
	}

	byMAC := map[string]exportedReservation{}
	for _, reservation := range exportedYAML(t, exported.Stdout).DHCPReservations {
		byMAC[reservation.MAC] = reservation
	}
	reserved, ok := byMAC["00:11:22:33:44:16"]
	if !ok {
		t.Fatalf("export left out the seeded reservation entirely:\n%s", exported.Stdout)
	}
	if reserved.IP != "192.168.1.66" {
		t.Errorf("exported %+v, want the address the Controller holds", reserved)
	}
	if _, wrote := byMAC["00:11:22:33:44:17"]; wrote {
		t.Errorf("export wrote a client that has no reservation:\n%s", exported.Stdout)
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

// A network can be held back by more than one kind of thing at once, and this is
// the first time that happens: a WLAN is on it and an address is reserved inside
// it. An operator told their network stays is owed the whole of what is keeping
// it — one sentence naming both, rather than the first thing found.
func TestPruneNamesEverythingKeepingANetworkNotJustTheFirst(t *testing.T) {
	testRig.seedNetwork(t, map[string]any{
		"name": "Reservation Held Twice", "purpose": "corporate", "enabled": true,
		"vlan_enabled": true, "vlan": 184, "ip_subnet": "10.184.0.1/24",
		"dhcpd_enabled": true, "dhcpd_start": "10.184.0.6", "dhcpd_stop": "10.184.0.254",
	})
	cleanupNetworks(t, "Reservation Held Twice")
	testRig.seedWLAN(t, map[string]any{
		"name": "Held Twice Wi-Fi", "enabled": true, "security": "open",
		"networkconf_id": testRig.networkID(t, "Reservation Held Twice"),
	})
	t.Cleanup(func() { testRig.deleteWLANsNamed(t, "Held Twice Wi-Fi") })
	seedReservationFor(t, "00:11:22:33:44:18", "10.184.0.50")

	// Networks only: the WLAN section is absent, so nothing in this plan deletes
	// the WLAN, and the reservations section is absent too — both hold the
	// network on the same terms (ADR-0006).
	path := configFile(t, liveNetworksExcept(t, "Reservation Held Twice"))

	res := apply(t, "--prune", path)

	stdout := string(res.Stdout)
	if strings.Contains(stdout, `- network "Reservation Held Twice"`) {
		t.Errorf("apply --prune proposed a deletion the Controller would have refused:\n%s", stdout)
	}
	// One caveat naming both, rather than two caveats or one that stops early.
	for _, fragment := range []string{
		`the WLAN "Held Twice Wi-Fi" on it`,
		`the DHCP reservation "00:11:22:33:44:18" reserving an address inside it`,
		" and ",
	} {
		if !strings.Contains(stdout, fragment) {
			t.Errorf("the caveat should name everything keeping the network, looking for %q:\n%s",
				fragment, stdout)
		}
	}
	if strings.Count(stdout, `"Reservation Held Twice" will not be deleted`) != 1 {
		t.Errorf("one network held back is one caveat, got:\n%s", stdout)
	}
}

// The other half of the hold-back, and the half a WLAN does not have. A WLAN
// names the network its clients join and the schema makes that name a network
// the same file defines, so a WLAN being created can never hold back a network
// prune was free to take. A reservation names no network at all — the Controller
// reads one off the address — so a file can perfectly well reserve an address
// inside a network it never mentions, and prune must see that coming rather than
// planning a deletion the Controller will refuse.
func TestPruneWillNotDeleteANetworkThisPlanIsAboutToReserveAnAddressInside(t *testing.T) {
	testRig.seedNetwork(t, map[string]any{
		"name": "Reservation Created Held", "purpose": "corporate", "enabled": true,
		"vlan_enabled": true, "vlan": 185, "ip_subnet": "10.185.0.1/24",
		"dhcpd_enabled": true, "dhcpd_start": "10.185.0.6", "dhcpd_stop": "10.185.0.254",
	})
	cleanupNetworks(t, "Reservation Created Held")
	// Something for the prune to actually delete, so this is a test about what a
	// working prune spares rather than about a prune that did nothing.
	testRig.seedNetwork(t, map[string]any{
		"name": "Reservation Created Bystander", "purpose": "corporate", "enabled": true,
		"vlan_enabled": true, "vlan": 186, "ip_subnet": "10.186.0.1/24",
	})
	cleanupNetworks(t, "Reservation Created Bystander")

	// The file names neither network, and reserves an address inside the first.
	path := managedReservation(t,
		liveNetworksExcept(t, "Reservation Created Held", "Reservation Created Bystander")+
			`dhcp-reservations:
  - mac: "00:11:22:33:44:19"
    ip: 10.185.0.50
`, "00:11:22:33:44:19")

	res := apply(t, "--prune", path)

	if found := testRig.networksNamed(t, "Reservation Created Bystander"); len(found) != 0 {
		t.Fatalf("the prune under test did not happen: %v", found)
	}
	stdout := string(res.Stdout)
	if strings.Contains(stdout, `- network "Reservation Created Held"`) {
		t.Errorf("apply --prune proposed a deletion the Controller would have refused:\n%s", stdout)
	}
	for _, fragment := range []string{
		`"Reservation Created Held" will not be deleted`,
		`"00:11:22:33:44:19" reserving an address inside it`,
	} {
		if !strings.Contains(stdout, fragment) {
			t.Errorf("the plan should say why it kept the network, looking for %q:\n%s", fragment, stdout)
		}
	}
	if found := testRig.networksNamed(t, "Reservation Created Held"); len(found) != 1 {
		t.Fatalf("prune deleted a network the Controller would not have let go of: %v", found)
	}
	if live := testRig.liveClient(t, "00:11:22:33:44:19"); live["fixed_ip"] != "10.185.0.50" {
		t.Errorf("fixed_ip = %#v — the reservation the hold-back is about was not created", live["fixed_ip"])
	}
}
