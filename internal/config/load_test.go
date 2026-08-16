package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/liambeeton/unifig/internal/config"
)

// Prune's scope is decided by which sections the file has (ADR-0006), and the
// only thing that distinguishes "no WLANs of mine" from "no opinion about
// WLANs" is a nil slice against an empty one. Nothing in the type system says
// yaml.v3 draws that line where unifig needs it drawn, and the consequence of
// being wrong is a prune that deletes every WLAN on the Controller — so it is
// asserted here rather than assumed.
func TestAnAbsentSectionLoadsAsNilAndAnEmptyOneDoesNot(t *testing.T) {
	for _, section := range []struct {
		what    string
		body    string
		wantNil bool
	}{
		{"no wlans key at all", "networks:\n  - name: IoT\n    vlan: 20\n", true},
		{"an empty wlans list", "networks:\n  - name: IoT\n    vlan: 20\nwlans: []\n", false},
	} {
		t.Run(section.what, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "unifig.yaml")
			if err := os.WriteFile(path, []byte(section.body), 0o600); err != nil {
				t.Fatalf("writing config: %v", err)
			}

			cfg, err := config.Load(path)
			if err != nil {
				t.Fatalf("loading config: %v", err)
			}
			if len(cfg.WLANs) != 0 {
				t.Fatalf("loaded %d WLANs from a file that lists none", len(cfg.WLANs))
			}
			if (cfg.WLANs == nil) != section.wantNil {
				t.Errorf("WLANs == nil is %v for %s, want %v", cfg.WLANs == nil, section.what, section.wantNil)
			}
		})
	}
}
