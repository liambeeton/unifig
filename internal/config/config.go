// Package config owns unifig.yaml: the document types every verb speaks, and
// the offline Load that turns a file on disk into one of them.
//
// Load is the only way config enters the tool, so everything that must be
// true of a config file is enforced in one place — env interpolation, JSON
// Schema validation, and cross-reference checks — and `unifig validate` is
// simply Load with nothing done afterwards. That is what makes validate's
// promise ("what validate accepts, plan and apply can consume") structural
// rather than a thing to keep in step by hand.
//
// Nothing here touches the network. Load must stay that way: validate is
// offline by design.
package config

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the whole unifig.yaml document. It grows one section per managed
// area; the shape here is mirrored field-for-field by the published JSON
// Schema, which is what actually validates a file.
//
// A nil section and an empty one mean different things, and the difference is
// load-bearing for prune: a file with no `wlans:` key at all states nothing
// about WLANs, while `wlans: []` states that there should be none. Load
// preserves that distinction, so nothing here may normalise a nil section into
// an empty slice.
type Config struct {
	Networks         []Network         `yaml:"networks,omitempty"`
	WLANs            []WLAN            `yaml:"wlans,omitempty"`
	Zones            []Zone            `yaml:"zones,omitempty"`
	FirewallPolicies []FirewallPolicy  `yaml:"firewall-policies,omitempty"`
	PortForwards     []PortForward     `yaml:"port-forwards,omitempty"`
	DHCPReservations []DHCPReservation `yaml:"dhcp-reservations,omitempty"`
	WAN              []WANSlot         `yaml:"wan,omitempty"`
	EncryptedDNS     *EncryptedDNS     `yaml:"encrypted-dns,omitempty"`
}

// Network is the operator-facing projection of a Controller network Resource,
// keyed by its natural key (name). Controller IDs never appear here (ADR-0001).
type Network struct {
	Name   string `yaml:"name"`
	VLAN   int    `yaml:"vlan,omitempty"`
	Subnet string `yaml:"subnet,omitempty"`
}

// WLAN is a wireless network Resource, keyed by name (the SSID) and bound to
// the Network its clients land on.
//
// Passphrase is the first secret unifig models, and the reason it can live in a
// committed file at all is that it is written as a `${ENV_VAR}` reference and
// resolved by Load. Nothing downstream distinguishes a passphrase that came
// from the environment from one typed into the file — by the time this struct
// exists, interpolation has already happened — so everywhere the value could
// escape (plan output, export) treats it as a secret regardless of where it
// came from.
type WLAN struct {
	Name       string `yaml:"name"`
	Network    string `yaml:"network"`
	Passphrase string `yaml:"passphrase,omitempty"`
}

// Zone is a named group of networks in the Controller's zone-based firewall,
// keyed by name like every other Resource.
//
// Networks is nil and empty for different reasons, in the same load-bearing way
// as a Config section and as EncryptedDNS.Servers: no `networks:` key leaves the
// Controller's own membership alone, while `networks: []` states that the zone
// should hold none. It is one field of one Resource rather than a collection, so
// stating it makes this list the zone's list — a network that drops out of it is
// one the next apply takes out of the zone.
//
// Which is why it is the one field here without `omitempty`. A zone the
// Controller holds with no networks in it has to export as `networks: []`, and
// `omitempty` would drop that to no key at all, which says the opposite.
type Zone struct {
	Name     string   `yaml:"name"`
	Networks []string `yaml:"networks"`
}

// FirewallPolicy is a rule governing traffic between a pair of Zones, keyed by
// its name together with the pair of Zones it governs — the Controller ships its
// own policies one per pair and reuses names across them (ADR-0001).
//
// Source and Destination name Zones, and deliberately not only Zones this file
// defines. The interesting policies are the ones that reach the Controller's own
// built-in zones — `External` is the internet — and those are never in the file,
// because a built-in Zone is matchable but not something unifig creates or
// prunes. So the reference is resolved against the Controller rather than
// offline, the same way a WAN slot's is (ADR-0010), and checkReferences says
// nothing about it.
//
// Action is required by the schema alongside the pair, unlike the optional
// fields everywhere else in this file. A policy exists to allow or block
// something, so there is no such thing as a policy unifig could create without
// knowing which — the same reason a WLAN has to name the network its clients
// join.
type FirewallPolicy struct {
	Name        string `yaml:"name"`
	Action      string `yaml:"action"`
	Source      string `yaml:"source"`
	Destination string `yaml:"destination"`
}

// PortForward is a rule sending traffic that arrives on a port of the internet
// side to an address and port inside the network, keyed by name.
//
// It is the one Resource whose target is not a reference. ForwardIP is an
// address rather than the name of a network this file defines, so there is
// nothing here for checkReferences to resolve, nothing for prune to hold back on
// its account, and no ordering it imposes on an apply.
//
// Everything but Source is required, on the reasoning a firewall policy's fields
// are: a forward exists to send one port somewhere, so there is no such thing as
// one unifig could create without knowing which port, which host and which
// protocol. Source is the exception because a forward is usually open to the
// whole internet and stating this field is how an operator narrows it — omitted,
// it is unmanaged like every other omitted field (ADR-0004), and a create the
// config states none for is one the plan says will accept traffic from anywhere.
//
// Ports are numbers rather than text, which is the one thing this type declines
// to model in full: the Controller will hold a range or a comma-separated list in
// either port, and unifig models a single port in each. A live forward stating
// otherwise is one export leaves out and prune never deletes, the same way a WLAN
// bound to something unifig cannot name is.
type PortForward struct {
	Name        string `yaml:"name"`
	Port        int    `yaml:"port"`
	ForwardIP   string `yaml:"forward-ip"`
	ForwardPort int    `yaml:"forward-port"`
	Protocol    string `yaml:"protocol"`
	Source      string `yaml:"source,omitempty"`
}

// DHCPReservation is a fixed address for one client, keyed by its MAC.
//
// It is the only Resource here that is not a Controller object. The Controller
// keeps one record per client it has ever seen — the thing its UI calls a
// client, carrying a name, a note, a user group, whether it is blocked — and a
// reservation is the fixed-IP half of that record and nothing else. So unifig
// writes those two fields and leaves the rest of the record exactly as it is,
// and a reservation removed from this file under `--prune` gives the address up
// rather than forgetting the device (ADR-0015).
//
// Both fields are required, on the reasoning a port forward's are: a
// reservation exists to pin one client to one address, so there is no such
// thing as one unifig could state without knowing which client and which
// address. The Controller agrees — it refuses a reservation with no address.
//
// There is no network to name, and that is the Controller's doing rather than
// an omission. It decides which network a reservation belongs to by which
// subnet the address falls in: an address inside no network's subnet is
// refused, and deleting a network with an address reserved inside it is refused
// too, both under that same reading. The per-client record does carry a
// network of its own, and unifig does not write it, because it is not what the
// Controller consults.
//
// MAC is the natural key, and the one natural key in this file that is not
// case-sensitive: the Controller lower-cases every MAC it stores, so two
// entries differing only in case are one reservation written twice, which
// checkReferences reports as the duplicate it is.
type DHCPReservation struct {
	MAC string `yaml:"mac"`
	IP  string `yaml:"ip"`
}

// NormalisedMAC is a MAC address as the Controller stores it, which is lower
// case. It is exported because the rule has two customers and one of them is
// outside this package: the reconcile matches a config entry to a client record
// by folded MAC, and checkReferences calls two entries that fold together one
// reservation written twice. Two spellings of that rule would be two answers to
// "are these the same client", and the file that validated would be the file
// that then applied in the wrong order.
//
// It lower-cases and nothing else. The schema has already refused anything that
// is not six hex pairs with colons between them, so there is no other spelling
// left to reconcile.
func NormalisedMAC(mac string) string { return strings.ToLower(mac) }

// WANSlot is one of the Controller's internet uplinks — a Setting rather than a
// Resource, and the difference shows up in this struct's key. A network is
// matched by a name the operator chose and can change; a WAN slot is matched by
// Slot, which is the Controller's own name for a physical uplink it always has.
// There is no field here that could create one, because nothing creates one.
//
// Password is the second secret unifig models and behaves exactly like a WLAN's
// passphrase: written as a `${ENV_VAR}` reference, resolved by Load, and
// redacted everywhere it could leave (ADR-0007).
type WANSlot struct {
	Slot     string `yaml:"slot"`
	Type     string `yaml:"type,omitempty"`
	Username string `yaml:"username,omitempty"`
	Password string `yaml:"password,omitempty"`
}

// EncryptedDNS is the Controller's Encrypted DNS setting — the second Setting
// unifig manages, and the first singleton. A Controller has exactly one, which
// is why this is a pointer on Config rather than a slice: there is no key to
// match on, and the only question the file answers is whether it says anything
// about encrypted DNS at all.
//
// Servers is nil and empty for different reasons, in the same load-bearing way
// as a Config section: no `servers:` key leaves the Controller's own list
// alone, while `servers: []` states that there should be none.
//
// Which is why it is the one field here without `omitempty`. A Controller with
// no custom resolvers has to export as `servers: []` — the file saying the list
// is empty — and `omitempty` would drop that to no key at all, which says the
// opposite: leave whatever is there alone. A distinction worth preserving on
// the way in is worth being able to write on the way out.
type EncryptedDNS struct {
	State   string      `yaml:"state,omitempty"`
	Servers []DNSServer `yaml:"servers"`
}

// DNSServer is one custom encrypted resolver — an entry in the Controller's own
// `custom_servers`.
//
// Stamp is the third secret unifig models. A stamp for a private endpoint
// carries the identifier of the account it belongs to, so it behaves exactly
// like a WLAN passphrase or a PPPoE password: written as a `${ENV_VAR}`
// reference, resolved by Load, and redacted everywhere it could leave
// (ADR-0007).
type DNSServer struct {
	Name  string `yaml:"name"`
	Stamp string `yaml:"stamp"`
}

// DefaultPath is where unifig looks for config when no file is named.
const DefaultPath = "unifig.yaml"

// Load reads path and returns the config it describes, or an *Error listing
// every problem found. The stages run in this order, and the order is the
// contract:
//
//  1. parse YAML — a syntax error stops everything, since nothing downstream
//     has a document to look at;
//  2. interpolate ${ENV_VAR} in values, so the schema sees what the operator
//     actually means rather than the placeholder text;
//  3. validate against the JSON Schema — the same file editors autocomplete
//     from, so what an editor flags and what validate rejects cannot diverge;
//  4. check cross-references between Resources, which JSON Schema cannot
//     express.
//
// Stages 2 through 4 each report every problem they find, but a later stage
// only runs if the earlier ones passed: a reference check over a document
// that failed its schema would report noise about fields it could not trust.
func Load(path string) (Config, error) {
	source, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Config{}, fmt.Errorf(
				"no config file at %s (run `unifig export > %s` to start from the Controller's current configuration)",
				path, path)
		}
		return Config{}, fmt.Errorf("reading %s: %w", path, err)
	}

	root, problems := parse(source)
	if len(problems) > 0 {
		return Config{}, &Error{File: path, Problems: problems}
	}

	doc := resolve(root)
	if len(doc.missing) > 0 {
		return Config{}, &Error{File: path, Problems: doc.missing}
	}

	problems, err = validateSchema(doc)
	if err != nil {
		return Config{}, err
	}
	if len(problems) > 0 {
		return Config{}, &Error{File: path, Problems: problems}
	}

	var cfg Config
	if err := root.Decode(&cfg); err != nil {
		// The schema has already vouched for the shape, so a decode failure
		// here means the schema and the Go types disagree — a bug in unifig,
		// not in the operator's file.
		return Config{}, fmt.Errorf("decoding config that passed schema validation (this is a bug in unifig): %w", err)
	}

	if problems := checkReferences(cfg, doc.index); len(problems) > 0 {
		return Config{}, &Error{File: path, Problems: problems}
	}
	return cfg, nil
}

// WriteYAML renders the config to w as a single YAML document.
func WriteYAML(w io.Writer, cfg Config) error {
	enc := yaml.NewEncoder(w)
	enc.SetIndent(2)
	if err := enc.Encode(cfg); err != nil {
		return err
	}
	return enc.Close()
}
