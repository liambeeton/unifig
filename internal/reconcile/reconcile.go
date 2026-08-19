// Package reconcile is unifig's engine: it computes the difference between a
// config file and the live Controller (a Plan), and executes it (an Apply).
//
// There is no state file (ADR-0001). A Plan is made by reading the Controller
// and comparing it to the config on the spot, which means a plan is only ever
// a statement about the Controller as it was a moment ago — and is why apply
// plans afresh rather than consuming a plan someone saved earlier.
//
// Three rules give the engine its shape, and all three fall out of
// statelessness:
//
//   - Resources are matched by natural key, never by stored ID. A live network
//     "is" the config's network when the names agree, and Controller IDs stay
//     inside this package: they never reach the config file, the plan output,
//     or the operator. Two live Resources sharing one key make the match
//     ambiguous, and that is where a reconcile stops rather than guesses.
//   - Only the fields the config actually states are written (ADR-0004), so
//     everything unifig does not model — DHCP ranges, DNS servers, IGMP
//     settings — survives an apply rather than being reset to the zero values
//     of a struct unifig built. A modelled field the file omits is unmanaged on
//     the same terms, never a request to empty it. How an update keeps that
//     promise is the endpoint's answer rather than one rule: the v1 verbs merge,
//     so an update sends its own fields and the Controller puts the object back
//     (ADR-0023), while a v2 PUT replaces, so an update carries back the whole
//     object the Controller sent (ADR-0021). Both were measured.
//   - Nothing is destroyed unless it was asked for. A Resource missing from
//     the config is unmanaged, not condemned; deleting it takes Options.Prune,
//     and even then the Controller's own undeletable objects are exempt, a
//     section the file leaves out entirely is out of prune's reach (ADR-0006),
//     and so is a Resource that something this plan leaves in place still
//     requires (ADR-0014). This is the same rule as the one above, one level
//     up: the file states what unifig manages, not what may exist.
//
// Two kinds of thing go through all of that, and only one of them has the
// lifecycle those rules describe. A Resource is created, updated and — on
// request — deleted. A Setting is a fixed slot the Controller always has, such
// as a WAN slot: it is only ever updated, no planner here can bring one into
// existence, and prune cannot see one. Both reach a plan as a Change, because
// what an operator reads and approves is the same either way.
//
// One thing sits above both: a Change that can cut the site off the internet
// carries the sentence that says so (Change.Risk), which is what makes the
// command layer stop and ask about that change on its own (ADR-0009).
//
// And one thing here is neither a Plan nor an Apply. Backup (backup.go) asks
// the Controller to write a copy of its own configuration before an apply
// touches anything, which changes nothing about the site and so is not a
// Change. It lives here because it is one more thing said to the Controller,
// through the same client, and because what it protects is the apply below it.
package reconcile

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/filipowm/go-unifi/v2/unifi"

	"github.com/liambeeton/unifig/internal/config"
)

// Action is what a Change does to the Controller.
//
// Delete only ever reaches a plan because prune was asked for. Removing a
// Resource from the config does not delete it; it means unifig stops managing
// it, and the two are different requests.
type Action string

const (
	Create Action = "create"
	Update Action = "update"
	Delete Action = "delete"
)

// actions is everything the rest of the package needs to know about an action
// beyond its name: where it sorts in a plan, the character that marks it, and
// how to say it once it has happened.
//
// One table rather than three maps in three files, because the alternative is
// that adding an action means remembering all three, and forgetting one is a
// silent blank in the output rather than a build failure.
//
// Delete sorts last, and that is the apply order too, because apply stops at
// the first failure: with the deletions at the end, an apply that fails partway
// has not destroyed anything, and the operator's Controller is missing changes
// rather than missing networks. It also puts the destructive half of a plan
// directly above the summary, where the eye lands.
var actions = map[Action]struct {
	order int
	mark  string
	past  string
}{
	Create: {order: 0, mark: "+", past: "created"},
	Update: {order: 1, mark: "~", past: "updated"},
	Delete: {order: 2, mark: "-", past: "deleted"},
}

// Kind is a managed type, as it appears in a plan.
//
// It covers both of the things unifig manages — Resources, which have a full
// create/update/delete lifecycle, and Settings, which are fixed slots that can
// only be updated — because a plan is a list of changes and a change to either
// reads the same way. Which one a kind is shows up in what its planner can
// produce: nothing plans a WAN slot into existence.
//
// It is a named type rather than a bare string for the same reason Action is:
// everything the engine needs to know about a type beyond its name lives in one
// table keyed by it, and a typo in a literal would otherwise be a silent
// mis-sort rather than a compile error.
type Kind string

const (
	Network         Kind = "network"
	WLAN            Kind = "wlan"
	Zone            Kind = "zone"
	FirewallPolicy  Kind = "firewall-policy"
	PortForward     Kind = "port-forward"
	DHCPReservation Kind = "dhcp-reservation"
	EncryptedDNS    Kind = "encrypted-dns"
	WANSlot         Kind = "wan"
)

// kinds is what the rest of the package needs to know about a managed type:
// where a change to it sits in a plan, and how to name it to an operator in the
// singular and the plural.
//
// The order is not cosmetic, because apply executes a plan in the order it
// printed and the two therefore cannot disagree. Among Resources it is
// dependency distance — a network references nothing, a WLAN references the
// network its clients join, a Zone references the networks it holds, a Firewall
// Policy references the Zones on either side of it — so building goes from the
// ground up. The direction flips for deletions, because the Controller will not
// let go of something still referenced.
//
// WLANs and Zones both reference networks and neither references the other, so
// which of them comes first is arbitrary; they are ordered as they were added,
// and a DHCP Reservation joins them on the same terms. Its reference to a
// network is the one the Controller infers rather than stores — an address is
// reserved on whichever network's subnet contains it — but it constrains the
// order exactly as a stated one would, in both directions: the network has to
// exist before an address inside it can be reserved, and the Controller refuses
// to delete a network with an address reserved inside it.
//
// A Port Forward references nothing at all — its target is an address rather
// than a name — so nothing constrains where it goes and it sits after the
// Resources that do reference something, which is where a reader looking for the
// dependency chain stops looking.
//
// A Setting references nothing and nothing references it, so its place is
// decided by what a mistake costs instead. The WAN slot goes last, which means
// an apply that fails earlier has not touched the uplink, and one that gets
// there has already done all the safe work. Encrypted DNS sits just before it,
// on the same reasoning one step down: it can break name resolution for the
// whole site, and there is no sense in changing how the site resolves names
// before the changes that decide what there is to resolve.
//
// A singleton's plural is unreachable by construction — the messages that use
// `many` are about telling two of something apart, and there are never two
// Encrypted DNS settings. It is filled in anyway rather than left blank,
// because the failure mode of a missing entry is a sentence with a hole in it
// rather than a compile error.
var kinds = map[Kind]struct {
	order     int
	one, many string
}{
	Network:         {order: 0, one: "network", many: "networks"},
	WLAN:            {order: 1, one: "WLAN", many: "WLANs"},
	Zone:            {order: 2, one: "zone", many: "zones"},
	FirewallPolicy:  {order: 3, one: "firewall policy", many: "firewall policies"},
	DHCPReservation: {order: 4, one: "DHCP reservation", many: "DHCP reservations"},
	PortForward:     {order: 5, one: "port forward", many: "port forwards"},
	EncryptedDNS:    {order: 6, one: "Encrypted DNS setting", many: "Encrypted DNS settings"},
	WANSlot:         {order: 7, one: "WAN slot", many: "WAN slots"},
}

// Options are the choices a verb makes on the operator's behalf for a whole
// reconcile, rather than for one Resource.
type Options struct {
	// Prune deletes live Resources of a managed type that the config does not
	// name. It is off unless explicitly asked for, and that default is the
	// promise that makes unifig safe to adopt on a configured Controller: a
	// file naming one network puts no other network at stake.
	Prune bool
}

// Plan is the previewed set of changes a reconcile would make. Computing one
// mutates nothing.
type Plan struct {
	Changes []Change `json:"changes"`
	// Caveats are the places where this plan is narrower than what was asked
	// for, and why. See Caveat.
	Caveats []Caveat `json:"caveats"`
}

// Caveat is something a plan has to say about itself: unifig was asked to do
// something, declined to plan part of it, and the operator would otherwise have
// no way to tell.
//
// It is not a change, so it is not in Changes — a plan is a list of what will
// happen (ADR-0005) — and it is not an error, because the run is still correct.
// It is the third thing: an absence with a reason, which is invisible unless
// something says it out loud. An empty plan carrying a caveat is the case this
// exists for, because "No changes. The Controller already matches the config."
// would otherwise be a lie told confidently.
type Caveat struct {
	// Kind is the managed type the plan is quiet about.
	Kind Kind `json:"kind"`
	// Reason reads as a sentence to an operator, and says what was not done as
	// well as why.
	Reason string `json:"reason"`
}

// referenced is what a plan leaves pointing at the Resources prune would
// otherwise delete: a target's natural key against the phrases naming what will
// still be pointing at it. The keys are always the natural keys of one kind, and
// the values are the sentence fragments a Caveat is built from rather than
// objects.
//
// Each phrase says what its referencer goes on doing to the target as well as
// which referencer it is — `the WLAN "Home IoT" on it`, `the DHCP reservation
// "aa:bb:…" reserving an address inside it` — rather than the kind supplying one
// verb for all of them. That is because a target can be held by more than one
// kind of thing at once: a network is held by the WLANs on it and by the
// addresses reserved inside it, and an operator reads a plan in the terms of
// each pair in front of them.
//
// It is prune's third exemption, after the Controller's own objects and the
// sections the file leaves out, and it is there because a plan is a statement
// about what will happen. The Controller refuses to delete a Resource another
// one still requires — `api.err.ResourceReferredBy` — so a deletion unifig can
// see a live reference to is one it can already tell would be refused, and
// planning it anyway promises the operator something that cannot happen
// (ADR-0014).
//
// What goes in it is decided by the plan rather than by the Controller as it
// stands, because the two differ in exactly the case that matters: a WLAN this
// same apply deletes is not one keeping its network alive, which is what the
// reversed dependency order of the deletions exists to arrange.
type referenced map[string][]string

// heldBack is the Caveat for a deletion prune declined because the plan leaves
// something pointing at the Resource.
//
// Every referencer is named rather than counted, for the reason uniquelyNamed
// names every duplicate: the list is the whole job, and an operator who wants
// the deletion needs to know what all of it takes. Each carries its own phrase
// (see referenced), so a network held by both a WLAN and a reservation reads as
// one sentence about both rather than as two caveats about one network.
func heldBack(kind Kind, name string, by []string) Caveat {
	return Caveat{Kind: kind, Reason: fmt.Sprintf(
		"the %s %q will not be deleted: this plan leaves %s",
		kinds[kind].one, name, andJoin(slices.Sorted(slices.Values(by))))}
}

// merge folds another collection of referencers into this one, so that a target
// held by two kinds of thing produces one Caveat naming both.
func (r referenced) merge(other referenced) {
	for target, by := range other {
		r[target] = append(r[target], by...)
	}
}

// Change is one thing apply would create, update or delete, in the terms an
// operator reads and a pipeline parses.
type Change struct {
	Action Action `json:"action"`
	// Kind is the managed type, e.g. "network".
	Kind Kind `json:"kind"`
	// Name is how unifig matched this one — a Resource's natural key, a
	// Setting's slot — and the only identity that appears anywhere outside
	// this package.
	//
	// It is empty for a singleton Setting such as Encrypted DNS, of which a
	// Controller has exactly one: there was nothing to match on, so there is
	// nothing to report. The field stays in the JSON rather than being omitted,
	// so a consumer reads "" where there is no name instead of having to know
	// which kinds have one.
	Name   string  `json:"name"`
	Fields []Field `json:"fields"`
	// Risk is what an operator stands to lose by applying this change, in the
	// plain words they need to see it coming, and empty for the ordinary
	// changes that risk nothing. It is the whole of what makes a change Risky:
	// the plan prints it, apply asks about the change on its own before making
	// it, and a pipeline reading the JSON can gate on its presence.
	Risk string `json:"risk,omitempty"`

	// write performs this one change against the Controller.
	//
	// It is an opaque closure rather than data because of what it closes
	// over: an update needs the Controller ID of the live Resource, which is
	// precisely what ADR-0001 keeps out of the operator's world. Holding it
	// here means the ID is reachable by apply and by nothing else — not by
	// the renderer, not by the JSON output, not by a caller.
	//
	// Changes planned together also share one set of bindings, so a write can
	// read an ID that a write ahead of it has only just learned. That sharing
	// is what makes a network and a WLAN that joins it applicable in one pass,
	// and it is why apply must run changes in the order the plan lists them.
	write func(ctx context.Context, client unifi.Client, site string) error
}

// Field is one field a Change would set, and what the Controller holds now.
//
// Values keep the config's own types — a VLAN is a number, a subnet a string —
// so the JSON output needs no decoding on the other side. From is null for a
// create, and either end is null for a field being added or dropped.
type Field struct {
	Name string `json:"name"`
	From any    `json:"from"`
	To   any    `json:"to"`
	// Notes are the consequences of the change that the config does not state,
	// in the plain words an operator needs to see them coming: a DHCP pool that
	// has to move because the subnet under it did, a WLAN that will be open
	// because no passphrase was given, a network that leaves another zone
	// because this one claimed it. Always shown — a plan that quietly did more
	// than it printed would not be a plan.
	//
	// There is more than one because one field can carry more than one, and a
	// zone's membership is where that stopped being hypothetical: a single
	// `networks:` list can displace a network out of another zone, hand a second
	// back to the Controller, and hold a third that unifig cannot name, all at
	// once (ADR-0020). A single note would have had to pick one of them or run
	// them into one sentence, and each is about a different network.
	Notes []string `json:"notes,omitempty"`
	// Secret marks a field whose value must not be printed. Both ends stay
	// null for one, so the plan says that the field is changing without saying
	// what to: a plan is read aloud, pasted into tickets and captured by CI
	// logs, and none of those are places for a passphrase.
	Secret bool `json:"secret,omitempty"`
}

// Empty reports whether the Controller already matches the config.
func (p Plan) Empty() bool { return len(p.Changes) == 0 }

// bindings is how the engine moves between a Resource's natural key and the
// Controller ID that references to it need — the stored IDs ADR-0001 keeps out
// of the config file entirely, held where only this package can reach them.
//
// Both directions live here because they are two halves of one table and are
// wanted at opposite ends of the same job: a name is what a WLAN's config states
// and its plan prints, an ID is what the Controller stores.
//
// The name-to-ID half is the one that makes dependency-ordered apply work. A
// WLAN names the network its clients join, but the Controller wants that
// network's ID, and when the network is being created in the same apply the ID
// does not exist at the moment the plan is made. So the map is seeded from the
// live Controller while planning, each create writes its new ID into it as it
// lands, and a reference's write reads it at the moment it runs rather than the
// moment it was planned. That, plus the order of the plan, is the whole of it.
//
// Two kinds of thing are bound rather than one, because references run two deep:
// a Zone names networks and a Firewall Policy names Zones, so a file declaring a
// network, a zone holding it and a policy governing it applies in a single pass
// on exactly the same mechanism.
type bindings struct {
	// networks maps a network's name to its Controller ID, and grows during
	// apply.
	networks map[string]string
	// networkNames maps the other way, and is fixed once the plan is made — a
	// network created during an apply is not one any live WLAN or Zone can
	// already be on.
	networkNames map[string]string
	// zones and zonesByID are the same pair for Zones: seeded from the live
	// Controller, and grown by each zone create so a policy planned onto a new
	// zone can find it. Both directions are kept for the same reason the
	// networks' are — a policy states its ends as names and the Controller
	// stores them as IDs, so each direction is wanted at one end of the job.
	zones     map[string]string
	zonesByID map[string]string
}

func newBindings(live []unifi.Network) bindings {
	bound := bindings{
		networks:     make(map[string]string, len(live)),
		networkNames: make(map[string]string, len(live)),
		zones:        map[string]string{},
		zonesByID:    map[string]string{},
	}
	for _, network := range live {
		bound.networks[network.Name] = network.ID
		bound.networkNames[network.ID] = network.Name
	}
	return bound
}

// bindZones seeds the Zone half from the live Controller, once the zones have
// been read. It is separate from newBindings because the two collections are
// read at different points and only some reconciles read the zones at all.
func (b bindings) bindZones(live []unifi.FirewallZone) {
	for _, zone := range live {
		b.zones[zone.Name] = zone.ID
		b.zonesByID[zone.ID] = zone.Name
	}
}

// zoneName is the name unifig knows a zone by, and empty for an ID that is not
// one of the zones this site has. Only the live zones are in it: a zone created
// during an apply is not one any live policy can already point at.
func (b bindings) zoneName(id string) string { return b.zonesByID[id] }

// networkID is the Controller ID of the network with this name, or the error
// saying there is none. The clause says what the network was wanted for, since
// that is the part of the sentence an operator needs in order to know which line
// of their file to look at.
func (b bindings) networkID(name, wanted string) (string, error) {
	id := b.networks[name]
	if id == "" {
		return "", fmt.Errorf("the Controller has no network named %q %s", name, wanted)
	}
	return id, nil
}

// networkName is the name unifig knows a network by, or empty when the ID is
// not one of the networks unifig manages.
func (b bindings) networkName(id string) string { return b.networkNames[id] }

// zoneID is the Controller ID of the zone with this name, or the error saying
// there is none — listing the zones the site really has, because a policy naming
// a zone that exists nowhere is most often a misspelling of a built-in one.
func (b bindings) zoneID(name string) (string, error) {
	id := b.zones[name]
	if id == "" {
		return "", noSuchZone(name, b.zoneNames())
	}
	return id, nil
}

// zoneNames is every zone unifig can resolve a reference to, in a stable order.
func (b bindings) zoneNames() []string {
	names := make([]string, 0, len(b.zones))
	for name := range b.zones {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// ComputePlan is the Plan that would make the site match cfg.
//
// A section the config does not have is not planned at all, and the check for
// that sits here — once, visibly applied to every section — rather than inside
// each section's planner, so that adding the next resource area cannot
// accidentally give prune a wider reach than the file asked for. That is
// ADR-0006: a file with no `wlans:` key says nothing about WLANs, so unifig
// manages none of them and a prune it takes part in cannot delete one.
//
// Without opts.Prune, live Resources the config does not mention are left out
// of the plan entirely. That is the "nothing is destroyed implicitly" rule
// rather than an omission: an operator adopting unifig on a configured
// Controller can start with one network in their file and trust that the rest
// are not at stake. With it, those same Resources become deletions — shown in
// the plan like any other change, so nothing is destroyed unannounced either.
//
// Skipping a section's plan is not skipping its read, and prune is the second
// reason for that after the network names a WLAN needs. A prune has to know what
// still points at the Resources it would delete, so the referencing collection
// is read whenever a deletion could be proposed for the collection it points at
// — even where the file has no section for it and nothing in it will change
// (ADR-0014). Which sections the file has still decides only what unifig will
// change, which is the part ADR-0006 is about.
//
// Each referencing area is therefore planned before the area it points at, so
// that the second can ask the first what it is leaving behind. That order has
// nothing to do with the order an operator reads: sortChanges decides that, and
// puts the deletions the other way round.
func ComputePlan(ctx context.Context, client unifi.Client, site string, cfg config.Config, opts Options) (Plan, error) {
	// Networks are read whichever sections the config has, because a WLAN's
	// binding is stated as a network name and stored as a network ID, and
	// nothing can translate between the two without them. The WAN slots come
	// out of the same read (see listNetworkConf).
	all, err := listNetworkConf(ctx, client, site)
	if err != nil {
		return Plan{}, err
	}
	live, err := lans(all)
	if err != nil {
		return Plan{}, err
	}
	bound := newBindings(live)

	var changes []Change
	var caveats []Caveat

	// The WLANs are read whenever the file manages them, and whenever a prune
	// could propose deleting a network: the Controller will not delete a network a
	// WLAN is still on, and a plan may not say otherwise.
	var wlans []unifi.WLAN
	if cfg.WLANs != nil || (opts.Prune && cfg.Networks != nil) {
		wlans, _, err = listWLANs(ctx, client, site, bound)
		if err != nil {
			return Plan{}, err
		}
	}
	// A section the file leaves out is one nothing in this plan deletes, so every
	// WLAN in it survives — and so does every network one of them is on.
	sparedWLANs := wlans
	if cfg.WLANs != nil {
		wlanChanges, left, err := planWLANs(cfg, wlans, bound, opts)
		if err != nil {
			return Plan{}, err
		}
		changes = append(changes, wlanChanges...)
		sparedWLANs = left
	}
	// The client records are read on the same terms as the WLANs, and the second
	// reason is the same one step sideways: the Controller will not delete a
	// network with an address reserved inside it, so a prune of the networks has
	// to know what is reserved whether or not the file manages reservations.
	var clients []unifi.User
	if cfg.DHCPReservations != nil || (opts.Prune && cfg.Networks != nil) {
		clients, err = listClientRecords(ctx, client, site)
		if err != nil {
			return Plan{}, err
		}
	}
	// A section the file leaves out is one nothing in this plan gives up, so
	// every live reservation survives — and so does every network one of them
	// reserves an address inside.
	sparedReservations := reservationsAmong(clients)
	if cfg.DHCPReservations != nil {
		reservationChanges, left, err := planDHCPReservations(cfg, clients, opts)
		if err != nil {
			return Plan{}, err
		}
		changes = append(changes, reservationChanges...)
		sparedReservations = left
	}
	if cfg.Networks != nil {
		byName := make(map[string]unifi.Network, len(live))
		for _, network := range live {
			byName[network.Name] = network
		}
		// Two collections hold a network back rather than one, and they are
		// merged rather than asked in turn, because what prune needs is one
		// answer per network: an operator told their network stays needs the
		// whole of what is keeping it, not the first thing found.
		inUse := networksInUse(sparedWLANs, cfg.WLANs, bound)
		inUse.merge(reservedWithin(sparedReservations, cfg.DHCPReservations, live))
		networkChanges, networkCaveats := planNetworks(cfg, byName, inUse, bound, opts)
		changes = append(changes, networkChanges...)
		caveats = append(caveats, networkCaveats...)
	}
	// The zones are read whenever either firewall section is present, because a
	// policy states its ends as zone names and the Controller stores them as
	// zone IDs — the same reason the networks are read whatever sections the
	// file has. Reading them once and binding them here is also what keeps the
	// two halves of one plan talking about the same moment in time.
	if cfg.Zones != nil || cfg.FirewallPolicies != nil {
		zones, err := listZones(ctx, client, site)
		if err != nil {
			return Plan{}, err
		}
		bound.bindZones(zones)

		// What the Controller says about its own zones is read whenever the
		// zones are, which used to be narrower: the answer was wanted for prune,
		// which needs to know which zones are built-in, and for a policy, which
		// needs to know which one is the gateway — so it was read on exactly the
		// terms the policies are. It is read unconditionally now because a third
		// question needs it and is asked by every plan that touches a
		// membership: which zone the Controller moves a network to when nothing
		// else holds it (ADR-0020). A plan that manages zones and can neither
		// delete one nor govern one still changes what is in them, and that is
		// the case the old condition left unable to say where a network went.
		facts := readZoneFacts(ctx, client, site)

		// The policies are read on the same terms as the WLANs above, and for the
		// same reason one step along: a zone with a policy on either end of it is
		// a zone something still requires.
		var policies []unifi.FirewallZonePolicy
		if cfg.FirewallPolicies != nil || (opts.Prune && cfg.Zones != nil) {
			policies, err = listFirewallPolicies(ctx, client, site)
			if err != nil {
				return Plan{}, err
			}
		}
		sparedPolicies := policies
		if cfg.FirewallPolicies != nil {
			policyChanges, left, policyCaveats, err := planFirewallPolicies(cfg, policies, facts, bound, opts)
			if err != nil {
				return Plan{}, err
			}
			changes = append(changes, policyChanges...)
			caveats = append(caveats, policyCaveats...)
			sparedPolicies = left
		}
		if cfg.Zones != nil {
			zoneChanges, zoneCaveats := planZones(cfg, zones, facts, zonesInUse(sparedPolicies, bound), bound, opts)
			changes = append(changes, zoneChanges...)
			caveats = append(caveats, zoneCaveats...)
		}
	}
	// The port forwards are read only when the file manages them, and never on
	// prune's account the way the WLANs and the policies are: nothing holds a
	// forward back and a forward holds nothing back, so no other section has a
	// question for this one.
	if cfg.PortForwards != nil {
		forwards, err := listPortForwards(ctx, client, site)
		if err != nil {
			return Plan{}, err
		}
		forwardChanges, forwardCaveats, err := planPortForwards(cfg, forwards, opts)
		if err != nil {
			return Plan{}, err
		}
		changes = append(changes, forwardChanges...)
		caveats = append(caveats, forwardCaveats...)
	}
	if cfg.EncryptedDNS != nil {
		dns, err := planEncryptedDNS(ctx, client, site, *cfg.EncryptedDNS)
		if err != nil {
			return Plan{}, err
		}
		changes = append(changes, dns...)
	}
	if cfg.WAN != nil {
		slots, err := planWANSlots(cfg, all)
		if err != nil {
			return Plan{}, err
		}
		changes = append(changes, slots...)
	}

	sortChanges(changes)
	sortCaveats(caveats)
	return Plan{Changes: changes, Caveats: caveats}, nil
}

// Project is the config that describes the Controller as it stands — the
// Controller-to-config direction of the same correspondence ComputePlan uses in
// reverse, and what export writes.
//
// It lives here rather than in export because a second opinion about which live
// Resources are in scope, or about what one looks like in config, would show up
// as an operator's freshly exported file planning dirty. There is one answer,
// and both verbs read it from here.
//
// The Notices are what the file could not say, so that one which came back
// short says so. Plan is silent about the same things — something unifig does
// not manage is not a change, and a plan is a list of changes — but export is
// the adoption path, and an operator being told a file describes their
// Controller should not discover otherwise later.
func Project(ctx context.Context, client unifi.Client, site string) (config.Config, Notices, error) {
	all, err := listNetworkConf(ctx, client, site)
	if err != nil {
		return config.Config{}, Notices{}, err
	}
	live, err := lans(all)
	if err != nil {
		return config.Config{}, Notices{}, err
	}
	networks := make([]config.Network, 0, len(live))
	for _, network := range live {
		networks = append(networks, fromLiveNetwork(network))
	}
	slices.SortFunc(networks, func(a, b config.Network) int { return strings.Compare(a.Name, b.Name) })

	bound := newBindings(live)
	liveWLANs, indescribable, err := listWLANs(ctx, client, site, bound)
	if err != nil {
		return config.Config{}, Notices{}, err
	}
	// Export matches too, in the sense that matters here: a file describing two
	// WLANs unifig cannot tell apart is a file it cannot plan afterwards.
	if err := uniquelyNamedWLANs(liveWLANs); err != nil {
		return config.Config{}, Notices{}, err
	}
	wlans := make([]config.WLAN, 0, len(liveWLANs))
	for _, wlan := range liveWLANs {
		wlans = append(wlans, fromLiveWLAN(wlan, bound))
	}
	slices.SortFunc(wlans, func(a, b config.WLAN) int { return strings.Compare(a.Name, b.Name) })

	liveZones, err := listZones(ctx, client, site)
	if err != nil {
		return config.Config{}, Notices{}, err
	}
	bound.bindZones(liveZones)
	zones, partialZones := projectZones(liveZones, bound)

	policies, indescribablePolicies, err := projectFirewallPolicies(ctx, client, site, bound)
	if err != nil {
		return config.Config{}, Notices{}, err
	}

	forwards, indescribableForwards, err := projectPortForwards(ctx, client, site)
	if err != nil {
		return config.Config{}, Notices{}, err
	}

	reservations, err := projectDHCPReservations(ctx, client, site)
	if err != nil {
		return config.Config{}, Notices{}, err
	}

	slots, partial, err := projectWANSlots(all)
	if err != nil {
		return config.Config{}, Notices{}, err
	}

	dns, unmodelledState, err := projectEncryptedDNS(ctx, client, site)
	if err != nil {
		return config.Config{}, Notices{}, err
	}

	return config.Config{
			Networks:         networks,
			WLANs:            wlans,
			Zones:            zones,
			FirewallPolicies: policies,
			PortForwards:     forwards,
			DHCPReservations: reservations,
			WAN:              slots,
			EncryptedDNS:     dns,
		},
		Notices{
			IndescribableWLANs:        indescribable,
			PartialZones:              partialZones,
			IndescribablePolicies:     indescribablePolicies,
			IndescribablePortForwards: indescribableForwards,
			PartialWANSlots:           partial,
			NoEncryptedDNS:            dns == nil,
			UnmodelledDNSState:        unmodelledState,
		}, nil
}

// Notices are the things export has to say out loud about the config it just
// wrote, because they are the places where the file is not the whole truth
// about the Controller.
//
// They are not failures and not warnings. Each names something unifig
// deliberately does not manage, which is also why prune leaves them alone.
type Notices struct {
	// IndescribableWLANs names the WLANs left out of the config entirely,
	// because there is no network unifig manages for them to name (see
	// listWLANs).
	IndescribableWLANs []string
	// PartialZones names the zones whose membership the config states only in
	// part, because they hold something that is not one of this site's LANs —
	// the WAN network in a built-in External zone, most often. The zone is in the
	// file and unifig manages the members it can name; the rest it leaves alone
	// rather than removing (see overwriteManagedZone).
	PartialZones []string
	// IndescribablePolicies names the firewall policies left out of the config
	// entirely, because a zone on one end of them is one unifig cannot name.
	IndescribablePolicies []string
	// IndescribablePortForwards names the port forwards left out of the config
	// entirely, because a port of theirs is a range or a list rather than the
	// single port unifig models.
	IndescribablePortForwards []string
	// PartialWANSlots names the WAN slots written as nothing but a slot,
	// because the way they connect is not one unifig models.
	PartialWANSlots []string
	// NoEncryptedDNS says the config has no `encrypted-dns:` section because
	// this Controller has no such setting to describe, rather than because
	// export declined to describe it.
	NoEncryptedDNS bool
	// UnmodelledDNSState is the Encrypted DNS state the config could not carry,
	// because it is not one unifig models — the same shape of shortfall as a
	// PartialWANSlot, and empty whenever the state came through.
	UnmodelledDNSState string
}

// uniquelyNamed reports whether any two live Resources share the name unifig
// matches them by, as the error saying so.
//
// Matching by natural key is unambiguous only while keys are unique (ADR-0001),
// so a duplicate is where unifig stops rather than where it guesses. Nothing
// here can resolve it: which of two identically named Resources the operator
// meant is a fact only they hold, and the place they hold it is the
// Controller's UI. Every duplicate goes into the one message, and the message
// says the rule rather than just the instance, so an operator can see that the
// list is the whole job and make that trip to the UI once.
func uniquelyNamed(kind Kind, names []string) error {
	counts := make(map[string]int, len(names))
	for _, name := range names {
		counts[name]++
	}

	var shared []string
	for name, count := range counts {
		if count > 1 {
			shared = append(shared, name)
		}
	}
	if len(shared) == 0 {
		return nil
	}

	slices.Sort(shared)
	found := make([]string, 0, len(shared))
	for _, name := range shared {
		found = append(found, fmt.Sprintf("%d named %q", counts[name], name))
	}
	return fmt.Errorf(
		"unifig matches %s on the Controller by name, so every %s needs a name of its own: this site has %s; rename or remove the extras in the Controller's UI, then run again",
		kinds[kind].many, kinds[kind].one, andJoin(found))
}

// quoted is the same values with quotes around them, for a message that lists
// what an operator wrote or what the Controller holds.
func quoted(values []string) []string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, fmt.Sprintf("%q", value))
	}
	return quoted
}

// andJoin joins phrases the way they would be said aloud: "a", "a and b",
// "a, b and c".
func andJoin(phrases []string) string {
	if len(phrases) < 2 {
		return strings.Join(phrases, "")
	}
	return strings.Join(phrases[:len(phrases)-1], ", ") + " and " + phrases[len(phrases)-1]
}

// sortChanges makes plan output depend on what is changing rather than on the
// order the operator happened to list things in: creates first, then updates,
// then deletions; the Risky ones last within their group; then the order the
// kinds table gives, and alphabetical within a kind. Two runs against the same
// Controller and config print byte-identical plans, which is what lets CI diff
// one against another.
//
// None of it is cosmetic. Apply walks this slice in order, so a network is
// created before the WLAN that joins it, and a WLAN is deleted before the
// network it was on.
//
// Risk sorting last is what leaves the changes that can cut the site off until
// the safe work is done. That used to be a property of the kinds table — the WAN
// slot is last in it — which worked while the WAN was the only Risky kind and
// stopped working the moment a Firewall Policy could be one: a policy sits at
// order 3 because it must follow the zones it names, so a policy that blocks the
// management path would otherwise be applied before the reservations, the
// forwards and the uplink, and an operator who approved it would find the rest
// of their plan unreachable. Sorting on risk instead makes "left until last" a
// property of being Risky, which is where ADR-0009 put every other part of this.
//
// It is dependency-safe because nothing references a Firewall Policy, and
// because a create that must precede another change is in the earlier action
// group either way. It changes nothing about a plan whose only Risky change is a
// WAN slot, which is every plan unifig produced before this.
func sortChanges(changes []Change) {
	slices.SortStableFunc(changes, func(a, b Change) int {
		if a.Action != b.Action {
			return actions[a.Action].order - actions[b.Action].order
		}
		if risky(a) != risky(b) {
			return risky(a) - risky(b)
		}
		if a.Kind != b.Kind {
			distance := kinds[a.Kind].order - kinds[b.Kind].order
			if a.Action == Delete {
				return -distance
			}
			return distance
		}
		return strings.Compare(a.Name, b.Name)
	})
}

// risky orders a Risky change after an ordinary one. It reads the sentence
// rather than the kind, because the sentence is the whole of what makes a change
// Risky (ADR-0009) and two changes of one kind need not agree: a Firewall Policy
// that opens a path carries none and one that closes the management path does.
func risky(c Change) int {
	if c.Risk != "" {
		return 1
	}
	return 0
}

// sortCaveats does for the caveats what sortChanges does for the changes, and
// for the same reason: a caveat about one Resource is found while walking a live
// collection, so without this the order two runs print them in is whatever order
// a map happened to hand them over. Kinds sort as they do in a plan, and the
// sentences alphabetically within a kind, which is by the name they lead with.
func sortCaveats(caveats []Caveat) {
	slices.SortStableFunc(caveats, func(a, b Caveat) int {
		if a.Kind != b.Kind {
			return kinds[a.Kind].order - kinds[b.Kind].order
		}
		return strings.Compare(a.Reason, b.Reason)
	})
}

// annotate attaches a note to the named field, and does nothing if the change
// does not include it. A consequence with nothing to hang it off is a
// consequence of something that is not happening, which is not a case worth
// distinguishing from having nothing to say.
//
// Notes accumulate in the order they are attached rather than replacing each
// other, so a caller with several things to say about one field says them all
// and decides the order it wants them read in.
func annotate(fields []Field, name string, notes ...string) {
	if len(notes) == 0 {
		return
	}
	for i := range fields {
		if fields[i].Name == name {
			fields[i].Notes = append(fields[i].Notes, notes...)
			return
		}
	}
}

// annotateFirst attaches a note to the first of several fields the change
// actually has, and to its first field if it has none of them.
//
// It exists because some consequences are not about one field. "This will
// encrypt nothing" follows from the state and the resolver list together, and
// which of the two is in the plan depends on which one the operator is
// changing — so hanging the note off a single named field would drop it exactly
// when the other one moved. The preferred names are the ones the note reads
// best under; the fallback is that a note about the change as a whole belongs
// in the change rather than nowhere.
func annotateFirst(fields []Field, note string, names ...string) {
	if len(fields) == 0 {
		return
	}
	for _, name := range names {
		for i := range fields {
			if fields[i].Name == name {
				fields[i].Notes = append(fields[i].Notes, note)
				return
			}
		}
	}
	fields[0].Notes = append(fields[0].Notes, note)
}

// number and text render a field the Controller does not have yet as nothing
// at all rather than as 0 or "", so that putting a VLAN on an untagged network
// reads as `(none) -> 20` instead of the `0 -> 20` that would look like a VLAN
// ID the network used to have.
func number(value int) any {
	if value == 0 {
		return nil
	}
	return value
}

func text(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// nameList renders a set of names as one field value: quoted and
// comma-separated, and nothing at all where there are none, so that adding the
// first entry to an empty list reads as `(none) -> "Quad9"` rather than
// as `"" -> "Quad9"`.
func nameList(names []string) any {
	return text(strings.Join(quoted(names), ", "))
}
