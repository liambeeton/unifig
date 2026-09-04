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
// "can" rather than "will", and that hedge is load-bearing — unifig does not know
// whether a policy is enabled, and reads its precedence (`index`) only far enough
// to tell which side of the generated tier it sits on (ADR-0033). So it knows that
// a rule closing the management path is being written and cannot know what the
// rule set as a whole will do with it (ADR-0018): one boundary is not an
// evaluation order.
const gatewayRisk = "the Controller answers in the Gateway zone, and blocking traffic to it can cut the path this site is managed over"

// blockingActions are the verdicts that close a path. A policy moving between
// two of them closes nothing that was not closed already, which is why the risk
// check asks what the verdict is changing *from* as well as to.
var blockingActions = map[string]bool{"block": true, "reject": true}

// specificPorts and anyPorts are the Controller's two port-matching modes that
// unifig understands. There is a third, portGroup, which points at a port group
// — unifig neither writes it nor has a word for it, and a policy carrying one
// reads back with an unmanaged narrowing that export counts rather than
// describes (ADR-0032).
const (
	specificPorts = "SPECIFIC"
	anyPorts      = "ANY"
	portGroup     = "OBJECT"
)

// anyMatch is the Controller's word for an end that matches on the zone there
// and nothing more, which is every end unifig writes.
//
// It is the same string as anyPorts and a different field's answer: that one is
// one of the port-matching modes unifig writes, this one is a `matching_target`
// unifig only ever reads. A constant standing for both would be one place saying
// two things, and the day the Controller spells one of them differently is the
// day that costs an hour.
const anyMatch = "ANY"

// policyProtocols are the six of the Controller's thirty-seven that unifig
// models, spelled exactly as the Controller spells them.
//
// Six rather than thirty-seven because ADR-0004 rejected inheriting the
// Internal API's surface as unifig's public one by name, and the other
// thirty-one are transport and tunnelling protocols nobody writes a home
// firewall rule about. Spelled as the Controller spells them because there is
// nothing to translate: unlike a verdict, which the Controller stores upper-case
// and the file states lower-case, `tcp_udp` is `tcp_udp` on both sides, and a
// translation table with nothing in it to translate is a table that can drift.
var policyProtocols = map[string]bool{
	"all": true, "tcp": true, "udp": true, "tcp_udp": true, "icmp": true, "icmpv6": true,
}

// portBearingProtocols are the modelled protocols a port can narrow.
//
// It holds the same three strings as portforward.go's modelledProtocols and is
// deliberately not the same variable: that one is the whole of what a forward's
// protocol may be, this one is which of six policy protocols carry ports. They
// coincide today because TCP and UDP are the protocols with ports; a seventh
// modelled protocol that had them would move this one and not that one.
//
// The other three — `all`, `icmp`, `icmpv6` — have no ports, so stating one of
// them is what clears a narrowing. `validate` refuses a file stating ports
// beside one (see portsNeedAProtocol), which is the same rule facing the other
// way, and both exist because the Controller enforces neither: it was measured
// accepting `all` beside a specific port and storing it, on the live migrated
// UDR on 25 August 2026 (ADR-0031).
var portBearingProtocols = map[string]bool{"tcp": true, "udp": true, "tcp_udp": true}

// narrowing is what a config entry says to do to a live policy's protocol and
// destination ports, which is not the same as what it states.
//
// The distinction is ADR-0004's and it is the whole of this type. A file that
// states no protocol is not asking for `all`; it is asking for nothing, and the
// live policy's own narrowing stays. A file that states a protocol with no ports
// is asking about the protocol only. And a file stating a protocol that has no
// ports is asking for two things at once — that protocol, and the ports gone —
// because unifig will not write a port beside a protocol that cannot carry one.
// That last case is ADR-0004's own "a modelled field's change strands an
// unmodelled one, so unifig repairs it and says so in the plan", and it is the
// only way the file has to widen a policy again.
type narrowing struct {
	// protocol is empty where the config states none, which means unmanaged.
	protocol string
	// ports is the Controller's own joined form, empty where the config states
	// none — which means unmanaged unless clears is set.
	ports string
	// clears is whether the stated protocol takes the port matching with it.
	clears bool
}

// narrowingOf reads the config's narrowing off an entry.
//
// It trusts `validate` for the combination it does not check: ports beside a
// protocol that has none is refused offline, so ports here always arrive beside
// a protocol that can hold them.
func narrowingOf(desired config.FirewallPolicy) narrowing {
	if desired.Protocol == "" {
		return narrowing{}
	}
	if !portBearingProtocols[desired.Protocol] {
		return narrowing{protocol: desired.Protocol, clears: true}
	}
	return narrowing{protocol: desired.Protocol, ports: joinPorts(desired.Ports)}
}

// manages is whether this narrowing asks for anything at all.
func (n narrowing) manages() bool { return n.protocol != "" }

// managesPorts is whether it asks for the port matching specifically, either by
// naming ports or by clearing them.
func (n narrowing) managesPorts() bool { return n.clears || n.ports != "" }

// joinPorts and splitPorts convert between the config's list and the
// Controller's single string, which holds the whole list — `"443,80"`,
// `"8000-8010"`, or a mix. The separator is the Controller's own, with no
// spaces: what goes on the wire is what a UDR was measured accepting and
// reading back unchanged (ADR-0031).
func joinPorts(ports []config.Port) string {
	if len(ports) == 0 {
		return ""
	}
	parts := make([]string, 0, len(ports))
	for _, port := range ports {
		parts = append(parts, string(port))
	}
	return strings.Join(parts, ",")
}

func splitPorts(joined string) []config.Port {
	parts := strings.Split(joined, ",")
	ports := make([]config.Port, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			ports = append(ports, config.Port(trimmed))
		}
	}
	if len(ports) == 0 {
		return nil
	}
	return ports
}

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

// asksForReturnRule is whether unifig requests the companion return rule for a
// policy, and it is the one place that decides. The create, the update and the
// plan all go through it so that what the plan promises, what a create asks for
// and what an update writes cannot come to mean different things — the same
// reason `plan --prune` and `apply --prune` share options().
//
// Two conditions, measured a week apart on the same live migrated UDR on
// Network 10.5.67, and neither inferable from the other:
//
// The verdict has to open a path. The Controller refuses the request beside any
// verdict that closes one, because there is no traffic to return (ADR-0022).
//
// The destination has to be a zone other than External. The Controller refuses
// the request for a policy into the internet with the very same message, and
// this one cost an operator a half-applied firewall to find: every `→ internet`
// allow in a config is a create unifig could not make (ADR-0030). Return traffic
// on that path is the gateway's own to keep track of, so there is nothing for a
// companion policy to do and the Controller declines to make one — it takes the
// same allow without the request and generates nothing.
//
// Its counterpart is the plan's: a policy this returns false for is one whose
// change must not promise a companion, which is why returnRuleNote and
// returnRuleField ask it rather than asking the verdict.
func asksForReturnRule(desired config.FirewallPolicy, facts zoneFacts) bool {
	return opensAPath(desired.Action) && !intoExternal(desired, facts)
}

// intoExternal is whether a policy's destination is the zone the Controller
// stands the internet up as, found by the Controller's own `zone_key` rather
// than by the name "External" — the same reading, and for the same reason, as
// the gateway zone a Risky change turns on (ADR-0018).
//
// Unknown facts mean unifig asks for the companion, which is what it did before
// any of this was known. That is the conservative end on purpose: the Controller
// answering the request with a 400 names the policy and stops the apply, where
// silently not asking would leave an allow policy without the companion its
// neighbours have and say nothing.
func intoExternal(desired config.FirewallPolicy, facts zoneFacts) bool {
	return facts.known && facts.external != "" && desired.Destination == facts.external
}

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
	byKey, err := policiesByKey(live, bound)
	if err != nil {
		return nil, nil, nil, err
	}
	held := policyNames(live)
	// Which pairs a Return Rule sits on, computed once for the whole plan: it
	// reads the live collection and the config together, and both halves are the
	// same for every policy in the loop below.
	carrying := pairsCarryingCompanions(cfg, live, facts, bound)

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
	var caveats []Caveat
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
			changes = append(changes, createFirewallPolicy(desired, cfg, facts, bound, carrying))
			continue
		}
		stated, _ := fromLivePolicy(current, bound)
		if change, differs := updateFirewallPolicy(desired, current, cfg, facts, bound, held, carrying); differs {
			// A Generated Policy is matchable and is not writable, so the
			// difference is real and the change is not one this plan may promise
			// (ADR-0027). The caveat is said only where something differs: the
			// nineteen `Allow All Traffic` policies a migrated router ships are
			// generated too, and a file stating the verdict they already have
			// has asked for nothing.
			if generated(current) {
				caveats = append(caveats,
					unwritablePolicy(keyOfDesiredPolicy(desired), closesTheGateway(stated, desired, facts)))
				continue
			}
			blocking = blocking || becomesBlocking(stated, desired)
			changes = append(changes, change)
		}
	}
	caveats = append(caveats, unreadableGateway(blocking, facts)...)
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

// generated is whether the Controller computed this policy for a pair of zones
// rather than storing it — a Generated Policy — which is the whole of whether
// unifig can write to it at all.
//
// It is asked of the `_id`, because the `_id` is what fails. A stored policy's
// is a document handle: twenty-four hex characters, handed out on create, and
// the thing the write endpoint resolves. A generated policy's is the source zone
// id, the destination zone id and the index concatenated — not a handle but a
// description of where the policy came from, and the Controller answers 404
// `api.err.FirewallPolicyNotFound` to it on GET and on PUT alike (ADR-0027).
//
// **`predefined` is not what this reads, and the difference is deliberate.** The
// two agree on every policy anyone has measured: all eighty-six a migrated router
// holds are `predefined: true` and every one carries a composite id, while the
// custom policy the probe created was `predefined: false` with a handle. But they
// are different claims — one says who made the policy, the other says whether
// there is anything to write to — and reading the marker to decide what a write
// can do is the mistake issue #34 already corrected once: `attr_no_edit` was
// taken for a statement about which zones may be edited, and it turned out to
// mark nothing of the kind (ADR-0019). A firmware that stores its own policies
// properly would be followed by this and refused forever by the marker.
//
// Note that this is not the built-in exemption and does not replace it.
// `sparedFromPrune` asks this too, beside `predefined` rather than instead of it
// (ADR-0028): a deletion needs an id as much as an update does, and prune's own
// question — whose object it is — is still the marker's (ADR-0005).
func generated(policy unifi.FirewallZonePolicy) bool { return !isDocumentHandle(policy.ID) }

// documentHandleLength and isDocumentHandle are the shape of an id the Controller
// resolves: twenty-four lowercase hex characters, which is what every object it
// stores is addressed by and what a create hands back.
const documentHandleLength = 24

// returnRule is whether this policy is the companion the Controller generates
// for the reply traffic of a policy created allowing — a Return Rule.
//
// It is asked of `connection_state_type`, **deliberately not of the id shape**.
// What disqualifies a companion from the config is not that it cannot be written
// to; it is that it is not a Resource at all. unifig never creates, names or
// deletes one: the config states its arrival and departure as a field of its
// parent's change (ADR-0026), so an entry of its own would be a second,
// competing statement about the same object. A test on the id would exclude the
// right policies for the wrong reason, and would go on excluding them only for
// as long as the id happened to fall that way.
//
// The id does fall that way on everything measured, and this does not lean on
// it. All twelve Return Rules the recording holds carry a composite id, and
// ADR-0026's write session read one off a companion whose parent was custom —
// unifig's own — and found the same shape, which is what makes the `_id` scheme
// a property of generated policies rather than of shipped ones. So `generated`
// would catch every companion anyone has seen. It is still the wrong question to
// ask about one.
//
// What actually links a companion to its parent is `origin_id`, which `go-unifi`
// v2.3.0 does not model (ADR-0021), so this is the strongest thing the struct
// can say — and strong enough for what it decides.
//
// It decides two things, and one predicate is what keeps them one decision.
// `projectFirewallPolicies` asks it to leave the companion out of the file, and
// `sparedFromPrune` asks it to leave the companion on the Controller. Those are
// the halves of ADR-0028's guarantee — a file unifig wrote must not be a file
// prune deletes from — and halves that read different fields are halves that can
// drift.
func returnRule(policy unifi.FirewallZonePolicy) bool {
	return policy.ConnectionStateType == respondOnly
}

// respondOnly is the Controller's word for a policy that matches only the reply
// half of a conversation, which is the whole of what a Return Rule is for.
const respondOnly = "RESPOND_ONLY"

func isDocumentHandle(id string) bool {
	if len(id) != documentHandleLength {
		return false
	}
	for _, c := range id {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

// unwritablePolicy is the Caveat for a change to a Generated Policy: the config
// asks for something the Controller has no way to be asked, so the plan says so
// instead of promising it (ADR-0014, ADR-0027).
//
// It is a Caveat rather than an error, on the reasoning the type gives: the run
// is still correct and the rest of the file still applies. It is a Caveat rather
// than a silence for the sharper half of that reasoning — an operator who edits
// the verdict of `Allow All Traffic` and reads "No changes" has been told a lie
// about their own file.
//
// The policy is named by its whole key rather than by its name, because a
// migrated router ships nineteen called `Allow All Traffic` and a sentence about
// one of them has to say which (ADR-0001, issue #24).
//
// It ends with the way out rather than with the refusal, and the way out is a
// measured fact rather than advice: the Controller's own policy on a pair sits at
// `index: 2147483647`, the lowest precedence there is, so a policy of the
// operator's own on the same pair is one that takes effect over it (ADR-0018).
// That is a policy unifig creates, owns and can change afterwards.
//
// **The rename in that sentence is load-bearing rather than stylistic.** A
// policy's key is its name together with its pair of zones (ADR-0001), so an
// entry keeping this policy's name on this policy's pair has this policy's key:
// planFirewallPolicies matches it to the generated policy, computes the same
// difference, and arrives back here with the same caveat. An operator who
// followed the advice literally would loop, and unifig would never create
// anything (issue #43). A name of the operator's own is a key of their own,
// which is nothing to match and therefore the create the way out promises.
//
// It gets more load-bearing once Export stops writing generated policies into
// the file (ADR-0028): an operator overriding one then has no exported line to
// edit, so they write a fresh entry and take the name off the UniFi UI in front
// of them — which is the name this sentence has to talk them out of.
//
// `closing` is whether the change being held back was one that would have closed
// the path to the Gateway zone, and it is here because otherwise this sentence
// tells an operator to go and do the one thing unifig stops to confirm. The mark
// is gone from the change — there is no change — and the danger is not: writing
// your own blocking policy over the Controller's `Allow All Traffic` to the
// Gateway is exactly the create ADR-0018 marks Risky, and it is what this
// paragraph would otherwise be recommending in passing. So the way out carries
// the same words the mark does, and an operator meets them here rather than
// discovering them at the confirmation prompt.
func unwritablePolicy(key policyKey, closing bool) Caveat {
	way := "the Controller's own sits at the lowest precedence there is, so a policy of your own on the same pair, " +
		"under a name of your own, takes precedence over it"
	if closing {
		// Named as a Risky change rather than only described, because that is the
		// word an operator already knows from every plan that carries one, and
		// because writing this policy is the change that would carry it.
		way += " — and that would be a Risky change here: " + gatewayRisk
	}
	return Caveat{Kind: FirewallPolicy, Reason: fmt.Sprintf(
		"the %s %s will not be changed: the Controller generates its own policy for a pair of zones "+
			"rather than storing one, so it has no id to write to and no endpoint can edit it; %s",
		kinds[FirewallPolicy].one, key, way)}
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

// policiesByKey is the live collection indexed by the key a config entry
// matches, and it refuses the site where a key has no one answer.
//
// **The index and the refusal are one function because they are one decision.**
// What the refusal exists for is the moment a config entry has to resolve to
// exactly one live policy, so "which policies is this site refused over" and
// "which policy does this key mean" are the same question asked from either
// side. Asked in two places they could come to disagree, and the shape that
// disagreement takes is a site unifig plans against one policy and refuses to
// export, or the reverse.
//
// **Two policies sharing a name is ordinary; sharing a name and a pair of zones
// is no longer the same thing as having no answer.** A policy the operator
// stored and one the Controller generates can share all three, and the
// Controller has already said which of them the site is about: it answered `201`
// to the create, kept both objects, and put the stored one at `index: 10000`
// against the generated one's `2147483647` — the lowest precedence there is, on
// its own ordering (ADR-0029, issue #46). So the pair is matchable rather than
// ambiguous: the stored policy is what a config entry means, the generated one
// is shadowed, and unifig stops refusing a whole site over a state ADR-0027's
// own way out invites an operator to create.
//
// What is still refused is a key more than one *stored* policy carries. Nothing
// has answered that one — both can be written to, so unifig would be guessing
// which the file meant — and it is the ambiguity this was built for.
//
// It is asked by the verbs that match policies to config rather than by the
// read, which is why listFirewallPolicies filters and refuses nothing.
//
// A policy without a key is left out of both answers rather than filed under an
// empty one. It is not one of a pair unifig had to choose between, and counting
// it would refuse the site over a clash between two ends that could not be
// named; nothing in the file can match it either, and prune must not reach it —
// deleting a policy unifig could not describe is a change it could not have
// shown the operator first.
func policiesByKey(
	live []unifi.FirewallZonePolicy,
	bound bindings,
) (map[policyKey]unifi.FirewallZonePolicy, error) {
	sharing := make(map[policyKey][]unifi.FirewallZonePolicy, len(live))
	for _, policy := range live {
		if key, keyed := keyOfLivePolicy(policy, bound); keyed {
			sharing[key] = append(sharing[key], policy)
		}
	}

	byKey := make(map[policyKey]unifi.FirewallZonePolicy, len(sharing))
	var shared []string
	for key, alike := range sharing {
		contenders := unshadowed(alike)
		if len(contenders) == 1 {
			byKey[key] = contenders[0]
			continue
		}
		// Whose the clashing policies are, which decides what the operator can
		// do about them. A contending group is all stored or all generated —
		// that is what unshadowed leaves — so the first says it for the group.
		mine := "of your own"
		if generated(contenders[0]) {
			mine = "the Controller generates itself"
		}
		shared = append(shared, fmt.Sprintf("%d %s matching %s", len(contenders), mine, key))
	}
	if len(shared) == 0 {
		return byKey, nil
	}

	slices.Sort(shared)
	// **The sentence names the end of the clash an operator can act on**, which
	// it used not to. "Rename or remove the extras in the Controller's UI" is
	// half-unreachable the moment a policy the Controller generates is one of the
	// extras: it has no id any endpoint resolves, so it can be neither renamed
	// nor deleted, and the UI has nothing to offer for it either (ADR-0027, issue
	// #46). Saying which are the operator's is what makes the instruction one
	// they can carry out — and on the clash where none of them is, it is what
	// stops the instruction being the whole answer.
	//
	// **The last clause is a way out rather than an aside.** Where the clash is
	// between policies the Controller generates, there is nothing to rename and
	// nothing to remove, and writing a policy of the operator's own on that name
	// and pair takes precedence over every generated policy carrying it — so the
	// key resolves on a create rather than on a deletion nobody can perform. It
	// doubles as why the count can be smaller than what the UI shows: a generated
	// policy under a stored one's key is shadowed, not counted.
	return nil, fmt.Errorf(
		"unifig matches firewall policies on the Controller by name and the pair of zones they govern, so no two "+
			"may share all three: this site has %s; rename or remove the extras of your own in the Controller's "+
			"UI, then run again — a policy the Controller generates for a pair of zones has no id, so it can be "+
			"neither renamed nor deleted, and one of your own sharing its name and pair takes precedence over it "+
			"rather than clashing with it",
		strings.Join(shared, ", "))
}

// unshadowed is which of the policies sharing a key a config entry could still
// mean, once the Controller's own precedence has been applied: the stored ones
// where the key has any, and all of them where it has none.
//
// **The gate is the `_id` shape rather than `predefined`**, for the reason
// `generated` gives and one this sharpens. What settles the clash is not whose
// policy it is but which of the two the operator can be talking about at all,
// and that is the one with something to write to: an entry matched to the
// generated policy could only ever produce the caveat (ADR-0027), while the same
// entry matched to the stored one is a change unifig makes. Reading the marker
// instead would put a policy an operator wrote and the Controller marked its own
// on the losing side of a precedence it should win, and would shadow forever the
// policies of a firmware that stored its own properly.
//
// **Where nothing is stored, nothing is shadowed.** Two policies the Controller
// generates sharing one key is a firmware nobody has met — the eighty-six a
// migrated router ships hold no such pair, which is what a healthy export against
// the baseline site says (issue #46) — and unifig can write to neither of them,
// so there is no precedence here to apply and choosing between them would be the
// guess the refusal exists to avoid. They go back as they came, and the caller
// refuses the key.
//
// It does not ask which policy the site *enforces*, and the distinction is worth
// keeping. That reading is the Controller's index model — lower is evaluated
// first — and it is the evidence the precedence rests on rather than the test it
// applies: the `10000` a create is assigned is a value the Controller chose
// unasked, measured once, and a match resolved on it would be unifig reading a
// field it has never had a reason to model.
func unshadowed(sharing []unifi.FirewallZonePolicy) []unifi.FirewallZonePolicy {
	stored := make([]unifi.FirewallZonePolicy, 0, len(sharing))
	for _, policy := range sharing {
		if !generated(policy) {
			stored = append(stored, policy)
		}
	}
	if len(stored) == 0 {
		return sharing
	}
	return stored
}

// projectFirewallPolicies projects the site's policies into the config that
// would describe them, names the ones it could not describe at all, and counts
// the ones it could describe and left out anyway.
//
// **Three policies never reach the file, and the order they are tested in is the
// order of the reasons.**
//
// A **Generated Policy** goes first and is counted. unifig can word one
// perfectly well — the plan prints one every time it holds a change back — but
// an entry naming it is a line no plan may ever act on, so a file carrying one
// claims to manage what it cannot change (ADR-0028). It is asked before
// describability because the two can both be true of one policy, and this is the
// truer thing to say: a policy with no id to write to is out whether or not its
// zones have names, and blaming the zones would send an operator looking for a
// shortfall that is not the one they have.
//
// A **Return Rule** goes out beside it, for a reason of its own: it is not a
// Resource, and the config already states it as the verdict of its parent
// (ADR-0026), so an entry of its own would be a second, competing statement
// about the same object. That is true whatever its `_id` turns out to be, which
// is why the test is `connection_state_type` and not the id shape.
//
// A policy unifig **cannot word** goes last and is named. That happens when a
// zone on either end of it is one unifig cannot name or when its verdict is one
// unifig does not model — the whole of a policy is its name, its verdict and its
// pair of zones, so a policy missing any of them is not a policy the config has a
// way to write. That is listWLANs' rule rather than fromLiveZone's: a zone can be
// described in part because its membership is a list, and a policy cannot,
// because every field it has is required.
//
// **What the count counts is a narrower question than what the loop excludes**,
// which is why it is asked afterwards rather than tallied inside — see
// unaccountedFor. It is a count rather than a list because a migrated router
// ships eighty-six of these under names it reuses across them, and the notice
// nobody reads protects nobody (ADR-0012, ADR-0028).
func projectFirewallPolicies(ctx context.Context, client unifi.Client, site string, bound bindings) (
	policies []config.FirewallPolicy, indescribable []string, unaccounted, partial int, err error,
) {
	live, err := listFirewallPolicies(ctx, client, site)
	if err != nil {
		return nil, nil, 0, 0, err
	}
	// Export matches too, in the sense that matters here: a file describing two
	// policies unifig cannot tell apart is a file it cannot plan afterwards. It
	// wants the refusal rather than the index — what it writes is decided per
	// policy, below — and it asks for both anyway, because the refusal a second
	// function computed would be one that could disagree with the plan's.
	if _, err := policiesByKey(live, bound); err != nil {
		return nil, nil, 0, 0, err
	}

	policies = make([]config.FirewallPolicy, 0, len(live))
	var left []unifi.FirewallZonePolicy
	for _, policy := range live {
		if generated(policy) || returnRule(policy) {
			left = append(left, policy)
			continue
		}
		described, ok := fromLivePolicy(policy, bound)
		if !ok {
			indescribable = append(indescribable, policy.Name)
			continue
		}
		if narrowedBeyondTheConfig(policy) {
			partial++
		}
		policies = append(policies, quietWideNarrowing(quietInvertedMatch(described, policy)))
	}
	slices.SortFunc(policies, func(a, b config.FirewallPolicy) int { return strings.Compare(a.Name, b.Name) })
	slices.Sort(indescribable)

	// Nil rather than empty when nothing survived. `omitempty` already keeps the
	// `firewall-policies:` key out of the YAML either way (ADR-0028), so this is
	// not what makes the key disappear — it is what keeps the Config itself
	// honest. Nil and empty are two different statements in this type: an absent
	// section is unmanaged, an empty one says there should be none and prune acts
	// on it (ADR-0006). A projection handing back `[]` would be a Config that
	// asks for every policy on the site to be deleted.
	if len(policies) == 0 {
		policies = nil
	}
	return policies, indescribable, unaccountedFor(left, writtenPolicyNames(policies)), partial, nil
}

// unaccountedFor is how many of the policies export left out the notice has to
// speak for: the ones that are gone from the file with nothing in the file to
// explain them.
//
// **It is a narrower question than "what was left out", and the gap is the
// companion.** Two things disqualify a policy from the config and they do not
// answer the same question. Having no id to write to is a fact about the policy,
// and it is what the notice is about — "the Controller generates rather than
// stores", with no id and no endpoint that could edit it. Being a Return Rule is
// a fact about the policy's *relationship*: it is left out because its parent
// already states it, so where that parent is in the file, the file does account
// for it. Counting it there would put a number in front of the operator that no
// line of their config explains, under a sentence saying unifig manages nothing
// about it — when unifig owns it exactly, through the request field on the
// parent (ADR-0026).
//
// **The Controller's own companions are counted, and that is not an exception.**
// The twelve a migrated router ships are named `Allow Return Traffic` rather than
// `<parent> (Return)` — the Controller's scheme for its own policies is not the
// one it uses for a policy unifig created, which is the same asymmetry
// policyNames is built around — and their parents are generated policies, left
// out too. Nothing in the file accounts for them, so the notice speaks for them
// like any other. That is what keeps the count on a migrated router at
// eighty-six rather than seventy-four.
//
// The parent is found by name, which is the strength policyNames already
// settled for: `origin_id` is what actually links a companion to its parent and
// `go-unifi` v2.3.0 does not model it (ADR-0021), so the name is what the struct
// has. What is claimed is only ever "the file holds a policy by the name this
// one is the companion of", which is exactly as strong as the omission it
// excuses.
func unaccountedFor(left []unifi.FirewallZonePolicy, written policyNameSet) int {
	var count int
	for _, policy := range left {
		if !generated(policy) {
			// Left out for being a companion, and carrying a handle: there is an
			// id here, so "the Controller generates rather than stores" is not a
			// true sentence about it and the notice may not claim it.
			continue
		}
		if parent, named := parentOfReturnRule(policy.Name); returnRule(policy) && named && written[parent] {
			continue
		}
		count++
	}
	return count
}

// writtenPolicyNames is policyNames asked of the config side: every policy that
// made it into the file, by the name the Controller would build a companion's
// out of.
func writtenPolicyNames(policies []config.FirewallPolicy) policyNameSet {
	written := make(policyNameSet, len(policies))
	for _, policy := range policies {
		written[policy.Name] = true
	}
	return written
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
		Protocol:    statedProtocol(policy),
		Ports:       statedPorts(policy),
	}, true
}

// statedProtocol is the policy's protocol as the config would write it, and
// empty where the config has no word for it.
//
// The Controller takes thirty-seven protocols and unifig models six, so this is
// where the other thirty-one become unmanaged rather than wrong: a policy on
// `esp` reads back as a policy stating no protocol, which under ADR-0004 is a
// policy whose protocol unifig does not manage. That is true, and it is what
// keeps an `esp` policy plannable — every other field of it still diffs.
//
// It does not quiet `all` down to nothing, though `all` is the Controller's
// default and means no narrowing at all. Quieting belongs to export, which is
// the only caller that wants it; the planner needs the live value, because a
// file stating `protocol: all` against a policy already on `all` has to come out
// as no change rather than as a perpetual one.
func statedProtocol(policy unifi.FirewallZonePolicy) string {
	if !policyProtocols[policy.Protocol] {
		return ""
	}
	return policy.Protocol
}

// statedPorts is the destination-side port narrowing as the config would write
// it, and nil wherever the config could not write it.
//
// Three things make it nil, and only one of them is "there is no narrowing".
// The port matching may be ANY, which is that one. It may be OBJECT — a
// reference to a port group, which this file has no way to name. Or the
// protocol may be one that has no ports, which is the Controller storing a
// combination unifig refuses to write (ADR-0031): `all` beside a specific port
// was measured being accepted and stored, and an entry repeating it back is one
// `validate` would reject, so export would be writing a file it could not read.
//
// All three come out as an unmanaged narrowing, which is honest. Saying so out
// loud is narrowedBeyondTheConfig's job, and it asks this function rather than
// reading the fields again, so that what the entry states and what the notice
// speaks for cannot come apart.
func statedPorts(policy unifi.FirewallZonePolicy) []config.Port {
	destination := policy.Destination
	if destination.PortMatchingType != specificPorts || destination.Port == "" {
		return nil
	}
	if !portBearingProtocols[policy.Protocol] {
		return nil
	}
	return splitPorts(destination.Port)
}

// narrowedBeyondTheConfig is whether the Controller holds this policy narrowed
// by something the entry export writes for it does not state — the count behind
// export's partial-narrowing notice (ADR-0032, issue #52).
//
// **It is asked of the live policy rather than of the entry**, because the
// shapes it catches are invisible in the entry: a policy narrowed to a port
// group and a policy narrowed to nothing at all write the same three lines. That
// is the whole defect. A file that came back short is supposed to say so, which
// is the promise every other export notice keeps, and this is the one place the
// firewall was quietly not keeping it — ADR-0031 made it sharper by putting a
// combination on the wire (`all` beside a specific port) that unifig will not
// write back.
//
// **The reading is per end and per mode rather than per field.** The Controller
// narrows a policy in five places — the protocol, the two ends' ports and the
// two ends' matching targets — and each mode is read together with what it points
// at, because a mode with nothing under it narrows nothing. Asking the modes
// catches the address lists, MAC lists, app ids and regions that hang off them
// without unifig keeping a list of the Controller's matching engines: a list like
// that fails by going quiet, which is the argument ADR-0005 and ADR-0018 have
// each already made about a different list.
//
// **The inversions are read beside the thing they invert**, for that same
// reason and one of their own: `match_opposite_protocol` and the two
// `match_opposite_ports` are plain bools with no `omitempty`, so they are on the
// wire of every policy the Controller sends and a notice reading one alone would
// speak for an omission that is not there. The address and network inversions
// are not read at all, and need not be — they mean something only beside a
// matching target of IP or NETWORK, and every target but ANY already counts.
//
// **It says nothing about the plan**, deliberately. An entry stating no
// narrowing manages none (ADR-0004), so a policy this counts still plans clean,
// which is what makes writing it the right call rather than dropping it the way
// export drops a port forward whose ports it cannot describe.
func narrowedBeyondTheConfig(policy unifi.FirewallZonePolicy) bool {
	// The protocol, and the two ways it goes unsaid. One outside the six unifig
	// models reads back as an entry stating none, which is unmanaged and true and
	// indistinguishable from a policy that narrows nothing at all; an inverted
	// one names the protocols the policy does *not* match, which the entry has no
	// way to say. Both are asked beside the protocol itself, because a policy
	// carrying none has nothing there to leave unsaid — and an inverted `all` is
	// counted rather than excused, since a policy matching everything-but-all
	// matches nothing and the entry would claim the opposite.
	if policy.Protocol != "" && (statedProtocol(policy) == "" || policy.MatchOppositeProtocol) {
		return true
	}
	// The destination's ports, and the two ways those go unsaid. The entry could
	// not carry them — a port group, or ports beside a protocol that has none,
	// which is what statedPorts already decides — or the Controller inverted
	// them, and an entry repeating a value that names the traffic the policy does
	// not match would say the opposite of what the Controller holds (which is why
	// export drops it as well as counting it, see quietInvertedMatch).
	destination := policy.Destination
	if narrowsPorts(destination.PortMatchingType, destination.Port, destination.PortGroupID) &&
		(destination.MatchOppositePorts || statedPorts(policy) == nil) {
		return true
	}
	// The source end, all of it and its inversion with it: the config states the
	// destination end only (ADR-0031), so any port narrowing here is one with no
	// field to go in.
	source := policy.Source
	if narrowsPorts(source.PortMatchingType, source.Port, source.PortGroupID) {
		return true
	}
	return narrowsTarget(destination.MatchingTarget) || narrowsTarget(source.MatchingTarget)
}

// narrowsPorts is whether an end's port matching narrows anything at all.
//
// It reads the mode together with what the mode points at, rather than the mode
// alone, because an end in a port-matching mode holding nothing narrows nothing
// — a shape nobody has read off a router, and one a notice would otherwise
// speak for. Anything outside the three modes counts whatever it holds: a
// firmware unifig has not met is exactly the case the notice exists for.
func narrowsPorts(matching, port, group string) bool {
	switch matching {
	case "", anyPorts:
		return false
	case specificPorts:
		return port != ""
	case portGroup:
		return group != ""
	default:
		return true
	}
}

// narrowsTarget is whether an end matches on something other than the zone at
// that end — an application, a category, a region, a domain, an address, a
// network, a MAC.
//
// Every value but the Controller's own ANY, rather than the four its UI offers,
// for the reason narrowedBeyondTheConfig gives: a list of matching engines is a
// list that fails by going quiet, and its failure here is a policy narrowed by
// next year's engine exporting as one narrowed by nothing.
//
// An absent field goes with ANY rather than with the rest, which is narrowsPorts'
// reading of an absent mode and needs saying here because the field is
// `omitempty`: every one of the 86 policies in the recording carries a target,
// so an end without one is an end the Controller said nothing about rather than
// one it narrowed by something with no name.
func narrowsTarget(target string) bool { return target != "" && target != anyMatch }

// Where a policy sits in the Controller's evaluation order, and why unifig is in
// the business of saying so at all.
//
// **The bug.** Live UDR on 10.6.101, 3 September 2026 (issue #54). A config
// stating the ordinary pair of intentions — trusted reaches IoT, IoT does not
// reach trusted — planned clean and left the pair like this:
//
//	10000        Ellingson -> Gibson     'Ellingson off the Gibson'      BLOCK  ALL
//	10000        Gibson    -> Ellingson  'Gibson to Ellingson'           ALLOW  ALL
//	30000        Ellingson -> Gibson     'Gibson to Ellingson (Return)'  ALLOW  RESPOND_ONLY
//	2147483647   Ellingson -> Gibson     'Block All Traffic'             BLOCK  ALL
//
// The companion was created exactly as ADR-0022 measured and is never reached:
// the operator's block sits 20000 above it, so the reply to a request the allow
// permits is dropped. Confirmed at packet level in the same session by moving one
// variable — disabling the block brought replies back, re-enabling it took them
// away again. 10000 is the Controller's choice rather than anybody's request, so
// there was nothing an operator could write to avoid it: unifig was creating
// blocks that structurally outranked the companions of its own allows.
//
// **`index` is not writable, and that is measured rather than assumed.** The
// first attempt at this named the index in the create body. On the same router,
// 4 September 2026, on a throwaway `Dmz -> Dmz` policy deleted afterwards:
//
//	POST  index: 40000  ->  201, stored at 10000
//	PUT   index: 40000  ->  200, stored at 10000
//
// The Controller takes the field on both verbs and ignores it. So the field is
// the Controller's to assign and the *position* is what a client may ask for,
// through a different endpoint entirely.
//
// **`PUT .../firewall-policies/batch-reorder` is that endpoint**, measured in the
// same session. It takes a pair and two lists of stored policy ids, and the
// Controller assigns the indices:
//
//	after_predefined_ids   ->  40000
//	before_predefined_ids  ->  10000, then 10001, …
//
// Which is the whole answer: `before` is where a create lands on its own, `after`
// is below the tier the companions are generated into, and 40000 is the
// Controller's own number for it rather than one unifig picked.
//
// **The reorder is a total statement about a pair.** Naming one of two stored
// policies on a pair answered
// `400 api.err.ShouldIncludeFirewallPolicyInBatchUpdate`; naming both answered
// 200 and placed them as asked. So unifig cannot move one policy without saying
// where every stored policy on that pair goes — including the ones the config
// does not manage. What it does with those is keep them exactly where they are
// (see reorderedPair), which is ADR-0004's rule reaching a place ADR-0004 did not
// anticipate: the file states what unifig manages, and an endpoint that will only
// take the whole list is not a reason to start managing the rest.
const (
	// beforePredefined and afterPredefined are the two sides of the Controller's
	// generated tier, named as the endpoint names them.
	beforePredefined = "before the return rules"
	afterPredefined  = "after the return rules"

	// companionPolicyIndex is where the Controller generates the companion
	// return rule for a policy unifig created: 30000, read off the router in
	// ADR-0026's write session and again on both companions in issue #54's
	// reading. It is the boundary between the two sides, and it is read rather
	// than assumed for one reason — the after side is 40000 and the before side
	// 10000, so any split between them would do, and this is the one that says
	// *why* there are two sides at all.
	companionPolicyIndex = 30000
)

// placedAfterPredefined is whether a live policy already sits below the tier the
// Controller generates companions into.
//
// This is unifig reading `index`, which ADR-0018 said it did not do, and the
// narrowing is deliberate and small: it reads the field to know which side of one
// boundary a policy is on, and still evaluates no rule set and still decides
// nothing from the indices of policies relative to each other (ADR-0033).
func placedAfterPredefined(policy unifi.FirewallZonePolicy) bool {
	return policy.Index > companionPolicyIndex
}

// belongsAfterPredefined is whether the config wants this policy below the
// companion tier: the verdicts that close a path, and no others.
//
// A companion is always an `ALLOW`, and always on the *reverse* pair, so only a
// policy that blocks can stop one being reached. An `allow` is left where the
// Controller puts it, and that is not symmetry for its own sake — the generated
// tier is not all return rules. `Isolated Networks` is an enabled `BLOCK ALL` at
// 30000 on `Internal -> Internal`, `Internal -> Hotspot` and `Internal -> Dmz`,
// with `Block Invalid Traffic`, `Post-Authorization Restrictions` and `Block
// Unauthorized Traffic` in the same band. An `allow` moved below those would be a
// policy the file states plainly and the firewall does not have, which is issue
// #54's own defect pointing the other way. There is no third position that
// escapes both, because the companion and `Isolated Networks` are at the *same
// index*: each verdict is placed by what it needs in order to mean what it says.
func belongsAfterPredefined(desired config.FirewallPolicy) bool {
	return blockingActions[desired.Action]
}

// zonePair is the pair of Zones a policy governs, by the names the config uses.
// It is policyKey without the name, because placement is a property of the pair
// rather than of one policy on it.
type zonePair struct{ source, destination string }

// pairsCarryingCompanions is every pair a Return Rule sits on, which is every
// pair where a policy above the companion tier can do the harm issue #54 measured.
//
// **This is what keeps the blast radius honest.** A blocking policy only outranks
// a companion where there *is* a companion: a block on `Internal -> External` has
// no reply traffic to strand, and moving it would be unifig reordering a firewall
// for no reason at all. Before this gate, a brownfield `export` produced a config
// whose first `plan` proposed moving every blocking policy on the site — which is
// a lot of change to justify with a bug that reaches almost none of them.
//
// It is read from two places because a companion can arrive from two, and both
// are known before anything is written:
//
//   - a live policy that is a Return Rule, which is the companion of an allow
//     somebody already made, unifig's or the Controller's own; and
//   - an allow this very config states on the *reverse* pair, which is where the
//     Controller will put its companion (ADR-0022). Reading the config rather than
//     waiting for the write is what makes one apply enough: a block and the allow
//     whose companion it would outrank can be created in either order, and the
//     order is whatever the names happened to sort to.
func pairsCarryingCompanions(
	cfg config.Config,
	live []unifi.FirewallZonePolicy,
	facts zoneFacts,
	bound bindings,
) map[zonePair]bool {
	carrying := map[zonePair]bool{}
	for _, policy := range live {
		if !returnRule(policy) {
			continue
		}
		source, destination := bound.zoneName(policy.Source.ZoneID), bound.zoneName(policy.Destination.ZoneID)
		if source == "" || destination == "" {
			continue
		}
		carrying[zonePair{source: source, destination: destination}] = true
	}
	for _, desired := range cfg.FirewallPolicies {
		if !asksForReturnRule(desired, facts) {
			continue
		}
		// The companion is generated on the reverse pair, which is the pair a
		// block would be on.
		carrying[zonePair{source: desired.Destination, destination: desired.Source}] = true
	}
	return carrying
}

// placementField is the plan line for a policy on the wrong side of the
// companion tier, and nothing at all for one already on the right side.
//
// It is a field of the policy's change rather than a change of its own, on the
// same reasoning as the return-rule field: the operator wrote one policy and what
// they need to read is everything about to happen to it. It is also what makes
// this converge, which the create-time-only version did not — every site that had
// this bug when it was found has its policies already created, so a fix that only
// ran on a create would have fixed nobody's firewall including the one it was
// reported from.
func placementField(
	desired config.FirewallPolicy,
	live unifi.FirewallZonePolicy,
	carrying map[zonePair]bool,
) (Field, bool) {
	if !carrying[zonePair{source: desired.Source, destination: desired.Destination}] {
		return Field{}, false
	}
	// A Generated Policy is not one of these, and the exemption is the same one
	// it has everywhere else: it has no document handle, so the reorder endpoint
	// has nothing to name it by, and it is what the two lists are named
	// *relative to* rather than a thing in them (ADR-0027, ADR-0028). Without
	// this every generated policy reads as misplaced, because the Controller's
	// own sit at 30000 and 2147483647 — which is above the boundary by
	// construction.
	if generated(live) {
		return Field{}, false
	}
	want := belongsAfterPredefined(desired)
	if want == placedAfterPredefined(live) {
		return Field{}, false
	}
	from, to := beforePredefined, afterPredefined
	if !want {
		from, to = afterPredefined, beforePredefined
	}
	return Field{Name: "placement", From: from, To: to}, true
}

// placementNote is what a create says about where the policy will end up, for
// the same reason returnRuleNote says the companion is coming: unifig is about to
// do something the config does not state and the plan may not be silent about it.
func placementNote(desired config.FirewallPolicy, carrying map[zonePair]bool) []string {
	if !belongsAfterPredefined(desired) {
		return nil
	}
	if !carrying[zonePair{source: desired.Source, destination: desired.Destination}] {
		return nil
	}
	return []string{
		"the Controller creates this above the return rules it generates, so unifig will move it below them; " +
			"a policy above one drops the reply to traffic an allow permits",
	}
}

// reorderPair puts every stored policy on one pair on the side it belongs, and
// is what both the create and the update call once the policy itself is written.
//
// It reads the pair at the moment it runs rather than at plan time, for the
// reason every write here does: a policy created earlier in this same apply is
// one this pair now holds. That read is the price of an endpoint that will not
// take a partial list.
func reorderPair(
	ctx context.Context,
	client unifi.Client,
	site string,
	cfg config.Config,
	source, destination string,
	bound bindings,
) error {
	sourceID, err := bound.zoneID(source)
	if err != nil {
		return err
	}
	destinationID, err := bound.zoneID(destination)
	if err != nil {
		return err
	}

	live, err := listFirewallPolicies(ctx, client, site)
	if err != nil {
		return err
	}
	before, after := reorderedPair(cfg, live, sourceID, destinationID, bound)
	if len(before)+len(after) == 0 {
		return nil
	}
	// An empty list goes as `[]` and never as `null`. Neither field is
	// `omitempty` in the DTO, so a nil slice marshals to `null` — and a body
	// carrying one answers 500 with a Tomcat error page rather than a message,
	// measured against the live UDR on a pair whose only stored policy was going
	// to the other side. That is the commonest shape there is: a pair with one
	// policy on it puts one list at nothing.
	if before == nil {
		before = []string{}
	}
	if after == nil {
		after = []string{}
	}
	_, err = client.ReorderFirewallPolicies(ctx, site, &unifi.FirewallPolicyOrderUpdate{
		SourceZoneId:        sourceID,
		DestinationZoneId:   destinationID,
		BeforePredefinedIds: before,
		AfterPredefinedIds:  after,
	})
	return err
}

// reorderedPair is the two lists the reorder endpoint wants: every stored policy
// on this pair, on the side it belongs, in the order it is in now.
//
// **A policy the config does not name keeps the side it is on.** That is the
// whole of what stops an endpoint requiring the complete list from turning into
// unifig managing the complete list (ADR-0004). Only a policy the file states
// moves, and it moves because its verdict says where it has to be.
//
// Generated Policies are left out entirely rather than sorted: they are what the
// two lists are named *relative to*, and they have no document handle to name
// anyway (ADR-0027, ADR-0028). A companion is one of those twice over.
//
// The order within each list is the order the Controller already has, because a
// list is a statement about relative order and unifig has nothing to say about
// the relative order of two policies the operator wrote. That is issue #54's
// option 1, still declined for ADR-0004's reason.
func reorderedPair(
	cfg config.Config,
	live []unifi.FirewallZonePolicy,
	sourceID, destinationID string,
	bound bindings,
) (before, after []string) {
	managed := make(map[policyKey]config.FirewallPolicy, len(cfg.FirewallPolicies))
	for _, desired := range cfg.FirewallPolicies {
		managed[keyOfDesiredPolicy(desired)] = desired
	}

	onThePair := make([]unifi.FirewallZonePolicy, 0, len(live))
	for _, policy := range live {
		if policy.Source.ZoneID != sourceID || policy.Destination.ZoneID != destinationID {
			continue
		}
		if generated(policy) {
			continue
		}
		onThePair = append(onThePair, policy)
	}
	slices.SortStableFunc(onThePair, func(a, b unifi.FirewallZonePolicy) int {
		return a.Index - b.Index
	})

	for _, policy := range onThePair {
		side := placedAfterPredefined(policy)
		if key, keyed := keyOfLivePolicy(policy, bound); keyed {
			if desired, named := managed[key]; named {
				side = belongsAfterPredefined(desired)
			}
		}
		if side {
			after = append(after, policy.ID)
		} else {
			before = append(before, policy.ID)
		}
	}
	return before, after
}

// createFirewallPolicy is the Change for a policy the Controller does not have.
//
// A policy created blocking the gateway is Risky for the same reason an existing
// one turned that way is, and more directly: the Controller's own predefined
// allow sits at the lowest precedence there is, so a policy written over it is
// one that takes effect.
func createFirewallPolicy(
	desired config.FirewallPolicy,
	cfg config.Config,
	facts zoneFacts,
	bound bindings,
	carrying map[zonePair]bool,
) Change {
	risk := ""
	if closesTheGateway(config.FirewallPolicy{}, desired, facts) {
		risk = gatewayRisk
	}
	return Change{
		Action: Create,
		Kind:   FirewallPolicy,
		Name:   desired.Name,
		Fields: setPolicyFields(desired, facts, carrying),
		Risk:   risk,
		write: func(ctx context.Context, client unifi.Client, site string) error {
			// Read at the moment of writing rather than the moment of planning:
			// either zone may have been created by a change earlier in this very
			// apply.
			policy := newFirewallPolicy(desired, facts)
			if err := overwriteManagedPolicy(&policy, desired, bound); err != nil {
				return err
			}
			if _, err := client.CreateFirewallZonePolicy(ctx, site, &policy); err != nil {
				return err
			}
			// The Controller puts a created policy above the tier it generates
			// companions into, and `index` is not a field a client may set
			// (ADR-0033), so the position is asked for afterwards or not at all.
			//
			// A failure here leaves the policy created and misplaced, and is
			// returned rather than swallowed. That is what makes it recoverable:
			// apply stops, says what did and did not happen, and the next plan
			// sees a live policy on the wrong side and proposes the placement as
			// a change of its own. A notice instead of an error would leave a
			// firewall that does not mean what the file says and nothing to make
			// it converge.
			if len(placementNote(desired, carrying)) == 0 {
				return nil
			}
			return reorderPair(ctx, client, site, cfg, desired.Source, desired.Destination, bound)
		},
	}
}

// updateFirewallPolicy is the Change that brings a live policy in line with the
// config, and whether there is one to make at all.
func updateFirewallPolicy(
	desired config.FirewallPolicy,
	live unifi.FirewallZonePolicy,
	cfg config.Config,
	facts zoneFacts,
	bound bindings,
	held policyNameSet,
	carrying map[zonePair]bool,
) (Change, bool) {
	current, _ := fromLivePolicy(live, bound)

	// Whether the site is holding the companion this policy would have. It is
	// asked of the live collection rather than of the policy's own
	// `create_allow_respond`, for the reason returnRuleField gives: the flag is
	// the request, and the companion is what an operator has.
	fields := changedPolicyFields(current, desired, facts, live.CreateAllowRespond, held[returnRuleName(live.Name)])
	// Where the policy sits is the one field here that is not written through the
	// policy endpoint at all, and the one that can be the only thing differing —
	// which is exactly the case issue #54 was reported from, where `plan` said
	// "No changes" about a firewall that was dropping replies.
	placement, misplaced := placementField(desired, live, carrying)
	if misplaced {
		fields = append(fields, placement)
	}
	if len(fields) == 0 {
		return Change{}, false
	}

	// The mark's subject used to be the Controller's own `Allow All Traffic` from
	// Internal to Gateway, "a one-line edit away from being the rule that locks
	// the operator out" — which was the leading argument in ADR-0018 for the
	// firewall carrying a risk at all. That policy is a Generated Policy and the
	// edit cannot be made: its `_id` is a composite the write endpoint answers 404
	// to, measured on the live migrated UDR (ADR-0027, issue #41). This still runs
	// for one — the caller asks whether there is a difference before it asks
	// whether the policy can be written, because a caveat is only worth saying
	// where something differs — and the Change it marks is then dropped rather
	// than planned. The mark an operator sees for that policy is the one in the
	// caveat's way out, which is where the danger actually is.
	//
	// What the mark now guards is every change that can still happen on that pair,
	// and ADR-0018's other argument is the one carrying it: a policy the operator
	// *creates* over the Controller's own takes effect, because the Controller's
	// sits at `index: 2147483647`, the lowest precedence there is. So the lockout
	// is still one line of config — it is a `+` rather than a `~`, and an update
	// to a policy unifig made is the same rule a second time.
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
			if err := mergeIntoStoredPolicy(ctx, client, site, live.ID, desired, facts, bound); err != nil {
				return err
			}
			// The placement goes through a different endpoint, so it is a second
			// request or none. It runs only where the plan said it would, because
			// a reorder is a statement about every stored policy on the pair and
			// making one nobody asked for is how an operator's own ordering
			// quietly stops being theirs (ADR-0033).
			if !misplaced {
				return nil
			}
			return reorderPair(ctx, client, site, cfg, desired.Source, desired.Destination, bound)
		},
	}, true
}

// pruneFirewallPolicies is the Changes that would delete every live policy the
// config does not name, and the policies it leaves on the Controller.
//
// What survives is sparedFromPrune's, and the reasons live there because there
// are five different questions in them. An exempt policy is spared rather than
// skipped, because what makes a policy hold a zone back is that it will still be
// governing it afterwards, whatever the reason it survived (ADR-0014).
//
// Spared is not the same list as holds-a-zone-back, and `predefined` is where the
// two part company: the Controller deletes its own generated policies along with
// the zone, so one of them is exempt here and counts for nothing in `zonesInUse`
// (ADR-0019, issue #28). That is a sixth question and it is left exactly where it
// was, on the marker (ADR-0028): what hardware measured is the
// Controller reclaiming the policies it marks as its own along with the zone, and
// neither of the clauses beside the marker measured anything.
//
// So on every policy anyone has read, nothing about those clauses changes which
// zones a plan holds back. On the firmware nobody has met — a generated policy
// carrying no marker, or a companion carrying none — a policy they newly spare
// does hold its zones back, because `zonesInUse` reads `predefined` and that
// policy has none. That is the cautious direction and it is worth stating rather
// than glossing: the operator is told which policy kept the zone and why, which
// is a hold-back with a reason rather than a deletion nobody could have shown
// them first. Whoever measures that firmware settles it, in `zonesInUse` and with
// the reclaim in hand.
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
		if sparedFromPrune(policy, named, bound) {
			spared = append(spared, policy)
			continue
		}
		changes = append(changes, deleteFirewallPolicy(policy, bound))
	}
	return changes, spared
}

// sparedFromPrune is whether prune leaves a live policy where it is.
//
// It is a predicate of its own because the six conditions below are five
// different questions rather than one question asked six ways, and a reader of
// the expression cannot tell which is which. They are listed here in the order
// they are asked.
//
// **Is it unifig's to delete at all?** A policy with no key is spared: unifig
// cannot describe it, so it was never prune's, and it governs zones unifig cannot
// name so it holds nothing back either.
//
// **Does the config say to keep it?** A policy the file names is a policy the
// operator asked for, which is the whole of what prune is.
//
// **Whose object is it?** A policy the Controller ships is exempt on its own
// marker, like every other built-in (ADR-0005). A policy's marker is
// `predefined`, which is how the zone-based firewall marks the default policies
// it generates for a pair of zones — and the eighty-six of those a migrated
// router ships are exactly what prune must not touch. `NoDelete` is checked
// beside it because the library models the field on this type, not because a
// policy has been seen carrying it: the marker is per Resource and only a network
// is known to use that one, so nothing here should be read as saying which field
// a new type would use.
//
// **Is there an object to address?** `generated` reads the `_id` rather than the
// marker, and it is a different question rather than the same one said twice
// (ADR-0027, ADR-0028). A deletion needs an id to send exactly as an update does,
// so a deletion of a Generated Policy is a promise a plan may not make —
// ADR-0014's rule arriving in one more place rather than a new rule.
//
// **Is it a Resource?** `returnRule` reads `connection_state_type`, the same
// predicate export excludes a companion on, and it is a fifth question again. A
// Return Rule is not an object unifig has any standing over: it never creates,
// names or deletes one, and the config states its arrival and departure as a
// field of its parent's change (ADR-0026). So prune has no more business deleting
// one than export has writing one, and the two halves read the same field so they
// cannot drift apart.
//
// **The last two clauses are inert on every policy anyone has measured, and this
// says so rather than implying otherwise.** All eighty-six a migrated router
// ships are `predefined: true` *and* carry a composite id, the twelve `Allow
// Return Traffic` companions among them, so the marker already spares every one;
// the one custom policy ADR-0027's probe created was `predefined: false` with a
// document handle. The companion ADR-0026's write session watched the Controller
// generate for a *custom* parent was measured on its id alone — a composite —
// which `generated` spares whatever its marker turned out to be. Nothing
// observable changes on any router unifig has seen.
//
// **What changed is that the exported file stopped covering the disagreement.**
// The file was the backstop: an export named all eighty-six and wrote the
// companion beside any allow policy of the operator's own, so `named` spared them
// whatever the markers said. Export leaves a Generated Policy out as of issue #45
// and a Return Rule with it, and from there export's tests and these are halves
// of one guarantee: a policy export omits is a policy prune must not delete. Were
// they to read different fields, a firmware that generated a policy without
// marking it `predefined` would have unifig writing a file that omits the policy
// and then proposing to delete it. **A file unifig wrote must not be a file prune
// deletes from.**
//
// The companion's version of that is worse than the Generated Policy's rather
// than milder. A generated policy at least fails loudly, 404 on an id that was
// never a handle; an unmarked companion may well carry a real handle and delete
// cleanly — and then its parent still carries `create_allow_respond: true`, so
// the very next plan proposes putting the companion back as a `return-rule` field
// change (ADR-0026). Prune deletes it, apply restores it, and nothing in either
// plan says the two are about the same object. That is the drift issue #40 was
// filed about, generated on a loop by unifig itself.
//
// Each is a clause rather than a replacement for anything, because the claims are
// all true and they answer different questions. Collapsing the marker into the
// `_id` test is the confusion ADR-0027 exists to prevent, and it would quietly
// narrow the exemption to lose a policy an operator wrote and the Controller
// marked its own.
func sparedFromPrune(policy unifi.FirewallZonePolicy, named map[policyKey]bool, bound bindings) bool {
	key, keyed := keyOfLivePolicy(policy, bound)
	return !keyed ||
		named[key] ||
		policy.NoDelete || policy.Predefined ||
		generated(policy) ||
		returnRule(policy)
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
func setPolicyFields(desired config.FirewallPolicy, facts zoneFacts, carrying map[zonePair]bool) []Field {
	fields := []Field{
		{Name: "action", To: desired.Action, Notes: append(returnRuleNote(desired, facts), placementNote(desired, carrying)...)},
		{Name: "source", To: desired.Source},
		{Name: "destination", To: desired.Destination},
		// The fourth is always listed and the fifth only sometimes, which is the
		// same rule as the first three rather than an exception to it. A create
		// really does set a protocol — `all`, where the file states none — so
		// there is always something to print. Ports are the part a create can
		// genuinely leave alone, and `protocol: all` already says there are
		// none, so a second line saying it again is a line that teaches an
		// operator to skim.
		{Name: "protocol", To: createdProtocol(desired)},
	}
	if ports := portList(desired.Ports); ports != nil {
		fields = append(fields, Field{Name: "ports", To: ports})
	}
	annotateWideGatewayBlock(fields, config.FirewallPolicy{}, desired, createdProtocol(desired), facts)
	return fields
}

// createdProtocol is the protocol a create actually writes, which is `all`
// wherever the file states none — newFirewallPolicy's own default, and the
// reason the plan can print a value for a field the config left empty.
func createdProtocol(desired config.FirewallPolicy) string {
	if desired.Protocol == "" {
		return "all"
	}
	return desired.Protocol
}

// portList renders a narrowing's ports as one field value, and nothing at all
// where there are none — so clearing one reads as `443, 80 -> (none)` rather
// than as `443, 80 -> ""`.
//
// It is nameList's shape without nameList's quotes: a port is a number, and
// quoting it would make `"443"` look like a string the Controller stores rather
// than the port it is. The separator is the plan's, not the wire's — what goes
// to the Controller is joinPorts, with no spaces.
func portList(ports []config.Port) any {
	if len(ports) == 0 {
		return nil
	}
	parts := make([]string, 0, len(ports))
	for _, port := range ports {
		parts = append(parts, string(port))
	}
	return strings.Join(parts, ", ")
}

// annotateWideGatewayBlock says what a blocking policy with no narrowing costs
// when it points at the zone the Controller answers in, which is more than the
// operator asking for it usually means.
//
// It is here because of what a custom Zone's path to the Controller is made of,
// read off the live migrated UDR: exactly two Generated Policies, `Allow mDNS`
// at index 30000 and `Allow All Traffic` at 2147483647. There is no generated
// `Allow DNS` and no `Allow DHCP` for a custom Zone — only the Hotspot zone
// ships those — so leases, name resolution and time all ride that catch-all, and
// a policy unifig creates outranks it. "Keep the IoT VLAN off the admin page"
// written without a narrowing does not keep it off the admin page. It keeps it
// off the network.
//
// **That reasoning used to say "above every one of them" and now holds only where
// the policy stays put.** A blocking policy on a pair carrying a Return Rule is
// reordered to 40000 (ADR-0033), and there it sits below `Allow mDNS` at 30000.
// The note is unchanged either way, because what it is about is unchanged: the
// services a custom Zone loses are the ones riding the catch-all `Allow All
// Traffic` at 2147483647, which 40000 still outranks.
//
// The wording hedges for ADR-0018's reason and stops where ADR-0018 stops. unifig
// reads no `index` and no `enabled` and will not evaluate a rule set, so this is
// a statement about what the policy says rather than a verdict on what the
// firewall will do — the same line ADR-0018 drew for the Risky mark, and the
// same one issue #1 drew when it put lockout analysis out of scope.
//
// It fires only where the change is the one bringing the block about: a create,
// a verdict turning blocking, or a protocol widening back to `all`. A policy
// that was already a wide gateway block and is having its return rule corrected
// has not newly cost anybody their DHCP, and a note on that change is one an
// operator learns to read past (ADR-0012).
func annotateWideGatewayBlock(fields []Field, from, to config.FirewallPolicy, effective string, facts zoneFacts) {
	if effective != "all" || !blockingActions[to.Action] {
		return
	}
	if !facts.known || facts.gateway == "" || to.Destination != facts.gateway {
		return
	}
	// Newly blocking, or newly wide: a verdict turning to block or reject brings
	// the block about, and a protocol widening back to `all` takes the ports off
	// one that was already there.
	brings := becomesBlocking(from, to) || (to.Protocol == "all" && from.Protocol != "all")
	if !brings {
		return
	}
	annotateFirst(fields,
		"this states no ports, so it blocks every service the Controller offers this zone — "+
			"DHCP leases and DNS as well as the management UI",
		"protocol", "action")
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
// It asks asksForReturnRule rather than the verdict, because the verdict is only
// half of what decides. The Controller refuses the request outright on a policy
// that blocks, so there is no second policy to announce; it refuses it just the
// same on an allow into the External zone (ADR-0030), where the note would be
// promising an operator a policy that is never going to appear. Both are cases
// where saying nothing is the true statement, and ADR-0014's standard — a plan
// is a statement about what will happen — is why the note has to know about the
// second one rather than only the create.
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
func returnRuleNote(desired config.FirewallPolicy, facts zoneFacts) []string {
	if !asksForReturnRule(desired, facts) {
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
// eighty-six policies a migrated router ships are `ALLOW` carrying the flag
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
func returnRuleName(parent string) string { return parent + returnRuleSuffix }

// parentOfReturnRule is that rule read backwards: the policy a companion by this
// name would belong to, and whether the name is one the Controller would have
// built that way at all.
//
// It shares the suffix with returnRuleName rather than spelling it again, for
// the reason that function is one function: a name unifig builds and a name it
// takes apart disagreeing about the suffix is unifig failing to recognise its
// own companion.
//
// The Controller's own companions are not named this way — the twelve a migrated
// router ships are `Allow Return Traffic` — so this answers false for those,
// which is correct rather than a miss: they belong to generated policies, not to
// anything unifig wrote.
func parentOfReturnRule(name string) (string, bool) {
	return strings.CutSuffix(name, returnRuleSuffix)
}

const returnRuleSuffix = " (Return)"

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
func returnRuleField(
	desired config.FirewallPolicy,
	facts zoneFacts,
	requested, companionHeld bool,
) (Field, bool) {
	want := asksForReturnRule(desired, facts)
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
// The first three are compared unconditionally, which is the opposite of how an
// optional field is treated everywhere else — and it is the same rule
// underneath. Omission means unmanaged, and the schema lets none of the three be
// omitted, so a policy in the config always states its verdict and both ends.
//
// The narrowing is where that stops being true and the ordinary rule shows
// through. `protocol` and `ports` may be omitted, so they are compared only when
// stated: a policy narrowed in the Controller's UI keeps its narrowing against a
// file that says nothing about ports, and reads as no change rather than as a
// widening nobody asked for (ADR-0004, ADR-0031).
func changedPolicyFields(
	current, desired config.FirewallPolicy,
	facts zoneFacts,
	requested, companionHeld bool,
) []Field {
	fields := make([]Field, 0, 6)
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
	// The narrowing, which is two fields and one decision: `narrowingOf` is what
	// says whether the file asked about the protocol, about the ports, or about
	// neither, and the plan lists exactly what it asked about. Stating a
	// protocol with no ports is a protocol change; stating one that has no ports
	// is that *and* the ports going, which is the only way the file has to widen
	// a policy again and so has to be visible as its own line.
	if want := narrowingOf(desired); want.manages() {
		if current.Protocol != desired.Protocol {
			fields = append(fields,
				Field{Name: "protocol", From: text(current.Protocol), To: desired.Protocol})
		}
		if want.managesPorts() && joinPorts(current.Ports) != want.ports {
			fields = append(fields,
				Field{Name: "ports", From: portList(current.Ports), To: portList(desired.Ports)})
		}
	}
	// The last field, and the only one not read off a line of the config: the
	// verdict decides whether a companion should be there, and the Controller
	// decides whether one is. An update runs when they disagree, which is what
	// makes a policy left at `allow` without a companion something unifig can
	// put right rather than only describe (ADR-0026).
	if field, differs := returnRuleField(desired, facts, requested, companionHeld); differs {
		fields = append(fields, field)
	}
	annotateWideGatewayBlock(fields, current, desired, effectiveProtocol(current, desired), facts)
	return fields
}

// effectiveProtocol is the protocol the policy will be on after this update:
// the one the file states, or the one the Controller already has where the file
// states none. Without the second half, a file that narrows nothing would look
// like a file that widens everything.
func effectiveProtocol(current, desired config.FirewallPolicy) string {
	if desired.Protocol == "" {
		return current.Protocol
	}
	return desired.Protocol
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
	applyNarrowing(policy, narrowingOf(desired))
	return nil
}

// applyNarrowing writes a config entry's narrowing onto the struct a create
// sends. It is the create half of the pair; storedPolicy.setNarrowing is the
// update half, and the two differ only in what they are writing onto.
//
// A create has no live narrowing to preserve, so "unmanaged" here means leaving
// newFirewallPolicy's own defaults — `all` and ANY — exactly where they are, and
// clearing is a no-op onto them rather than a separate case. It is still written
// out, because the two halves are one rule and a rule kept in one half is a rule
// that drifts.
func applyNarrowing(policy *unifi.FirewallZonePolicy, want narrowing) {
	if !want.manages() {
		return
	}
	policy.Protocol = want.protocol
	switch {
	case want.clears:
		policy.Destination.PortMatchingType = anyPorts
		policy.Destination.Port = ""
	case want.ports != "":
		policy.Destination.PortMatchingType = specificPorts
		policy.Destination.Port = want.ports
	}
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
// eighty-six policies a migrated router ships, `icmp_typename` and
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
	facts zoneFacts,
	bound bindings,
) error {
	stored, err := readStoredPolicy(ctx, client, site, id)
	if err != nil {
		return err
	}
	stored.dropMarkers()
	if err := stored.overwriteManaged(desired, facts, bound); err != nil {
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
// What that costs is one read of all eighty-six policies per policy updated,
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
func (p storedPolicy) overwriteManaged(desired config.FirewallPolicy, facts zoneFacts, bound bindings) error {
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
	if err := p.setNarrowing(desired); err != nil {
		return err
	}
	return p.setReturnRuleRequest(desired, facts)
}

// setNarrowing writes the config's narrowing onto the policy the Controller
// sent. It runs after setZone, because both read-modify-write the destination
// end and this one has to see the zone the other just put there.
//
// The clearing branch **deletes** the `port` key rather than emptying it, and
// that is a measurement rather than a preference: a PUT carrying
// `port_matching_type: ANY` with the key gone was read back on the live migrated
// UDR with no port at all, on 25 August 2026 (ADR-0031). Under a v2 policy PUT,
// which replaces (ADR-0021), a key that is not in the body is a key that is not
// in the object — which is exactly the property that makes removal expressible
// here and inexpressible on the zone endpoint, where a body may only carry three
// fields (ADR-0024).
func (p storedPolicy) setNarrowing(desired config.FirewallPolicy) error {
	want := narrowingOf(desired)
	if !want.manages() {
		return nil
	}
	if err := p.set("protocol", want.protocol); err != nil {
		return err
	}
	if !want.managesPorts() {
		return nil
	}

	fields := storedPolicy{}
	if sent, held := p["destination"]; held {
		if err := json.Unmarshal(sent, &fields); err != nil {
			return fmt.Errorf("the destination end of the policy the Controller sent is not an object: %w", err)
		}
	}
	if want.clears {
		if err := fields.set("port_matching_type", anyPorts); err != nil {
			return err
		}
		delete(fields, "port")
	} else {
		if err := fields.set("port_matching_type", specificPorts); err != nil {
			return err
		}
		if err := fields.set("port", want.ports); err != nil {
			return err
		}
	}
	return p.set("destination", fields)
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
func (p storedPolicy) setReturnRuleRequest(desired config.FirewallPolicy, facts zoneFacts) error {
	return p.set("create_allow_respond", asksForReturnRule(desired, facts))
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
// policy been seen carrying a marker — none of the eighty-six a migrated
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
// of the eighty-six policies a migrated router ships, sixty-six are
// `protocol: all` and sixty-four are `ip_version: BOTH`, because they are
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
// all eighty-six. `predefined: false` differs from all eighty-six and should:
// a policy unifig made is not one the Controller ships. `create_allow_respond`
// was neither, which is the whole of issue #36 — and it is the one value here
// that is not fixed, because the Controller refuses it on any verdict but
// `allow`. That is why this takes the policy it is building rather than nothing.
//
// They apply on create only. An operator who afterwards narrows the policy to a
// port, a client or an evening keeps that forever, because an update merges into
// the object the Controller sent and writes only the same four values
// (mergeIntoStoredPolicy, ADR-0021).
func newFirewallPolicy(desired config.FirewallPolicy, facts zoneFacts) unifi.FirewallZonePolicy {
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
		// Whether it is asked for at all is asksForReturnRule's, and there are two
		// conditions rather than one. A create of a `block` policy carrying it
		// true is a 400, `Firewall policy create respond traffic not allowed`,
		// which fails the whole apply — measured in the same session, and the
		// reason `newFirewallPolicy` had to learn what verdict it is building for.
		// So is a create of an `allow` policy whose destination is the External
		// zone, with the identical message and a week later (ADR-0030) — which is
		// the reason it had to learn the ends too, and why this takes the facts.
		CreateAllowRespond: asksForReturnRule(desired, facts),
		// The Controller rejects a policy with no schedule outright, so this is
		// less a default than a field with one permitted value at creation. It
		// is not parity either: all eighty-six policies the recording holds
		// carry `{mode: ALWAYS}` and no `time_all_day` at all. Sending one is
		// inventing a field on the object, which is a loss on the update path
		// (ADR-0021) and nothing on this one, where there is no object yet and
		// no operator's value to write over.
		Schedule: unifi.FirewallZonePolicySchedule{Mode: "ALWAYS", TimeAllDay: true},
		Source: unifi.FirewallZonePolicySource{
			MatchingTarget:   anyMatch,
			PortMatchingType: anyPorts,
		},
		Destination: unifi.FirewallZonePolicyDestination{
			MatchingTarget:   anyMatch,
			PortMatchingType: anyPorts,
		},
	}
}

// quietWideNarrowing drops a `protocol: all` that narrows nothing from an
// exported entry.
//
// `all` is what 126 of the 135 policies on a migrated router carry, and it is
// the Controller's word for "no narrowing" — so writing it into every entry
// would be 126 lines saying nothing, which is how an operator learns to skim a
// file. Leaving it out says the same thing in ADR-0004's terms: an entry stating
// no protocol manages none, which is exactly true of a policy that has no
// narrowing to manage.
//
// It is export's and not fromLivePolicy's, because the planner needs the
// opposite. A file stating `protocol: all` against a policy already on `all`
// has to plan as no change, and it can only do that if the value it compares
// against is the live one.
func quietWideNarrowing(described config.FirewallPolicy) config.FirewallPolicy {
	if described.Protocol == "all" && len(described.Ports) == 0 {
		described.Protocol = ""
	}
	return described
}

// quietInvertedMatch drops from an exported entry a narrowing the Controller has
// inverted: `match_opposite_ports` on the destination, `match_opposite_protocol`
// on the policy.
//
// The value beside one of those flags names the traffic the policy does *not*
// match, so `ports: [53]` written off an inverted policy would be the file
// saying the opposite of what the Controller holds — worse than the omission
// this whole notice is about, and the one shape where writing the narrowing is
// less honest than leaving it out. Left out, the entry states no narrowing,
// which manages none, which is true (ADR-0004).
//
// **The planner is deliberately left as it was**, which is why this is export's
// and not statedPorts'. A file that states the inverted value plans as no change
// today, and would plan as a perpetual update if the projection dropped it for
// both readers, because an update writing the ports back leaves the inversion
// where it is. Neither is a decision this notice needed to make, and issue #52
// scoped the plan side out by name.
func quietInvertedMatch(described config.FirewallPolicy, live unifi.FirewallZonePolicy) config.FirewallPolicy {
	if live.Destination.MatchOppositePorts {
		described.Ports = nil
	}
	if live.MatchOppositeProtocol {
		described.Protocol = ""
		described.Ports = nil
	}
	return described
}
