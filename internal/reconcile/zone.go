package reconcile

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/filipowm/go-unifi/v2/unifi"

	"github.com/liambeeton/unifig/internal/config"
)

// Zones are the first Resource unifig manages that the Controller itself ships
// instances of. A network has one built-in (Default) and a WLAN has none; a
// zone-based firewall arrives with a full set — External, Internal, Gateway,
// VPN, Hotspot — and the interesting configuration is mostly about them rather
// than about zones an operator invents.
//
// That makes ADR-0005 load-bearing here in a way it has not been before. A
// built-in zone is matched from config and managed like any other, and is exempt
// from deletion because the Controller marks it undeletable — not because unifig
// keeps a list of the names Ubiquiti ships. The list would be wrong the first
// time a firmware added a zone, and being wrong here means proposing to delete
// the zone that stands for the internet.
//
// The other thing a zone brings is a reference in both directions: it holds
// networks, and firewall policies hold it. So it sits between the two in the
// plan's ordering, and its create records the new zone's ID where a policy
// created in the same pass can find it.

// planZones is the zone half of a reconcile. Its caller only reaches it when the
// config has a `zones:` section at all (ADR-0006), so a file that says nothing
// about zones leaves every one of them alone.
func planZones(cfg config.Config, live []unifi.FirewallZone, bound bindings, opts Options) []Change {
	byName := make(map[string]unifi.FirewallZone, len(live))
	for _, zone := range live {
		byName[zone.Name] = zone
	}

	changes := make([]Change, 0, len(cfg.Zones))
	named := make(map[string]bool, len(cfg.Zones))
	for _, desired := range cfg.Zones {
		named[desired.Name] = true

		current, exists := byName[desired.Name]
		if !exists {
			changes = append(changes, createZone(desired, bound))
			continue
		}
		if change, differs := updateZone(desired, current, bound); differs {
			changes = append(changes, change)
		}
	}
	if opts.Prune {
		changes = append(changes, pruneZones(byName, named, bound)...)
	}
	return changes
}

// listZones reads the site's firewall zones, refusing a site where two of them
// share the name unifig matches them by.
//
// Nothing is filtered out. Every zone on the site is one unifig can match and
// manage, the Controller's own built-ins included — what a built-in is exempt
// from is deletion, and that exemption is read off the object when prune looks
// at it rather than by leaving it out of the collection here.
func listZones(ctx context.Context, client unifi.Client, site string) ([]unifi.FirewallZone, error) {
	live, err := client.ListFirewallZone(ctx, site)
	if err != nil {
		return nil, fmt.Errorf("listing firewall zones for site %q: %w", site, err)
	}

	names := make([]string, 0, len(live))
	for _, zone := range live {
		names = append(names, zone.Name)
	}
	if err := uniquelyNamed(Zone, names); err != nil {
		return nil, err
	}
	return live, nil
}

// projectZones projects the site's zones into the config that would describe
// them, and names the ones it could only half describe.
func projectZones(live []unifi.FirewallZone, bound bindings) ([]config.Zone, []string) {
	zones := make([]config.Zone, 0, len(live))
	var partial []string
	for _, zone := range live {
		described, whole := fromLiveZone(zone, bound)
		if !whole {
			partial = append(partial, zone.Name)
		}
		zones = append(zones, described)
	}
	slices.SortFunc(zones, func(a, b config.Zone) int { return strings.Compare(a.Name, b.Name) })
	slices.Sort(partial)
	return zones, partial
}

// fromLiveZone projects a live zone into the config that would describe it, and
// says whether the config could describe the whole of its membership.
//
// A zone can hold something that is not one of the LANs unifig manages — the
// built-in External zone holds the WAN — and there is no name for the config to
// put in the list for one. Rather than an error or an omission, such a member is
// simply not unifig's: it stays out of the file, and overwriteManagedZone leaves
// it on the Controller untouched, so a zone unifig only half describes still
// round-trips without losing anything. That is ADR-0004 one level in — unifig
// owns the part of the membership it can name, and the file states what it
// manages rather than what may exist.
//
// The names are sorted because the order the Controller holds them in is not
// something the config says anything about: two exports of an unchanged
// Controller have to be the same file.
func fromLiveZone(zone unifi.FirewallZone, bound bindings) (described config.Zone, whole bool) {
	described = config.Zone{
		Name:     zone.Name,
		Networks: make([]string, 0, len(zone.NetworkIDs)),
	}
	whole = true
	for _, id := range zone.NetworkIDs {
		name := bound.networkName(id)
		if name == "" {
			whole = false
			continue
		}
		described.Networks = append(described.Networks, name)
	}
	slices.Sort(described.Networks)
	return described, whole
}

// createZone is the Change for a zone the Controller does not have.
func createZone(desired config.Zone, bound bindings) Change {
	return Change{
		Action: Create,
		Kind:   Zone,
		Name:   desired.Name,
		Fields: setZoneFields(desired),
		write: func(ctx context.Context, client unifi.Client, site string) error {
			zone := unifi.FirewallZone{NetworkIDs: []string{}}
			// Read at the moment of writing rather than the moment of planning:
			// a network this zone holds may have been created by a change
			// earlier in this very apply.
			if err := overwriteManagedZone(&zone, desired, bound); err != nil {
				return err
			}
			created, err := client.CreateFirewallZone(ctx, site, &zone)
			if err != nil {
				return err
			}
			// A policy planned onto this zone has been waiting for exactly this:
			// until now there was no ID to point it at.
			bound.zones[desired.Name] = created.ID
			return nil
		},
	}
}

// updateZone is the Change that brings a live zone in line with the config, and
// whether there is one to make at all.
func updateZone(desired config.Zone, live unifi.FirewallZone, bound bindings) (Change, bool) {
	current, whole := fromLiveZone(live, bound)
	fields := changedZoneFields(current, desired)
	if len(fields) == 0 {
		return Change{}, false
	}

	// The membership the plan is showing is only the part unifig can name, and
	// an operator reading `networks: "IoT" -> "IoT", "Guest"` on a built-in zone
	// deserves to know that the WAN sitting in it is neither shown nor at stake.
	// Saying so is the difference between a plan that looks like it empties a
	// zone and one that says what it does.
	if !whole && desired.Networks != nil {
		annotate(fields, "networks",
			"this zone also holds something that is not one of this site's LANs, which unifig does not name or change")
	}

	return Change{
		Action: Update,
		Kind:   Zone,
		Name:   desired.Name,
		Fields: fields,
		write: func(ctx context.Context, client unifi.Client, site string) error {
			// The live object goes back with only unifig's own fields changed,
			// so everything the Controller holds about the zone that unifig does
			// not model survives an apply.
			updated := live
			if err := overwriteManagedZone(&updated, desired, bound); err != nil {
				return err
			}
			_, err := client.UpdateFirewallZone(ctx, site, &updated)
			return err
		},
	}, true
}

// pruneZones is the Changes that would delete every live zone the config does
// not name.
//
// The Controller's own zones are exempt, and the exemption is read off the
// object's undeletable marker rather than from a list of built-in names unifig
// keeps (ADR-0005). That matters more here than anywhere else it applies: a
// list that had never heard of some firmware's new built-in would have prune
// propose deleting the zone that stands for the internet, and an operator who
// approved it would find out what that means.
func pruneZones(live map[string]unifi.FirewallZone, named map[string]bool, bound bindings) []Change {
	changes := make([]Change, 0, len(live))
	for name, zone := range live {
		if named[name] || zone.NoDelete {
			continue
		}
		changes = append(changes, deleteZone(zone, bound))
	}
	return changes
}

// deleteZone is the Change that removes a live zone.
func deleteZone(live unifi.FirewallZone, bound bindings) Change {
	current, _ := fromLiveZone(live, bound)
	return Change{
		Action: Delete,
		Kind:   Zone,
		Name:   live.Name,
		Fields: []Field{{Name: "networks", From: nameList(current.Networks)}},
		write: func(ctx context.Context, client unifi.Client, site string) error {
			return client.DeleteFirewallZone(ctx, site, live.ID)
		},
	}
}

// setZoneFields lists what a create would set. A zone with no networks is listed
// rather than left out, for the same reason a WLAN with no passphrase is: a
// create has to produce a zone, and one with nothing in it is a zone no traffic
// is ever in — which is a consequence the config does not state.
func setZoneFields(desired config.Zone) []Field {
	if len(desired.Networks) == 0 {
		return []Field{{
			Name: "networks",
			Note: "no network is listed, so this zone will hold none and no traffic will be in it",
		}}
	}
	return []Field{{Name: "networks", To: nameList(desired.Networks)}}
}

// changedZoneFields lists the managed fields on which the Controller and the
// config disagree.
//
// A zone the config states without a `networks:` key manages nothing about the
// membership, which is ADR-0004 as usual and is what lets an operator name a
// built-in zone in order to write policies about it without taking over what is
// in it. A zone that does state the key states the whole list.
func changedZoneFields(current, desired config.Zone) []Field {
	if desired.Networks == nil {
		return nil
	}
	held, wanted := slices.Clone(current.Networks), slices.Clone(desired.Networks)
	slices.Sort(held)
	slices.Sort(wanted)
	if slices.Equal(held, wanted) {
		return nil
	}
	return []Field{{Name: "networks", From: nameList(held), To: nameList(wanted)}}
}

// overwriteManagedZone writes the config's values onto a Controller zone and
// touches nothing else — the single place that decides which zone fields unifig
// owns.
//
// The membership is owned per member rather than wholesale. A network unifig
// cannot name is kept exactly where it was, so stating `networks:` on the
// built-in External zone sets the LANs in it without taking the WAN out — and an
// operator who exports their Controller and applies the file back has not
// quietly detached their uplink from the zone that stands for the internet.
func overwriteManagedZone(zone *unifi.FirewallZone, desired config.Zone, bound bindings) error {
	zone.Name = desired.Name
	if desired.Networks == nil {
		return nil
	}

	ids := make([]string, 0, len(desired.Networks)+len(zone.NetworkIDs))
	for _, name := range desired.Networks {
		id, err := bound.networkID(name, fmt.Sprintf("to put in the zone %q", desired.Name))
		if err != nil {
			return err
		}
		ids = append(ids, id)
	}
	for _, id := range zone.NetworkIDs {
		if bound.networkName(id) == "" {
			ids = append(ids, id)
		}
	}
	zone.NetworkIDs = ids
	return nil
}

// noSuchZone is the error for a config naming a zone that is neither on the
// Controller nor created by the same file. It lists the zones the site has,
// because the likeliest cause is a misspelled built-in — and which built-ins a
// Controller ships is the Controller's answer rather than a list unifig keeps.
func noSuchZone(name string, present []string) error {
	has := "this site has none at all"
	if len(present) > 0 {
		has = "this site has " + andJoin(quoted(present))
	}
	return fmt.Errorf(
		"the Controller has no zone named %q, and this file does not define one: a firewall policy governs the traffic between two zones, so both ends have to exist — %s",
		name, has)
}
