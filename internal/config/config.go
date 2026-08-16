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
	Networks []Network `yaml:"networks,omitempty"`
	WLANs    []WLAN    `yaml:"wlans,omitempty"`
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
