package reconcile

import (
	"context"
	"encoding/json"
	"errors"
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
// gatewayRisk is what an operator stands to lose by blocking traffic to the
// Gateway zone, said in the words they need to see it coming.
//
// It names the consequence rather than the field, for wanRisk's reason: "action
// is changing to block" is not something an operator can weigh at eleven at
// night and "there may be no way back to this site" is. Like wanRisk it says
// "can" rather than "will", and that hedge is load-bearing — unifig models
// neither a policy's precedence (`index`) nor whether it is enabled, so it knows
// that a rule closing the management path is being written and cannot know what
// the rule set as a whole will do with it (ADR-0018).
const gatewayRisk = "the Controller answers in the Gateway zone, and blocking traffic to it can cut the path this site is managed over"

// blockingActions are the verdicts that close a path. A policy moving between
// two of them closes nothing that was not closed already, which is why the risk
// check asks what the verdict is changing *from* as well as to.
var blockingActions = map[string]bool{"block": true, "reject": true}

// opensAPath is whether a verdict lets traffic through, and it is the only
// distinction the Controller's return-rule rule turns on: it generates the
// companion on a create that allows, and refuses a body that asks for one beside
// any other verdict (ADR-0022). Three places state that rule — the note a plan
// puts on a create, the flag a create sends, and the flag an update has to take
// back off — and they are one rule rather than three agreeing constants.
//
// It is deliberately not `blockingActions` below, which is a list of the
// verdicts that close a path. The Controller's message names no verdict at all;
// it objects to the request on anything that is not an allow, so a firmware that
// ships a fourth verdict is covered here and would have to be added there.
func opensAPath(action string) bool { return action == "allow" }

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
//
// Like the WLAN half, it reports the live policies it leaves in place, because
// that is what decides whether the zone half may propose deleting a zone
// (ADR-0014).
func planFirewallPolicies(
	cfg config.Config,
	live []unifi.FirewallZonePolicy,
	facts zoneFacts,
	bound bindings,
	opts Options,
) ([]Change, []unifi.FirewallZonePolicy, []Caveat, error) {
	if err := uniquelyKeyed(live, bound); err != nil {
		return nil, nil, nil, err
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
	held := policyNames(live)

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
	// Whether this plan holds a change the gateway check would have looked at,
	// which is what decides if an unreadable gateway is worth a caveat.
	blocking := false
	for _, desired := range cfg.FirewallPolicies {
		named[keyOfDesiredPolicy(desired)] = true
		for _, zone := range []string{desired.Source, desired.Destination} {
			if !slices.Contains(reachable, zone) {
				return nil, nil, nil, noSuchZone(zone, bound.zoneNames())
			}
		}

		current, exists := byKey[keyOfDesiredPolicy(desired)]
		if !exists {
			blocking = blocking || becomesBlocking(config.FirewallPolicy{}, desired)
			changes = append(changes, createFirewallPolicy(desired, facts, bound))
			continue
		}
		if change, differs := updateFirewallPolicy(desired, current, facts, bound, held); differs {
			stated, _ := fromLivePolicy(current, bound)
			blocking = blocking || becomesBlocking(stated, desired)
			changes = append(changes, change)
		}
	}
	caveats := unreadableGateway(blocking, facts)
	if !opts.Prune {
		return changes, live, caveats, nil
	}
	deletions, spared := pruneFirewallPolicies(live, named, bound)
	return append(changes, deletions...), spared, caveats, nil
}

// closesTheGateway is whether this change would newly block traffic to the zone
// the Controller answers in — the whole of what makes a Firewall Policy change
// Risky (ADR-0018).
//
// It asks three things, and each of them narrows a way of getting this wrong.
// The destination must be the gateway, because a policy to any other zone leaves
// the Controller reachable and the fix one field away (ADR-0012). The verdict
// must be becoming blocking, because a policy already blocking that pair cannot
// cut a path that is already cut — `block` to `reject` costs nothing and a
// confirmation in front of it is one an operator learns to click through. And
// the gateway must be known: a guess here is a name unifig invented.
func closesTheGateway(from, to config.FirewallPolicy, facts zoneFacts) bool {
	return facts.known && facts.gateway != "" && to.Destination == facts.gateway && becomesBlocking(from, to)
}

// becomesBlocking is whether this change turns a path that was open into one
// that is closed. A create has no `from`, so its empty verdict is not a blocking
// one and a create stating `block` qualifies.
func becomesBlocking(from, to config.FirewallPolicy) bool {
	return blockingActions[to.Action] && !blockingActions[from.Action]
}

// unreadableGateway is the plan's admission that it could not check for the one
// firewall change it would have marked Risky.
//
// It is the counterpart of the caveat prune carries when ownership could not be
// established (issue #23), and it exists for the same reason: an absence with a
// reason is invisible unless something says it out loud. Reaching the Controller
// and not being told which zone is the gateway is the same silence as reaching
// it and not being told which zones are its own.
//
// It is only said when the plan holds a change the check would have looked at —
// something turning a verdict to block or reject. A plan with nothing blocking
// in it had no question to answer, and a caveat on every firewall plan is one an
// operator reads past by the third run.
func unreadableGateway(blocking bool, facts zoneFacts) []Caveat {
	if !blocking || (facts.known && facts.gateway != "") {
		return nil
	}
	return []Caveat{{
		Kind: FirewallPolicy,
		Reason: "no firewall policy is marked as a Risky change: unifig could not read which zone the Controller " +
			"answers in, so it cannot tell whether a policy in this plan would block the path this site is managed over",
	}}
}

// zonesInUse names the zones a policy this plan leaves in place still governs,
// against the phrases naming those policies — the zone half's counterpart of
// networksInUse, and what prune asks before proposing to delete a zone
// (ADR-0014).
//
// A policy the Controller generated holds nothing back. It is spared from
// deletion on its marker like every other built-in (ADR-0005) and it will still
// be governing its zone afterwards, so by the general rule it would hold the zone
// back — and it did, until hardware was asked. A write session against a migrated
// UDR created a custom zone, watched the Controller generate eighteen predefined
// policies for its pairs, and then deleted the zone: `DELETE` answered 204 and
// the Controller reclaimed all eighteen itself (ADR-0019, issue #28). So the
// deletion the general rule declines to propose is one the Controller performs
// without complaint, and declining it made `--prune` useless for custom zones on
// every router unifig targets. What holds a zone back is a policy an operator
// wrote, which is the scope issue #22 asked for.
//
// A policy is named by its whole key rather than by its name, because the
// Controller ships nineteen called "Allow All Traffic" and a Caveat naming one of
// them has to say which (ADR-0001, issue #24). A policy whose zones unifig cannot
// name holds nothing back: neither end is a zone prune could have proposed
// deleting. And a policy from a zone to itself is counted once, because it is one
// policy however many of its ends land on the same zone.
func zonesInUse(spared []unifi.FirewallZonePolicy, bound bindings) referenced {
	inUse := referenced{}
	for _, policy := range spared {
		if policy.Predefined {
			continue
		}
		key, keyed := keyOfLivePolicy(policy, bound)
		if !keyed {
			continue
		}
		governs := fmt.Sprintf("the %s %s governing it", kinds[FirewallPolicy].one, key)
		inUse[key.source] = append(inUse[key.source], governs)
		if key.destination != key.source {
			inUse[key.destination] = append(inUse[key.destination], governs)
		}
	}
	return inUse
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

// listFirewallPolicies reads the site's firewall policies.
//
// Nothing is filtered and nothing is refused here, for the reason
// uniquelyNamedWLANs states: reading is not matching. A prune of the zones reads
// the policies only to see which zones still have one on either end (ADR-0014),
// and a pair unifig could not tell apart is no obstacle to that.
func listFirewallPolicies(ctx context.Context, client unifi.Client, site string) ([]unifi.FirewallZonePolicy, error) {
	live, err := client.ListFirewallZonePolicy(ctx, site)
	if err != nil {
		return nil, fmt.Errorf("listing firewall policies for site %q: %w", site, err)
	}
	return live, nil
}

// uniquelyKeyed refuses a site holding two policies unifig could not tell apart.
// It reads like uniquelyNamed's message because it is the same failure, reported
// about a wider key.
//
// Two policies sharing a name is ordinary and expected; two sharing a name *and*
// a zone pair is the ambiguity that has no answer, and it is still refused. It is
// asked by the verbs that match policies to config rather than by the read.
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
	live, err := listFirewallPolicies(ctx, client, site)
	if err != nil {
		return nil, nil, err
	}
	// Export matches too, in the sense that matters here: a file describing two
	// policies unifig cannot tell apart is a file it cannot plan afterwards.
	if err := uniquelyKeyed(live, bound); err != nil {
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
//
// A policy created blocking the gateway is Risky for the same reason an existing
// one turned that way is, and more directly: the Controller's own predefined
// allow sits at the lowest precedence there is, so a policy written over it is
// one that takes effect.
func createFirewallPolicy(desired config.FirewallPolicy, facts zoneFacts, bound bindings) Change {
	risk := ""
	if closesTheGateway(config.FirewallPolicy{}, desired, facts) {
		risk = gatewayRisk
	}
	return Change{
		Action: Create,
		Kind:   FirewallPolicy,
		Name:   desired.Name,
		Fields: setPolicyFields(desired),
		Risk:   risk,
		write: func(ctx context.Context, client unifi.Client, site string) error {
			// Read at the moment of writing rather than the moment of planning:
			// either zone may have been created by a change earlier in this very
			// apply.
			policy := newFirewallPolicy(desired)
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
func updateFirewallPolicy(
	desired config.FirewallPolicy,
	live unifi.FirewallZonePolicy,
	facts zoneFacts,
	bound bindings,
	held policyNameSet,
) (Change, bool) {
	current, _ := fromLivePolicy(live, bound)

	// Whether the site is holding the companion this policy would have. It is
	// asked of the live collection rather than of the policy's own
	// `create_allow_respond`, for the reason returnRuleField gives: the flag is
	// the request, and the companion is what an operator has.
	fields := changedPolicyFields(current, desired, live.CreateAllowRespond, held[returnRuleName(live.Name)])
	if len(fields) == 0 {
		return Change{}, false
	}

	// The Controller's own policy on this pair is matchable like any other, and
	// only prune exempts it (ADR-0005) — so `Allow All Traffic` from Internal to
	// Gateway is a one-line edit away from being the rule that locks the operator
	// out. That is the change this mark exists for.
	//
	// It is not updatable, though, and this mark currently guards a change that
	// cannot be applied. A policy the Controller ships has no addressable `_id`
	// — it is a composite of both zone ids and the index, which the write
	// endpoint answers 404 to — so the PUT below has nowhere to land, while the
	// collection read it merges into succeeds. Measured on the live migrated UDR
	// on 19 August 2026, off the back of #37, and filed as issue #41 with the
	// reading that would confirm it. Nothing is changed here for it: what a plan
	// should say about a policy it cannot write is that issue's to decide.
	risk := ""
	if closesTheGateway(current, desired, facts) {
		risk = gatewayRisk
	}

	return Change{
		Action: Update,
		Kind:   FirewallPolicy,
		Name:   desired.Name,
		Fields: fields,
		Risk:   risk,
		write: func(ctx context.Context, client unifi.Client, site string) error {
			// The live object goes back with only unifig's own fields changed, so
			// the schedule, the port and address matching, the logging switch and
			// everything else an operator set in the Controller's UI survive.
			//
			// That promise used to carry a qualification, and the qualification
			// turned out to be the whole of it. What went back was a struct, and
			// a v2 PUT replaces rather than merges — measured on a migrated UDR,
			// where an apply changing one policy's verdict reverted the ICMP type
			// an operator had narrowed it to, in the same request and without
			// saying so (ADR-0021, issue #35). So the object that goes back is
			// the one the Controller sent.
			return mergeIntoStoredPolicy(ctx, client, site, live.ID, desired, bound)
		},
	}, true
}

// pruneFirewallPolicies is the Changes that would delete every live policy the
// config does not name, and the policies it leaves on the Controller.
//
// A policy the Controller ships is exempt on its own marker, like every other
// built-in (ADR-0005). A policy's marker is `predefined`, which is how the
// zone-based firewall marks the default policies it generates for a pair of
// zones — and the eighty-three of those a migrated router ships are exactly what
// prune must not touch. `NoDelete` is checked beside it because the library
// models the field on this type, not because a policy has been seen carrying it:
// the marker is per Resource and only a network is known to use that one, so
// nothing here should be read as saying which field a new type would use.
//
// An exempt policy is spared rather than skipped, because what makes a policy
// hold a zone back is that it will still be governing it afterwards, whatever the
// reason it survived (ADR-0014). A policy with no key is spared too: unifig
// cannot describe it, so it was never prune's to delete, and it governs zones
// unifig cannot name so it holds nothing back either.
//
// Spared is not the same list as holds-a-zone-back, and `predefined` is where the
// two part company: the Controller deletes its own generated policies along with
// the zone, so one of them is exempt here and counts for nothing in `zonesInUse`
// (ADR-0019, issue #28).
//
// It walks the live collection rather than the keyed index so that the deletions
// come out in the order the Controller listed them. Two policies of one name are
// ordinary here, and sortChanges leaves ties in the order it was given — which
// would otherwise be a map's, and a plan has to print the same bytes twice.
func pruneFirewallPolicies(
	live []unifi.FirewallZonePolicy,
	named map[policyKey]bool,
	bound bindings,
) (changes []Change, spared []unifi.FirewallZonePolicy) {
	changes = make([]Change, 0, len(live))
	spared = make([]unifi.FirewallZonePolicy, 0, len(live))
	for _, policy := range live {
		key, keyed := keyOfLivePolicy(policy, bound)
		if !keyed || named[key] || policy.NoDelete || policy.Predefined {
			spared = append(spared, policy)
			continue
		}
		changes = append(changes, deleteFirewallPolicy(policy, bound))
	}
	return changes, spared
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
		{Name: "action", To: desired.Action, Notes: returnRuleNote(desired)},
		{Name: "source", To: desired.Source},
		{Name: "destination", To: desired.Destination},
	}
}

// returnRuleNote is what a create does beyond the policy it names: on an allow,
// the Controller generates a second policy for the reply traffic, and the plan
// has to say so.
//
// Measured on the live migrated UDR on 18 August 2026 (ADR-0022). One allow
// policy created with `create_allow_respond` took the site from 86 policies to
// 88 — unifig's own, and a companion the Controller named `<name> (Return)`,
// marked `predefined` and back-referenced to its parent — and deleting the
// parent returned it to 86. Without the flag the same create made 87 and no
// companion, which is what makes this the plan describing a consequence rather
// than a coincidence.
//
// It is on the verdict because the verdict decides it. The Controller refuses
// the request outright on a policy that blocks, so there is no second policy to
// announce and a blocking create carries no note — the one case where saying
// nothing is the true statement.
//
// A note rather than a Change of its own, on ADR-0010's distinction: unifig does
// not create the companion, cannot name it in a config file, and will not be the
// one to delete it. What an operator needs is to know it is coming, which is the
// same thing a zone's membership note gives them about a network leaving another
// zone (ADR-0020).
//
// It is one statement because a create is one story: unifig decides the request,
// so it knows the answer. The update path is three, and they are next door in
// returnRuleUpdateNote.
func returnRuleNote(desired config.FirewallPolicy) []string {
	if !opensAPath(desired.Action) {
		return nil
	}
	return []string{fmt.Sprintf(
		"the Controller will also create %q for the reply traffic, and delete it with this policy",
		returnRuleName(desired.Name))}
}

// policyNameSet is every name the Controller's policies carry, and policyNames
// builds it. It answers one question: does the companion this plan is about to
// describe actually exist?
//
// The flag alone cannot answer it. `create_allow_respond` records the request
// made at creation rather than anything about the policy now (ADR-0022), and the
// recording proves the gap is real rather than theoretical: fifty-two of the
// eighty-three policies a migrated router ships are `ALLOW` carrying the flag
// true, and **not one** of them has a `<name> (Return)` beside it. The twelve
// companions such a router does hold are named `Allow Return Traffic` and sit on
// reverse pairs — the Controller's own scheme for its own policies, which is not
// the one it uses for a policy unifig created. A plan that read the flag as
// proof of a companion would promise to delete fifty-two policies that do not
// exist.
//
// It matches on name rather than on `origin_id`, which is what actually links a
// companion to its parent. Two reasons, and the second is the load-bearing one:
// `go-unifi` v2.3.0 does not model the field, so the struct a plan reads has no
// access to it (ADR-0021) — and the name is what the plan has to print either
// way. What is claimed is only ever "a policy by the name the Controller would
// give the companion is here", which is exactly as strong as the line the plan
// goes on to print.
type policyNameSet map[string]bool

func policyNames(live []unifi.FirewallZonePolicy) policyNameSet {
	named := make(policyNameSet, len(live))
	for _, policy := range live {
		named[policy.Name] = true
	}
	return named
}

// returnRuleName is what the Controller calls the companion it generates for a
// policy — the parent's name and a suffix, measured on the live migrated UDR on
// 18 August 2026 (ADR-0022).
//
// It is one function because it is one rule, and both notes state it: the create
// promises a policy by this name and the update says what becomes of it. A
// second spelling of the suffix would be two plans disagreeing about the name of
// the same object.
func returnRuleName(parent string) string { return parent + " (Return)" }

// returnRuleField is what an update does to the companion return rule, said as a
// field of the change rather than as a note on one.
//
// It is a field because it is a thing that can differ on its own. Everything
// else unifig owns on a policy comes from a line of the config; this comes from
// the verdict *and* from what the Controller is holding, and the two can
// disagree with no config edit behind them — a policy sitting at `allow` with no
// companion is exactly the state issue #40 was filed about, and it has to be
// plannable or it can never be corrected. A note has to hang off a field that is
// already changing, so a note could not have said this at all.
//
// Both ends are the companion rather than the flag. `create_allow_respond` is
// what unifig writes and what the Controller acts on, but it is not what an
// operator gets or loses: fifty-two of the eighty-six policies the live router
// holds carry the flag true with no `<name> (Return)` anywhere on the site
// (their companions are the twelve `Allow Return Traffic` policies, on reverse
// pairs, which is the Controller's scheme for its own policies). Rendering the
// flag would put a line in the plan for those saying a return rule goes away
// when there is none to go. What is printed is the policy an operator will have
// or not have.
//
// Empty when the two ends agree, which is the ordinary case: a policy that
// allowed and still allows keeps its companion, and a plan does not mention what
// is not moving.
func returnRuleField(desired config.FirewallPolicy, requested, companionHeld bool) (Field, bool) {
	want := opensAPath(desired.Action)
	// Nothing for unifig to write: the request the Controller is holding already
	// says what the verdict says. This is what keeps an exported firewall
	// planning clean — the fifty-two shipped `ALLOW` policies carry the flag
	// true and want it true, so none of them is a change.
	if requested == want {
		return Field{}, false
	}

	// Quoted, the way every other policy name in a plan is: `(Return)` is part
	// of the name the Controller gives it, and unquoted it reads as an aside
	// about the line rather than as the object it is.
	companion := fmt.Sprintf("%q", returnRuleName(desired.Name))
	var from, to any
	if companionHeld {
		from = companion
	}
	if want {
		to = companion
	}
	// The write is worth making and there is nothing to show for it: a policy
	// carrying the request with no companion by that name, closing. That is
	// every one of the Controller's own policies, whose companions are the
	// twelve `Allow Return Traffic` on reverse pairs rather than a `<name>
	// (Return)`. The flag still goes out correct on any update that happens for
	// another reason — it has to, or the Controller refuses the body — and a
	// plan does not carry a line for a change with no consequence to state.
	if from == to {
		return Field{}, false
	}
	return Field{Name: "return-rule", From: from, To: to}, true
}

// changedPolicyFields lists the managed fields on which the Controller and the
// config disagree.
//
// Every field is compared unconditionally, which is the opposite of how an
// optional field is treated everywhere else — and it is the same rule
// underneath. Omission means unmanaged, and the schema lets none of these be
// omitted, so a policy in the config always states its verdict and both ends.
func changedPolicyFields(current, desired config.FirewallPolicy, requested, companionHeld bool) []Field {
	fields := make([]Field, 0, 4)
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
	// The fourth field, and the only one not read off a line of the config: the
	// verdict decides whether a companion should be there, and the Controller
	// decides whether one is. An update runs when they disagree, which is what
	// makes a policy left at `allow` without a companion something unifig can
	// put right rather than only describe (ADR-0026).
	if field, differs := returnRuleField(desired, requested, companionHeld); differs {
		fields = append(fields, field)
	}
	return fields
}

// overwriteManagedPolicy writes the config's values onto a Controller policy and
// touches nothing else. It is what a **create** writes onto unifig's own
// defaults for a new policy; an update writes the same four values onto the
// object the Controller sent, in storedPolicy.overwriteManaged.
//
// The two are apart because the verbs are: a create has no stored object and no
// operator's fields to preserve, so it starts from a struct and the wire shape
// it produces is one a UDR has accepted (ADR-0019). They are the same list of
// fields, though — a fifth field unifig comes to own is a change to both.
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

// markerPrefix is what the Controller names its read-only markers with, and the
// whole of how one is recognised here. The family is `attr_hidden`,
// `attr_hidden_id`, `attr_no_delete` and `attr_no_edit` today, and a firmware
// that ships a fifth is covered by the same line.
const markerPrefix = "attr_"

// storedPolicy is a live policy as the Controller holds it: every field it sent,
// kept as the JSON it sent rather than read through a struct that has an opinion
// about which fields a policy has.
//
// It exists because a v2 PUT replaces the object rather than merging into it
// (ADR-0021). Under a replace, the difference between the read shape and the
// struct is not a detail of deserialisation — it is the list of fields an update
// takes off the policy.
type storedPolicy map[string]json.RawMessage

// mergeIntoStoredPolicy is unifig's update to a live policy: the object the
// Controller holds, with unifig's own fields written onto it, put back whole.
//
// Two things here are deliberate and answer the same measurement. It reads the
// policy as JSON rather than as a `unifi.FirewallZonePolicy`, because a struct
// leaves out every field go-unifi v2.3.0 does not model — `origin_id` on all
// eighty-three policies a migrated router ships, `icmp_typename` and
// `icmp_v6_typename`,
// `origin_type`, `hits`, `last_hit` — and every modelled field `omitempty`
// elides at its zero value, which is how an empty `description` went missing
// too. And it reads at the moment of writing rather than sending back the copy
// the plan was computed from, so a narrowing the operator made in the UI while
// they were reading the plan is not reverted by approving it.
//
// Whether the write endpoint *refuses* those fields coming back was the one
// thing #35 could not measure, and issue #37 measured it on the live migrated
// UDR on 19 August 2026: it does not. A body carrying all six came back 200, no
// field named, nothing like #27. So nothing has to be withheld, and the zone's
// DTO — which refuses a field it has not heard of (ADR-0019) — really is the
// other endpoint's shape rather than this one's. That was worth a reading rather
// than a symmetry, which was the whole of the issue.
//
// What the same reading showed is that four of the six are not read off the body
// at all: `origin_id`, `origin_type`, `hits` and `last_hit` were sent and were
// absent from the stored policy afterwards, while `icmp_typename` and
// `icmp_v6_typename` were kept — and those two are the operator's narrowing this
// exists to carry, so the part that matters is the part that lands.
//
// It does not follow that a policy's existing `origin_id` survives an update,
// and this deliberately does not claim it. The probe added those four to a
// custom policy that had none, so what was measured is that the DTO does not
// *take* them from a body — not that it leaves a generated policy's own values
// alone. Only a policy the Controller generated carries them, and #37's probe was
// scoped to a throwaway nothing rides. Sending them back is still the right side
// to err on under a replace: a body that carries a field cannot be the reason it
// was dropped.
//
// The field that did refuse was not one of the six. See setReturnRuleRequest.
func mergeIntoStoredPolicy(
	ctx context.Context,
	client unifi.Client,
	site, id string,
	desired config.FirewallPolicy,
	bound bindings,
) error {
	stored, err := readStoredPolicy(ctx, client, site, id)
	if err != nil {
		return err
	}
	stored.dropMarkers()
	if err := stored.overwriteManaged(desired, bound); err != nil {
		return err
	}
	return client.Put(ctx, policyPath(site, id), stored, nil)
}

// readStoredPolicy is one live policy as the Controller holds it, read again at
// the moment of writing.
//
// It asks for the collection and picks the policy out of it rather than asking
// for that one policy by ID. Both are paths go-unifi knows, and only the
// collection is one unifig has ever sent: it is what every plan reads and what
// the recording holds, so an update depends on no endpoint a plan did not
// already depend on.
//
// What that costs is one read of all eighty-three policies per policy updated,
// where the single-object path would read one. It is the same request count and
// a bigger body, on a router with fewer than a hundred policies, and an apply
// updating several of them is rare — a poor trade for depending on an endpoint
// nothing here has ever exercised.
//
// A policy that is not in the answer is not an error to report at the library's
// wording. The operator asked to change a policy and there is no longer one to
// change, which is a thing to say plainly.
func readStoredPolicy(ctx context.Context, client unifi.Client, site, id string) (storedPolicy, error) {
	var held []storedPolicy
	if err := client.Get(ctx, policiesPath(site), nil, &held); err != nil {
		return nil, fmt.Errorf("reading the firewall policies of site %q back before writing one: %w", site, err)
	}
	for _, stored := range held {
		if stored.id() == id {
			return stored, nil
		}
	}
	return nil, errors.New("the Controller no longer has it")
}

// policiesPath and policyPath are the Controller's v2 firewall-policy
// endpoints, written out in full because go-unifi resolves a relative path
// under the v1 base and exposes no v2 one — the gap readZoneFacts describes,
// from the other side of it.
//
// Only the new-style base is named, where readZoneFacts tries both. A client
// that reached an old-style Controller could not have authenticated to it at
// all: API-key auth is a UniFi OS gate, so the SDK refuses to build a client for
// one (ADR-0003), and a write is not the place to guess at a path a read could
// afford to be wrong about.
func policiesPath(site string) string {
	return fmt.Sprintf("%s/site/%s/firewall-policies", unifi.NewStyleAPI.ApiV2Path, site)
}

func policyPath(site, id string) string {
	return fmt.Sprintf("%s/%s", policiesPath(site), id)
}

// id is the Controller's own ID for a stored policy, and empty when the field is
// absent or is not a string — neither of which any policy anyone has read does,
// and both of which are a policy this cannot be asked to match.
func (p storedPolicy) id() string {
	var id string
	if err := json.Unmarshal(p["_id"], &id); err != nil {
		return ""
	}
	return id
}

// overwriteManaged writes the config's values onto a stored policy and touches
// nothing else: the update half of what overwriteManagedPolicy does for a
// create, kept over the object the Controller sent rather than over the struct
// go-unifi could read out of it. The four fields are the whole of what unifig
// owns on a policy, and the other half of that list is up there.
//
// The fifth is `create_allow_respond`, which unifig does own and which is not a
// line of the config: it is the verdict, restated as the request the Controller
// acts on. See setReturnRuleRequest.
func (p storedPolicy) overwriteManaged(desired config.FirewallPolicy, bound bindings) error {
	source, err := bound.zoneID(desired.Source)
	if err != nil {
		return err
	}
	destination, err := bound.zoneID(desired.Destination)
	if err != nil {
		return err
	}

	if err := p.set("name", desired.Name); err != nil {
		return err
	}
	if err := p.set("action", storedActions[desired.Action]); err != nil {
		return err
	}
	if err := p.setZone("source", source); err != nil {
		return err
	}
	if err := p.setZone("destination", destination); err != nil {
		return err
	}
	return p.setReturnRuleRequest(desired)
}

// setReturnRuleRequest writes the request for the companion return rule to match
// the verdict the config states — the fifth field unifig owns on a policy, and
// the one it came to own last.
//
// It only ever *cleared* until now, and the reason was a missing reading rather
// than a principle. The Controller refuses a body asking for a companion beside
// a verdict that closes a path (ADR-0022), so clearing was forced; setting was
// a write nobody had watched a Controller answer, so ADR-0025 left the flag
// alone on an opening verdict and had the plan describe the resulting
// inconsistency instead.
//
// Issue #40's own probe took both readings, on the live migrated UDR on 19
// August 2026, on a throwaway `Dmz` -> `Dmz` policy that nothing rides. Each
// moved one variable, with the verdict held at `ALLOW` throughout — which is
// what ADR-0022's reading could not do, because its request changed the flag and
// the verdict together:
//
//	create allow, flag true    86 -> 88 policies, companion present
//	flag true -> false         88 -> 87 policies, companion GONE
//	flag false -> true         87 -> 88 policies, companion BACK
//	delete the policy          88 -> 86 policies, id for id
//
// So the flag drives the companion on an update exactly as it does on a create,
// in both directions. unifig owns it, the config decides it, and a policy's
// companion follows the file rather than the policy's history (ADR-0026).
//
// It writes the key unconditionally, where clearing wrote it only where the
// Controller had sent one carrying `true`. That is not the `schedule.time_all_day`
// defect of ADR-0021 in a new place, and the difference is ownership: inventing
// a field is writing a Go zero onto something the operator set and unifig does
// not model, while this is a field unifig now states on purpose, on every create
// already, and every policy either site holds carries it.
func (p storedPolicy) setReturnRuleRequest(desired config.FirewallPolicy) error {
	return p.set("create_allow_respond", opensAPath(desired.Action))
}

// set writes one field of a stored policy.
func (p storedPolicy) set(field string, value any) error {
	written, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("building the %s of this update: %w", field, err)
	}
	p[field] = written
	return nil
}

// setZone writes the zone at one end of a stored policy, leaving the rest of
// that end alone — the ports it matches, the addresses, the networks, whether it
// matches the opposite of any of them.
//
// An end written whole would be this file's own subject one level down: a policy
// an operator narrowed to a port would go back matching every port, because the
// config says which zone an end is and nothing else about it.
func (p storedPolicy) setZone(end, zone string) error {
	fields := storedPolicy{}
	if sent, held := p[end]; held {
		if err := json.Unmarshal(sent, &fields); err != nil {
			return fmt.Errorf("the %s end of the policy the Controller sent is not an object: %w", end, err)
		}
	}
	if err := fields.set("zone_id", zone); err != nil {
		return err
	}
	return p.set(end, fields)
}

// dropMarkers takes the Controller's read-only markers off a stored policy.
//
// The rule is writableZone's and the reasoning is there — a marker the
// Controller sends is not a field unifig sends back (ADR-0019). What differs
// here is the evidence, and it is worth being plain about rather than letting
// the symmetry imply otherwise: a real UDR was measured refusing `attr_no_edit`
// on a zone, and nobody has ever sent one to the policy endpoint. Nor has a
// policy been seen carrying a marker — none of the eighty-three a migrated
// router ships has an `attr_*` field at all — so this fixes no failure anyone
// has met.
//
// It is still unifig's rule to keep, because a policy is the other object unifig
// writes whole. The first firmware to mark a policy reproduces #27 here, on the
// one endpoint whose refusal has never been read, and the correlation would look
// like a rule about which policies may be edited for the same reason it did the
// first time (issue #34).
//
// Where writablePolicy cleared the four markers go-unifi models and let
// `omitempty` do the rest, this drops the whole family by name. That is not a
// widening of the rule — it is the rule as ADR-0019 and both marker tests state
// it — but merging into what the Controller sent is what makes stating it that
// way possible: a marker the library has never heard of used to be dropped by
// accident and would now be sent back.
func (p storedPolicy) dropMarkers() {
	for field := range p {
		if strings.HasPrefix(field, markerPrefix) {
			delete(p, field)
		}
	}
}

// newFirewallPolicy builds the Controller object for a policy unifig is
// creating.
//
// The config models four fields and a policy has thirty, so the rest are set
// here: the policy applies to every protocol, in both address families, at all
// times, from anywhere in the source zone to anywhere in the destination zone.
// A policy created from a bare struct would instead be disabled, on no schedule,
// and matching nothing, which would govern no traffic at all. That reasoning is
// the whole of why these values are what they are, and it is all this comment
// claims for them: it used to say they were "the Controller's own defaults for a
// new policy — matching what its UI creates", which nobody has checked, because
// no policy created in the UI has ever been read back field by field (issue #36).
//
// What has been checked is that the Controller takes them: #30 created a policy
// on the live migrated UDR and every field here came back as sent (ADR-0019).
// Accepted is not matched, and the recording is where the difference shows —
// of the eighty-three policies a migrated router ships, sixty-three are
// `protocol: all` and sixty-one are `ip_version: BOTH`, because they are
// purpose-built rules rather than anybody's starting shape. They are the wrong
// table to read a default off, and the one field below that *was* read off them
// is read off them for a reason that has nothing to do with defaults.
//
// That field is named because of what the unnamed ones do. go-unifi v2.3.0
// models six of a policy's booleans without `omitempty`, so all six go on the
// wire on every create whether unifig names them or not; `enabled` is the one
// unifig names, and the other five go as Go's zero rather than as anyone's
// decision. Three of those five sit where the Controller's own policies sit
// anyway — `logging`, `match_ip_sec` and `match_opposite_protocol` are false on
// all eighty-three. `predefined: false` differs from all eighty-three and should:
// a policy unifig made is not one the Controller ships. `create_allow_respond`
// was neither, which is the whole of issue #36 — and it is the one value here
// that is not fixed, because the Controller refuses it on any verdict but
// `allow`. That is why this takes the policy it is building rather than nothing.
//
// They apply on create only. An operator who afterwards narrows the policy to a
// port, a client or an evening keeps that forever, because an update merges into
// the object the Controller sent and writes only the same four values
// (mergeIntoStoredPolicy, ADR-0021).
func newFirewallPolicy(desired config.FirewallPolicy) unifi.FirewallZonePolicy {
	return unifi.FirewallZonePolicy{
		Enabled:             true,
		Protocol:            "all",
		IPVersion:           "BOTH",
		ConnectionStateType: "ALL",
		// This asks the Controller to generate the companion return rule, and
		// it is a request rather than a property: measured on the live migrated
		// UDR on 18 August 2026, creating one allow policy with it true made the
		// site go from 86 policies to 88 — unifig's own, and a second the
		// Controller named `<name> (Return)`, `RESPOND_ONLY`, `predefined: true`,
		// carrying `origin_type: custom_firewall_rule` and an `origin_id` back
		// to its parent. The same create with it false made 87 and no companion,
		// which is what makes this the cause rather than a correlation. Deleting
		// the parent took the companion with it (issue #36, ADR-0022).
		//
		// It is asked for on an allow and not otherwise, because the Controller
		// refuses the pair. A create of a `block` policy carrying it true is a
		// 400, `Firewall policy create respond traffic not allowed`, which fails
		// the whole apply — measured in the same session, and the reason
		// `newFirewallPolicy` had to learn what verdict it is building for. A
		// `reject` was never sent, since the apply stopped at the block; it goes
		// out false with every other non-allow verdict, which is what unifig sent
		// on everything it created before any of this and what the Controller has
		// always taken.
		CreateAllowRespond: opensAPath(desired.Action),
		// The Controller rejects a policy with no schedule outright, so this is
		// less a default than a field with one permitted value at creation. It
		// is not parity either: all eighty-three policies the recording holds
		// carry `{mode: ALWAYS}` and no `time_all_day` at all. Sending one is
		// inventing a field on the object, which is a loss on the update path
		// (ADR-0021) and nothing on this one, where there is no object yet and
		// no operator's value to write over.
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
