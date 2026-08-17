package reconcile

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/filipowm/go-unifi/v2/unifi"

	"github.com/liambeeton/unifig/internal/config"
)

// A Firewall Policy is the second half of the zone-based firewall and the first
// Resource unifig manages whose identity is not enough to describe it: a zone is
// a name and a membership, but a policy is a name, a verdict and the pair of
// zones the verdict is about. All three are required by the schema, because a
// policy that allowed or blocked nothing in particular would not be a policy.
//
// It references zones exactly as a WLAN references a network, with one
// difference that shapes the whole file: the zone on either end need not be one
// this config file defines. The policies worth writing are mostly about the
// Controller's own built-in zones — External is the internet — so the reference
// is resolved against the Controller rather than against the file, and a name
// that matches nothing is reported when unifig reads the site, listing the zones
// it really has (ADR-0010's rule for WAN slots, applied to a Resource).

// storedActions maps a verdict as the config states it to the Controller's own
// spelling of it; statedActions is the same table read the other way, for
// projecting a live policy back into config. They are named for what they
// produce, so a call site says which direction it is going.
//
// The config's are lowercase because every other closed set unifig models is —
// `dhcp`, `pppoe`, `custom` — and an operator should not have to remember which
// fields shout. The Controller's are uppercase because that is what it stores,
// and the two are kept apart here rather than by uppercasing on the way through:
// a verdict unifig has never heard of then reads as one it does not model,
// rather than as one it invents a spelling for.
var storedActions = map[string]string{
	"allow":  "ALLOW",
	"block":  "BLOCK",
	"reject": "REJECT",
}

var statedActions = func() map[string]string {
	stated := make(map[string]string, len(storedActions))
	for config, controller := range storedActions {
		stated[controller] = config
	}
	return stated
}()

// planFirewallPolicies is the policy half of a reconcile. Its caller only
// reaches it when the config has a `firewall-policies:` section at all
// (ADR-0006), and only after the zones have been read and bound, since a policy
// is stated in terms of them.
func planFirewallPolicies(
	ctx context.Context,
	client unifi.Client,
	site string,
	cfg config.Config,
	bound bindings,
	opts Options,
) ([]Change, error) {
	live, err := listFirewallPolicies(ctx, client, site, bound)
	if err != nil {
		return nil, err
	}
	// A policy without a key is left out of the collection rather than filed
	// under an empty one: nothing in the file can match it, and prune must not
	// reach it either — deleting a policy unifig could not describe is a change
	// it could not have shown the operator first.
	byKey := make(map[policyKey]unifi.FirewallZonePolicy, len(live))
	for _, policy := range live {
		if key, keyed := keyOfLivePolicy(policy, bound); keyed {
			byKey[key] = policy
		}
	}

	// A zone a policy names is resolved against the Controller and against the
	// zones this file creates, together — the file's own zones do not exist yet,
	// and a policy onto one of them is the ordinary case rather than an error.
	// Checking here rather than at write time is what makes it a planning
	// failure: an operator learns that they misspelled "Exteral" before anything
	// has been applied, not halfway through applying it.
	reachable := bound.zoneNames()
	for _, zone := range cfg.Zones {
		if !slices.Contains(reachable, zone.Name) {
			reachable = append(reachable, zone.Name)
		}
	}

	changes := make([]Change, 0, len(cfg.FirewallPolicies))
	named := make(map[policyKey]bool, len(cfg.FirewallPolicies))
	for _, desired := range cfg.FirewallPolicies {
		named[keyOfDesiredPolicy(desired)] = true
		for _, zone := range []string{desired.Source, desired.Destination} {
			if !slices.Contains(reachable, zone) {
				return nil, noSuchZone(zone, bound.zoneNames())
			}
		}

		current, exists := byKey[keyOfDesiredPolicy(desired)]
		if !exists {
			changes = append(changes, createFirewallPolicy(desired, bound))
			continue
		}
		if change, differs := updateFirewallPolicy(desired, current, bound); differs {
			changes = append(changes, change)
		}
	}
	if opts.Prune {
		changes = append(changes, pruneFirewallPolicies(byKey, named, bound)...)
	}
	return changes, nil
}

// policyKey is what identifies a firewall policy — its name together with the
// pair of zones it governs.
//
// ADR-0001 handles identity with per-type natural keys rather than one rule for
// everything, and this is the type whose key is not its name. The Controller
// ships its predefined policies one per zone pair and reuses the same name
// across them: a migrated UDR answers with nineteen called "Allow All Traffic",
// one for each ordered pair of its six zones, and sixteen called "Block All
// Traffic". Matching those on name alone finds one policy where there are
// nineteen, and refusing the site as ambiguous — which is what unifig did — asks
// an operator to rename policies that are not theirs to rename (issue #24).
//
// The pair is held as zone *names* rather than ids so that a policy in a file
// and a policy on the Controller can be compared at all: the file names its
// zones and the Controller stores ids for them.
type policyKey struct {
	name        string
	source      string
	destination string
}

func (k policyKey) String() string {
	return fmt.Sprintf("%q (%s to %s)", k.name, k.source, k.destination)
}

// keyOfLivePolicy is a live policy's key, and whether the policy has one at all.
//
// A policy whose zones unifig cannot name has no key: both ends would be empty,
// which no config policy can equal and which two such policies would share with
// each other. It is unmatchable rather than mismatched — projectFirewallPolicies
// reports it as one it cannot describe, and nothing here should mistake two of
// them for a clash.
func keyOfLivePolicy(policy unifi.FirewallZonePolicy, bound bindings) (policyKey, bool) {
	source := bound.zoneName(policy.Source.ZoneID)
	destination := bound.zoneName(policy.Destination.ZoneID)
	if source == "" || destination == "" {
		return policyKey{}, false
	}
	return policyKey{name: policy.Name, source: source, destination: destination}, true
}

func keyOfDesiredPolicy(desired config.FirewallPolicy) policyKey {
	return policyKey{name: desired.Name, source: desired.Source, destination: desired.Destination}
}

// listFirewallPolicies reads the site's firewall policies, refusing a site where
// two of them share the key unifig matches them by.
//
// Two policies sharing a name is ordinary and expected; two sharing a name *and*
// a zone pair is the ambiguity that has no answer, and it is still refused.
func listFirewallPolicies(ctx context.Context, client unifi.Client, site string, bound bindings) ([]unifi.FirewallZonePolicy, error) {
	live, err := client.ListFirewallZonePolicy(ctx, site)
	if err != nil {
		return nil, fmt.Errorf("listing firewall policies for site %q: %w", site, err)
	}

	if err := uniquelyKeyed(live, bound); err != nil {
		return nil, err
	}
	return live, nil
}

// uniquelyKeyed refuses a site holding two policies unifig could not tell apart.
// It reads like uniquelyNamed's message because it is the same failure, reported
// about a wider key.
//
// A policy without a key is left out rather than counted: it is not one of a
// pair unifig had to choose between, and counting it would refuse the site over
// a clash between two ends that could not be named.
func uniquelyKeyed(live []unifi.FirewallZonePolicy, bound bindings) error {
	counts := make(map[policyKey]int, len(live))
	for _, policy := range live {
		if key, keyed := keyOfLivePolicy(policy, bound); keyed {
			counts[key]++
		}
	}

	var shared []string
	for key, count := range counts {
		if count > 1 {
			shared = append(shared, fmt.Sprintf("%d matching %s", count, key))
		}
	}
	if len(shared) == 0 {
		return nil
	}

	slices.Sort(shared)
	return fmt.Errorf(
		"unifig matches firewall policies on the Controller by name and the pair of zones they govern, so no two may share all three: this site has %s; rename or remove the extras in the Controller's UI, then run again",
		strings.Join(shared, ", "))
}

// projectFirewallPolicies projects the site's policies into the config that
// would describe them, and names the ones it could not describe at all.
//
// A policy is left out when a zone on either end of it is one unifig cannot name
// or when its verdict is one unifig does not model — the whole of a policy is
// its name, its verdict and its pair of zones, so a policy missing any of them
// is not a policy the config has a way to write. That is listWLANs' rule rather
// than fromLiveZone's: a zone can be described in part because its membership is
// a list, and a policy cannot, because every field it has is required.
func projectFirewallPolicies(ctx context.Context, client unifi.Client, site string, bound bindings) ([]config.FirewallPolicy, []string, error) {
	live, err := listFirewallPolicies(ctx, client, site, bound)
	if err != nil {
		return nil, nil, err
	}

	policies := make([]config.FirewallPolicy, 0, len(live))
	var indescribable []string
	for _, policy := range live {
		described, ok := fromLivePolicy(policy, bound)
		if !ok {
			indescribable = append(indescribable, policy.Name)
			continue
		}
		policies = append(policies, described)
	}
	slices.SortFunc(policies, func(a, b config.FirewallPolicy) int { return strings.Compare(a.Name, b.Name) })
	slices.Sort(indescribable)
	return policies, indescribable, nil
}

// fromLivePolicy projects a live policy into the config that would describe it,
// and whether the config can describe it at all.
func fromLivePolicy(policy unifi.FirewallZonePolicy, bound bindings) (config.FirewallPolicy, bool) {
	action, modelled := statedActions[policy.Action]
	source := bound.zoneName(policy.Source.ZoneID)
	destination := bound.zoneName(policy.Destination.ZoneID)
	if !modelled || source == "" || destination == "" {
		return config.FirewallPolicy{}, false
	}
	return config.FirewallPolicy{
		Name:        policy.Name,
		Action:      action,
		Source:      source,
		Destination: destination,
	}, true
}

// createFirewallPolicy is the Change for a policy the Controller does not have.
func createFirewallPolicy(desired config.FirewallPolicy, bound bindings) Change {
	return Change{
		Action: Create,
		Kind:   FirewallPolicy,
		Name:   desired.Name,
		Fields: setPolicyFields(desired),
		write: func(ctx context.Context, client unifi.Client, site string) error {
			// Read at the moment of writing rather than the moment of planning:
			// either zone may have been created by a change earlier in this very
			// apply.
			policy := newFirewallPolicy()
			if err := overwriteManagedPolicy(&policy, desired, bound); err != nil {
				return err
			}
			_, err := client.CreateFirewallZonePolicy(ctx, site, &policy)
			return err
		},
	}
}

// updateFirewallPolicy is the Change that brings a live policy in line with the
// config, and whether there is one to make at all.
func updateFirewallPolicy(desired config.FirewallPolicy, live unifi.FirewallZonePolicy, bound bindings) (Change, bool) {
	current, _ := fromLivePolicy(live, bound)

	fields := changedPolicyFields(current, desired)
	if len(fields) == 0 {
		return Change{}, false
	}

	return Change{
		Action: Update,
		Kind:   FirewallPolicy,
		Name:   desired.Name,
		Fields: fields,
		write: func(ctx context.Context, client unifi.Client, site string) error {
			// The live object goes back with only unifig's own fields changed, so
			// the schedule, the port and address matching, the logging switch and
			// everything else an operator set in the Controller's UI survive.
			updated := live
			if err := overwriteManagedPolicy(&updated, desired, bound); err != nil {
				return err
			}
			_, err := client.UpdateFirewallZonePolicy(ctx, site, &updated)
			return err
		},
	}, true
}

// pruneFirewallPolicies is the Changes that would delete every live policy the
// config does not name.
//
// A policy the Controller ships is exempt on its own marker, like every other
// built-in (ADR-0005). A policy's marker is `predefined`, which is how the
// zone-based firewall marks the default policies it generates for a pair of
// zones — and the eighty-three of those a migrated router ships are exactly what
// prune must not touch. `NoDelete` is checked beside it because the library
// models the field on this type, not because a policy has been seen carrying it:
// the marker is per Resource and only a network is known to use that one, so
// nothing here should be read as saying which field a new type would use.
func pruneFirewallPolicies(live map[policyKey]unifi.FirewallZonePolicy, named map[policyKey]bool, bound bindings) []Change {
	changes := make([]Change, 0, len(live))
	for key, policy := range live {
		if named[key] || policy.NoDelete || policy.Predefined {
			continue
		}
		changes = append(changes, deleteFirewallPolicy(policy, bound))
	}
	return changes
}

// deleteFirewallPolicy is the Change that removes a live policy.
//
// It lists the verdict and the pair, because that is how an operator recognises
// the rule they are about to lose: "the one that blocks IoT reaching the LAN" is
// the policy, and its name may well be less memorable than what it does.
func deleteFirewallPolicy(live unifi.FirewallZonePolicy, bound bindings) Change {
	current, _ := fromLivePolicy(live, bound)

	return Change{
		Action: Delete,
		Kind:   FirewallPolicy,
		Name:   live.Name,
		Fields: []Field{
			{Name: "action", From: text(current.Action)},
			{Name: "source", From: text(current.Source)},
			{Name: "destination", From: text(current.Destination)},
		},
		write: func(ctx context.Context, client unifi.Client, site string) error {
			return client.DeleteFirewallZonePolicy(ctx, site, live.ID)
		},
	}
}

// setPolicyFields lists what a create would set. All three are always listed,
// because the schema requires all three: there is no such thing as a policy the
// config states only part of.
func setPolicyFields(desired config.FirewallPolicy) []Field {
	return []Field{
		{Name: "action", To: desired.Action},
		{Name: "source", To: desired.Source},
		{Name: "destination", To: desired.Destination},
	}
}

// changedPolicyFields lists the managed fields on which the Controller and the
// config disagree.
//
// Every field is compared unconditionally, which is the opposite of how an
// optional field is treated everywhere else — and it is the same rule
// underneath. Omission means unmanaged, and the schema lets none of these be
// omitted, so a policy in the config always states its verdict and both ends.
func changedPolicyFields(current, desired config.FirewallPolicy) []Field {
	fields := make([]Field, 0, 3)
	if current.Action != desired.Action {
		fields = append(fields, Field{Name: "action", From: text(current.Action), To: desired.Action})
	}
	if current.Source != desired.Source {
		fields = append(fields, Field{Name: "source", From: text(current.Source), To: desired.Source})
	}
	if current.Destination != desired.Destination {
		fields = append(fields,
			Field{Name: "destination", From: text(current.Destination), To: desired.Destination})
	}
	return fields
}

// overwriteManagedPolicy writes the config's values onto a Controller policy and
// touches nothing else — the single place that decides which policy fields
// unifig owns.
func overwriteManagedPolicy(policy *unifi.FirewallZonePolicy, desired config.FirewallPolicy, bound bindings) error {
	source, err := bound.zoneID(desired.Source)
	if err != nil {
		return err
	}
	destination, err := bound.zoneID(desired.Destination)
	if err != nil {
		return err
	}

	policy.Name = desired.Name
	policy.Action = storedActions[desired.Action]
	policy.Source.ZoneID = source
	policy.Destination.ZoneID = destination
	return nil
}

// newFirewallPolicy builds the Controller object for a policy unifig is
// creating.
//
// The config models three fields and a policy has thirty, so the rest are the
// Controller's own defaults for a new policy — matching what its UI creates: the
// policy applies to every protocol, in both address families, at all times, from
// anywhere in the source zone to anywhere in the destination zone. A policy
// created from a bare struct would instead be disabled, on no schedule, and
// matching nothing, which would govern no traffic at all.
//
// They apply on create only. An operator who afterwards narrows the policy to a
// port, a client or an evening keeps that forever, because updates go through
// overwriteManagedPolicy, which never touches anything here.
func newFirewallPolicy() unifi.FirewallZonePolicy {
	return unifi.FirewallZonePolicy{
		Enabled:             true,
		Protocol:            "all",
		IPVersion:           "BOTH",
		ConnectionStateType: "ALL",
		// The Controller rejects a policy with no schedule outright, so this is
		// less a default than a field with one permitted value at creation.
		Schedule: unifi.FirewallZonePolicySchedule{Mode: "ALWAYS", TimeAllDay: true},
		Source: unifi.FirewallZonePolicySource{
			MatchingTarget:   "ANY",
			PortMatchingType: "ANY",
		},
		Destination: unifi.FirewallZonePolicyDestination{
			MatchingTarget:   "ANY",
			PortMatchingType: "ANY",
		},
	}
}
