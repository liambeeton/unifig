package e2e

import (
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// exportedConfig is the operator-facing shape of `unifig export` output. The
// expected values are independent literals: the demo Controller's built-in
// Default network, and networks the rig seeds through the Controller's API.
type exportedConfig struct {
	Networks []exportedNetwork `yaml:"networks"`
}

type exportedNetwork struct {
	Name   string `yaml:"name"`
	VLAN   int    `yaml:"vlan"`
	Subnet string `yaml:"subnet"`
}

func exportNetworks(t *testing.T) exportedConfig {
	t.Helper()
	res := testRig.runUnifig(t, []string{"export"}, nil)
	if res.ExitCode != 0 {
		t.Fatalf("unifig export exited %d\nstderr: %s", res.ExitCode, res.Stderr)
	}
	var cfg exportedConfig
	if err := yaml.Unmarshal(res.Stdout, &cfg); err != nil {
		t.Fatalf("stdout is not valid YAML: %v\nstdout: %s", err, res.Stdout)
	}
	t.Logf("unifig export stdout:\n%s", res.Stdout)
	return cfg
}

func TestExportPrintsTheSitesNetworksAsYAML(t *testing.T) {
	cfg := exportNetworks(t)

	for _, n := range cfg.Networks {
		if n.Name == "Default" {
			if n.Subnet != "192.168.1.1/24" {
				t.Errorf("Default network subnet = %q, want %q", n.Subnet, "192.168.1.1/24")
			}
			return
		}
	}
	t.Errorf("exported networks %v do not include the Controller's Default network", cfg.Networks)
}

func TestExportListsVLANsByNameAndLeavesWANSlotsOut(t *testing.T) {
	// A VLAN network is a Resource and must be exported; a WAN entry is a
	// Setting (fixed slot) and must not appear among networks.
	testRig.seedNetwork(t, map[string]any{
		"name": "IoT", "purpose": "corporate", "enabled": true,
		"vlan_enabled": true, "vlan": 20, "ip_subnet": "10.20.0.1/24",
	})
	testRig.seedNetwork(t, map[string]any{
		"name": "Primary (WAN1)", "purpose": "wan",
		"wan_networkgroup": "WAN", "wan_type": "dhcp",
	})

	cfg := exportNetworks(t)

	byName := map[string]exportedNetwork{}
	var names []string
	for _, n := range cfg.Networks {
		byName[n.Name] = n
		names = append(names, n.Name)
	}

	if !slices.IsSorted(names) {
		t.Errorf("exported network names %v are not in name order", names)
	}
	if _, ok := byName["Primary (WAN1)"]; ok {
		t.Errorf("exported networks %v include the WAN slot", names)
	}
	iot, ok := byName["IoT"]
	if !ok {
		t.Fatalf("exported networks %v do not include the seeded IoT VLAN", names)
	}
	if iot.VLAN != 20 || iot.Subnet != "10.20.0.1/24" {
		t.Errorf("IoT exported as vlan=%d subnet=%q, want vlan=20 subnet=%q", iot.VLAN, iot.Subnet, "10.20.0.1/24")
	}
	if def := byName["Default"]; def.VLAN != 0 {
		t.Errorf("Default network exported with vlan=%d, want no vlan", def.VLAN)
	}
}

func TestExportWithBadAPIKeyFailsAndPrintsNoYAML(t *testing.T) {
	res := testRig.runUnifig(t, []string{"export"}, map[string]string{"UNIFIG_API_KEY": "not-the-rigs-key"})

	if res.ExitCode != 1 {
		t.Errorf("exit code = %d, want 1", res.ExitCode)
	}
	if len(res.Stdout) != 0 {
		t.Errorf("stdout should stay empty on auth failure, got: %s", res.Stdout)
	}
	stderr := strings.ToLower(string(res.Stderr))
	if !strings.Contains(stderr, "401") && !strings.Contains(stderr, "unauthorized") {
		t.Errorf("stderr should report the auth failure, got: %s", res.Stderr)
	}
}

func TestExportWithoutConnectionConfigFailsNamingTheVariable(t *testing.T) {
	res := testRig.runUnifig(t, []string{"export"}, map[string]string{"UNIFIG_API_KEY": ""})

	if res.ExitCode != 1 {
		t.Errorf("exit code = %d, want 1", res.ExitCode)
	}
	if !strings.Contains(string(res.Stderr), "UNIFIG_API_KEY") {
		t.Errorf("stderr should name the missing variable, got: %s", res.Stderr)
	}
}
