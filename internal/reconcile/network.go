package reconcile

import (
	"context"
	"encoding/binary"
	"fmt"
	"net/netip"

	"github.com/filipowm/go-unifi/v2/unifi"

	"github.com/liambeeton/unifig/internal/config"
)

// lanPurposes are the networkconf purposes unifig manages as networks. WAN
// entries share the collection but are Settings — fixed slots that are updated,
// never created — and so are matched by wan.go rather than here; the VPN
// purposes are out of scope for v1 and are not matched at all.
var lanPurposes = map[string]bool{
	"corporate": true,
	"guest":     true,
	"vlan-only": true,
}

// managed reports whether a live networkconf entry is one of the Network
// Resources unifig manages.
func managed(network unifi.Network) bool { return lanPurposes[network.Purpose] }

// planNetworks is the network half of a reconcile.
func planNetworks(cfg config.Config, live map[string]unifi.Network, bound bindings, opts Options) []Change {
	changes := make([]Change, 0, len(cfg.Networks))
	named := make(map[string]bool, len(cfg.Networks))
	for _, desired := range cfg.Networks {
		named[desired.Name] = true

		current, exists := live[desired.Name]
		if !exists {
			changes = append(changes, createNetwork(desired, bound))
			continue
		}
		if change, differs := updateNetwork(desired, current); differs {
			changes = append(changes, change)
		}
	}
	if opts.Prune {
		changes = append(changes, pruneNetworks(live, named)...)
	}
	return changes
}

// listNetworkConf reads the site's whole networkconf collection: the LANs, the
// WAN slots and everything else in it.
//
// It is unfiltered because two managed types come out of this one collection,
// and reading it twice would be both a second round trip and a second moment in
// time — a plan is a statement about the Controller as it was, and two halves of
// one plan disagreeing about when that was is not a thing worth allowing.
func listNetworkConf(ctx context.Context, client unifi.Client, site string) ([]unifi.Network, error) {
	all, err := client.ListNetwork(ctx, site)
	if err != nil {
		return nil, fmt.Errorf("listing networks for site %q: %w", site, err)
	}
	return all, nil
}

// lans keeps the networkconf entries unifig manages as networks, refusing a
// site where two of them share the name unifig matches them by.
func lans(all []unifi.Network) ([]unifi.Network, error) {
	kept := make([]unifi.Network, 0, len(all))
	names := make([]string, 0, len(all))
	for _, network := range all {
		if managed(network) {
			kept = append(kept, network)
			names = append(names, network.Name)
		}
	}
	if err := uniquelyNamed(Network, names); err != nil {
		return nil, err
	}
	return kept, nil
}

// fromLiveNetwork projects a live network into the config that would describe
// it.
//
// Export uses this too, and that is the point rather than a coincidence. The
// config export writes has to be config that plans clean, and the only way to
// guarantee that is for the projection export writes and the projection plan
// compares against to be one function — a second implementation could drift,
// and the symptom would be an operator's brand-new export showing changes.
func fromLiveNetwork(network unifi.Network) config.Network {
	desired := config.Network{Name: network.Name, Subnet: network.IPSubnet}
	if network.VLANEnabled {
		desired.VLAN = network.VLAN
	}
	return desired
}

// createNetwork is the Change for a network the Controller does not have.
func createNetwork(desired config.Network, bound bindings) Change {
	return Change{
		Action: Create,
		Kind:   Network,
		Name:   desired.Name,
		Fields: setNetworkFields(desired),
		write: func(ctx context.Context, client unifi.Client, site string) error {
			network := newNetwork(desired)
			created, err := client.CreateNetwork(ctx, site, &network)
			if err != nil {
				return err
			}
			// A WLAN planned onto this network has been waiting for exactly
			// this: until now there was no ID to attach it to. Recording it
			// here is what lets the two apply in one pass.
			bound.ids[desired.Name] = created.ID
			return nil
		},
	}
}

// updateNetwork is the Change that brings a live network in line with the
// config, and whether there is one to make at all.
func updateNetwork(desired config.Network, live unifi.Network) (Change, bool) {
	fields := changedNetworkFields(fromLiveNetwork(live), desired)
	if len(fields) == 0 {
		return Change{}, false
	}

	pool, relocated := relocateDHCP(live, desired)
	if relocated {
		annotate(fields, "subnet", fmt.Sprintf(
			"the DHCP pool no longer fits and moves to %s-%s", pool.start, pool.stop))
	}

	return Change{
		Action: Update,
		Kind:   Network,
		Name:   desired.Name,
		Fields: fields,
		write: func(ctx context.Context, client unifi.Client, site string) error {
			// The live object goes back with only unifig's own fields
			// changed, so the Controller keeps every setting unifig does not
			// model — the DNS servers, the lease time, the IGMP switches —
			// instead of having them reset by an object unifig built from
			// scratch. It also carries the Controller ID the update needs.
			updated := live
			overwriteManagedNetwork(&updated, desired)
			if relocated {
				updated.DHCPDStart, updated.DHCPDStop = pool.start, pool.stop
			}
			_, err := client.UpdateNetwork(ctx, site, &updated)
			return err
		},
	}, true
}

// pruneNetworks is the Changes that would delete every live network the config
// does not name — prune, and one of the two places in the engine that proposes
// destroying anything.
//
// It walks the live networks rather than the config, which is the whole shape
// of prune: what is at stake is what is on the Controller, and the config's
// only role is to say which of it stays.
func pruneNetworks(live map[string]unifi.Network, named map[string]bool) []Change {
	changes := make([]Change, 0, len(live))
	for name, network := range live {
		if named[name] || network.NoDelete {
			continue
		}
		changes = append(changes, deleteNetwork(network))
	}
	return changes
}

// deleteNetwork is the Change that removes a live network.
//
// Whether the Controller would allow it at all is read off the object's own
// attr_no_delete marker rather than from a list of built-in names unifig keeps
// (ADR-0005), and an exempt network is left out of the plan rather than listed
// as a change that will not happen — it is not a change, and a plan is a list
// of changes.
func deleteNetwork(live unifi.Network) Change {
	return Change{
		Action: Delete,
		Kind:   Network,
		Name:   live.Name,
		Fields: currentNetworkFields(fromLiveNetwork(live)),
		write: func(ctx context.Context, client unifi.Client, site string) error {
			return client.DeleteNetwork(ctx, site, live.ID)
		},
	}
}

// dhcpPool is the address range a network's DHCP server hands out.
type dhcpPool struct{ start, stop string }

// relocateDHCP reports where a network's DHCP pool has to move to because the
// subnet under it changed, and whether it has to move at all.
//
// This is the one place unifig writes a field its config does not model, and
// it is not really an exception to the rule. A pool is a range of addresses
// *within* a subnet; once the subnet moves, the old range does not describe a
// narrower intent that could be respected — it describes addresses that no
// longer exist, and the Controller rejects the whole update as an invalid DHCP
// range. So a pool that still fits is left exactly as the operator set it, and
// only one that cannot survive is rebuilt from the new subnet.
func relocateDHCP(live unifi.Network, desired config.Network) (dhcpPool, bool) {
	// No DHCP server, or no subnet change to strand it — an omitted subnet
	// being no change at all, the same as everywhere else.
	if !live.DHCPDEnabled || desired.Subnet == "" || live.IPSubnet == desired.Subnet {
		return dhcpPool{}, false
	}
	if within(desired.Subnet, live.DHCPDStart) && within(desired.Subnet, live.DHCPDStop) {
		return dhcpPool{}, false
	}
	start, stop, ok := dhcpRange(desired.Subnet)
	if !ok {
		// No pool can be derived, so there is nothing better to offer than
		// what is already there — and the Controller's own complaint about it
		// is a clearer thing for the operator to read than a guess.
		return dhcpPool{}, false
	}
	return dhcpPool{start: start, stop: stop}, true
}

// within reports whether an address falls inside a subnet. Anything it cannot
// parse is not within: the caller is asking whether a pool is safe to leave
// alone, and an answer it cannot establish is not a yes.
func within(subnet, address string) bool {
	prefix, err := netip.ParsePrefix(subnet)
	if err != nil {
		return false
	}
	addr, err := netip.ParseAddr(address)
	if err != nil {
		return false
	}
	return prefix.Contains(addr)
}

// setNetworkFields lists what a create would set. Name is left out: it is the
// Resource's identity, already named by the Change itself, and repeating it as
// a field would read as though it were being changed.
func setNetworkFields(desired config.Network) []Field {
	fields := make([]Field, 0, 2)
	if desired.VLAN != 0 {
		fields = append(fields, Field{Name: "vlan", To: desired.VLAN})
	}
	if desired.Subnet != "" {
		fields = append(fields, Field{Name: "subnet", To: desired.Subnet})
	}
	return fields
}

// currentNetworkFields is setNetworkFields' mirror: what a delete would take
// away, on the From side because there is no value on the other one.
//
// A delete lists fields at all because of who reads it. Prune's plan is the
// only place an operator sees what is inside a network before agreeing to lose
// it, and "the VLAN 20 one on 10.20.0.1/24" is how they will recognise whether
// it is the network they meant.
func currentNetworkFields(current config.Network) []Field {
	fields := make([]Field, 0, 2)
	if current.VLAN != 0 {
		fields = append(fields, Field{Name: "vlan", From: current.VLAN})
	}
	if current.Subnet != "" {
		fields = append(fields, Field{Name: "subnet", From: current.Subnet})
	}
	return fields
}

// changedNetworkFields lists the managed fields on which the Controller and the
// config disagree — nothing else is a change, however different the two
// objects are elsewhere.
//
// A field the config leaves out is a field unifig does not manage, not a
// request to empty it (ADR-0004). The config has no way to ask for a network
// with no VLAN or no subnet — the schema's `vlan` starts at 1 and its `subnet`
// must match a CIDR — so reading omission as removal would delete a setting
// the operator could never have asked to delete, which is the one thing this
// tool exists not to do.
func changedNetworkFields(current, desired config.Network) []Field {
	fields := make([]Field, 0, 2)
	if desired.VLAN != 0 && current.VLAN != desired.VLAN {
		fields = append(fields, Field{Name: "vlan", From: number(current.VLAN), To: desired.VLAN})
	}
	if desired.Subnet != "" && current.Subnet != desired.Subnet {
		fields = append(fields, Field{Name: "subnet", From: text(current.Subnet), To: desired.Subnet})
	}
	return fields
}

// overwriteManagedNetwork writes the config's values onto a Controller network
// and touches nothing else. It is the single place that decides which fields
// unifig owns, which is what stops plan (what would change) and apply (what
// does change) from ever disagreeing about the answer.
//
// "Owns" is per field and per file, not per type: the omissions
// changedNetworkFields declines to report are the same omissions this declines
// to write, so a network named in the config keeps every setting the config did
// not name.
func overwriteManagedNetwork(network *unifi.Network, desired config.Network) {
	network.Name = desired.Name
	if desired.VLAN != 0 {
		network.VLAN = desired.VLAN
		network.VLANEnabled = true
	}
	if desired.Subnet != "" {
		network.IPSubnet = desired.Subnet
	}
}

// newNetwork builds the Controller object for a network unifig is creating.
//
// The config models three fields, but a networkconf has a hundred, and the
// Controller stores whatever it is sent — so a network created from a bare
// struct would come out with NAT off, no internet access and no DHCP server,
// which is not a LAN in any useful sense. The values below are therefore the
// Controller's own defaults for a new LAN, matching what its UI creates.
//
// They apply on create only. An operator who afterwards changes the DHCP
// range, turns off mDNS or narrows the lease time keeps those edits forever:
// updates go through overwriteManagedNetwork, which never touches anything here.
func newNetwork(desired config.Network) unifi.Network {
	network := unifi.Network{
		Purpose:               "corporate",
		NetworkGroup:          "LAN",
		Enabled:               true,
		IsNAT:                 true,
		InternetAccessEnabled: true,
		MdnsEnabled:           true,
		DomainName:            "localdomain",
		IPV6InterfaceType:     "none",
	}
	overwriteManagedNetwork(&network, desired)

	// A subnet too small to hold a pool (or absent altogether) gets no DHCP
	// server rather than a broken one.
	if start, stop, ok := dhcpRange(desired.Subnet); ok {
		network.DHCPDEnabled = true
		network.DHCPDStart = start
		network.DHCPDStop = stop
		network.DHCPDLeaseTime = dhcpLeaseSeconds
	}
	return network
}

// dhcpLeaseSeconds is 24 hours, the Controller's own default lease.
const dhcpLeaseSeconds = 86400

// dhcpRange derives the DHCP pool the Controller itself would use for a
// subnet: the sixth address through the last usable one. For 192.168.1.1/24
// that is 192.168.1.6 to 192.168.1.254 — exactly what the Controller's
// built-in Default network is configured with, which is where the otherwise
// arbitrary-looking six comes from.
func dhcpRange(subnet string) (start, stop string, ok bool) {
	prefix, err := netip.ParsePrefix(subnet)
	if err != nil || !prefix.Addr().Is4() {
		return "", "", false
	}

	// The arithmetic is 64-bit because the schema's pattern permits prefixes
	// as short as /0, whose size is 2^32 and would wrap a uint32 to nothing.
	network := prefix.Masked().Addr().As4()
	base := uint64(binary.BigEndian.Uint32(network[:]))
	size := uint64(1) << (32 - prefix.Bits())

	// base+size-1 is the broadcast address, so the pool stops one below it.
	first, last := base+6, base+size-2
	if first >= last {
		return "", "", false
	}
	return ipv4(first), ipv4(last), true
}

func ipv4(value uint64) string {
	var octets [4]byte
	binary.BigEndian.PutUint32(octets[:], uint32(value))
	return netip.AddrFrom4(octets).String()
}
