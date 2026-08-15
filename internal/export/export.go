// Package export generates YAML config from live Controller state — the
// brownfield adoption path (Export in the domain glossary).
//
// It produces the same config.Config that `unifig validate` checks and that
// plan and apply consume, so what export writes is by construction something
// unifig can read back.
package export

import (
	"context"
	"fmt"
	"sort"

	"github.com/filipowm/go-unifi/v2/unifi"

	"github.com/liambeeton/unifig/internal/config"
)

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
func Networks(ctx context.Context, client unifi.Client, site string) (config.Config, error) {
	live, err := client.ListNetwork(ctx, site)
	if err != nil {
		return config.Config{}, fmt.Errorf("listing networks for site %q: %w", site, err)
	}

	networks := make([]config.Network, 0, len(live))
	for _, n := range live {
		if !lanPurposes[n.Purpose] {
			continue
		}
		networks = append(networks, config.Network{
			Name:   n.Name,
			VLAN:   n.VLAN,
			Subnet: n.IPSubnet,
		})
	}
	sort.Slice(networks, func(i, j int) bool { return networks[i].Name < networks[j].Name })
	return config.Config{Networks: networks}, nil
}
