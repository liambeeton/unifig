package reconcile

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/filipowm/go-unifi/v2/unifi"

	"github.com/liambeeton/unifig/internal/config"
)

// Encrypted DNS — DNS Shield in the Controller's UI — is the second Setting
// unifig manages and the first singleton, and that word is what this file is
// about. A WAN slot is one of several fixed slots, matched by the Controller's
// own name for it (ADR-0010). A Controller has exactly one Encrypted DNS
// setting, so there is nothing to match on at all: the config section is a
// mapping rather than a list, the change it produces carries no name, and the
// question "which one?" simply does not arise. Everything else a Setting means
// still holds — the only verb is update, no planner here can bring one into
// existence, and prune cannot see it.
//
// The one genuinely new thing is a list-valued field. `custom_servers` is a
// single field of one setting rather than a collection of Resources, so
// ADR-0004 applies to it whole: a config with no `servers:` key does not manage
// the list at all, and a config that states it states the list. A server that
// drops out of the file is therefore removed from the Controller — announced in
// the plan like every other change, and never a prune, because prune is about
// Resources the file has stopped naming and this is one field taking a new
// value.

// stateCustom is the Controller's own word for using the custom servers rather
// than its built-in providers. Everything unifig writes to the list is
// pointless in any other state, which is why the planner says so.
const stateCustom = "custom"

// dohStates are the states unifig models, and the list the JSON Schema's own
// enum mirrors. It is the Encrypted DNS counterpart of wanTypes and exists for
// the same reason: what the Controller can be set to is the Controller's
// answer, so a state unifig has never heard of is one it describes by leaving
// the field out rather than by writing something validate would then reject
// (ADR-0010).
var dohStates = map[string]bool{
	"off":       true,
	"auto":      true,
	"manual":    true,
	stateCustom: true,
}

// planEncryptedDNS is the Encrypted DNS half of a reconcile. Its caller only
// reaches it when the config has an `encrypted-dns:` section at all, so a file
// that says nothing about it does not even read the setting.
//
// It takes the section rather than the whole config because the nil check that
// decides whether to call it lives in the caller, and a function that would
// panic if that check were ever missed should not be reachable with a nil to
// dereference.
//
// At most one Change comes back, because there is at most one thing to change.
func planEncryptedDNS(ctx context.Context, client unifi.Client, site string, desired config.EncryptedDNS) ([]Change, error) {
	live, held, err := readEncryptedDNS(ctx, client, site)
	if err != nil {
		return nil, err
	}
	// A Controller that does not have the setting is an error rather than an
	// empty one, and the difference matters: unifig cannot create a Setting, so
	// there is nothing it could do with the section beyond report that this
	// Controller has nowhere to put it. Older Network versions predate the
	// feature entirely. Export takes the other branch — see projectEncryptedDNS.
	if !held {
		return nil, noEncryptedDNSSetting(site)
	}
	if _, err := fromLiveEncryptedDNS(*live); err != nil {
		return nil, err
	}
	if change, differs, err := updateEncryptedDNS(desired, *live); err != nil {
		return nil, err
	} else if differs {
		return []Change{change}, nil
	}
	return nil, nil
}

// readEncryptedDNS reads the Controller's Encrypted DNS setting and says
// whether there was one, leaving what to make of its absence to the caller —
// which is the only thing plan and export disagree about here.
func readEncryptedDNS(ctx context.Context, client unifi.Client, site string) (*unifi.SettingDoh, bool, error) {
	live, err := client.GetSettingDoh(ctx, site)
	if errors.Is(err, unifi.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("reading the Encrypted DNS setting for site %q: %w", site, err)
	}
	return live, true, nil
}

// projectEncryptedDNS is the export direction: the section that describes the
// Controller's Encrypted DNS setting, the state it could not describe, or
// nothing at all where the Controller has no such setting.
//
// Absence is not an error here, which is the one place this differs from
// planning. Export is the adoption path and runs against whatever Controller it
// is pointed at; a file that leaves the section out is a correct description of
// a Controller that does not have one, and the notice says so out loud.
func projectEncryptedDNS(ctx context.Context, client unifi.Client, site string) (*config.EncryptedDNS, string, error) {
	live, held, err := readEncryptedDNS(ctx, client, site)
	if err != nil || !held {
		return nil, "", err
	}
	described, err := fromLiveEncryptedDNS(*live)
	if err != nil {
		return nil, "", err
	}

	// The state the file could not carry, so export can say so rather than
	// quietly writing a section that describes less than it appears to.
	var unmodelled string
	if described.State == "" {
		unmodelled = live.State
	}
	return &described, unmodelled, nil
}

// fromLiveEncryptedDNS projects the live setting into the config that would
// describe it, in the same one-implementation-for-both-directions way as
// networks, WLANs and WAN slots: what export writes is what plan compares
// against.
//
// A state unifig does not model comes back empty — unmanaged, the same answer
// fromLiveWANSlot gives a slot connected in a way the config cannot state. The
// alternative is worse than a gap: the schema's `state` is a closed set, so
// copying an unknown one through would have export write a file its own
// validate rejects, which is the brownfield promise broken by a firmware
// upgrade (ADR-0010).
//
// The stamps come back from the Internal API in the clear, so they diff like
// any other field (ADR-0007). The servers are sorted by name because the
// Controller's own order is not something the config states — two exports of an
// unchanged Controller have to be the same file.
func fromLiveEncryptedDNS(live unifi.SettingDoh) (config.EncryptedDNS, error) {
	described := config.EncryptedDNS{
		Servers: make([]config.DNSServer, 0, len(live.CustomServers)),
	}
	if dohStates[live.State] {
		described.State = live.State
	}

	names := make([]string, 0, len(live.CustomServers))
	for _, server := range live.CustomServers {
		described.Servers = append(described.Servers,
			config.DNSServer{Name: server.ServerName, Stamp: server.SdnsStamp})
		names = append(names, server.ServerName)
	}
	if err := uniquelyNamedResolvers(names); err != nil {
		return config.EncryptedDNS{}, err
	}

	slices.SortFunc(described.Servers, func(a, b config.DNSServer) int {
		return strings.Compare(a.Name, b.Name)
	})
	return described, nil
}

// uniquelyNamedResolvers refuses a setting holding two custom servers under one
// name, which is the Setting's version of the duplicate-natural-key rule
// (ADR-0001) applied inside a field.
//
// A resolver's name is how the config addresses it and how the diff matches it,
// so two of them sharing one leaves unifig with the same unanswerable question
// two identically named networks do — and two worse consequences than usual if
// it guessed. Export would write a file its own validate rejects, breaking the
// brownfield promise; and an apply would collapse the pair into one, deleting a
// resolver nothing in the plan mentioned.
func uniquelyNamedResolvers(names []string) error {
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
	return fmt.Errorf(
		"the Controller's Encrypted DNS setting holds more than one custom DNS server named %s, and unifig matches them by name, so it cannot tell which is which; rename or remove the extras in the Controller's UI, then run again",
		andJoin(quoted(shared)))
}

// updateEncryptedDNS is the Change that brings the live setting in line with
// the config, and whether there is one to make at all.
func updateEncryptedDNS(desired config.EncryptedDNS, live unifi.SettingDoh) (Change, bool, error) {
	current, err := fromLiveEncryptedDNS(live)
	if err != nil {
		return Change{}, false, err
	}
	fields := changedEncryptedDNSFields(current, desired)
	if len(fields) == 0 {
		return Change{}, false, nil
	}
	annotateEncryptedDNS(fields, current, desired)

	return Change{
		Action: Update,
		Kind:   EncryptedDNS,
		Fields: fields,
		write: func(ctx context.Context, client unifi.Client, site string) error {
			// The live setting goes back with only unifig's own fields changed,
			// so the built-in providers chosen in the Controller's UI and the
			// enabled toggles on the resolvers survive an apply.
			//
			// "Only its own fields" is bounded by what the SDK's own type
			// models, here and on every other write in this engine: a field of
			// the setting that unifig.SettingDoh has no place for was dropped
			// when the response was decoded and cannot be handed back. Today's
			// recording has no such field, so nothing would catch it if the
			// Controller grew one — which is worth knowing rather than
			// implying otherwise.
			updated := live
			overwriteManagedEncryptedDNS(&updated, desired)
			_, err := client.UpdateSettingDoh(ctx, site, &updated)
			return err
		},
	}, true, nil
}

// changedEncryptedDNSFields lists the managed fields on which the Controller
// and the config disagree.
//
// The servers produce up to two kinds of line, because they are two different
// pieces of news. `servers` is the list itself changing — a resolver arriving
// or leaving, which is the half an operator has to be able to read and refuse.
// `server "name"` is one resolver's stamp being written, said without saying
// what to, because a stamp is a secret.
func changedEncryptedDNSFields(current, desired config.EncryptedDNS) []Field {
	fields := make([]Field, 0, 2)
	if desired.State != "" && current.State != desired.State {
		fields = append(fields, Field{Name: "state", From: text(current.State), To: desired.State})
	}
	if desired.Servers == nil {
		return fields
	}

	held, wanted := serverNames(current.Servers), serverNames(desired.Servers)
	if !slices.Equal(held, wanted) {
		fields = append(fields, Field{Name: "servers", From: nameList(held), To: nameList(wanted)})
	}

	stamps := make(map[string]string, len(current.Servers))
	for _, server := range current.Servers {
		stamps[server.Name] = server.Stamp
	}
	for _, server := range desired.Servers {
		if stamps[server.Name] != server.Stamp {
			fields = append(fields, Field{Name: fmt.Sprintf("server %q", server.Name), Secret: true})
		}
	}
	return fields
}

// annotateEncryptedDNS says the two things the config states without stating,
// each of which is a way of configuring encrypted DNS that quietly does
// nothing.
//
// Both are read against the state the apply will leave behind rather than
// against the config alone: the config's state where it states one, and the
// Controller's where it does not. The same goes for the resolvers, with one
// deliberate exception — the second note is about resolvers *this file*
// declares, since telling an operator that servers they did not write will not
// be used is a remark about somebody else's decision.
func annotateEncryptedDNS(fields []Field, current, desired config.EncryptedDNS) {
	state := desired.State
	if state == "" {
		state = current.State
	}
	servers := desired.Servers
	if servers == nil {
		servers = current.Servers
	}

	switch {
	case state == stateCustom && len(servers) == 0:
		annotateFirst(fields,
			"no custom DNS server is listed here or set on the Controller, so there is nothing for this to encrypt with",
			"state", "servers")
	case state != stateCustom && len(desired.Servers) > 0:
		annotateFirst(fields, fmt.Sprintf(
			"encrypted DNS is %s rather than custom, so these servers will be stored and not used", describeState(state)),
			"servers", "state")
	}
}

// describeState names the state in the words the plan is already using, and
// says so plainly where the Controller has not chosen one.
func describeState(state string) string {
	if state == "" {
		return "not set"
	}
	return fmt.Sprintf("%q", state)
}

// overwriteManagedEncryptedDNS writes the config's values onto the Controller's
// setting and touches nothing else — the single place that decides which fields
// of this Setting unifig owns.
//
// A server the Controller already holds keeps everything about it unifig does
// not model, its enabled toggle included: an operator who switched a resolver
// off in the UI has said something, and rotating its stamp is not a reason to
// switch it back on. A server unifig is adding is enabled, for the same reason a
// WLAN passphrase implies WPA-PSK — on the Controller the two are one decision,
// and a resolver stored with a flag saying it is unused is a config line that
// does nothing.
func overwriteManagedEncryptedDNS(setting *unifi.SettingDoh, desired config.EncryptedDNS) {
	if desired.State != "" {
		setting.State = desired.State
	}
	if desired.Servers == nil {
		return
	}

	held := make(map[string]unifi.SettingDohCustomServers, len(setting.CustomServers))
	for _, server := range setting.CustomServers {
		held[server.ServerName] = server
	}

	servers := make([]unifi.SettingDohCustomServers, 0, len(desired.Servers))
	for _, server := range desired.Servers {
		updated, kept := held[server.Name]
		if !kept {
			updated.Enabled = true
		}
		updated.ServerName = server.Name
		updated.SdnsStamp = server.Stamp
		servers = append(servers, updated)
	}
	setting.CustomServers = servers
}

// serverNames is the sorted names of a list of custom servers — what the plan
// compares, because the order the Controller happens to hold them in is not
// something the config says anything about.
func serverNames(servers []config.DNSServer) []string {
	names := make([]string, 0, len(servers))
	for _, server := range servers {
		names = append(names, server.Name)
	}
	slices.Sort(names)
	return names
}

// noEncryptedDNSSetting is the error for a config with an `encrypted-dns:`
// section against a Controller that has no such setting. Like a WAN slot the
// router does not have, it is not something unifig can create — so the message
// says which of the two has to change.
func noEncryptedDNSSetting(site string) error {
	return fmt.Errorf(
		"the Controller has no Encrypted DNS setting for site %q, and unifig never creates one: settings are the Controller's own, so either this Network version predates encrypted DNS or the site does not have it — remove the `encrypted-dns:` section, or upgrade the Controller",
		site)
}
