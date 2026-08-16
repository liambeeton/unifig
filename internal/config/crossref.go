package config

import "fmt"

// checkReferences enforces what JSON Schema structurally cannot: that the
// document is consistent with itself.
//
// Every check here exists for the same reason, which is how unifig identifies
// things. Resources are matched by natural key rather than by stored ID
// (ADR-0001), so a name is not a label — it is the identity. That makes a
// duplicate name ambiguous and a reference to an undefined name unresolvable,
// and neither can be caught by looking at one field in isolation. A Setting is
// keyed by the Controller's own slot rather than by a name, and duplicates one
// for one for the same reason.
//
// Duplicates are checked before references, and a duplicated name still counts
// as defined: reporting "no network named X" alongside "X is defined twice"
// would be actively misleading.
//
// WLAN names are checked for duplicates even though nothing in this file
// resolves a reference *to* a WLAN. The reason is the one above rather than
// reference resolution: a natural key that appears twice is ambiguous to
// whatever eventually consumes it, and an operator would much rather hear that
// while looking at the file than partway through an Apply.
func checkReferences(cfg Config, idx index) []Problem {
	var problems []Problem

	defined := make(map[string]bool, len(cfg.Networks))
	var networkNames []string
	for i, network := range cfg.Networks {
		if defined[network.Name] {
			at := idx.field("networks", i, "name")
			problems = append(problems, Problem{
				Line: at.line, Path: at.path,
				Message: alreadyDefined("network", "networks", network.Name),
			})
			continue
		}
		defined[network.Name] = true
		networkNames = append(networkNames, network.Name)
	}

	named := make(map[string]bool, len(cfg.WLANs))
	for i, wlan := range cfg.WLANs {
		if named[wlan.Name] {
			at := idx.field("wlans", i, "name")
			problems = append(problems, Problem{
				Line: at.line, Path: at.path,
				Message: alreadyDefined("WLAN", "WLANs", wlan.Name),
			})
		}
		named[wlan.Name] = true

		if !defined[wlan.Network] {
			at := idx.field("wlans", i, "network")
			problems = append(problems, Problem{Line: at.line, Path: at.path, Message: fmt.Sprintf(
				"no network named %q is defined in this file; %s",
				wlan.Network, availableNetworks(networkNames))})
		}
	}

	// A WAN slot is checked for the same ambiguity as a Resource name, and the
	// reason it needs its own check is that its key is not a name the operator
	// chose: the Controller has exactly one of each slot, so two entries for one
	// slot are two answers to a question with one.
	configured := make(map[string]bool, len(cfg.WAN))
	for i, slot := range cfg.WAN {
		if configured[slot.Slot] {
			at := idx.field("wan", i, "slot")
			problems = append(problems, Problem{Line: at.line, Path: at.path, Message: fmt.Sprintf(
				"the %s slot is already configured in this file; the Controller has one of each slot, so unifig would not know which of these to apply",
				slot.Slot)})
		}
		configured[slot.Slot] = true
	}

	return problems
}

func alreadyDefined(kind, kinds, name string) string {
	return fmt.Sprintf(
		"a %s named %q is already defined; unifig matches %s on the Controller by name, so two cannot share one",
		kind, name, kinds)
}

// availableNetworks turns the file's own contents into the other half of the
// advice: an operator who mistyped a reference needs to see what was on offer.
func availableNetworks(names []string) string {
	if len(names) == 0 {
		return "this file defines no networks"
	}
	return "defined networks are " + quoteAll(names)
}
