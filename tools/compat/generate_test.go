// What this generator must not do is publish a claim nobody's run supports, so
// the tests are mostly about what it refuses. They need no Docker: the runs are
// fixtures here, and the real suite is read as source rather than executed.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liambeeton/unifig/internal/compat"
)

// The committed configuration is checked against the committed suite here
// rather than only in CI's matrix job. A test file nobody named would otherwise
// go unnoticed until a run that takes half an hour and needs a Docker daemon,
// and the answer — "the table would not cover these tests" — has nothing to do
// with any Controller.
func TestTheCommittedConfigurationCoversTheCommittedSuite(t *testing.T) {
	cfg, tests := committed(t)

	if err := agree(cfg, tests); err != nil {
		t.Errorf("%v", err)
	}
	for _, area := range cfg.Areas {
		if _, err := tests.evidenceFor(area); err != nil {
			t.Errorf("%v", err)
		}
	}
}

// The one area whose absence from the version columns the parent spec singles
// out (#11, ADR-0013). If a container ever ships a zone-based firewall, the
// firewall tests stop replaying a recording, this test fails, and the row moves
// into the version columns — which is the way round it should fail.
func TestTheFirewallIsEvidenceAboutARecordingRatherThanAContainer(t *testing.T) {
	cfg, tests := committed(t)

	for _, area := range cfg.Areas {
		if !strings.Contains(area.Name, "Firewall") {
			continue
		}
		evidence, err := tests.evidenceFor(area)
		if err != nil {
			t.Fatalf("%v", err)
		}
		if evidence != compat.FromRecording {
			t.Errorf("%s is %s evidence; the table would claim a tested container version for the firewall",
				area.Name, evidence)
		}
		return
	}
	t.Fatal("no area covers the firewall")
}

func TestATableIsBuiltFromTheRunsRatherThanTheConfiguration(t *testing.T) {
	matrix := built(t, map[string]results{
		"10.5.67":  ran("TestNetworks", "TestZones"),
		"10.0.162": ran("TestNetworks", "TestZones"),
	})

	if got, want := matrix.Versions, []string{"10.5.67", "10.0.162"}; !equal(got, want) {
		t.Errorf("versions = %v, want %v", got, want)
	}
	networks, zones := row(t, matrix, "Networks"), row(t, matrix, "Zones")
	if networks.Evidence != compat.FromContainer || !equal(networks.Versions, matrix.Versions) {
		t.Errorf("the container row should carry every version it passed on, got %+v", networks)
	}
	// The replayed row is evidence about the recording, whichever containers
	// happened to be booted while its tests ran.
	if zones.Evidence != compat.FromRecording || !equal(zones.Versions, []string{"10.5.67"}) {
		t.Errorf("the replayed row should be attributed to the recording, got %+v", zones)
	}
}

// A helper between the test and the stand-in must not turn a replayed test into
// a container one: that row would be published with version columns naming
// Controllers it was never near.
func TestATestThatReachesTheRecordingThroughAHelperIsStillReplayed(t *testing.T) {
	dir := t.TempDir()
	writeSource(t, filepath.Join(dir, "indirect_test.go"), `package e2e

import "testing"

func recordedController(t *testing.T) *replay { return startReplay(t) }
func aRecordedSlot(t *testing.T) string        { return recordedController(t).aSlot(t) }

func TestTwoHopsFromTheRecording(t *testing.T) { aRecordedSlot(t) }
`)
	tests, err := readSuite(dir)
	if err != nil {
		t.Fatalf("reading the suite: %v", err)
	}

	if !tests.replayed["TestTwoHopsFromTheRecording"] {
		t.Error("a test two helpers away from the recording was read as a container test")
	}
	area := compat.Area{Name: "Indirect", Tests: "indirect_test.go", Why: "no container has one"}
	evidence, err := tests.evidenceFor(area)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if evidence != compat.FromRecording {
		t.Errorf("evidence = %q, want %q", evidence, compat.FromRecording)
	}
}

// A version whose tests did not all pass is not one to publish, and neither is
// a table generated as though it had.
func TestAVersionThatDidNotPassIsRefusedRatherThanLeftOut(t *testing.T) {
	_, err := buildErr(t, map[string]results{
		"10.5.67":  ran("TestNetworks", "TestZones"),
		"10.0.162": {tests: map[string]string{"TestNetworks": failed, "TestZones": passed}},
	})
	if err == nil {
		t.Fatal("a failing run was published")
	}
	if !strings.Contains(err.Error(), "10.0.162") {
		t.Errorf("the error should name the version that failed, got: %v", err)
	}
}

func TestAVersionNobodyRanIsRefused(t *testing.T) {
	_, err := buildErr(t, map[string]results{"10.5.67": ran("TestNetworks", "TestZones")})
	if err == nil {
		t.Fatal("a table was generated for a version nobody ran")
	}
	if !strings.Contains(err.Error(), "10.0.162") {
		t.Errorf("the error should name the missing run, got: %v", err)
	}
}

// A run that never got as far as reporting tests is not a pass, however few
// failures it managed to report.
func TestARunThatBrokeIsNotAPass(t *testing.T) {
	got, err := readResults(strings.NewReader(`{"Action":"build-fail","Output":"e2e: cannot load package\n"}`), nil)
	if err != nil {
		t.Fatalf("reading results: %v", err)
	}
	if got.ok() {
		t.Error("a build failure was read as a passing run")
	}
}

func TestResultsCountTopLevelTestsAndNotTheirSubtests(t *testing.T) {
	got, err := readResults(strings.NewReader(strings.Join([]string{
		`{"Action":"run","Test":"TestNetworks"}`,
		`{"Action":"output","Test":"TestNetworks","Output":"=== RUN   TestNetworks\n"}`,
		`{"Action":"pass","Test":"TestNetworks/a_subtest"}`,
		`{"Action":"pass","Test":"TestNetworks"}`,
		`{"Action":"skip","Test":"TestSkipped"}`,
		`{"Action":"pass"}`,
	}, "\n")), nil)
	if err != nil {
		t.Fatalf("reading results: %v", err)
	}
	if len(got.tests) != 2 {
		t.Errorf("read %d tests, want 2 (the subtest is part of its parent): %v", len(got.tests), got.tests)
	}
	// A skip is not a failure — nothing disagreed — but it is not evidence
	// either, so it leaves a gap in its area's row rather than a tick.
	if !got.ok() {
		t.Error("a run with nothing failing in it was read as unpublishable")
	}
	if allPassed(got, []string{"TestSkipped"}) {
		t.Error("a skipped test was counted as evidence that the area was exercised")
	}
}

// Which Controller answered is read out of the tests themselves, so an area
// cannot be labelled by hand as something it is not.
func TestAnAreaWhoseTestsDisagreeAboutTheirControllerIsRefused(t *testing.T) {
	dir := t.TempDir()
	writeSource(t, filepath.Join(dir, "mixed_test.go"), `package e2e

import "testing"

func TestAgainstTheContainer(t *testing.T) { testRig.runUnifig(t, nil, nil) }
func TestAgainstTheRecording(t *testing.T) { r := startReplay(t); _ = r }
`)
	tests, err := readSuite(dir)
	if err != nil {
		t.Fatalf("reading the suite: %v", err)
	}

	area := compat.Area{Name: "Mixed", Tests: "mixed_test.go"}
	if _, err := tests.evidenceFor(area); err == nil {
		t.Fatal("an area whose tests use both Controllers was published as one row")
	}
}

func TestAReplayedAreaHasToSayWhyAContainerCannotAnswerForIt(t *testing.T) {
	dir := t.TempDir()
	writeSource(t, filepath.Join(dir, "replayed_test.go"), `package e2e

import "testing"

func TestAgainstTheRecording(t *testing.T) { startReplay(t) }
`)
	tests, err := readSuite(dir)
	if err != nil {
		t.Fatalf("reading the suite: %v", err)
	}

	silent := compat.Area{Name: "Replayed", Tests: "replayed_test.go"}
	if _, err := tests.evidenceFor(silent); err == nil {
		t.Error("a row absent from every version column was published with no explanation")
	}
	explained := silent
	explained.Why = "no container has one"
	if _, err := tests.evidenceFor(explained); err != nil {
		t.Errorf("an explained row was refused anyway: %v", err)
	}
}

func TestTestsNoAreaNamesAreRefusedRatherThanQuietlyUncovered(t *testing.T) {
	dir := t.TempDir()
	writeSource(t, filepath.Join(dir, "named_test.go"), "package e2e\n\nimport \"testing\"\n\nfunc TestOne(t *testing.T) {}\n")
	writeSource(t, filepath.Join(dir, "forgotten_test.go"), "package e2e\n\nimport \"testing\"\n\nfunc TestTwo(t *testing.T) {}\n")
	tests, err := readSuite(dir)
	if err != nil {
		t.Fatalf("reading the suite: %v", err)
	}

	cfg := compat.Config{Areas: []compat.Area{{Name: "Named", Tests: "named_test.go"}}}
	err = agree(cfg, tests)
	if err == nil {
		t.Fatal("a test file no area names was accepted")
	}
	if !strings.Contains(err.Error(), "forgotten_test.go") {
		t.Errorf("the error should name the file, got: %v", err)
	}
}

// The recording's version is read out of the recording, which is what makes
// re-recording enough to move the table.
func TestTheRecordedVersionComesFromTheRecording(t *testing.T) {
	recording, err := readRecording(filepath.Join("..", "..", sysinfoFile))
	if err != nil {
		t.Fatalf("reading the recording: %v", err)
	}
	if recording.Version == "" {
		t.Error("the recording does not say which Controller version it came from")
	}
}

// The table has to state the limit rather than leave it to be inferred from a
// row that is not there.
func TestTheTableSaysWhatItDoesNotCover(t *testing.T) {
	table := render(built(t, map[string]results{
		"10.5.67":  ran("TestNetworks", "TestZones"),
		"10.0.162": ran("TestNetworks", "TestZones"),
	}))

	container, recorded, found := strings.Cut(table, "## Tested against a recording")
	if !found {
		t.Fatal("the table does not separate what a container answered for from what it did not")
	}
	if strings.Contains(container, "| Zones |") {
		t.Error("the container table has a Zones row, which would claim a tested container version for it")
	}
	for _, expected := range []string{"| Zones |", "10.5.67", "no container has one"} {
		if !strings.Contains(recorded, expected) {
			t.Errorf("the recorded section should carry %q:\n%s", expected, recorded)
		}
	}
}

// committed is the repository's own configuration and suite, read from the
// tool's own directory.
func committed(t *testing.T) (compat.Config, suite) {
	t.Helper()
	cfg, err := compat.LoadConfig(filepath.Join("..", "..", configFile))
	if err != nil {
		t.Fatalf("reading the committed configuration: %v", err)
	}
	tests, err := readSuite(filepath.Join("..", "..", suiteDir))
	if err != nil {
		t.Fatalf("reading the committed suite: %v", err)
	}
	return cfg, tests
}

// fixture is a two-area suite: one area the container answers for and one the
// recording does, which is the shape the real table has.
func fixture(t *testing.T) (compat.Config, suite) {
	t.Helper()
	dir := t.TempDir()
	writeSource(t, filepath.Join(dir, "network_test.go"), `package e2e

import "testing"

func TestNetworks(t *testing.T) {}
`)
	writeSource(t, filepath.Join(dir, "zone_test.go"), `package e2e

import "testing"

func TestZones(t *testing.T) { startReplay(t) }
`)
	tests, err := readSuite(dir)
	if err != nil {
		t.Fatalf("reading the suite: %v", err)
	}

	cfg := compat.Config{
		Floor:     "10.0",
		Container: compat.Container{Image: "example/controller", Database: "mongo:8"},
		Versions:  []string{"10.5.67", "10.0.162"},
		Areas: []compat.Area{
			{Name: "Networks", Tests: "network_test.go"},
			{Name: "Zones", Tests: "zone_test.go", Why: "no container has one"},
		},
	}
	return cfg, tests
}

func built(t *testing.T, runs map[string]results) compat.Matrix {
	t.Helper()
	matrix, err := buildErr(t, runs)
	if err != nil {
		t.Fatalf("building the table: %v", err)
	}
	return matrix
}

func buildErr(t *testing.T, runs map[string]results) (compat.Matrix, error) {
	t.Helper()
	cfg, tests := fixture(t)
	return build(cfg, tests, runs, compat.Recording{Version: "10.5.67", Source: "e2e/testdata/udr"})
}

// ran is a run in which every named test passed and nothing else happened.
func ran(tests ...string) results {
	outcomes := map[string]string{}
	for _, test := range tests {
		outcomes[test] = passed
	}
	return results{tests: outcomes}
}

func row(t *testing.T, m compat.Matrix, name string) compat.Coverage {
	t.Helper()
	for _, area := range m.Areas {
		if area.Name == name {
			return area
		}
	}
	t.Fatalf("the table has no %q row", name)
	return compat.Coverage{}
}

func writeSource(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func equal(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
