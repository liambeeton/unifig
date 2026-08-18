package reconcile

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/filipowm/go-unifi/v2/unifi"

	"github.com/liambeeton/unifig/internal/config"
)

// Zones are the first Resource unifig manages that the Controller itself ships
// instances of. A network has one built-in (Default) and a WLAN has none; a
// zone-based firewall arrives with a full set — six of them on the router this
// repository's recording came from — and the interesting configuration is mostly
// about them rather than about zones an operator invents.
//
// That makes ADR-0005 load-bearing here in a way it has not been before. A
// built-in zone is matched from config and managed like any other, and is exempt
// from deletion because the Controller marks it its own with `default_zone` —
// not because unifig keeps a list of the names Ubiquiti ships. The list would be
// wrong the first time a firmware added a zone, and being wrong here means
// proposing to delete the zone that stands for the internet. This comment used
// to name the five zones a hand-written fixture guessed at, which is the same
// mistake in prose: a real router ships six and spells two differently.
//
// The other thing a zone brings is a reference in both directions: it holds
// networks, and firewall policies hold it. So it sits between the two in the
// plan's ordering, and its create records the new zone's ID where a policy
// created in the same pass can find it.

// planZones is the zone half of a reconcile. Its caller only reaches it when the
// config has a `zones:` section at all (ADR-0006), so a file that says nothing
// about zones leaves every one of them alone. inUse is what the policy half is
// leaving on each zone, which is what decides whether prune may propose deleting
// one (ADR-0014).
//
// The placement is worked out once, before any zone is planned, and that is not
// only an economy. A membership change is about two zones rather than one, so
// every change here needs to know something about a zone it is not about — and
// answering that from inside a zone's own plan would be answering it from inside
// the model that cannot see the other side (placement).
func planZones(
	cfg config.Config,
	live []unifi.FirewallZone,
	facts zoneFacts,
	inUse referenced,
	bound bindings,
	opts Options,
) ([]Change, []Caveat) {
	byName := make(map[string]unifi.FirewallZone, len(live))
	for _, zone := range live {
		byName[zone.Name] = zone
	}
	placed := placeNetworks(cfg, live, facts, bound)

	changes := make([]Change, 0, len(cfg.Zones))
	named := make(map[string]bool, len(cfg.Zones))
	for _, desired := range cfg.Zones {
		named[desired.Name] = true

		current, exists := byName[desired.Name]
		if !exists {
			changes = append(changes, createZone(desired, bound, placed))
			continue
		}
		if change, differs := updateZone(desired, current, bound, placed); differs {
			changes = append(changes, change)
		}
	}
	if !opts.Prune {
		return changes, nil
	}
	// Prune was asked for and the zones were left out of it, so the plan says so
	// rather than looking like a site with nothing to prune (ADR-0005, #23).
	if !facts.known {
		return changes, []Caveat{{
			Kind: Zone,
			Reason: "no zone will be deleted: unifig could not read which zones the Controller marks as its own, " +
				"and deleting the wrong one would take the site off the internet",
		}}
	}
	deletions, caveats := pruneZones(byName, named, facts, inUse, bound)
	return append(changes, deletions...), caveats
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

// zoneMarker is the part of a zone that go-unifi's FirewallZone does not carry:
// the library models `attr_no_delete` and `attr_no_edit`, and a real zone has
// neither of the first and uses the second for what its own UI offers rather
// than for anything the API enforces (ADR-0019).
//
// It answers two questions off one response, and they are different questions.
// `default_zone` is the Controller saying a zone is its own, which is what
// prune's exemption turns on (ADR-0005). `zone_key` is the Controller saying
// *which* of its own a zone is, which is what a Risky change turns on: the
// Gateway zone is where the Controller answers, so a policy blocking traffic to
// it can cut the path the site is managed over (ADR-0018).
//
// An earlier version of this type refused `zone_key`, on the grounds that
// reading it would be keeping a list of Ubiquiti's zones by another route. That
// holds for ownership, where the Controller answers directly and a list of names
// would be second-guessing it. It does not hold here, because `default_zone`
// cannot say which zone is the gateway at all: the choice is not between the
// Controller's answer and a list, it is between `zone_key` and hard-coding the
// string "Gateway" — which is the construct that had prune proposing to delete
// every built-in (issue #23).
type zoneMarker struct {
	ID      string `json:"_id"`
	Name    string `json:"name"`
	Default bool   `json:"default_zone"`
	ZoneKey string `json:"zone_key"`
}

// gatewayZoneKey is the Controller's own key for the zone it answers in, and
// internalZoneKey for the zone it puts a network in when nothing else holds one.
//
// The key is matched and the name beside it is read, rather than the name being
// matched: an operator cannot rename a built-in today, but a firmware that
// presented either zone under another name would silently stop the rule that
// depends on it — a Risky mark that never fires, or a plan telling an operator
// their network lands somewhere it does not — and silence is the one failure
// mode neither rule can afford.
const (
	gatewayZoneKey  = "gateway"
	internalZoneKey = "internal"
)

// zoneFacts is what the Controller says about its own zones, and whether it
// could be asked at all.
//
// The distinction is the whole point of the type. An empty set means the site
// genuinely has no built-in zones; an unknown set means unifig could not find
// out, and the two must not lead to the same behaviour, because one of them ends
// in a plan proposing to delete the zone that stands for the internet.
//
// All three fields share the one `known` flag because they come from one
// response: there is no state in which the Controller answered about its zones
// and unifig learned one of these and not the others.
type zoneFacts struct {
	known   bool
	builtIn map[string]bool
	// gateway is the name of the zone the Controller answers in, and empty when
	// the site has none. It is held as a name rather than an ID because a policy
	// states its ends as names and is matched by them (policyKey), so a name is
	// what the risk check has to compare.
	gateway string
	// internal is the name of the zone the Controller moves a network to when
	// nothing else holds it, and empty when the site has none — or when unifig
	// could not read the keys at all, which is why a plan that cannot name it
	// says the Controller will choose rather than naming the wrong zone
	// (ADR-0020).
	internal string
}

func (f zoneFacts) owns(zone unifi.FirewallZone) bool { return f.builtIn[zone.ID] }

// readZoneFacts reads what the Controller says about its own zones: which are
// built-in, which of those is the one it answers in, and which is the one it
// falls back to for a network no other zone holds.
//
// It reads `default_zone`, which is the marker a zone carries. That is not the
// marker a network carries: a network says the same thing with
// `attr_no_delete`, and unifig read the network's marker on a zone until a
// recording from migrated hardware showed no zone has that field at all
// (issue #23, ADR-0005). The exemption is still read off the object rather than
// from a list of built-in names unifig keeps — what changed is which field is
// read, not the principle.
//
// It reads `zone_key` beside it for the gateway, and both come out of one
// response rather than two: it is one GET, and asking twice would give two
// answers about a Controller that may have changed between them.
//
// The request goes through the same client, so it carries the same
// authentication and reaches the same Controller as everything else. What it
// cannot borrow is the path: go-unifi resolves a relative path under the v1 API
// base and only a leading-slash path is used as it stands, so the v2 path has to
// be given in full — and it differs between the two controller styles, which the
// client does not expose after resolving. Hence trying both.
//
// Only the first of the two has ever answered anything, and this used to say
// otherwise: the dockerized Controller is old-style, but the rig fronts it with
// a proxy that answers the SDK's style probe (ADR-0003), so unifig sees a
// new-style Controller in every suite it has. It sees one in the field too —
// API-key auth is a UniFi OS gate, and the SDK refuses to build a client for a
// Controller that answers the probe the old way. So the second base is
// belt-and-braces rather than a style anybody exercises, and a write does not
// get to be that relaxed: mergeIntoStoredPolicy names the new-style base and
// nothing else.
//
// Not reaching the endpoint and not understanding it are different answers, and
// only the first is worth trying the other path for. The body is taken as raw
// JSON so the two stay apart: a request that failed may be the wrong style, and
// the other one is tried; a request that succeeded and answered something this
// cannot read is a Controller unifig does not understand, which is a settled
// "cannot tell" rather than a reason to go asking elsewhere.
func readZoneFacts(ctx context.Context, client unifi.Client, site string) zoneFacts {
	for _, base := range []string{unifi.NewStyleAPI.ApiV2Path, unifi.OldStyleAPI.ApiV2Path} {
		var body json.RawMessage
		if err := client.Get(ctx, fmt.Sprintf("%s/site/%s/firewall/zone", base, site), nil, &body); err != nil {
			continue
		}

		var marked []zoneMarker
		if err := json.Unmarshal(body, &marked); err != nil {
			return zoneFacts{}
		}
		facts := zoneFacts{known: true, builtIn: make(map[string]bool, len(marked))}
		for _, zone := range marked {
			if zone.Default {
				facts.builtIn[zone.ID] = true
			}
			switch zone.ZoneKey {
			case gatewayZoneKey:
				facts.gateway = zone.Name
			case internalZoneKey:
				facts.internal = zone.Name
			}
		}
		return facts
	}
	return zoneFacts{}
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
func createZone(desired config.Zone, bound bindings, placed placement) Change {
	return Change{
		Action: Create,
		Kind:   Zone,
		Name:   desired.Name,
		Fields: setZoneFields(desired, placed),
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
func updateZone(desired config.Zone, live unifi.FirewallZone, bound bindings, placed placement) (Change, bool) {
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
	//
	// It goes first because it is about how to read the line above it, where the
	// notes after it are about what this change does to zones the plan does not
	// otherwise mention.
	if !whole && desired.Networks != nil {
		annotate(fields, "networks",
			"this zone also holds something that is not one of this site's LANs, which unifig does not name or change")
	}
	annotate(fields, "networks", placed.notes(desired.Name, current.Networks, desired.Networks)...)

	return Change{
		Action: Update,
		Kind:   Zone,
		Name:   desired.Name,
		Fields: fields,
		write: func(ctx context.Context, client unifi.Client, site string) error {
			// The live object goes back with only unifig's own fields changed,
			// so what the zone carries beside them survives an apply — as far
			// as it can: a field go-unifi does not model was dropped at
			// unmarshal, and a field the write endpoint refuses to be told
			// about is cleared here rather than sent.
			updated := writableZone(live)
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
// object's own marker rather than from a list of built-in names unifig keeps
// (ADR-0005). That matters more here than anywhere else it applies: a list that
// had never heard of some firmware's new built-in would have prune propose
// deleting the zone that stands for the internet, and an operator who approved
// it would find out what that means.
//
// If ownership could not be established, nothing is pruned: planZones returns
// before it calls this, carrying a caveat that says so. A zone deletion is the
// one change here whose blast radius makes silence the wrong default — not
// knowing which zones are the Controller's has to mean leaving all of them
// alone, rather than treating every one of them as fair game (issue #23) — and
// an unexplained empty result would be exactly that silence.
//
// A zone a policy will still be governing afterwards is not proposed either, and
// says so — the same rule as a network with a WLAN on it, and the same reason: a
// plan may not propose a deletion unifig can already tell would be refused
// (ADR-0014).
func pruneZones(
	live map[string]unifi.FirewallZone,
	named map[string]bool,
	facts zoneFacts,
	inUse referenced,
	bound bindings,
) ([]Change, []Caveat) {
	changes := make([]Change, 0, len(live))
	var caveats []Caveat
	for name, zone := range live {
		// `owns` reads `default_zone`, the marker a zone actually carries.
		// `NoDelete` is checked beside it because the library models the field on
		// this type, not because a zone has been seen carrying it: the marker is
		// per Resource and only a network is known to use that one (issue #23,
		// ADR-0005), so nothing here should be read as saying which field a new
		// type would use.
		if named[name] || zone.NoDelete || facts.owns(zone) {
			continue
		}
		if by := inUse[name]; len(by) > 0 {
			caveats = append(caveats, heldBack(Zone, name, by))
			continue
		}
		changes = append(changes, deleteZone(zone, bound))
	}
	return changes, caveats
}

// deleteZone is the Change that removes a live zone.
//
// A zone that goes lets go of its networks as surely as one that is emptied, so
// the line listing them owes the operator the same sentence an emptying does —
// otherwise `networks: "Lab"` under a `-` reads as a network going with the
// zone. What it does not owe them is a zone name. Where the Controller puts a
// network whose zone was deleted has never been measured: the write probe
// deleted an empty custom zone and one whose member another zone had already
// claimed (ADR-0019), so `Internal` here would be an inference from a different
// operation, which is the shape of guess this project has paid for three times.
// What the measurement does entail is that the network survives and is in some
// zone afterwards, because a network is in exactly one zone always — and that is
// the whole of what this says (ADR-0020).
func deleteZone(live unifi.FirewallZone, bound bindings) Change {
	current, _ := fromLiveZone(live, bound)
	return Change{
		Action: Delete,
		Kind:   Zone,
		Name:   live.Name,
		Fields: []Field{{Name: "networks", From: nameList(current.Networks), Notes: survives(current.Networks)}},
		write: func(ctx context.Context, client unifi.Client, site string) error {
			return client.DeleteFirewallZone(ctx, site, live.ID)
		},
	}
}

// setZoneFields lists what a create would set. A zone with no networks is listed
// rather than left out, for the same reason a WLAN with no passphrase is: a
// create has to produce a zone, and one with nothing in it is a zone no traffic
// is ever in — which is a consequence the config does not state.
//
// A zone born holding a network empties whichever zone held it, exactly as an
// update does, and the operator reading `+ zone` has even less reason to expect
// that than the one reading `~ zone`. There is no losing side to describe: a
// zone that did not exist a moment ago had nothing to let go of.
func setZoneFields(desired config.Zone, placed placement) []Field {
	if len(desired.Networks) == 0 {
		return []Field{{
			Name:  "networks",
			Notes: []string{"no network is listed, so this zone will hold none and no traffic will be in it"},
		}}
	}
	return []Field{{
		Name:  "networks",
		To:    nameList(desired.Networks),
		Notes: placed.notes(desired.Name, nil, desired.Networks),
	}}
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

// writableZone is a live zone as the Controller's write endpoint will take it:
// the read-only markers it sends on a GET and refuses on a PUT, cleared.
//
// A zone's read shape is not its write shape, and nothing in the type says so.
// The Controller answers a body carrying `attr_no_edit` with
// `400 JSON parse error: Unrecognized field "attr_no_edit" ... not marked as
// ignorable` — its write DTO has never heard of the field it has just been
// sent — and `omitempty` did the rest: the field goes out exactly when it is
// true, so unifig's payload was well-formed for the zones whose marker is false
// and malformed for the three whose marker is true. `Vpn` and `External` were
// both refused where the unmarked controls were not — a correlation the marker
// causes none of: a hand-built PUT carrying only `_id`, `name` and
// `network_ids` was accepted on `Vpn`, marked, by a real UDR (ADR-0019,
// issue #27).
//
// All four markers go rather than the one that has been seen to bite. They are
// the same shape with the same `omitempty`, so a firmware that starts putting
// `attr_hidden` on a zone reproduces this exactly — and would take a second
// hardware session to find out why, on a different zone.
func writableZone(live unifi.FirewallZone) unifi.FirewallZone {
	live.Hidden = false
	live.HiddenID = ""
	live.NoDelete = false
	live.NoEdit = false
	return live
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

// A network belongs to exactly one firewall zone, and the Controller is the one
// keeping it that way. Putting a network in a second zone takes it out of the
// first, in the same request unifig sent about the second, and taking it out of
// a zone does not leave it in none — the Controller moves it to the zone it
// keys `internal`. Neither move is anything unifig asked for, and neither used
// to appear anywhere in a plan: one PUT went to one zone, the plan said `1 to
// update`, and two zones changed (ADR-0020, measured in ADR-0019).
//
// placement is what lets a plan say so. It holds three things that only make
// sense beside each other — where each network is now, where this file puts it,
// and where the Controller puts one that nothing claims — because every sentence
// about a membership change needs at least two of them. It is built once per
// plan, from reads the plan already does.
//
// The model underneath is the correction. unifig holds membership as a property
// of the zone (`config.Zone.Networks`, `zone.NetworkIDs`) and the Controller
// holds it as a property of the network. Both models describe the same site, and
// only the second one can say what a single write does — so the writes stay as
// they are and the plan is generated with the network's side in hand.
type placement struct {
	// holders names the zones the Controller has each network in now.
	//
	// It is a list rather than a single zone even though a network is in exactly
	// one, because that is the Controller's rule rather than a shape unifig can
	// enforce on what it reads. A response saying otherwise is a response to
	// report faithfully rather than one to pick a winner from — the plan names
	// every zone the network is leaving, and the sentence reads in the plural.
	// This repository's own recording was such a response until issue #32, and
	// the fix was to the recording (`tools/record-udr/scrub.go`) rather than to
	// a reader that had quietly decided which zone to believe.
	holders map[string][]string
	// claimed names the zone this file states each network into, and is what
	// keeps two sentences about one network from contradicting each other: a
	// network the plan itself rehomes is not one the Controller has to find a
	// zone for.
	claimed map[string]string
	// reassigned is the zone the Controller moves an unclaimed network to, and
	// is empty when unifig could not tell which zone that is. Empty is not
	// "none": it is "unifig cannot say which", and the sentence changes shape
	// rather than naming a guess.
	reassigned string
}

// placeNetworks reads where every network the site's zones hold is now, and
// where this config would put it.
//
// Only members unifig can name are in it. A zone can hold something the config
// has no word for — the built-in External zone holds the WAN — and such a member
// is neither displaced nor rehomed by anything here, because nothing here can
// state it (ADR-0004, overwriteManagedZone).
func placeNetworks(cfg config.Config, live []unifi.FirewallZone, facts zoneFacts, bound bindings) placement {
	placed := placement{
		holders:    make(map[string][]string, len(bound.networkNames)),
		claimed:    make(map[string]string, len(cfg.Zones)),
		reassigned: facts.internal,
	}
	for _, zone := range live {
		for _, id := range zone.NetworkIDs {
			if name := bound.networkName(id); name != "" {
				placed.holders[name] = append(placed.holders[name], zone.Name)
			}
		}
	}
	for _, zones := range placed.holders {
		slices.Sort(zones)
	}
	// A zone stating no membership claims nothing — ADR-0004, and the reason an
	// operator can name a built-in zone to write policies about it without
	// taking over what is in it. Two zones claiming one network is refused
	// offline (config.checkReferences), so the first one wins here only as the
	// answer to a question that cannot be asked.
	for _, zone := range cfg.Zones {
		if zone.Networks == nil {
			continue
		}
		for _, name := range zone.Networks {
			if _, taken := placed.claimed[name]; !taken {
				placed.claimed[name] = zone.Name
			}
		}
	}
	return placed
}

// notes is what a zone's membership change does beyond the list it prints:
// which networks it takes out of another zone, and where the ones it lets go
// end up. Both sides in one call, because both are read off the same before and
// after and an operator reads them under the same field.
//
// before is the membership unifig can name, so a create passes none.
func (p placement) notes(zone string, before, after []string) []string {
	var notes []string
	for _, network := range after {
		if slices.Contains(before, network) {
			continue
		}
		if from := p.elsewhere(network, zone); len(from) > 0 {
			notes = append(notes, displaces(network, from))
		}
	}
	for _, network := range before {
		if slices.Contains(after, network) {
			continue
		}
		notes = append(notes, p.rehomes(network, zone))
	}
	return notes
}

// elsewhere is the zones holding this network that are not the one gaining it.
func (p placement) elsewhere(network, zone string) []string {
	from := make([]string, 0, len(p.holders[network]))
	for _, holder := range p.holders[network] {
		if holder != zone {
			from = append(from, holder)
		}
	}
	return from
}

// survives says that the networks a deleted zone holds are not deleted with it,
// and is nothing at all for a zone holding none unifig can name.
//
// One sentence for the whole membership rather than one per network, because
// unlike a displacement it says the same thing about each of them and names no
// second zone to tell them apart by.
func survives(networks []string) []string {
	if len(networks) == 0 {
		return nil
	}
	noun, is, them := kinds[Network].one, "is", "it"
	if len(networks) > 1 {
		noun, is, them = kinds[Network].many, "are", "each of them"
	}
	return []string{fmt.Sprintf(
		"the %s %s %s not deleted with the zone: a network belongs to exactly one zone, so the Controller will put %s in another one — unifig cannot say which, having never watched it happen",
		noun, andJoin(quoted(networks)), is, them)}
}

// displaces says that joining this zone is also leaving another one — the
// sentence issue #32 exists for. It names the zone being emptied, because the
// operator moving a network into a zone is the one person who cannot see it
// happening: nothing in the plan and nothing in the apply's report used to
// mention that zone at all, and on a site using zones for isolation the one
// silently emptied is the one that was containing something.
func displaces(network string, from []string) string {
	one := kinds[Zone].one
	if len(from) > 1 {
		one = kinds[Zone].many
	}
	return fmt.Sprintf(
		"the network %q is in the %s %s now, and a network belongs to exactly one zone: applying this takes it out",
		network, one, andJoin(quoted(from)))
}

// rehomes says where a network this zone lets go ends up, which is never
// nowhere. A membership rendered as `"Guest" -> (none)` reads as a network
// left outside every zone, and that is the one outcome the Controller will not
// produce.
//
// Three sentences rather than one, because there are three different answers.
// The plan may be putting the network somewhere itself, in which case saying
// the Controller will choose would be a plan contradicting itself two lines
// apart. Otherwise the Controller chooses, and unifig either knows which zone
// that is or does not — and a plan that could not read the keys says so rather
// than naming the zone that usually wins.
func (p placement) rehomes(network, zone string) string {
	outside := fmt.Sprintf("the network %q does not end up outside every zone: ", network)
	if to := p.claimed[network]; to != "" && to != zone {
		return outside + fmt.Sprintf("this plan puts it in the zone %q", to)
	}
	if p.reassigned == "" {
		return outside + "a network belongs to exactly one, and the Controller will move this one to a zone of its own choosing"
	}
	return outside + fmt.Sprintf("a network belongs to exactly one, so the Controller will move this one to %q", p.reassigned)
}
