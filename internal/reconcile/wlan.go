package reconcile

import (
	"context"
	"fmt"
	"slices"

	"github.com/filipowm/go-unifi/v2/unifi"

	"github.com/liambeeton/unifig/internal/config"
)

// planWLANs is the WLAN half of a reconcile. Its caller only reaches it when
// the config has a `wlans:` section at all (ADR-0006), so the Controller's
// WLANs are not even read for a file that says nothing about them.
func planWLANs(
	ctx context.Context,
	client unifi.Client,
	site string,
	cfg config.Config,
	bound bindings,
	opts Options,
) ([]Change, error) {
	live, err := liveWLANs(ctx, client, site, bound)
	if err != nil {
		return nil, err
	}

	changes := make([]Change, 0, len(cfg.WLANs))
	named := make(map[string]bool, len(cfg.WLANs))
	for _, desired := range cfg.WLANs {
		named[desired.Name] = true

		current, exists := live[desired.Name]
		if !exists {
			changes = append(changes, createWLAN(desired, bound))
			continue
		}
		if change, differs := updateWLAN(desired, current, bound); differs {
			changes = append(changes, change)
		}
	}
	if opts.Prune {
		changes = append(changes, pruneWLANs(live, named, bound)...)
	}
	return changes, nil
}

// listWLANs reads the site's WLANs and keeps the ones unifig manages, refusing
// a site where two of those share the name unifig matches them by.
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
	names := make([]string, 0, len(all))
	for _, wlan := range all {
		if bound.networkName(wlan.NetworkID) == "" {
			indescribable = append(indescribable, wlan.Name)
			continue
		}
		kept = append(kept, wlan)
		names = append(names, wlan.Name)
	}
	slices.Sort(indescribable)
	if err := uniquelyNamed(WLAN, names); err != nil {
		return nil, nil, err
	}
	return kept, indescribable, nil
}

// liveWLANs indexes the site's managed WLANs by their natural key.
func liveWLANs(ctx context.Context, client unifi.Client, site string, bound bindings) (map[string]unifi.WLAN, error) {
	all, _, err := listWLANs(ctx, client, site, bound)
	if err != nil {
		return nil, err
	}

	byName := make(map[string]unifi.WLAN, len(all))
	for _, wlan := range all {
		byName[wlan.Name] = wlan
	}
	return byName, nil
}

// fromLiveWLAN projects a live WLAN into the config that would describe it, in
// the same one-implementation-for-both-directions way as networks.
//
// The passphrase comes back from the Internal API in the clear (ADR-0007), so
// it takes part in the diff like any other field rather than needing write-only
// semantics. Nothing else in unifig prints it.
//
// Network is never empty for a WLAN that reached here, because listWLANs keeps
// only WLANs whose binding it can name.
func fromLiveWLAN(wlan unifi.WLAN, bound bindings) config.WLAN {
	return config.WLAN{
		Name:       wlan.Name,
		Network:    bound.networkName(wlan.NetworkID),
		Passphrase: wlan.XPassphrase,
	}
}

// createWLAN is the Change for a WLAN the Controller does not have.
func createWLAN(desired config.WLAN, bound bindings) Change {
	return Change{
		Action:   Create,
		Resource: WLAN,
		Name:     desired.Name,
		Fields:   setWLANFields(desired),
		write: func(ctx context.Context, client unifi.Client, site string) error {
			// Read at the moment of writing rather than the moment of
			// planning: the network this WLAN joins may have been created by a
			// change earlier in this very apply.
			networkID, err := bound.networkID(desired.Network)
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

	return Change{
		Action:   Update,
		Resource: WLAN,
		Name:     desired.Name,
		Fields:   fields,
		write: func(ctx context.Context, client unifi.Client, site string) error {
			networkID, err := bound.networkID(desired.Network)
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
// not name. Like networks, an object the Controller marks undeletable is left
// out of the plan rather than listed as a change that will not happen
// (ADR-0005).
func pruneWLANs(live map[string]unifi.WLAN, named map[string]bool, bound bindings) []Change {
	changes := make([]Change, 0, len(live))
	for name, wlan := range live {
		if named[name] || wlan.NoDelete {
			continue
		}
		changes = append(changes, deleteWLAN(wlan, bound))
	}
	return changes
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
		Action:   Delete,
		Resource: WLAN,
		Name:     live.Name,
		Fields:   []Field{{Name: "network", From: current.Network}},
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
func overwriteManagedWLAN(wlan *unifi.WLAN, desired config.WLAN, networkID string) {
	wlan.Name = desired.Name
	wlan.NetworkID = networkID
	if desired.Passphrase != "" {
		wlan.XPassphrase = desired.Passphrase
		wlan.Security = "wpapsk"
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
