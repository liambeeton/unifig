package reconcile

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/filipowm/go-unifi/v2/unifi"

	"github.com/liambeeton/unifig/internal/config"
)

// WAN slots are the first Setting unifig manages, and everything in this file
// follows from that one word. A Setting is a fixed slot: the Controller has the
// slots it has, so there is no create here, no delete, and nothing for prune to
// reach. The only verb is update, and the config's role is to say what a slot
// unifig was given should look like — never which slots should exist.
//
// They live in the same networkconf collection as the LANs and are told apart
// by purpose, which is why the collection is read once in ComputePlan and split
// two ways rather than being fetched by each half separately.
//
// The other thing that makes WAN different is what a mistake costs. Every
// change here carries wanRisk, and that is what makes apply stop and ask about
// it on its own even when the operator already approved the plan.

// wanRisk is what an operator stands to lose by applying a WAN change, said
// once and in the same words wherever it appears: the plan prints it, the JSON
// carries it, and apply asks it back as a question.
//
// It names the consequence rather than the field, because "wan_type is
// changing" is not something an operator can weigh at eleven at night and "this
// site loses its internet connection" is.
const wanRisk = "this is the site's internet uplink, and changing it can cut the connection until the new settings work"

// wanPurpose is the networkconf purpose that marks an entry as a WAN slot
// rather than one of the LANs unifig manages as networks.
const wanPurpose = "wan"

// wanTypes are the connection types unifig models. The Controller has more —
// static addressing, DS-Lite, MAP-E — and each of those needs fields unifig's
// config has no way to state, so a slot configured that way is one unifig
// describes by slot alone and manages nothing about (see projectWANSlots).
var wanTypes = map[string]bool{
	"dhcp":     true,
	"pppoe":    true,
	"disabled": true,
}

// planWANSlots is the WAN half of a reconcile. Its caller only reaches it when
// the config has a `wan:` section at all, so a file that says nothing about the
// uplinks leaves them entirely alone.
//
// Every change it can return is an update. A slot the config names and the
// Controller does not have is an error rather than a create, because there is
// no such thing as making one: the slots are the hardware's, and a config
// naming a slot this Controller lacks is an operator who has misread their
// router, not one asking for a new uplink.
func planWANSlots(cfg config.Config, all []unifi.Network) ([]Change, error) {
	live, err := liveWANSlots(all)
	if err != nil {
		return nil, err
	}

	changes := make([]Change, 0, len(cfg.WAN))
	for _, desired := range cfg.WAN {
		current, exists := live[desired.Slot]
		if !exists {
			return nil, noSuchSlot(desired.Slot, live)
		}
		if change, differs := updateWANSlot(desired, current); differs {
			changes = append(changes, change)
		}
	}
	return changes, nil
}

// liveWANSlots indexes the site's WAN slots by the slot each occupies —
// wan_networkgroup, the Controller's own name for which uplink an entry is.
//
// The slot is the natural key, and deliberately not the entry's name: an
// operator can rename "WAN" to "Fibre" in the Controller's UI, and it is still
// the primary uplink. That is the difference between a Setting and a Resource
// stated as code.
//
// An entry with no slot at all is skipped, and silently, which is the opposite
// of what listWLANs does with a WLAN it cannot describe. The difference is what
// the silence would cost. A WLAN left out of an export is one prune could then
// delete, so it has to be named; a WAN entry claiming no slot is one nothing
// here can match, delete or change, and the Controller does not produce one —
// `wan_networkgroup` is how it decides which uplink an entry is.
func liveWANSlots(all []unifi.Network) (map[string]unifi.Network, error) {
	slots := make(map[string]unifi.Network)
	var shared []string
	for _, entry := range all {
		if entry.Purpose != wanPurpose || entry.WANNetworkGroup == "" {
			continue
		}
		if _, taken := slots[entry.WANNetworkGroup]; taken {
			shared = append(shared, entry.WANNetworkGroup)
			continue
		}
		slots[entry.WANNetworkGroup] = entry
	}

	if len(shared) > 0 {
		slices.Sort(shared)
		return nil, twoEntriesForOneSlot(slices.Compact(shared))
	}
	return slots, nil
}

// projectWANSlots projects the site's WAN slots into the config that would
// describe them, and names the ones it could only half describe.
//
// A slot whose connection type unifig does not model comes back as its slot and
// nothing else — the WAN spelling of `- name: IoT`, meaning "this exists, and
// unifig manages nothing about it". Leaving it out altogether would be the
// worse answer: export is the adoption path, and a slot missing from the file
// reads as a slot the Controller does not have.
func projectWANSlots(all []unifi.Network) ([]config.WANSlot, []string, error) {
	live, err := liveWANSlots(all)
	if err != nil {
		return nil, nil, err
	}

	slots := make([]config.WANSlot, 0, len(live))
	var partial []string
	for _, entry := range live {
		described := fromLiveWANSlot(entry)
		if described.Type == "" {
			partial = append(partial, described.Slot)
		}
		slots = append(slots, described)
	}
	slices.SortFunc(slots, func(a, b config.WANSlot) int { return compareSlots(a.Slot, b.Slot) })
	slices.Sort(partial)
	return slots, partial, nil
}

// compareSlots orders slots the way the router labels them — the primary
// uplink, then the numbered ones, then the cellular backup — which is what
// alphabetical order gets wrong at both ends.
func compareSlots(a, b string) int {
	if rank := slotRank(a) - slotRank(b); rank != 0 {
		return rank
	}
	return strings.Compare(a, b)
}

// slotRank is the three groups that ordering comes down to. Anything unifig has
// not heard of sorts with the numbered slots rather than being dropped: which
// slots a router has is the router's answer, not a list kept here.
func slotRank(slot string) int {
	switch slot {
	case "WAN":
		return 0
	case "WAN_LTE_FAILOVER":
		return 2
	default:
		return 1
	}
}

// fromLiveWANSlot projects one live WAN slot into the config that would
// describe it, in the same one-implementation-for-both-directions way as
// networks and WLANs: what export writes is what plan compares against.
//
// The PPPoE credentials come back from the Internal API in the clear, so they
// diff like any other field (ADR-0007) — and they are read only for a slot that
// actually uses PPPoE, because credentials left behind on a slot that has since
// moved to DHCP describe nothing about how it connects today.
func fromLiveWANSlot(live unifi.Network) config.WANSlot {
	slot := config.WANSlot{Slot: live.WANNetworkGroup}
	if !wanTypes[live.WANType] {
		return slot
	}

	slot.Type = live.WANType
	if live.WANType == "pppoe" {
		slot.Username = live.WANUsername
		slot.Password = live.XWANPassword
	}
	return slot
}

// updateWANSlot is the Change that brings a live WAN slot in line with the
// config, and whether there is one to make at all.
func updateWANSlot(desired config.WANSlot, live unifi.Network) (Change, bool) {
	fields := changedWANSlotFields(fromLiveWANSlot(live), desired)
	if len(fields) == 0 {
		return Change{}, false
	}

	// A PPPoE uplink with no credentials anywhere cannot sign in, and the config
	// does not say so — it says `type: pppoe` and stops. So the plan says it,
	// the same way it says that a WLAN with no passphrase will be open.
	if desired.Type == "pppoe" && desired.Username == "" && live.WANUsername == "" {
		annotate(fields, "type", "no PPPoE username is set here or on the Controller, so this uplink will have nothing to sign in with")
	}

	return Change{
		Action: Update,
		Kind:   WANSlot,
		Name:   desired.Slot,
		Fields: fields,
		Risk:   wanRisk,
		write: func(ctx context.Context, client unifi.Client, site string) error {
			// The live entry goes back with only unifig's own fields changed, so
			// the failover priority, the DNS preference, the VLAN tag the ISP
			// requires and everything else on the slot survive an apply.
			updated := live
			overwriteManagedWANSlot(&updated, desired)
			_, err := client.UpdateNetwork(ctx, site, &updated)
			return err
		},
	}, true
}

// changedWANSlotFields lists the managed fields on which the Controller and the
// config disagree.
//
// A field the config leaves out is unmanaged as usual (ADR-0004), which is what
// lets an operator put their uplink under unifig's care one field at a time —
// `- slot: WAN` alone matches the slot and changes nothing about it.
func changedWANSlotFields(current, desired config.WANSlot) []Field {
	fields := make([]Field, 0, 3)
	if desired.Type != "" && current.Type != desired.Type {
		fields = append(fields, Field{Name: "type", From: text(current.Type), To: desired.Type})
	}
	if desired.Username != "" && current.Username != desired.Username {
		fields = append(fields, Field{Name: "username", From: text(current.Username), To: desired.Username})
	}
	if desired.Password != "" && current.Password != desired.Password {
		fields = append(fields, Field{Name: "password", Secret: true})
	}
	return fields
}

// overwriteManagedWANSlot writes the config's values onto a Controller WAN
// entry and touches nothing else — the single place that decides which WAN
// fields unifig owns.
//
// Two things it deliberately never writes: the slot, because that is the
// identity unifig matched on and moving an entry between slots is not an edit
// but a different uplink; and the entry's name, which the config does not model
// at all and an operator may well have changed to their ISP's.
//
// Writing a credential also switches on the Controller's flag for using it, for
// the same reason a WLAN passphrase implies WPA-PSK: on the Controller the two
// are one decision, and a username stored beside a flag saying it is unused is
// an uplink that quietly does not sign in.
//
// Nothing here clears anything, which is ADR-0004 rather than an oversight: a
// slot moved from PPPoE to DHCP keeps the credentials the Controller stored,
// because the config never asked for them to go and removing a value is a
// Controller-side operation. They stay out of everything unifig prints —
// fromLiveWANSlot reads credentials only for a slot actually using PPPoE, so
// they are not exported and not diffed either.
func overwriteManagedWANSlot(slot *unifi.Network, desired config.WANSlot) {
	if desired.Type != "" {
		slot.WANType = desired.Type
	}
	if desired.Username != "" {
		slot.WANUsername = desired.Username
		slot.WANPppoeUsernameEnabled = true
	}
	if desired.Password != "" {
		slot.XWANPassword = desired.Password
		slot.WANPppoePasswordEnabled = true
	}
}

// noSuchSlot is the error for a config naming a slot this Controller does not
// have. It lists the slots the site does have, because the likeliest cause is
// an operator who wrote WAN2 for a router with one uplink, and the answer they
// need is which slots are really there.
func noSuchSlot(slot string, live map[string]unifi.Network) error {
	present := make([]string, 0, len(live))
	for name := range live {
		present = append(present, name)
	}
	slices.SortFunc(present, compareSlots)

	has := "this site has none at all"
	if len(present) > 0 {
		has = "this site has " + andJoin(quoted(present))
	}
	return fmt.Errorf(
		"the Controller has no %q WAN slot, and unifig never creates one: %s are the router's own, and a config can only say what to do with the ones it has — %s",
		slot, kinds[WANSlot].many, has)
}

// twoEntriesForOneSlot is the Setting's version of the duplicate natural key
// rule (ADR-0001): with two entries claiming one uplink, which of them the
// operator means is a fact only they hold, so unifig stops rather than guesses.
func twoEntriesForOneSlot(slots []string) error {
	noun := "slot"
	if len(slots) > 1 {
		noun = "slots"
	}
	return fmt.Errorf(
		"the Controller holds more than one WAN entry for the %s %s, so unifig cannot tell which one is that uplink; remove the extras in the Controller's UI, then run again",
		andJoin(quoted(slots)), noun)
}
