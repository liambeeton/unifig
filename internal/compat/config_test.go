// compatibility.yaml is read by three things that never run together — the e2e
// rig, CI's matrix, and the generator behind the published table — so what it
// means is worth pinning down where all three can see it.
package compat_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liambeeton/unifig/internal/compat"
)

// The committed file is the one the promise is actually made of, so it is
// tested as itself rather than only through fixtures. A version below the
// floor, a matrix out of order or an area naming tests that are not there
// would all be caught by the generator in CI; catching them here costs no
// Docker and names the file.
func TestTheCommittedConfigurationIsValid(t *testing.T) {
	cfg, err := compat.LoadConfig(filepath.Join("..", "..", "compatibility.yaml"))
	if err != nil {
		t.Fatalf("the committed compatibility.yaml is not valid: %v", err)
	}
	if len(cfg.Versions) < 2 {
		t.Errorf("the matrix carries %d Controller version(s); the promise is that CI runs against more than one",
			len(cfg.Versions))
	}
	if cfg.Container.Image == "" || cfg.Container.Database == "" {
		t.Errorf("the container recipe is incomplete: %+v", cfg.Container)
	}
	if len(cfg.Areas) == 0 {
		t.Error("the table would have no rows")
	}
}

func TestControllerImageJoinsTheRepositoryAndTheVersion(t *testing.T) {
	cfg := load(t, validConfig)
	if got, want := cfg.ControllerImage("10.4.57"), "example/controller:10.4.57"; got != want {
		t.Errorf("ControllerImage = %q, want %q", got, want)
	}
}

// The newest version is what a bare `make e2e` boots, which is the whole
// reason the list has an order rather than a set.
func TestTheNewestVersionIsTheFirstOneListed(t *testing.T) {
	cfg := load(t, validConfig)
	if got, want := cfg.Newest(), "10.5.67"; got != want {
		t.Errorf("Newest = %q, want %q", got, want)
	}
}

func TestAVersionBelowTheFloorIsRefused(t *testing.T) {
	_, err := loadErr(t, strings.Replace(validConfig, `- "10.1.84"`, `- "9.5.21"`, 1))
	if err == nil {
		t.Fatal("a version below the floor was accepted")
	}
	for _, fragment := range []string{"9.5.21", "10.0"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("the error should name %q, got: %v", fragment, err)
		}
	}
}

// Out of order is not a formatting complaint: the first entry is the version
// the rig boots by default, so a list that is not newest-first quietly points
// the everyday loop at the oldest Controller in the matrix.
func TestVersionsThatAreNotNewestFirstAreRefused(t *testing.T) {
	_, err := loadErr(t, strings.Replace(validConfig,
		"  - \"10.5.67\"\n  - \"10.4.57\"", "  - \"10.4.57\"\n  - \"10.5.67\"", 1))
	if err == nil {
		t.Fatal("a matrix listed oldest-first was accepted")
	}
	if !strings.Contains(err.Error(), "newest first") {
		t.Errorf("the error should say how to fix it, got: %v", err)
	}
}

func TestTheSameVersionTwiceIsRefused(t *testing.T) {
	_, err := loadErr(t, strings.Replace(validConfig, `- "10.4.57"`, `- "10.5.67"`, 1))
	if err == nil {
		t.Fatal("a matrix carrying the same version twice was accepted")
	}
}

// Two rows reading the same tests would publish one result twice under
// different names, which reads as twice the evidence.
func TestTwoAreasNamingTheSameTestsAreRefused(t *testing.T) {
	_, err := loadErr(t, strings.Replace(validConfig, "tests: wlan_test.go", "tests: reconcile_test.go", 1))
	if err == nil {
		t.Fatal("two areas naming the same test file were accepted")
	}
	if !strings.Contains(err.Error(), "reconcile_test.go") {
		t.Errorf("the error should name the file, got: %v", err)
	}
}

func TestAnEmptyMatrixIsRefused(t *testing.T) {
	_, err := loadErr(t, strings.Split(validConfig, "versions:")[0]+"areas:\n  - name: Networks\n    tests: reconcile_test.go\n")
	if err == nil {
		t.Fatal("a file with no versions in it was accepted")
	}
}

const validConfig = `floor: "10.0"
container:
  image: example/controller
  database: mongo:8
versions:
  - "10.5.67"
  - "10.4.57"
  - "10.1.84"
areas:
  - name: Networks
    tests: reconcile_test.go
  - name: WLANs
    tests: wlan_test.go
`

func load(t *testing.T, body string) compat.Config {
	t.Helper()
	cfg, err := loadErr(t, body)
	if err != nil {
		t.Fatalf("loading the configuration: %v", err)
	}
	return cfg
}

func loadErr(t *testing.T, body string) (compat.Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "compatibility.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing the configuration: %v", err)
	}
	return compat.LoadConfig(path)
}
