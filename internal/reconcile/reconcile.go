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
//   - Only the fields the config actually states are written (ADR-0004). An
//     update reads the live Resource, overwrites those fields and puts the
//     whole object back, so everything unifig does not model — DHCP ranges,
//     DNS servers, IGMP settings — survives an apply rather than being reset
//     to the zero values of a struct unifig built. A modelled field the file
//     omits is unmanaged on the same terms, never a request to empty it.
//   - Nothing is destroyed unless it was asked for. A Resource missing from
//     the config is unmanaged, not condemned; deleting it takes Options.Prune,
//     and even then the Controller's own undeletable objects are exempt, and a
//     section the file leaves out entirely is out of prune's reach (ADR-0006).
//     This is the same rule as the one above, one level up: the file states
//     what unifig manages, not what may exist.
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

// Resource is a managed type, as it appears in a plan.
//
// It is a named type rather than a bare string for the same reason Action is:
// everything the engine needs to know about a type beyond its name lives in one
// table keyed by it, and a typo in a literal would otherwise be a silent
// mis-sort rather than a compile error.
type Resource string

const (
	Network Resource = "network"
	WLAN    Resource = "wlan"
)

// resources is what the rest of the package needs to know about a managed type:
// how far it sits from the things it references, and how to name it to an
// operator in the singular and the plural.
//
// The distance is what makes a plan dependency-ordered — a network references
// nothing, a WLAN references the network its clients join — and apply executes a
// plan in the order it printed, so the two cannot disagree. The direction flips
// for deletions: building goes from the ground up, and dismantling goes the
// other way, because the Controller will not let go of something still
// referenced.
var resources = map[Resource]struct {
	depends   int
	one, many string
}{
	Network: {depends: 0, one: "network", many: "networks"},
	WLAN:    {depends: 1, one: "WLAN", many: "WLANs"},
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
}

// Change is one Resource apply would create, update or delete, in the terms an
// operator reads and a pipeline parses.
type Change struct {
	Action Action `json:"action"`
	// Resource is the managed type, e.g. "network".
	Resource Resource `json:"resource"`
	// Name is the Resource's natural key — how unifig matched it, and the
	// only identity that appears anywhere outside this package.
	Name   string  `json:"name"`
	Fields []Field `json:"fields"`

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
	// Note is a consequence of the change that the config does not state, in
	// the plain words an operator needs to see it coming: a DHCP pool that
	// has to move because the subnet under it did, a WLAN that will be open
	// because no passphrase was given. Always shown — a plan that quietly did
	// more than it printed would not be a plan.
	Note string `json:"note,omitempty"`
	// Secret marks a field whose value must not be printed. Both ends stay
	// null for one, so the plan says that the field is changing without saying
	// what to: a plan is read aloud, pasted into tickets and captured by CI
	// logs, and none of those are places for a passphrase.
	Secret bool `json:"secret,omitempty"`
}

// Empty reports whether the Controller already matches the config.
func (p Plan) Empty() bool { return len(p.Changes) == 0 }

// bindings is how the engine moves between a network's natural key and the
// Controller ID that references to it need — the stored IDs ADR-0001 keeps out
// of the config file entirely, held where only this package can reach them.
//
// Both directions live here because they are two halves of one table and are
// wanted at opposite ends of the same job: a name is what a WLAN's config
// states and its plan prints, an ID is what the Controller stores.
//
// The name-to-ID half is the one that makes dependency-ordered apply work. A
// WLAN names the network its clients join, but the Controller wants that
// network's ID, and when the network is being created in the same apply the ID
// does not exist at the moment the plan is made. So the map is seeded from the
// live Controller while planning, each network create writes its new ID into it
// as it lands, and a WLAN's write reads it at the moment it runs rather than the
// moment it was planned. That, plus the order of the plan, is the whole of it.
type bindings struct {
	// ids maps a network's name to its Controller ID, and grows during apply.
	ids map[string]string
	// names maps the other way, and is fixed once the plan is made — a network
	// created during an apply is not one any live WLAN can already be on.
	names map[string]string
}

func newBindings(live []unifi.Network) bindings {
	bound := bindings{
		ids:   make(map[string]string, len(live)),
		names: make(map[string]string, len(live)),
	}
	for _, network := range live {
		bound.ids[network.Name] = network.ID
		bound.names[network.ID] = network.Name
	}
	return bound
}

// networkID is the Controller ID of the network with this name, or the error
// saying there is none.
func (b bindings) networkID(name string) (string, error) {
	id := b.ids[name]
	if id == "" {
		return "", fmt.Errorf(
			"the Controller has no network named %q for this WLAN's clients to join", name)
	}
	return id, nil
}

// networkName is the name unifig knows a network by, or empty when the ID is
// not one of the networks unifig manages.
func (b bindings) networkName(id string) string { return b.names[id] }

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
func ComputePlan(ctx context.Context, client unifi.Client, site string, cfg config.Config, opts Options) (Plan, error) {
	// Networks are read whichever sections the config has, because a WLAN's
	// binding is stated as a network name and stored as a network ID, and
	// nothing can translate between the two without them.
	live, err := listNetworks(ctx, client, site)
	if err != nil {
		return Plan{}, err
	}
	bound := newBindings(live)

	var changes []Change
	if cfg.Networks != nil {
		byName := make(map[string]unifi.Network, len(live))
		for _, network := range live {
			byName[network.Name] = network
		}
		changes = append(changes, planNetworks(cfg, byName, bound, opts)...)
	}
	if cfg.WLANs != nil {
		wlans, err := planWLANs(ctx, client, site, cfg, bound, opts)
		if err != nil {
			return Plan{}, err
		}
		changes = append(changes, wlans...)
	}

	sortChanges(changes)
	return Plan{Changes: changes}, nil
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
// The second return names the WLANs left out because unifig cannot describe
// them (see listWLANs). Plan says nothing about those — a Resource unifig does
// not manage is not a change, and a plan is a list of changes — but export is
// the adoption path, and a file that quietly came back short is one an operator
// should hear about while they are still adopting.
func Project(ctx context.Context, client unifi.Client, site string) (config.Config, []string, error) {
	live, err := listNetworks(ctx, client, site)
	if err != nil {
		return config.Config{}, nil, err
	}
	networks := make([]config.Network, 0, len(live))
	for _, network := range live {
		networks = append(networks, fromLiveNetwork(network))
	}
	slices.SortFunc(networks, func(a, b config.Network) int { return strings.Compare(a.Name, b.Name) })

	bound := newBindings(live)
	liveWLANs, indescribable, err := listWLANs(ctx, client, site, bound)
	if err != nil {
		return config.Config{}, nil, err
	}
	wlans := make([]config.WLAN, 0, len(liveWLANs))
	for _, wlan := range liveWLANs {
		wlans = append(wlans, fromLiveWLAN(wlan, bound))
	}
	slices.SortFunc(wlans, func(a, b config.WLAN) int { return strings.Compare(a.Name, b.Name) })

	return config.Config{Networks: networks, WLANs: wlans}, indescribable, nil
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
func uniquelyNamed(resource Resource, names []string) error {
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
		resources[resource].many, resources[resource].one, andJoin(found))
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
// then deletions, each group in dependency order and alphabetical within a
// type. Two runs against the same Controller and config print byte-identical
// plans, which is what lets CI diff one against another.
//
// The dependency ordering is not cosmetic. Apply walks this slice in order, so
// a network is created before the WLAN that joins it and a WLAN is deleted
// before the network it was on — which is what lets a config that declares both
// apply in one pass.
func sortChanges(changes []Change) {
	slices.SortStableFunc(changes, func(a, b Change) int {
		if a.Action != b.Action {
			return actions[a.Action].order - actions[b.Action].order
		}
		if a.Resource != b.Resource {
			distance := resources[a.Resource].depends - resources[b.Resource].depends
			if a.Action == Delete {
				return -distance
			}
			return distance
		}
		return strings.Compare(a.Name, b.Name)
	})
}

// annotate attaches a note to the named field, and does nothing if the change
// does not include it. A consequence with nothing to hang it off is a
// consequence of something that is not happening, which is not a case worth
// distinguishing from having nothing to say.
func annotate(fields []Field, name, note string) {
	for i := range fields {
		if fields[i].Name == name {
			fields[i].Note = note
			return
		}
	}
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
