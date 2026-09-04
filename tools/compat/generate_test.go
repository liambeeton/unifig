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
		"10.5.67":  ran("TestNetworks", "TestZones", "TestZonesAgain"),
		"10.0.162": ran("TestNetworks", "TestZones", "TestZonesAgain"),
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
		"10.5.67":  ran("TestNetworks", "TestZones", "TestZonesAgain"),
		"10.0.162": {tests: map[string]string{"TestNetworks": failed, "TestZones": passed, "TestZonesAgain": passed}},
	})
	if err == nil {
		t.Fatal("a failing run was published")
	}
	if !strings.Contains(err.Error(), "10.0.162") {
		t.Errorf("the error should name the version that failed, got: %v", err)
	}
}

func TestAVersionNobodyRanIsRefused(t *testing.T) {
	_, err := buildErr(t, map[string]results{"10.5.67": ran("TestNetworks", "TestZones", "TestZonesAgain")})
	if err == nil {
		t.Fatal("a table was generated for a version nobody ran")
	}
	if !strings.Contains(err.Error(), "10.0.162") {
		t.Errorf("the error should name the missing run, got: %v", err)
	}
}

// A test the suite holds that the run never mentions is stale evidence, not a
// failure. Adding a test and generating the table before the suite has been run
// again would otherwise drop the version from that test's row — publishing a
// narrower claim than the runs support, with nothing said about why.
func TestATestTheRunNeverMentionsIsRefusedRatherThanReadAsAFailure(t *testing.T) {
	_, err := buildErr(t, map[string]results{
		"10.5.67":  ran("TestNetworks", "TestZones", "TestZonesAgain"),
		"10.0.162": ran("TestNetworks", "TestZones"),
	})
	if err == nil {
		t.Fatal("a table was generated from results that predate a test in the suite")
	}
	if !strings.Contains(err.Error(), "TestZonesAgain") {
		t.Errorf("the error should name the test the run never mentions, got: %v", err)
	}
	if !strings.Contains(err.Error(), "10.0.162") {
		t.Errorf("the error should name the version whose run is stale, got: %v", err)
	}
}

// A skip is a test that did not run, but the run is still the one this suite
// asked for: it is the table's business (the row loses the version), not a
// reason to refuse the whole run as stale.
func TestASkippedTestIsNotAStaleRun(t *testing.T) {
	matrix := built(t, map[string]results{
		"10.5.67":  ran("TestNetworks", "TestZones", "TestZonesAgain"),
		"10.0.162": {tests: map[string]string{"TestNetworks": skipped, "TestZones": passed, "TestZonesAgain": passed}},
	})
	if got, want := row(t, matrix, "Networks").Versions, []string{"10.5.67"}; !equal(got, want) {
		t.Errorf("Networks versions = %v, want %v", got, want)
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
		"10.5.67":  ran("TestNetworks", "TestZones", "TestZonesAgain"),
		"10.0.162": ran("TestNetworks", "TestZones", "TestZonesAgain"),
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

// A fact about a write, which is the one thing a recording of GETs cannot hold.
var onTheReorderEndpoint = measurement{
	version: "10.6.101",
	what:    "the reorder endpoint, in a live write session",
}

func TestZones(t *testing.T) { startReplay(t); measuredOn(t, onTheReorderEndpoint) }

func TestZonesAgain(t *testing.T) { startReplay(t); measuredOn(t, onTheReorderEndpoint) }
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
	return build(cfg, tests, runs, recorded)
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

// A recording holds `GET`s and nothing else, so what a replayed test asserts
// about a *write* was measured somewhere the recording cannot speak for. The
// test names that Controller itself, which is what stops the row's recorded
// version from being read as covering it.
func TestAReplayedTestNamesTheControllerItsEvidenceWasMeasuredOn(t *testing.T) {
	cfg, tests := fixture(t)

	got, err := tests.measurementsFor(area(t, cfg, "Zones"), compat.FromRecording, recorded)
	if err != nil {
		t.Fatalf("reading the measurements: %v", err)
	}
	// Two tests cite the one measurement, and the table publishes it once: what
	// the row is a claim about is the fact, not how many tests rest on it.
	if len(got) != 1 {
		t.Fatalf("read %d measurements, want the one both tests cite: %+v", len(got), got)
	}
	if got[0].Version != "10.6.101" {
		t.Errorf("measured on %q, want the version the test names", got[0].Version)
	}
	if !strings.Contains(got[0].What, "reorder") {
		t.Errorf("the measurement should say what was measured, got %q", got[0].What)
	}
}

// The same trap as the recording itself: a test reaching its declaration
// through a helper is still a test resting on it, and reading only direct calls
// would publish a row whose exception nobody had stated.
func TestAMeasurementCitedThroughAHelperIsStillTheTestsEvidence(t *testing.T) {
	tests := read(t, "indirect_test.go", `package e2e

import "testing"

var onAWrite = measurement{
	version: "10.6.101",
	what:    "a write endpoint, in a session on newer firmware",
}

func aMeasuredReorder(t *testing.T) { measuredOn(t, onAWrite) }

func TestTwoHopsFromTheMeasurement(t *testing.T) { startReplay(t); aMeasuredReorder(t) }
`)

	if got := tests.measured["TestTwoHopsFromTheMeasurement"]; len(got) != 1 {
		t.Fatalf("a test one helper away from its measurement carries %d, want 1", len(got))
	}
}

// A container area's evidence is the run, on a version the table names in its
// own column. A measurement there would be a second, quieter claim about the
// same tests.
func TestAContainerAreaCannotNameAMeasurement(t *testing.T) {
	tests := read(t, "container_test.go", `package e2e

import "testing"

var onSomethingElse = measurement{
	version: "10.6.101",
	what:    "a write endpoint, in a session on newer firmware",
}

func TestAgainstTheContainer(t *testing.T) { measuredOn(t, onSomethingElse) }
`)

	area := compat.Area{Name: "Container", Tests: "container_test.go"}
	if _, err := tests.measurementsFor(area, compat.FromContainer, recorded); err == nil {
		t.Error("a container area published a measurement, which its version columns already answer for")
	}
}

// Naming the recording's own version is not an exception to the row, it is the
// row — and a second copy of it is what goes stale when `make record-udr` moves
// the first.
func TestAMeasurementNamingTheRecordingsOwnVersionIsRefused(t *testing.T) {
	tests := read(t, "same_test.go", `package e2e

import "testing"

var onTheRecordingsOwnVersion = measurement{
	version: "10.5.67",
	what:    "a write endpoint, on the firmware the recording came off",
}

func TestAgainstTheRecording(t *testing.T) { startReplay(t); measuredOn(t, onTheRecordingsOwnVersion) }
`)

	area := compat.Area{Name: "Same", Tests: "same_test.go", Why: "no container has one"}
	_, err := tests.measurementsFor(area, compat.FromRecording, recorded)
	if err == nil {
		t.Fatal("a measurement restating the recording's own version was published beside it")
	}
	if !strings.Contains(err.Error(), recorded.Version) {
		t.Errorf("the error should name the version it duplicates, got: %v", err)
	}
}

// A declaration nothing cites is a claim in the table with no test behind it,
// which is the whole failure this generator exists to prevent — the same rule
// as a test file no area names, one level down.
func TestAMeasurementNobodyCitesIsRefused(t *testing.T) {
	_, err := readSuite(sourceDir(t, "stale_test.go", `package e2e

import "testing"

var onSomethingNobodyTests = measurement{
	version: "10.6.101",
	what:    "an endpoint no test rests on",
}

func TestAgainstTheRecording(t *testing.T) { startReplay(t) }
`))
	if err == nil {
		t.Fatal("a measurement no test cites was accepted")
	}
	if !strings.Contains(err.Error(), "onSomethingNobodyTests") {
		t.Errorf("the error should name the declaration, got: %v", err)
	}
}

func TestAMeasurementCitedByNameNothingDeclaresIsRefused(t *testing.T) {
	_, err := readSuite(sourceDir(t, "unknown_test.go", `package e2e

import "testing"

func TestAgainstTheRecording(t *testing.T) { startReplay(t); measuredOn(t, onNothingAtAll) }
`))
	if err == nil {
		t.Fatal("a citation of a measurement nothing declares was accepted")
	}
	if !strings.Contains(err.Error(), "onNothingAtAll") {
		t.Errorf("the error should name what was cited, got: %v", err)
	}
}

// The published table has to carry the exception, or the narrowing happened
// only in the suite.
func TestTheTableSaysWhatAReplayedTestWasMeasuredOn(t *testing.T) {
	table := render(built(t, map[string]results{
		"10.5.67":  ran("TestNetworks", "TestZones", "TestZonesAgain"),
		"10.0.162": ran("TestNetworks", "TestZones", "TestZonesAgain"),
	}))

	recorded, measured, found := strings.Cut(table, "### What a replayed test was measured on")
	if !found {
		t.Fatal("the table says nothing about the tests whose evidence is not the recording's")
	}
	// The row above must not be left to speak for them on its own.
	if !strings.Contains(recorded, "10.6.101") {
		t.Errorf("the Zones row does not point at its exception:\n%s", recorded)
	}
	// And the column it sits in must not call it recorded, which it was not.
	if strings.Contains(recorded, "Recorded Controller version") {
		t.Errorf("a measured version is published under a heading calling it recorded:\n%s", recorded)
	}
	for _, expected := range []string{"| Zones |", "10.6.101", "reorder"} {
		if !strings.Contains(measured, expected) {
			t.Errorf("the measured section should carry %q:\n%s", expected, measured)
		}
	}
}

// An area with nothing to except carries no such section, so the table does not
// grow a heading that says "none".
func TestATableWithNothingMeasuredElsewhereSaysNothingAboutIt(t *testing.T) {
	cfg, tests := fixture(t)
	tests.measured = map[string][]compat.Measurement{}

	matrix, err := build(cfg, tests, map[string]results{
		"10.5.67":  ran("TestNetworks", "TestZones", "TestZonesAgain"),
		"10.0.162": ran("TestNetworks", "TestZones", "TestZonesAgain"),
	}, recorded)
	if err != nil {
		t.Fatalf("building the table: %v", err)
	}
	if strings.Contains(render(matrix), "### What a replayed test was measured on") {
		t.Error("a table with no measurements still carries the section")
	}
}

// The committed suite's own declarations, checked without Docker for the reason
// TestTheCommittedConfigurationCoversTheCommittedSuite is.
func TestTheCommittedSuitesMeasurementsArePublishable(t *testing.T) {
	cfg, tests := committed(t)
	recording, err := readRecording(filepath.Join("..", "..", sysinfoFile))
	if err != nil {
		t.Fatalf("reading the recording: %v", err)
	}

	for _, area := range cfg.Areas {
		evidence, err := tests.evidenceFor(area)
		if err != nil {
			continue // The area's own test says so.
		}
		if _, err := tests.measurementsFor(area, evidence, recording); err != nil {
			t.Errorf("%v", err)
		}
	}
}

// recorded is the hardware the fixture's replayed area is evidence about.
var recorded = compat.Recording{Version: "10.5.67", Source: "e2e/testdata/udr"}

func area(t *testing.T, cfg compat.Config, name string) compat.Area {
	t.Helper()
	for _, area := range cfg.Areas {
		if area.Name == name {
			return area
		}
	}
	t.Fatalf("the configuration has no %q area", name)
	return compat.Area{}
}

// sourceDir is a suite of one file, for the cases where reading it is what is
// under test.
func sourceDir(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	writeSource(t, filepath.Join(dir, name), body)
	return dir
}

func read(t *testing.T, name, body string) suite {
	t.Helper()
	tests, err := readSuite(sourceDir(t, name, body))
	if err != nil {
		t.Fatalf("reading the suite: %v", err)
	}
	return tests
}

// The phrase the table prints is a paragraph, so it is written the way every
// other paragraph in this codebase is — literals joined with `+` to stay inside
// a line length. Reading only the first one would publish a sentence that stops
// halfway.
func TestAMeasurementWrittenAcrossSeveralLinesIsReadWhole(t *testing.T) {
	tests := read(t, "joined_test.go", `package e2e

import "testing"

var onAWrite = measurement{
	version: "10.6.101",
	what: "the reorder endpoint, which takes two lists" +
		" and assigns the indices itself",
}

func TestAgainstTheRecording(t *testing.T) { startReplay(t); measuredOn(t, onAWrite) }
`)

	got := tests.measured["TestAgainstTheRecording"]
	if len(got) != 1 {
		t.Fatalf("read %d measurements, want 1", len(got))
	}
	if want := "the reorder endpoint, which takes two lists and assigns the indices itself"; got[0].What != want {
		t.Errorf("what = %q, want %q", got[0].What, want)
	}
}

// A phrase the generator would have to run the suite to know is one nobody can
// check against the tests it is published beside.
func TestAMeasurementThatIsNotWrittenDownIsRefused(t *testing.T) {
	_, err := readSuite(sourceDir(t, "computed_test.go", `package e2e

import "testing"

func what() string { return "something worked out at runtime" }

var onAWrite = measurement{
	version: "10.6.101",
	what:    what(),
}

func TestAgainstTheRecording(t *testing.T) { startReplay(t); measuredOn(t, onAWrite) }
`))
	if err == nil {
		t.Fatal("a measurement whose phrase is computed was accepted")
	}
	if !strings.Contains(err.Error(), "what") {
		t.Errorf("the error should name the field it could not read, got: %v", err)
	}
}

// The shapes the reader does not read. Each was written as a measurement and
// each used to vanish without a word, which is worse than either publishing or
// refusing it: the slice case took its exception with it and left the row
// claiming the recording's version for tests nobody had measured there.
func TestAMeasurementDeclaredInAShapeTheGeneratorCannotReadIsRefused(t *testing.T) {
	for _, declaration := range []struct {
		name string
		body string
	}{
		{"a slice of them", `var onWrites = []measurement{{
	version: "10.6.101",
	what:    "the reorder endpoint",
}}`},
		{"a pointer to one", `var onAWrite = &measurement{
	version: "10.6.101",
	what:    "the reorder endpoint",
}`},
		{"a zero value", "var onAWrite measurement"},
	} {
		t.Run(declaration.name, func(t *testing.T) {
			_, err := readSuite(sourceDir(t, "shape_test.go", `package e2e

import "testing"

`+declaration.body+`

func TestAgainstTheRecording(t *testing.T) { startReplay(t) }
`))
			if err == nil {
				t.Fatal("a measurement the generator cannot read was accepted, and the exception it carries dropped")
			}
			if !strings.Contains(err.Error(), "onAWrite") && !strings.Contains(err.Error(), "onWrites") {
				t.Errorf("the error should name the declaration, got: %v", err)
			}
		})
	}
}

// A `var` that has nothing to do with a measurement is not this reader's
// business, and refusing one would make every fixture in the suite illegal.
func TestAnOrdinaryDeclarationIsLeftAlone(t *testing.T) {
	tests := read(t, "ordinary_test.go", `package e2e

import "testing"

var policies = []string{"Gibson to Ellingson"}

var seeded = map[string]bool{"Dmz": true}

func TestAgainstTheRecording(t *testing.T) { startReplay(t); _ = policies; _ = seeded }
`)

	if got := len(tests.measured); got != 0 {
		t.Errorf("read %d measurements out of a file with none", got)
	}
}
