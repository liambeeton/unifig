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

	// Zones and Firewall Policies are checked for duplicates and for nothing
	// else, which is the whole of what this file can honestly say about them.
	//
	// A policy names the pair of Zones it governs, and that reference is not
	// resolved here the way a WLAN's network is. The difference is what a Zone
	// name is allowed to be: the policies worth writing reach the Controller's
	// own built-in Zones — `External` is the internet — and those are matchable
	// but never created or pruned by unifig, so they are not in the file and
	// could not be. Checking against the file alone would reject the commonest
	// policy there is; keeping a list of built-in names here would be the guess
	// about somebody else's product that ADR-0005 exists to avoid. So the
	// reference is resolved when unifig reads the Controller, against the Zones
	// the site really has, exactly as a WAN slot's is (ADR-0010).
	zoned := make(map[string]bool, len(cfg.Zones))
	for i, zone := range cfg.Zones {
		if zoned[zone.Name] {
			at := idx.field("zones", i, "name")
			problems = append(problems, Problem{
				Line: at.line, Path: at.path,
				Message: alreadyDefined("zone", "zones", zone.Name),
			})
		}
		zoned[zone.Name] = true

		// A zone's membership is checked against the file exactly as a WLAN's
		// network is, and for the same reason: it names a network, so it can
		// name one that is not there. The zone's own name is the thing that
		// cannot be checked offline, because a built-in zone is not in the file
		// — the two halves of a zone are not alike, and only one of them is
		// resolvable here.
		for j, network := range zone.Networks {
			if defined[network] {
				continue
			}
			at := idx.nestedEntry("zones", i, "networks", j)
			problems = append(problems, Problem{Line: at.line, Path: at.path, Message: fmt.Sprintf(
				"no network named %q is defined in this file; %s",
				network, availableNetworks(networkNames))})
		}
	}

	governed := make(map[string]bool, len(cfg.FirewallPolicies))
	for i, policy := range cfg.FirewallPolicies {
		if governed[policy.Name] {
			at := idx.field("firewall-policies", i, "name")
			problems = append(problems, Problem{
				Line: at.line, Path: at.path,
				Message: alreadyDefined("firewall policy", "firewall policies", policy.Name),
			})
		}
		governed[policy.Name] = true
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

	// A custom DNS server is named by the operator, and that name is how unifig
	// tells one entry in the Encrypted DNS setting from another — so two of them
	// sharing one name is the same ambiguity as two networks sharing one, and is
	// caught here for the same reason.
	if dns := cfg.EncryptedDNS; dns != nil {
		resolvers := make(map[string]bool, len(dns.Servers))
		for i, server := range dns.Servers {
			if resolvers[server.Name] {
				at := idx.nestedField("encrypted-dns", "servers", i, "name")
				problems = append(problems, Problem{
					Line: at.line, Path: at.path,
					Message: alreadyDefined("custom DNS server", "custom DNS servers", server.Name),
				})
			}
			resolvers[server.Name] = true
		}
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
