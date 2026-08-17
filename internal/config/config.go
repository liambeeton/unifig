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
	Networks     []Network     `yaml:"networks,omitempty"`
	WLANs        []WLAN        `yaml:"wlans,omitempty"`
	WAN          []WANSlot     `yaml:"wan,omitempty"`
	EncryptedDNS *EncryptedDNS `yaml:"encrypted-dns,omitempty"`
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
