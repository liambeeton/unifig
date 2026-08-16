// The tests here close the one loop the process-level suites cannot: that the
// YAML unifig *writes* is YAML unifig will *accept*.
//
// Export renders a config.Config and validate loads one, so the two agree only
// as long as the Go types, their YAML tags and the published JSON Schema all
// say the same thing. Nothing in the compiler enforces that. These tests do.
package config_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liambeeton/unifig/internal/config"
	"github.com/liambeeton/unifig/schema"
)

func TestWrittenConfigValidates(t *testing.T) {
	// Every field the document type has, populated — an empty Config would
	// prove nothing about the fields that could drift.
	written := config.Config{
		Networks: []config.Network{
			{Name: "Default", Subnet: "192.168.1.1/24"},
			{Name: "IoT", VLAN: 20, Subnet: "10.20.0.1/24"},
		},
		WLANs: []config.WLAN{
			{Name: "Home IoT", Network: "IoT", Passphrase: "correct horse battery"},
		},
		WAN: []config.WANSlot{
			{Slot: "WAN", Type: "pppoe", Username: "isp-user", Password: "correct-horse-battery"},
			{Slot: "WAN2"},
		},
	}

	path := filepath.Join(t.TempDir(), "unifig.yaml")
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("creating config: %v", err)
	}
	if err := config.WriteYAML(file, written); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("closing config: %v", err)
	}

	loaded, err := config.Load(path)
	if err != nil {
		rendered, _ := os.ReadFile(path)
		t.Fatalf("config unifig wrote is not config unifig accepts: %v\n%s", err, rendered)
	}
	if len(loaded.Networks) != len(written.Networks) ||
		len(loaded.WLANs) != len(written.WLANs) ||
		len(loaded.WAN) != len(written.WAN) {
		t.Fatalf("loaded %+v, want %+v", loaded, written)
	}
	for i, network := range written.Networks {
		if loaded.Networks[i] != network {
			t.Errorf("networks[%d] loaded as %+v, want %+v", i, loaded.Networks[i], network)
		}
	}
	for i, wlan := range written.WLANs {
		if loaded.WLANs[i] != wlan {
			t.Errorf("wlans[%d] loaded as %+v, want %+v", i, loaded.WLANs[i], wlan)
		}
	}
	for i, slot := range written.WAN {
		if loaded.WAN[i] != slot {
			t.Errorf("wan[%d] loaded as %+v, want %+v", i, loaded.WAN[i], slot)
		}
	}
}

// The example is what an operator copies to start from, and it carries the
// modeline that documents how to wire the schema into an editor. If it stops
// validating, the documentation is wrong.
//
// The variables set here are the ones the example tells its reader to export,
// so this is the example being loaded the way it is meant to be — and it fails
// if the two ever stop naming the same thing.
func TestTheShippedExampleValidates(t *testing.T) {
	t.Setenv("WIFI_IOT_PASSPHRASE", "correct horse battery")
	t.Setenv("WAN_PPPOE_USERNAME", "isp-user")
	t.Setenv("WAN_PPPOE_PASSWORD", "correct-horse-battery")

	if _, err := config.Load(filepath.Join("..", "..", "examples", "unifig.yaml")); err != nil {
		t.Fatalf("examples/unifig.yaml does not validate: %v", err)
	}
}

// And the other half: an example whose secret is written inline would be an
// example teaching the operator to commit one.
func TestTheShippedExampleKeepsItsSecretInTheEnvironment(t *testing.T) {
	example, err := os.ReadFile(filepath.Join("..", "..", "examples", "unifig.yaml"))
	if err != nil {
		t.Fatalf("reading example: %v", err)
	}
	for _, secret := range []string{"passphrase: ${", "password: ${"} {
		if !strings.Contains(string(example), secret) {
			t.Errorf("examples/unifig.yaml should show every secret as a ${ENV_VAR} reference, and %q is missing:\n%s",
				secret, example)
		}
	}
}

// The example's modeline has to point at where the schema is actually
// published, or an editor silently gets no schema at all.
func TestTheShippedExamplePointsAtTheSchema(t *testing.T) {
	example, err := os.ReadFile(filepath.Join("..", "..", "examples", "unifig.yaml"))
	if err != nil {
		t.Fatalf("reading example: %v", err)
	}
	modeline := "# yaml-language-server: $schema=" + schema.URL
	if !strings.Contains(string(example), modeline) {
		t.Errorf("examples/unifig.yaml should carry the modeline %q", modeline)
	}
}

// The schema's $id is how an editor identifies it and how unifig compiles it;
// a moved file with a stale $id would silently stop resolving.
func TestSchemaIDMatchesWhereItIsPublished(t *testing.T) {
	var doc struct {
		ID string `json:"$id"`
	}
	if err := json.Unmarshal(schema.JSON, &doc); err != nil {
		t.Fatalf("embedded schema is not valid JSON: %v", err)
	}
	if doc.ID != schema.URL {
		t.Errorf("schema $id is %q, want %q", doc.ID, schema.URL)
	}
	if !strings.HasSuffix(doc.ID, "/schema/unifig.schema.json") {
		t.Errorf("schema $id %q should end in the path the file lives at in the repo", doc.ID)
	}
}
