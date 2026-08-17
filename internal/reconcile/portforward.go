package reconcile

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/filipowm/go-unifi/v2/unifi"

	"github.com/liambeeton/unifig/internal/config"
)

// A Port Forward is the Resource with nothing on either side of it. A WLAN names
// the network its clients join, a Zone the networks it holds, a Firewall Policy
// the Zones it governs — a forward names an address, which is a value rather than
// a reference. So nothing here resolves a name against the Controller, nothing
// binds an ID for something else to read, and no deletion anywhere is held back
// because a forward still points at something (ADR-0014): the Controller has no
// reference to refuse.
//
// What it does have is exposure, and that is the whole reason the section exists:
// a file that lists the forwards is a file that says what the network answers to
// from the internet. That shapes two decisions. Every field but the source is
// required, because a forward that did not state its port, its host and its
// protocol would be a record of nothing; and a create the config states no source
// for says in the plan that it will accept traffic from anywhere, because
// unifig will have made that so and the file did not say it.
//
// None of it is Risky by the test ADR-0012 settled. Opening a port cannot cut the
// site off the internet or take the Controller out of reach, and closing one that
// should have stayed open is undone by putting it back — recovery never needs
// physical access, so a forward is a change to show, not a change to stop and ask
// about one at a time.

// modelledProtocols are the values unifig's `protocol` models, which are the
// Controller's own three spellings rather than a translation of them: they are
// already lowercase, so there is nothing for a mapping table to fix and nothing
// for the two spellings to drift apart over.
var modelledProtocols = map[string]bool{"tcp": true, "udp": true, "tcp_udp": true}

// anySource is the Controller's own value for a forward that accepts traffic
// from anywhere, and what a create with no source stated ends up with. It is a
// constant rather than two literals because the plan prints it and the write
// sends it: a plan naming a different value from the one apply writes would be
// exactly the quiet difference a plan exists to rule out.
const anySource = "any"

// planPortForwards is the port forward half of a reconcile. Its caller only
// reaches it when the config has a `port-forwards:` section at all (ADR-0006), so
// a file that says nothing about forwards changes none of them.
//
// Unlike the WLAN and policy halves it reports no spared collection, because
// nothing is spared on its account: a forward holds nothing back from prune.
func planPortForwards(cfg config.Config, live []unifi.PortForward, opts Options) ([]Change, []Caveat, error) {
	if err := uniquelyNamedPortForwards(live); err != nil {
		return nil, nil, err
	}

	byName := make(map[string]unifi.PortForward, len(live))
	for _, forward := range live {
		byName[forward.Name] = forward
	}

	changes := make([]Change, 0, len(cfg.PortForwards))
	named := make(map[string]bool, len(cfg.PortForwards))
	for _, desired := range cfg.PortForwards {
		named[desired.Name] = true

		current, exists := byName[desired.Name]
		if !exists {
			changes = append(changes, createPortForward(desired))
			continue
		}
		if change, differs := updatePortForward(desired, current); differs {
			changes = append(changes, change)
		}
	}
	if !opts.Prune {
		return changes, nil, nil
	}
	deletions, caveats := prunePortForwards(live, named)
	return append(changes, deletions...), caveats, nil
}

// listPortForwards reads the site's port forwards.
//
// Nothing is filtered here, for the reason listFirewallPolicies filters nothing:
// reading is not matching. Which of them unifig can describe is a question the
// projection answers, and the answer differs by verb — a forward the config names
// is managed whether or not export could have written it.
func listPortForwards(ctx context.Context, client unifi.Client, site string) ([]unifi.PortForward, error) {
	live, err := client.ListPortForward(ctx, site)
	if err != nil {
		return nil, fmt.Errorf("listing port forwards for site %q: %w", site, err)
	}
	return live, nil
}

// uniquelyNamedPortForwards refuses a site holding two forwards unifig could not
// tell apart (ADR-0001).
//
// It is asked by the verbs that match forwards to config rather than by the read,
// on the same reasoning as uniquelyNamedWLANs: nothing is bound to a forward, so a
// duplicate obstructs only the matching.
func uniquelyNamedPortForwards(live []unifi.PortForward) error {
	names := make([]string, 0, len(live))
	for _, forward := range live {
		names = append(names, forward.Name)
	}
	return uniquelyNamed(PortForward, names)
}

// projectPortForwards projects the site's forwards into the config that would
// describe them, and names the ones it could not describe at all.
//
// A forward is left out when either of its ports is not a single port — the
// Controller will hold a range or a comma-separated list, and unifig models one
// port arriving and one port inside. There is no partial way to write one: a port
// is half of what a forward is, so it is left out whole, the way an
// indescribable policy is rather than the way a partial zone is.
func projectPortForwards(ctx context.Context, client unifi.Client, site string) ([]config.PortForward, []string, error) {
	live, err := listPortForwards(ctx, client, site)
	if err != nil {
		return nil, nil, err
	}
	// Export matches too, in the sense that matters here: a file describing two
	// forwards unifig cannot tell apart is a file it cannot plan afterwards.
	if err := uniquelyNamedPortForwards(live); err != nil {
		return nil, nil, err
	}

	forwards := make([]config.PortForward, 0, len(live))
	var indescribable []string
	for _, forward := range live {
		described, ok := fromLivePortForward(forward)
		if !ok {
			indescribable = append(indescribable, forward.Name)
			continue
		}
		forwards = append(forwards, described)
	}
	slices.SortFunc(forwards, func(a, b config.PortForward) int { return strings.Compare(a.Name, b.Name) })
	slices.Sort(indescribable)
	return forwards, indescribable, nil
}

// fromLivePortForward projects a live forward into the config that would describe
// it, and whether the config can describe the whole of it.
//
// It fills in every field it can either way, and the flag is about the forward
// rather than about the struct, because the two callers want different things
// from the same reading. Anything writing config has to check the flag: a forward
// missing a field the schema requires is one export leaves out whole. Anything
// diffing wants the fields regardless — a forward whose ports are a range is
// still on the address it is on, and reporting that address as absent would put
// a change in the plan that nothing is changing.
//
// The source comes back as the Controller holds it, `any` included, rather than
// being dropped as a default worth omitting. A file that leaves the key out says
// unifig manages nothing about where traffic may come from, and an exported file
// saying that about a forward open to the whole internet would be describing the
// exposure by omitting it.
func fromLivePortForward(live unifi.PortForward) (config.PortForward, bool) {
	port, single := singlePort(live.DstPort)
	forwardPort, singleForward := singlePort(live.FwdPort)
	described := config.PortForward{
		Name:        live.Name,
		Port:        port,
		ForwardIP:   live.Fwd,
		ForwardPort: forwardPort,
		Protocol:    live.Proto,
		Source:      live.Src,
	}
	return described, single && singleForward && modelledProtocols[live.Proto] && live.Fwd != ""
}

// createPortForward is the Change for a forward the Controller does not have.
func createPortForward(desired config.PortForward) Change {
	return Change{
		Action: Create,
		Kind:   PortForward,
		Name:   desired.Name,
		Fields: setPortForwardFields(desired),
		write: func(ctx context.Context, client unifi.Client, site string) error {
			forward := newPortForward(desired)
			_, err := client.CreatePortForward(ctx, site, &forward)
			return err
		},
	}
}

// updatePortForward is the Change that brings a live forward in line with the
// config, and whether there is one to make at all.
func updatePortForward(desired config.PortForward, live unifi.PortForward) (Change, bool) {
	fields := changedPortForwardFields(live, desired)
	if len(fields) == 0 {
		return Change{}, false
	}

	return Change{
		Action: Update,
		Kind:   PortForward,
		Name:   desired.Name,
		Fields: fields,
		write: func(ctx context.Context, client unifi.Client, site string) error {
			// The live object goes back with only unifig's own fields changed, so
			// the uplink the forward listens on, the logging switch and whether it
			// is enabled at all survive an apply rather than being reset by an
			// object unifig built from scratch.
			updated := live
			overwriteManagedPortForward(&updated, desired)
			_, err := client.UpdatePortForward(ctx, site, &updated)
			return err
		},
	}, true
}

// prunePortForwards is the Changes that would delete every live forward the
// config does not name, and the Caveats for the ones it declined to.
//
// A forward unifig cannot describe is spared, and that is this type's own
// exemption. Export leaves such a forward out of the file, so an operator who
// adopted their Controller has a config that does not name it — and prune
// deleting what the adoption could not describe is the failure that would make
// the brownfield path unusable. It is the same rule that keeps a WLAN bound to
// something unnameable out of prune's reach, arrived at from the other end:
// there, the file could not name it; here, the file could not state it.
//
// Unlike that WLAN, this one is said out loud. An unnameable WLAN never reaches
// the collection prune walks, while this is a decision taken about a forward that
// did — an operator who asked for a prune and got all but one of it is owed the
// sentence saying which (ADR-0005). It is a Caveat rather than a change, because
// what is being reported is a deletion that will not happen.
//
// `NoDelete` is checked because the library models the field on this type, not
// because a forward has been seen carrying it: the marker is per Resource, only a
// network is known to use that one, and the recording carries no forwards to
// answer the question from (ADR-0005, ADR-0011). Nothing here should be read as
// saying which field marks a forward the Controller owns, or that it ships any.
func prunePortForwards(live []unifi.PortForward, named map[string]bool) ([]Change, []Caveat) {
	changes := make([]Change, 0, len(live))
	var caveats []Caveat
	for _, forward := range live {
		if named[forward.Name] || forward.NoDelete {
			continue
		}
		if _, describable := fromLivePortForward(forward); !describable {
			caveats = append(caveats, indescribableForward(forward.Name))
			continue
		}
		changes = append(changes, deletePortForward(forward))
	}
	return changes, caveats
}

// indescribableForward is the Caveat for a deletion prune declined because the
// config has no way to state the forward in the first place.
func indescribableForward(name string) Caveat {
	return Caveat{Kind: PortForward, Reason: fmt.Sprintf(
		"the %s %q will not be deleted: a port of it is a range or a list rather than a single port, which unifig cannot state, so export leaves it out of the config and prune leaves it where it is",
		kinds[PortForward].one, name)}
}

// deletePortForward is the Change that removes a live forward.
//
// It lists everything the forward does, because that is how an operator
// recognises what they are about to close: "the one sending 443 to the NAS" is
// the forward, and its name may say less about it than the mapping does.
func deletePortForward(live unifi.PortForward) Change {
	return Change{
		Action: Delete,
		Kind:   PortForward,
		Name:   live.Name,
		Fields: currentPortForwardFields(live),
		write: func(ctx context.Context, client unifi.Client, site string) error {
			return client.DeletePortForward(ctx, site, live.ID)
		},
	}
}

// setPortForwardFields lists what a create would set.
//
// A forward with no source stated is listed rather than left out, which is the
// same exception setWLANFields makes and for the same reason. Omission means
// unmanaged, and for an update that means nothing happens; but a create has to
// produce a forward, and a forward unifig creates without a source is one anyone
// on the internet can reach. That is a consequence the config does not state, so
// the plan states it.
//
// It states it as a value and not only as a sentence, which is where it parts
// company with setWLANFields. That one has nothing truthful to put on the To
// side: a WLAN with no passphrase is created `open`, and open is not a value of
// the field the note hangs off. A forward with no source is created with `any`,
// which is a value `source` really takes — so leaving it blank would print a
// plan that did more than it said (ADR-0004), and would leave a pipeline reading
// the JSON with nothing but prose to gate on. The note says why the value is
// there; the value says what will be written.
func setPortForwardFields(desired config.PortForward) []Field {
	fields := []Field{
		{Name: "port", To: desired.Port},
		{Name: "forward-ip", To: desired.ForwardIP},
		{Name: "forward-port", To: desired.ForwardPort},
		{Name: "protocol", To: desired.Protocol},
	}
	if desired.Source != "" {
		return append(fields, Field{Name: "source", To: desired.Source})
	}
	return append(fields, Field{
		Name: "source",
		To:   anySource,
		Note: "the config states no source, so this forward accepts traffic from anywhere on the internet",
	})
}

// currentPortForwardFields is setPortForwardFields' mirror: what a delete would
// take away, on the From side because there is no value on the other one.
func currentPortForwardFields(live unifi.PortForward) []Field {
	current, _ := fromLivePortForward(live)
	return []Field{
		{Name: "port", From: portValue(live.DstPort)},
		{Name: "forward-ip", From: text(current.ForwardIP)},
		{Name: "forward-port", From: portValue(live.FwdPort)},
		{Name: "protocol", From: text(current.Protocol)},
		{Name: "source", From: text(current.Source)},
	}
}

// changedPortForwardFields lists the managed fields on which the Controller and
// the config disagree.
//
// Everything but the source is compared unconditionally, because the schema lets
// none of them be omitted: a forward in the config always states its port, its
// host and its protocol. A source the config leaves out is unmanaged as usual, so
// unifig leaves whatever the Controller has — `any`, or the one address an
// operator restricted it to in the UI.
//
// It reads the live object as well as the projection of it, and that is what the
// From side of a port is rendered from. A forward whose port unifig cannot state
// still has one, and an operator taking `27015-27020` down to a single port
// should read that rather than a `(none)` claiming the Controller held nothing
// there. The projection decides what *differs*, as it does for every other kind;
// the live object decides how the losing side is written.
func changedPortForwardFields(live unifi.PortForward, desired config.PortForward) []Field {
	current, _ := fromLivePortForward(live)

	fields := make([]Field, 0, 5)
	if current.Port != desired.Port {
		fields = append(fields, Field{Name: "port", From: portValue(live.DstPort), To: desired.Port})
	}
	if current.ForwardIP != desired.ForwardIP {
		fields = append(fields,
			Field{Name: "forward-ip", From: text(current.ForwardIP), To: desired.ForwardIP})
	}
	if current.ForwardPort != desired.ForwardPort {
		fields = append(fields,
			Field{Name: "forward-port", From: portValue(live.FwdPort), To: desired.ForwardPort})
	}
	if current.Protocol != desired.Protocol {
		fields = append(fields,
			Field{Name: "protocol", From: text(current.Protocol), To: desired.Protocol})
	}
	if desired.Source != "" && current.Source != desired.Source {
		fields = append(fields, Field{Name: "source", From: text(current.Source), To: desired.Source})
	}
	return fields
}

// overwriteManagedPortForward writes the config's values onto a Controller port
// forward and touches nothing else — the single place that decides which fields
// unifig owns.
func overwriteManagedPortForward(forward *unifi.PortForward, desired config.PortForward) {
	forward.Name = desired.Name
	forward.DstPort = strconv.Itoa(desired.Port)
	forward.Fwd = desired.ForwardIP
	forward.FwdPort = strconv.Itoa(desired.ForwardPort)
	forward.Proto = desired.Protocol
	if desired.Source != "" {
		forward.Src = desired.Source
	}
}

// newPortForward builds the Controller object for a forward unifig is creating.
//
// The four values below are the Controller's own defaults for a new forward,
// matching what its UI creates: on, from anywhere, on the primary uplink, to
// whichever address that uplink answers on. A forward built from a bare struct
// would instead be disabled and bound to no uplink, which forwards nothing.
//
// They apply on create only. An operator who afterwards moves the forward onto a
// second WAN, turns on logging or disables it for a while keeps that, because
// updates go through overwriteManagedPortForward, which never touches anything
// here — and `any` is replaced immediately below when the config states a source.
func newPortForward(desired config.PortForward) unifi.PortForward {
	forward := unifi.PortForward{
		Enabled:       true,
		PfwdInterface: "wan",
		DestinationIP: "any",
		Src:           anySource,
	}
	overwriteManagedPortForward(&forward, desired)
	return forward
}

// singlePort reads one of the Controller's port fields as the single port unifig
// models, and reports whether it is one at all. A range or a comma-separated list
// is not, and neither is a number outside the range a port can take.
func singlePort(value string) (int, bool) {
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return 0, false
	}
	return port, true
}

// portValue renders a Controller port field for a plan: the number where it is a
// single port, and otherwise the Controller's own text, so that a range shows up
// as the range it is.
func portValue(value string) any {
	if port, single := singlePort(value); single {
		return port
	}
	return text(value)
}
