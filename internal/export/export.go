// Package export generates YAML config from live Controller state — the
// brownfield adoption path (Export in the domain glossary).
package export

import (
	"context"
	"fmt"
	"io"
	"sort"

	"github.com/filipowm/go-unifi/v2/unifi"
	"gopkg.in/yaml.v3"
)

// Config is the exported YAML document. It will grow one section per managed
// area; the walking skeleton exports networks only.
type Config struct {
	Networks []Network `yaml:"networks"`
}

// Network is the operator-facing projection of a Controller network Resource,
// keyed by its natural key (name). Controller IDs never appear here (ADR-0001).
type Network struct {
	Name   string `yaml:"name"`
	VLAN   int    `yaml:"vlan,omitempty"`
	Subnet string `yaml:"subnet,omitempty"`
}

// lanPurposes are the networkconf purposes exported as networks. WAN entries
// are Settings (fixed slots, exported separately), and VPN purposes are out
// of scope for v1.
var lanPurposes = map[string]bool{
	"corporate": true,
	"guest":     true,
	"vlan-only": true,
}

// Networks reads the site's networks from the live Controller and projects
// them into config form, sorted by name so output is deterministic.
func Networks(ctx context.Context, client unifi.Client, site string) (Config, error) {
	live, err := client.ListNetwork(ctx, site)
	if err != nil {
		return Config{}, fmt.Errorf("listing networks for site %q: %w", site, err)
	}

	networks := make([]Network, 0, len(live))
	for _, n := range live {
		if !lanPurposes[n.Purpose] {
			continue
		}
		networks = append(networks, Network{
			Name:   n.Name,
			VLAN:   n.VLAN,
			Subnet: n.IPSubnet,
		})
	}
	sort.Slice(networks, func(i, j int) bool { return networks[i].Name < networks[j].Name })
	return Config{Networks: networks}, nil
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
