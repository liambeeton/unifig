package reconcile

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/filipowm/go-unifi/v2/unifi"

	"github.com/liambeeton/unifig/internal/config"
)

// A DHCP Reservation is the one managed type with no object of its own on the
// Controller. Everything else here is a row the Controller stores because
// unifig or an operator asked it to; a reservation is two fields of a record
// the Controller keeps anyway — one per client it has ever seen, carrying that
// client's name, note, user group and whether it is blocked. unifig writes
// `use_fixedip` and `fixed_ip` and reads the rest only to hand it straight back
// (ADR-0015).
//
// That shapes every verb in this file:
//
//   - What is live is not the collection. `rest/user` answers with every client
//     record the site holds, and the Reservations are the ones with
//     `use_fixedip` set. A record without it is a client unifig manages nothing
//     about, which is why prune cannot see one and export does not write one.
//   - A create is not always a POST. The Reservation is what does not exist; the
//     record underneath it may. So a client the Controller already knows gets
//     its record updated, and the plan still says `+`, because what the operator
//     is adding is the reservation.
//   - A delete gives the address up rather than forgetting the device. That is
//     the whole of ADR-0015, and the plan says so on the line, because
//     `- dhcp-reservation` would otherwise read as the record going too.
//
// The key is the MAC address, and it is the one natural key in unifig that is
// not case-sensitive: the Controller lower-cases every MAC it stores, so the
// config's `AA:BB:…` and the Controller's `aa:bb:…` are one client. Matching
// folds both, and everything unifig writes or exports is in the Controller's
// own lower case.
//
// There is no network named anywhere here, and that is the Controller's design
// rather than a gap in unifig's. Which network a reservation belongs to is
// decided by whose subnet the address falls in — an address inside no network's
// subnet is refused outright (`api.err.InvalidFixedIP`), and a network with an
// address reserved inside it cannot be deleted (`api.err.ResourceReferredBy`,
// reference type `FIXED_IP_OVERLAPS_NETWORK_SUBNET`). The record does carry a
// network of its own, and unifig neither writes nor reads it, because it is not
// what the Controller consults.
//
// None of it is Risky by the test ADR-0012 settled. A client that loses its
// fixed address takes a dynamic one from the same network's pool; the Controller
// stays reachable, and the way back is to put the reservation back. Recovery
// never needs physical access.

// planDHCPReservations is the reservation half of a reconcile. Its caller only
// reaches it when the config has a `dhcp-reservations:` section at all
// (ADR-0006), so a file that says nothing about reservations changes none of
// them — though its caller may well have read them anyway, because a prune has
// to know what has an address reserved inside a network before it can propose
// deleting it (ADR-0014).
//
// It reports the live reservations it leaves in place alongside the changes, on
// the same terms as planWLANs and for the same reason: a reservation this plan
// gives up is not one standing between a network and prune.
func planDHCPReservations(
	cfg config.Config,
	live []unifi.User,
	opts Options,
) ([]Change, []unifi.User, error) {
	reservations := reservationsAmong(live)
	if err := uniquelyReservedMACs(reservations); err != nil {
		return nil, nil, err
	}

	byMAC := make(map[string]unifi.User, len(reservations))
	for _, reservation := range reservations {
		byMAC[config.NormalisedMAC(reservation.MAC)] = reservation
	}
	// The records the Controller holds no reservation for, so that a create can
	// tell "this client is new" from "this client is known and has no address
	// reserved" — two different writes behind one plan line.
	records := make(map[string]unifi.User, len(live))
	for _, record := range live {
		records[config.NormalisedMAC(record.MAC)] = record
	}

	changes := make([]Change, 0, len(cfg.DHCPReservations))
	reservedFor := make(map[string]bool, len(cfg.DHCPReservations))
	for _, desired := range cfg.DHCPReservations {
		mac := config.NormalisedMAC(desired.MAC)
		reservedFor[mac] = true

		current, exists := byMAC[mac]
		if !exists {
			changes = append(changes, createDHCPReservation(desired, records[mac]))
			continue
		}
		if change, differs := updateDHCPReservation(desired, current); differs {
			changes = append(changes, change)
		}
	}
	if !opts.Prune {
		return changes, reservations, nil
	}
	deletions, spared := pruneDHCPReservations(reservations, reservedFor)
	return append(changes, deletions...), spared, nil
}

// reservedWithin names the networks a reservation this plan leaves in place will
// have an address inside once it has run, against the phrase naming that
// reservation — what prune asks before proposing to delete a network, and what
// the Caveat about the one it did not propose says (ADR-0014).
//
// It is networksInUse read through the Controller's own rule for this type. A
// WLAN says which network it is on; a reservation says an address, and the
// Controller decides the network from it — `FIXED_IP_OVERLAPS_NETWORK_SUBNET` is
// the reason it gives for refusing to delete a network out from under one. So
// this asks the same question by containment rather than by name, and the answer
// holds the network back on exactly the terms a WLAN's does.
//
// "Will have" rather than "has", for the reason networksInUse says "will be on":
// a file moving a reservation to another network's subnet states the network it
// is leaving nowhere, so prune can propose deleting that one, and the move is
// applied before any deletion is.
//
// Where it parts company with networksInUse is that a reservation this plan is
// *creating* counts, and that is the whole reason this walks the config as well
// as the live collection. networksInUse leaves creates out and says so, on the
// grounds that a config stating a WLAN states the network its clients join, so
// that network is named in the file and was never prune's to take. Nothing of
// the sort holds here: a reservation names no network, the Controller reads one
// off the address, and so a file can perfectly well reserve an address inside a
// network it never mentions. Leaving those out planned `+ dhcp-reservation`
// and `- network` in one run — every create sorts before every delete, so the
// address landed and the Controller then refused the deletion the plan had
// already promised (ADR-0014).
//
// So the addresses are gathered first and matched to subnets after: what the
// config states, at the address it states, and what the config does not name
// but prune spares, where the Controller has it. A reservation this plan gives
// up is in neither, which is the point.
//
// A network whose subnet unifig cannot parse holds nothing back. That is not a
// judgement about the network — it is that this function's whole job is to say
// which deletions the Controller would refuse, and a subnet nothing can read is
// one this cannot answer for. The deletion is proposed and the Controller has
// the last word, which is where it was going to be anyway.
func reservedWithin(spared []unifi.User, desired []config.DHCPReservation, networks []unifi.Network) referenced {
	addresses := make(map[string]string, len(desired)+len(spared))
	for _, reservation := range desired {
		addresses[config.NormalisedMAC(reservation.MAC)] = reservation.IP
	}
	for _, reservation := range spared {
		mac := config.NormalisedMAC(reservation.MAC)
		if _, stated := addresses[mac]; !stated {
			addresses[mac] = reservation.FixedIP
		}
	}

	// Walked as a map, so the order these arrive in is not stable — heldBack
	// sorts what it is given, which is where the plan's own order comes from.
	inUse := referenced{}
	for mac, address := range addresses {
		for _, network := range networks {
			if !within(network.IPSubnet, address) {
				continue
			}
			inUse[network.Name] = append(inUse[network.Name], fmt.Sprintf(
				"the %s %q reserving an address inside it", kinds[DHCPReservation].one, mac))
		}
	}
	return inUse
}

// listClientRecords reads the site's client records — all of them, reservation
// or not.
//
// The filtering happens in reservationsAmong rather than here, because the
// records without a reservation are not noise: a create has to know whether the
// Controller already has a record for the client, since that decides whether the
// write is a new record or an edit to one the operator named in the UI.
func listClientRecords(ctx context.Context, client unifi.Client, site string) ([]unifi.User, error) {
	live, err := client.ListUser(ctx, site)
	if err != nil {
		return nil, fmt.Errorf("listing client records for site %q: %w", site, err)
	}
	return live, nil
}

// reservationsAmong keeps the client records that are Reservations: the ones
// with a fixed address switched on, and an address to go with it.
//
// Both halves are tested rather than just the switch, and for the reason
// fromLiveWLAN reads a passphrase only off a WPA-PSK WLAN: turning a fixed
// address off leaves `fixed_ip` behind on the record, so an address on a record
// that is not using one describes nothing about how that client is addressed
// today. The Controller will not accept the other case — it refuses
// `use_fixedip` with no address — so a record with the switch on and no address
// is not a state a site can be in; the test costs nothing and keeps the
// definition of a Reservation in one place rather than depending on somebody
// else's validation staying where it is.
func reservationsAmong(live []unifi.User) []unifi.User {
	kept := make([]unifi.User, 0, len(live))
	for _, record := range live {
		if record.UseFixedIP && record.FixedIP != "" {
			kept = append(kept, record)
		}
	}
	return kept
}

// uniquelyReservedMACs refuses a site holding two reservations unifig could not
// tell apart (ADR-0001).
//
// The Controller enforces this itself today — a second record for a MAC it
// already has comes back `api.err.MacUsed`, whatever case the MAC arrives in —
// so this looks at first like a check for a state that cannot happen. It is not,
// and the reason is unifig's own doing: matching folds case, so two records the
// Controller considers distinct would be one reservation here. Whether that is
// reachable is a question about somebody else's product, and matching by natural
// key is unifig's rule rather than a fact borrowed from the Controller.
//
// It says all this itself rather than through uniquelyNamed, which is the shared
// message every other kind uses. That one tells an operator to rename the extras
// in the Controller's UI, and a MAC address is not a name and cannot be renamed:
// the advice would be impossible to follow, about the one collision this can
// actually see. Forgetting the client is what is left, and the case difference
// is the thing to point at, because the two records do not look alike on the
// page — which is the same sentence checkReferences makes about two lines of one
// file.
func uniquelyReservedMACs(reservations []unifi.User) error {
	counts := make(map[string]int, len(reservations))
	for _, reservation := range reservations {
		counts[config.NormalisedMAC(reservation.MAC)]++
	}

	var shared []string
	for mac, count := range counts {
		if count > 1 {
			shared = append(shared, fmt.Sprintf("%d for %q", count, mac))
		}
	}
	if len(shared) == 0 {
		return nil
	}

	slices.Sort(shared)
	return fmt.Errorf(
		"unifig matches %s on the Controller by MAC address, and the Controller stores every MAC in lower case, so two client records whose MAC addresses differ only in case are one client to unifig: this site has %s; forget the extras in the Controller's UI, then run again",
		kinds[DHCPReservation].many, andJoin(shared))
}

// fromLiveReservation projects a live reservation into the config that would
// describe it, in the same one-implementation-for-both-directions way as every
// other type. The MAC comes back as the Controller stores it, which is lower
// case, so an exported file and a re-read of the Controller agree without
// anything having to fold case a second time.
func fromLiveReservation(live unifi.User) config.DHCPReservation {
	return config.DHCPReservation{MAC: config.NormalisedMAC(live.MAC), IP: live.FixedIP}
}

// createDHCPReservation is the Change for a reservation the Controller does not
// have — whether or not it has a record for the client.
//
// `record` is that record where there is one, and the zero value where there is
// not, which is what decides the write. A client the Controller has never seen
// needs one creating; a client it knows already has a name, a note and a user
// group on file, and creating a second record for it would be refused
// (`api.err.MacUsed`) even if it were the right thing to want.
//
// Either way the plan says `+`. What is being created is the reservation, which
// is the thing the config states and the thing that did not exist; which HTTP
// verb carries it is mechanism, and a plan that said `~` because the record
// happened to exist would be reporting unifig's internals rather than the
// operator's change.
func createDHCPReservation(desired config.DHCPReservation, record unifi.User) Change {
	return Change{
		Action: Create,
		Kind:   DHCPReservation,
		Name:   config.NormalisedMAC(desired.MAC),
		Fields: setReservationFields(desired),
		write: func(ctx context.Context, client unifi.Client, site string) error {
			if record.ID != "" {
				// The client's own record goes back with only unifig's two
				// fields changed, so the name the operator gave the device in
				// the Controller's UI survives being given an address here.
				updated := record
				overwriteManagedReservation(&updated, desired)
				_, err := client.UpdateUser(ctx, site, &updated)
				return err
			}
			reservation := newClientRecord(desired)
			_, err := client.CreateUser(ctx, site, &reservation)
			return err
		},
	}
}

// updateDHCPReservation is the Change that moves a reservation to the address
// the config states, and whether there is one to make at all.
func updateDHCPReservation(desired config.DHCPReservation, live unifi.User) (Change, bool) {
	fields := changedReservationFields(fromLiveReservation(live), desired)
	if len(fields) == 0 {
		return Change{}, false
	}

	return Change{
		Action: Update,
		Kind:   DHCPReservation,
		Name:   config.NormalisedMAC(desired.MAC),
		Fields: fields,
		write: func(ctx context.Context, client unifi.Client, site string) error {
			// The live record goes back with only unifig's own fields changed,
			// so the client's name, note, user group and blocked state survive
			// an apply rather than being reset by a record unifig built from
			// scratch. It also carries the Controller ID the update needs.
			updated := live
			overwriteManagedReservation(&updated, desired)
			_, err := client.UpdateUser(ctx, site, &updated)
			return err
		},
	}, true
}

// pruneDHCPReservations is the Changes that would give up every reservation the
// config does not name, and the reservations it leaves in place.
//
// It walks the live reservations rather than every client record, which is the
// whole of why prune is safe here: a client the Controller merely knows about is
// not a Reservation, so it is not of a managed type, so an empty
// `dhcp-reservations:` section puts every reserved address at stake and no
// client record at all.
//
// `NoDelete` is checked because the library models the field on this type, not
// because a client record has been seen carrying it: the marker is per Resource,
// only a network is known to use that one, and the recording carries no client
// records to answer the question from (ADR-0005, ADR-0011). Nothing here should
// be read as saying which field marks a record the Controller owns, or that it
// holds any.
func pruneDHCPReservations(live []unifi.User, reservedFor map[string]bool) (changes []Change, spared []unifi.User) {
	changes = make([]Change, 0, len(live))
	spared = make([]unifi.User, 0, len(live))
	for _, reservation := range live {
		if reservedFor[config.NormalisedMAC(reservation.MAC)] || reservation.NoDelete {
			spared = append(spared, reservation)
			continue
		}
		changes = append(changes, deleteDHCPReservation(reservation))
	}
	return changes, spared
}

// deleteDHCPReservation is the Change that gives up a live reservation.
//
// The write clears the fixed address and puts the rest of the record back
// exactly as it was. That is ADR-0015, and it is why this is the one deletion in
// unifig that carries a note: `- dhcp-reservation "aa:bb:…"` reads like the
// client being forgotten, and an operator approving a prune has to know that it
// is not — the device keeps its name, its note and its place in the Controller,
// and gives up only the address.
//
// The Controller's own name for the client goes in that sentence when it has
// one, because a MAC address is not how anyone recognises a device. It is the
// same job the mapping does on a port forward's deletion: the name unifig
// matched on may say far less about what is being lost than one unmanaged field
// does.
func deleteDHCPReservation(live unifi.User) Change {
	return Change{
		Action: Delete,
		Kind:   DHCPReservation,
		Name:   config.NormalisedMAC(live.MAC),
		// The address goes in unwrapped rather than through text(), because a
		// reservation with no address is not one reservationsAmong would have
		// kept: there is no absence to render here.
		Fields: []Field{{Name: "ip", From: live.FixedIP, Notes: []string{givesUpAddress(live)}}},
		write: func(ctx context.Context, client unifi.Client, site string) error {
			updated := live
			updated.UseFixedIP = false
			_, err := client.UpdateUser(ctx, site, &updated)
			return err
		},
	}
}

// givesUpAddress is the sentence a deletion carries: what happens to the client
// behind the reservation, which is nothing.
func givesUpAddress(live unifi.User) string {
	if name := clientName(live); name != "" {
		return fmt.Sprintf(
			"the Controller calls this client %q, and its record stays exactly as it is — only the fixed address is given up, so it takes one from the pool instead",
			name)
	}
	return "the Controller's record for this client stays exactly as it is — only the fixed address is given up, so it takes one from the pool instead"
}

// clientName is what the Controller would show for a client, and empty where it
// would show nothing but the MAC. The operator's own name comes first, because a
// device they took the trouble to name is one they will recognise by that name;
// the hostname the device announced is the fallback, which is what the
// Controller's own UI falls back to.
func clientName(live unifi.User) string {
	if live.Name != "" {
		return live.Name
	}
	return live.Hostname
}

// setReservationFields lists what a create would set. The MAC is left out: it is
// the Resource's identity, already named by the Change itself.
func setReservationFields(desired config.DHCPReservation) []Field {
	return []Field{{Name: "ip", To: desired.IP}}
}

// changedReservationFields lists the managed fields on which the Controller and
// the config disagree.
//
// The address is compared unconditionally, because the schema lets it be
// omitted no more than a MAC can be: a reservation in the config always states
// the address it pins. So this is one field and always will be — a reservation
// is a client and an address, and everything else on the record belongs to the
// operator.
func changedReservationFields(current, desired config.DHCPReservation) []Field {
	if current.IP == desired.IP {
		return nil
	}
	return []Field{{Name: "ip", From: text(current.IP), To: desired.IP}}
}

// overwriteManagedReservation writes the config's values onto a client record
// and touches nothing else — the single place that decides which fields unifig
// owns on a record that is mostly not unifig's.
//
// The MAC is written as the Controller stores it. On an update that is a no-op
// by construction, since matching is what found this record; on the create path
// it is what stops a config written in upper case from producing a record whose
// MAC does not match the one the next plan reads back.
func overwriteManagedReservation(record *unifi.User, desired config.DHCPReservation) {
	record.MAC = config.NormalisedMAC(desired.MAC)
	record.UseFixedIP = true
	record.FixedIP = desired.IP
}

// newClientRecord builds the Controller object for a client unifig is creating a
// record for.
//
// There is nothing here but the reservation itself, and unlike newNetwork or
// newPortForward that is not a set of defaults left implicit — it is the whole
// object. A client record has no switch to turn on and nothing to bind it to:
// the Controller fills in what it knows about the device (its OUI, whether it is
// wired) and leaves the operator's fields — name, note, user group — empty, which
// is exactly right for a device that has never been seen. Naming it here would be
// unifig inventing a fact about somebody's network.
func newClientRecord(desired config.DHCPReservation) unifi.User {
	var record unifi.User
	overwriteManagedReservation(&record, desired)
	return record
}

// projectDHCPReservations projects the site's reservations into the config that
// would describe them.
//
// Nothing comes back alongside them, unlike every other projection in the
// engine. There is no reservation the config cannot state: it is a MAC and an
// address, the Controller refuses one without an address, and a MAC is a MAC. So
// export never comes up short here and has nothing to say about it.
func projectDHCPReservations(ctx context.Context, client unifi.Client, site string) ([]config.DHCPReservation, error) {
	live, err := listClientRecords(ctx, client, site)
	if err != nil {
		return nil, err
	}
	reservations := reservationsAmong(live)
	// Export matches too, in the sense that matters here: a file describing two
	// reservations unifig cannot tell apart is a file it cannot plan afterwards.
	if err := uniquelyReservedMACs(reservations); err != nil {
		return nil, err
	}

	described := make([]config.DHCPReservation, 0, len(reservations))
	for _, reservation := range reservations {
		described = append(described, fromLiveReservation(reservation))
	}
	slices.SortFunc(described, func(a, b config.DHCPReservation) int {
		return strings.Compare(a.MAC, b.MAC)
	})
	return described, nil
}
