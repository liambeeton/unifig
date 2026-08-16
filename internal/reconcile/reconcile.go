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
//     and even then the Controller's own undeletable objects are exempt. This
//     is the same rule as the one above, one level up: the file states what
//     unifig manages, not what may exist.
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

// Change is one Resource apply would create or update, in the terms an
// operator reads and a pipeline parses.
type Change struct {
	Action Action `json:"action"`
	// Resource is the managed type, e.g. "network".
	Resource string `json:"resource"`
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
	// has to move because the subnet under it did. Rare, and always shown —
	// a plan that quietly did more than it printed would not be a plan.
	Note string `json:"note,omitempty"`
}

// Empty reports whether the Controller already matches the config.
func (p Plan) Empty() bool { return len(p.Changes) == 0 }

// Networks computes the Plan that would make the site's networks match cfg.
//
// Without opts.Prune, live networks the config does not mention are left out of
// the plan entirely. That is the "nothing is destroyed implicitly" rule rather
// than an omission: an operator adopting unifig on a configured Controller can
// start with one network in their file and trust that the rest are not at
// stake. With it, those same networks become deletions — shown in the plan
// like any other change, so nothing is destroyed unannounced either.
func Networks(ctx context.Context, client unifi.Client, site string, cfg config.Config, opts Options) (Plan, error) {
	live, err := liveNetworks(ctx, client, site)
	if err != nil {
		return Plan{}, err
	}

	changes := make([]Change, 0, len(cfg.Networks))
	named := make(map[string]bool, len(cfg.Networks))
	for _, desired := range cfg.Networks {
		named[desired.Name] = true

		current, exists := live[desired.Name]
		if !exists {
			changes = append(changes, createNetwork(desired))
			continue
		}
		if change, differs := updateNetwork(desired, current); differs {
			changes = append(changes, change)
		}
	}
	if opts.Prune {
		changes = append(changes, pruneNetworks(live, named)...)
	}
	sortChanges(changes)
	return Plan{Changes: changes}, nil
}

// ListNetworks reads the site's networks and keeps the ones unifig manages.
//
// Export calls this too, for the same reason it shares the projection: the
// config export writes must be config that plans clean, and it cannot be if
// the two verbs disagree about which live networks are even in scope. The
// uniqueness check below is part of that agreement — an export of a Controller
// with two networks named the same would write a file that unifig's own
// validate rejects.
func ListNetworks(ctx context.Context, client unifi.Client, site string) ([]unifi.Network, error) {
	all, err := client.ListNetwork(ctx, site)
	if err != nil {
		return nil, fmt.Errorf("listing networks for site %q: %w", site, err)
	}

	managed := make([]unifi.Network, 0, len(all))
	for _, network := range all {
		if Managed(network) {
			managed = append(managed, network)
		}
	}
	if err := uniquelyNamed(managed); err != nil {
		return nil, err
	}
	return managed, nil
}

// uniquelyNamed reports whether any two live networks share the name unifig
// matches them by, as the error saying so.
//
// Matching by natural key is unambiguous only while keys are unique (ADR-0001),
// so a duplicate is where unifig stops rather than where it guesses. Nothing
// here can resolve it: which of two identically named networks the operator
// meant is a fact only they hold, and the place they hold it is the
// Controller's UI. Every duplicate goes into the one message, and the message
// says the rule rather than just the instance, so an operator can see that the
// list is the whole job and make that trip to the UI once.
func uniquelyNamed(live []unifi.Network) error {
	counts := make(map[string]int, len(live))
	for _, network := range live {
		counts[network.Name]++
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
		"unifig matches networks on the Controller by name, so every network needs a name of its own: this site has %s; rename or remove the extras in the Controller's UI, then run again",
		andJoin(found))
}

// andJoin joins phrases the way they would be said aloud: "a", "a and b",
// "a, b and c".
func andJoin(phrases []string) string {
	if len(phrases) < 2 {
		return strings.Join(phrases, "")
	}
	return strings.Join(phrases[:len(phrases)-1], ", ") + " and " + phrases[len(phrases)-1]
}

// liveNetworks indexes the site's managed networks by their natural key.
func liveNetworks(ctx context.Context, client unifi.Client, site string) (map[string]unifi.Network, error) {
	all, err := ListNetworks(ctx, client, site)
	if err != nil {
		return nil, err
	}

	byName := make(map[string]unifi.Network, len(all))
	for _, network := range all {
		byName[network.Name] = network
	}
	return byName, nil
}

// sortChanges makes plan output depend on what is changing rather than on the
// order the operator happened to list things in: creates first, then updates,
// each alphabetical. Two runs against the same Controller and config print
// byte-identical plans, which is what lets CI diff one against another.
func sortChanges(changes []Change) {
	slices.SortStableFunc(changes, func(a, b Change) int {
		if a.Action != b.Action {
			return actions[a.Action].order - actions[b.Action].order
		}
		return strings.Compare(a.Name, b.Name)
	})
}
