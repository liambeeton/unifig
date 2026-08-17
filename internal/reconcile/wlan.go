package reconcile

import (
	"context"
	"fmt"
	"slices"

	"github.com/filipowm/go-unifi/v2/unifi"

	"github.com/liambeeton/unifig/internal/config"
)

// wpaPSK is the Controller's security mode for a WLAN clients join with a
// pre-shared key, and the only mode a passphrase belongs to. Stating one in the
// config is therefore stating this, which is what overwriteManagedWLAN writes.
const wpaPSK = "wpapsk"

// passphraseSecurities are the security modes on which a stored passphrase
// describes how clients actually join. There is one, and it is a whitelist
// rather than a test for `open` for the same reason wanTypes is one: the
// Controller leaves x_passphrase behind on a WLAN whose security has moved on,
// so an enterprise (`wpaeap`) WLAN carries a stale passphrase exactly as an open
// one does, and only the security mode says whether anything uses it.
var passphraseSecurities = map[string]bool{wpaPSK: true}

// planWLANs is the WLAN half of a reconcile. Its caller only reaches it when
// the config has a `wlans:` section at all (ADR-0006), so a file that says
// nothing about WLANs changes none of them — though its caller may well have
// read them anyway, because a prune has to know what is on a network before it
// can propose deleting one (ADR-0014).
//
// It reports the live WLANs it leaves in place alongside the changes, because
// that is the question the network half has to ask and this is the half that
// knows the answer: a WLAN this same plan deletes is not one standing between a
// network and prune.
func planWLANs(cfg config.Config, live []unifi.WLAN, bound bindings, opts Options) ([]Change, []unifi.WLAN, error) {
	if err := uniquelyNamedWLANs(live); err != nil {
		return nil, nil, err
	}

	byName := make(map[string]unifi.WLAN, len(live))
	for _, wlan := range live {
		byName[wlan.Name] = wlan
	}

	changes := make([]Change, 0, len(cfg.WLANs))
	named := make(map[string]bool, len(cfg.WLANs))
	for _, desired := range cfg.WLANs {
		named[desired.Name] = true

		current, exists := byName[desired.Name]
		if !exists {
			changes = append(changes, createWLAN(desired, bound))
			continue
		}
		if change, differs := updateWLAN(desired, current, bound); differs {
			changes = append(changes, change)
		}
	}
	if !opts.Prune {
		return changes, live, nil
	}
	deletions, spared := pruneWLANs(live, named, bound)
	return append(changes, deletions...), spared, nil
}

// networksInUse names the networks a WLAN this plan leaves in place will be on
// once it has run, against the phrase naming that WLAN — what prune asks before
// proposing to delete a network, and what the Caveat about the one it did not
// propose says (ADR-0014).
//
// "Will be on" rather than "is on", because those differ in a case that reaches
// an operator: a file moving a WLAN to another network states the network it
// leaves nowhere, so prune can propose deleting it, and the move is applied
// before any deletion is. Reading the live binding here would hold that network
// back and print a plan that contradicted itself two lines further up — the
// update saying the WLAN is leaving, the caveat saying it stays.
//
// So a spared WLAN the config names is on the network the config states, which
// the schema requires it to state; one the config does not name is where the
// Controller has it. A WLAN being created is not here at all, and needs no
// handling: a config that states one states the network too, so that network is
// named in the file and was never prune's to take.
//
// A WLAN bound to something that is not one of this site's LANs holds nothing
// back, because there is no network unifig manages for it to be on. listWLANs
// has already left those out; the guard is here because this reads a name off a
// binding, and a binding that resolves to nothing must not become a key.
func networksInUse(spared []unifi.WLAN, desired []config.WLAN, bound bindings) referenced {
	stated := make(map[string]string, len(desired))
	for _, wlan := range desired {
		stated[wlan.Name] = wlan.Network
	}

	inUse := referenced{}
	for _, wlan := range spared {
		network := stated[wlan.Name]
		if network == "" {
			network = bound.networkName(wlan.NetworkID)
		}
		if network == "" {
			continue
		}
		inUse[network] = append(inUse[network],
			fmt.Sprintf("the %s %q", kinds[WLAN].one, wlan.Name))
	}
	return inUse
}

// listWLANs reads the site's WLANs and keeps the ones unifig manages.
//
// What it keeps is a WLAN bound to a network unifig manages, and that binding is
// the whole test — the WLAN counterpart of the purpose filter on networkconf.
// A WLAN whose networkconf_id names nothing, or names a WAN slot, cannot be
// written as config at all: `network` is required and has to name a network the
// same file defines, and unifig has no name to put there. Rather than being an
// error, it is simply not unifig's: out of scope is a state the engine already
// has a word for, and the alternative — refusing to export a whole Controller
// because one WLAN on it is bound to something odd — would fail on a stock
// dockerized Controller, which ships exactly such a WLAN.
//
// Deciding it here, once, is what keeps the two verbs in step. Export writing a
// file that omits a WLAN which prune would then delete is the failure this
// placement rules out: both read the same list. The names of what was left out
// come back alongside, so export can say so rather than quietly coming up short.
func listWLANs(ctx context.Context, client unifi.Client, site string, bound bindings) (kept []unifi.WLAN, indescribable []string, err error) {
	all, err := client.ListWLAN(ctx, site)
	if err != nil {
		return nil, nil, fmt.Errorf("listing WLANs for site %q: %w", site, err)
	}

	kept = make([]unifi.WLAN, 0, len(all))
	for _, wlan := range all {
		if bound.networkName(wlan.NetworkID) == "" {
			indescribable = append(indescribable, wlan.Name)
			continue
		}
		kept = append(kept, wlan)
	}
	slices.Sort(indescribable)
	return kept, indescribable, nil
}

// uniquelyNamedWLANs refuses a site holding two WLANs unifig could not tell
// apart (ADR-0001).
//
// It is asked by the verbs that match WLANs to config rather than by the read
// itself, because reading is not matching. A prune of the networks reads the
// WLANs only to see which network each is on (ADR-0014), and two WLANs sharing a
// name are no obstacle to that: both hold their network either way. Refusing the
// run there would fail a file that says nothing about WLANs over an ambiguity
// unifig was never going to have to resolve.
//
// The networks and the zones keep their own refusal inside their reads, and that
// is not an inconsistency. Those two reads produce a binding — a name against the
// Controller ID that references to it are stored as — so two of one name there
// leaves every reference to that name ambiguous, whatever the file manages.
// Nothing is bound to a WLAN, and nothing is bound to a policy.
func uniquelyNamedWLANs(live []unifi.WLAN) error {
	names := make([]string, 0, len(live))
	for _, wlan := range live {
		names = append(names, wlan.Name)
	}
	return uniquelyNamed(WLAN, names)
}

// fromLiveWLAN projects a live WLAN into the config that would describe it, in
// the same one-implementation-for-both-directions way as networks.
//
// The passphrase comes back from the Internal API in the clear (ADR-0007), so
// it takes part in the diff like any other field rather than needing write-only
// semantics. Nothing else in unifig prints it. It is read only for a WLAN whose
// security actually uses one, because a passphrase left behind on a WLAN the
// Controller now holds as open describes nothing about how clients join it
// today — the same sentence fromLiveWANSlot makes about PPPoE credentials on a
// slot that has since moved to DHCP, and the same Controller behaviour
// underneath: a value that stops being used is left where it was.
//
// So an open WLAN is described by its name and its network, and no notice goes
// with it, unlike a WAN slot unifig can only half describe. An absent
// `passphrase:` already means "unifig manages nothing about how clients join
// this", which is the whole truth about an open WLAN rather than a gap in
// stating it — where a partial WAN slot is one whose connection type the config
// has no way to state at all. A file that came back short says so; this one did
// not come back short.
//
// Network is never empty for a WLAN that reached here, because listWLANs keeps
// only WLANs whose binding it can name.
func fromLiveWLAN(wlan unifi.WLAN, bound bindings) config.WLAN {
	described := config.WLAN{
		Name:    wlan.Name,
		Network: bound.networkName(wlan.NetworkID),
	}
	if passphraseSecurities[wlan.Security] {
		described.Passphrase = wlan.XPassphrase
	}
	return described
}

// createWLAN is the Change for a WLAN the Controller does not have.
func createWLAN(desired config.WLAN, bound bindings) Change {
	return Change{
		Action: Create,
		Kind:   WLAN,
		Name:   desired.Name,
		Fields: setWLANFields(desired),
		write: func(ctx context.Context, client unifi.Client, site string) error {
			// Read at the moment of writing rather than the moment of
			// planning: the network this WLAN joins may have been created by a
			// change earlier in this very apply.
			networkID, err := bound.networkID(desired.Network, "for this WLAN's clients to join")
			if err != nil {
				return err
			}
			groups, err := apGroupIDs(ctx, client, site)
			if err != nil {
				return err
			}
			wlan := newWLAN(desired, networkID, groups)
			_, err = client.CreateWLAN(ctx, site, &wlan)
			return err
		},
	}
}

// updateWLAN is the Change that brings a live WLAN in line with the config, and
// whether there is one to make at all.
func updateWLAN(desired config.WLAN, live unifi.WLAN, bound bindings) (Change, bool) {
	fields := changedWLANFields(fromLiveWLAN(live, bound), desired)
	if len(fields) == 0 {
		return Change{}, false
	}

	// Writing a passphrase writes WPA-PSK with it (see overwriteManagedWLAN), so
	// stating one for a WLAN the Controller does not hold that way changes how
	// every client joins it. unifig does not model security, so the config
	// cannot say that and the plan does — the update-path counterpart of
	// setWLANFields telling a create that it is about to make an open WLAN. The
	// mode the Controller is in goes into the sentence, because that is the fact
	// an operator checks it against: a guest WLAN they meant to leave open reads
	// nothing like an enterprise one they had forgotten was enterprise.
	//
	// A note rather than a Risk, and by the test ADR-0012 settled rather than by
	// omission: everyone on the WLAN drops off it, and the Controller is still
	// reachable, the operator is still an operator, and the way back is to join
	// again with the passphrase they just set. Recovery cannot need physical
	// access, so this is a consequence to state, not a change to stop and ask
	// about one at a time.
	if desired.Passphrase != "" && !passphraseSecurities[live.Security] {
		annotate(fields, "passphrase", fmt.Sprintf(
			"the Controller has this WLAN's security as %q, and a passphrase sets WPA-PSK, so every client on it now is disconnected until it joins again with the passphrase",
			live.Security))
	}

	return Change{
		Action: Update,
		Kind:   WLAN,
		Name:   desired.Name,
		Fields: fields,
		write: func(ctx context.Context, client unifi.Client, site string) error {
			networkID, err := bound.networkID(desired.Network, "for this WLAN's clients to join")
			if err != nil {
				return err
			}
			// The live object goes back with only unifig's own fields changed,
			// so band selection, PMF, minimum data rates, MAC filters and
			// everything else the operator set in the Controller's UI survive.
			updated := live
			overwriteManagedWLAN(&updated, desired, networkID)
			_, err = client.UpdateWLAN(ctx, site, &updated)
			return err
		},
	}, true
}

// pruneWLANs is the Changes that would delete every live WLAN the config does
// not name, and the WLANs it leaves on the Controller.
//
// Like networks, an object the Controller marks undeletable is left out of the
// plan rather than listed as a change that will not happen (ADR-0005) — and it is
// spared rather than skipped, because what makes a WLAN hold its network back is
// that it will still be on it afterwards, whatever the reason it survived.
func pruneWLANs(live []unifi.WLAN, named map[string]bool, bound bindings) (changes []Change, spared []unifi.WLAN) {
	changes = make([]Change, 0, len(live))
	spared = make([]unifi.WLAN, 0, len(live))
	for _, wlan := range live {
		if named[wlan.Name] || wlan.NoDelete {
			spared = append(spared, wlan)
			continue
		}
		changes = append(changes, deleteWLAN(wlan, bound))
	}
	return changes, spared
}

// deleteWLAN is the Change that removes a live WLAN.
//
// It lists the network the WLAN was on and nothing else. The passphrase is the
// other field unifig models, and printing "(hidden)" next to a WLAN about to be
// destroyed would take a line to say nothing: what an operator needs in order
// to recognise the WLAN they are about to lose is where its clients were
// landing, not that it had a passphrase.
func deleteWLAN(live unifi.WLAN, bound bindings) Change {
	current := fromLiveWLAN(live, bound)
	return Change{
		Action: Delete,
		Kind:   WLAN,
		Name:   live.Name,
		Fields: []Field{{Name: "network", From: current.Network}},
		write: func(ctx context.Context, client unifi.Client, site string) error {
			return client.DeleteWLAN(ctx, site, live.ID)
		},
	}
}

// setWLANFields lists what a create would set.
//
// A WLAN with no passphrase is listed rather than left out, which is the
// opposite of how an omitted field is treated everywhere else — and it is the
// same rule underneath. Omission means unmanaged, and for an update that means
// nothing happens; but a create has to produce a WLAN, and a WLAN unifig
// creates without a passphrase is one anyone in range can join. That is a
// consequence the config does not state, so the plan states it.
func setWLANFields(desired config.WLAN) []Field {
	fields := []Field{{Name: "network", To: desired.Network}}
	if desired.Passphrase != "" {
		return append(fields, Field{Name: "passphrase", Secret: true})
	}
	return append(fields, Field{
		Name: "passphrase",
		Note: "no passphrase is set, so this WLAN will be open — anyone in range can join it",
	})
}

// changedWLANFields lists the managed fields on which the Controller and the
// config disagree.
//
// Network is not guarded the way an optional field is: the schema requires it,
// so a WLAN in the config always states the network its clients join, and
// listWLANs guarantees the live one can be named too. A passphrase the config
// omits is unmanaged as usual — on an existing WLAN that means unifig leaves
// whatever security the Controller has configured exactly as it is, open or WPA
// or enterprise.
//
// A passphrase the config does state is always a change on a WLAN the
// Controller does not hold as WPA-PSK, because fromLiveWLAN reads no passphrase
// off one and so there is nothing for it to already agree with. That is right
// rather than incidental: the apply moves the WLAN onto WPA-PSK whatever the
// value, so it is a change even where the operator happened to write the value
// the Controller still has lying on it.
func changedWLANFields(current, desired config.WLAN) []Field {
	fields := make([]Field, 0, 2)
	if current.Network != desired.Network {
		fields = append(fields, Field{Name: "network", From: current.Network, To: desired.Network})
	}
	if desired.Passphrase != "" && current.Passphrase != desired.Passphrase {
		fields = append(fields, Field{Name: "passphrase", Secret: true})
	}
	return fields
}

// overwriteManagedWLAN writes the config's values onto a Controller WLAN and
// touches nothing else — the WLAN counterpart of overwriteManagedNetwork, and
// the single place that decides which WLAN fields unifig owns.
//
// Setting a passphrase also sets the security mode, because on the Controller
// the two are one decision: an x_passphrase on a WLAN whose security is `open`
// or `wpaeap` is a password nothing asks for. So a config that states a
// passphrase is a config that states WPA-PSK, and one that omits it leaves the
// Controller's own choice alone.
//
// fromLiveWLAN is the same sentence read backwards, and the two have to stay
// that way: what is written under one security mode is what is read under it.
// A passphrase harvested off a WLAN the Controller holds as open would arrive
// back here as a config asking for WPA-PSK, and unifig would lock an open WLAN
// on nobody's instruction.
func overwriteManagedWLAN(wlan *unifi.WLAN, desired config.WLAN, networkID string) {
	wlan.Name = desired.Name
	wlan.NetworkID = networkID
	if desired.Passphrase != "" {
		wlan.XPassphrase = desired.Passphrase
		wlan.Security = wpaPSK
	}

	// Not a modelled field, and not really a write either: the Controller
	// rejects `schedule_with_duration: null` outright, and it does not return
	// the field on a read — so a live WLAN decoded and handed straight back
	// would be refused for a field the operator never touched. Normalising the
	// empty case here covers create and update in one place.
	if wlan.ScheduleWithDuration == nil {
		wlan.ScheduleWithDuration = []unifi.WLANScheduleWithDuration{}
	}
}

// newWLAN builds the Controller object for a WLAN unifig is creating.
//
// There is far less here than newNetwork needs, because the Controller fills in
// far more: a wlanconf created with a name, a network and a security mode comes
// back with band selection, DTIM intervals, minimum data rates and a user group
// already chosen. Only what the Controller will not choose is set — it must be
// enabled to broadcast at all, and `open` is the security mode that matches a
// config stating no passphrase, which overwriteManagedWLAN then replaces if one
// was stated.
func newWLAN(desired config.WLAN, networkID string, apGroups []string) unifi.WLAN {
	wlan := unifi.WLAN{
		Enabled:    true,
		Security:   "open",
		ApGroupIDs: apGroups,
	}
	overwriteManagedWLAN(&wlan, desired, networkID)
	return wlan
}

// apGroupIDs are the AP groups a new WLAN broadcasts from.
//
// The Controller refuses to create a WLAN without at least one, and the config
// does not model AP groups — so unifig uses the Controller's own default group,
// the "All APs" one its UI puts every new WLAN on, identified by the marker the
// Controller puts on it rather than by its name. This is a create-time default
// like every other (ADR-0004): an operator who afterwards moves the WLAN onto
// specific APs keeps that, because updates never touch it.
func apGroupIDs(ctx context.Context, client unifi.Client, site string) ([]string, error) {
	groups, err := client.ListAPGroup(ctx, site)
	if err != nil {
		return nil, fmt.Errorf("listing AP groups for site %q: %w", site, err)
	}
	for _, group := range groups {
		if group.HiddenID == "default" {
			return []string{group.ID}, nil
		}
	}
	if len(groups) > 0 {
		return []string{groups[0].ID}, nil
	}
	return nil, fmt.Errorf(
		"site %q has no AP group for a new WLAN to broadcast from; create one in the Controller's UI, then run again", site)
}
