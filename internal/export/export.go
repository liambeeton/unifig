// Package export generates YAML config from live Controller state — the
// brownfield adoption path (Export in the domain glossary).
//
// It produces the same config.Config that `unifig validate` checks and that
// plan and apply consume, so what export writes is by construction something
// unifig can read back.
package export

import (
	"context"
	"sort"

	"github.com/filipowm/go-unifi/v2/unifi"

	"github.com/liambeeton/unifig/internal/config"
	"github.com/liambeeton/unifig/internal/reconcile"
)

// Networks reads the site's networks from the live Controller and projects
// them into config form, sorted by name so output is deterministic.
//
// Which networks count and what a network looks like in config are both the
// engine's questions, not export's, so both answers come from there. That is
// what makes export's output config that plans clean: a second opinion about
// either would show up as changes pending on a freshly exported file.
func Networks(ctx context.Context, client unifi.Client, site string) (config.Config, error) {
	live, err := reconcile.ListNetworks(ctx, client, site)
	if err != nil {
		return config.Config{}, err
	}

	networks := make([]config.Network, 0, len(live))
	for _, network := range live {
		networks = append(networks, reconcile.FromLive(network))
	}
	sort.Slice(networks, func(i, j int) bool { return networks[i].Name < networks[j].Name })
	return config.Config{Networks: networks}, nil
}
