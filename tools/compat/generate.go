package main

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/liambeeton/unifig/internal/compat"
)

// build turns what the runs did into what the table publishes.
//
// Everything here is a join rather than a claim: an area is in the matrix
// because its tests are in the file it names and they passed on that version,
// and a version is in the matrix because nothing failed on it. The one thing
// configuration decides is what the rows are called.
func build(cfg compat.Config, s suite, runs map[string]results, recording compat.Recording) (compat.Matrix, error) {
	if err := agree(cfg, s); err != nil {
		return compat.Matrix{}, err
	}

	matrix := compat.Matrix{
		Image:     cfg.Container.Image,
		Floor:     cfg.Floor,
		Recording: recording,
	}
	for _, version := range cfg.Versions {
		run, ok := runs[version]
		if !ok {
			return compat.Matrix{}, fmt.Errorf("no results for UniFi Network %s; the matrix says CI runs the suite"+
				" against it, so a table generated without that run would claim a version nobody tested", version)
		}
		if !run.ok() {
			return compat.Matrix{}, fmt.Errorf("the run against UniFi Network %s did not pass (%s), so the table"+
				" cannot publish it: fix it, or take the version out of compatibility.yaml", version, run.summary())
		}
		matrix.Versions = append(matrix.Versions, version)
	}

	for _, area := range cfg.Areas {
		tests := s.testsByFile[area.Tests]
		evidence, err := s.evidenceFor(area)
		if err != nil {
			return compat.Matrix{}, err
		}

		measurements, err := s.measurementsFor(area, evidence, recording)
		if err != nil {
			return compat.Matrix{}, err
		}

		row := compat.Coverage{
			Name:         area.Name,
			Tests:        area.Tests,
			Evidence:     evidence,
			Why:          area.Why,
			Measurements: measurements,
		}
		for _, version := range matrix.Versions {
			if allPassed(runs[version], tests) {
				row.Versions = append(row.Versions, version)
			}
		}
		if evidence == compat.FromRecording {
			// The replayed areas answer the same recording whichever container
			// the rest of the suite booted, so the versions they passed against
			// say nothing about them. What they are evidence about is the
			// router the recording came from — and only if they actually
			// passed, everywhere they ran.
			passedEverywhere := len(row.Versions) == len(matrix.Versions) && len(matrix.Versions) > 0
			row.Versions = nil
			if passedEverywhere {
				row.Versions = []string{recording.Version}
			}
		}
		matrix.Areas = append(matrix.Areas, row)
	}

	return matrix, nil
}

// evidenceFor says which kind of Controller answered an area's tests, and
// refuses an area whose tests do not agree with each other. A row that mixed
// the two would be published under one version column while half of it was
// never near that version.
func (s suite) evidenceFor(area compat.Area) (compat.Evidence, error) {
	tests := s.testsByFile[area.Tests]
	replayed := 0
	for _, test := range tests {
		if s.replayed[test] {
			replayed++
		}
	}
	switch {
	case replayed == 0:
		if area.Why != "" {
			return "", fmt.Errorf("the area %q is tested against the container, so the note explaining why it"+
				" cannot be would not be published; remove `why` from compatibility.yaml", area.Name)
		}
		return compat.FromContainer, nil
	case replayed == len(tests):
		if area.Why == "" {
			return "", fmt.Errorf("the area %q is tested against the recording rather than a container, and"+
				" compatibility.yaml does not say why; add a `why` for it, because the table has to tell an"+
				" operator what its absence from the version columns means", area.Name)
		}
		return compat.FromRecording, nil
	default:
		return "", fmt.Errorf("%d of the %d tests in %s use the recording and the rest use the container, so the"+
			" area %q cannot be one row: split it, or move the odd tests out",
			replayed, len(tests), area.Tests, area.Name)
	}
}

// measurementsFor is what an area's tests rest on that its own Controller could
// not answer for: the facts measured in a live write session on firmware the
// recording is not of (ADR-0036).
//
// The row publishes each one once, however many tests cite it. What the table
// is a claim about is the fact rather than the tests, so adding a test beside
// the ones already resting on it leaves this generated file alone — which is
// what keeps half an hour of Docker out of the way of a one-line test
// (compat.Coverage).
func (s suite) measurementsFor(area compat.Area, evidence compat.Evidence, recording compat.Recording) ([]compat.Measurement, error) {
	var measurements []compat.Measurement
	for _, test := range s.testsByFile[area.Tests] {
		for _, measurement := range s.measured[test] {
			if evidence == compat.FromContainer {
				return nil, fmt.Errorf("the test %s names a Controller its evidence was measured on, and the area"+
					" %q is tested against the container — whose versions the table already publishes in its own"+
					" columns; take the %s out, or move the test to an area the container cannot answer for",
					test, area.Name, measuredOn)
			}
			if measurement.Version == recording.Version {
				return nil, fmt.Errorf("the test %s says its evidence was measured on UniFi Network %s, which is"+
					" the version the recording is already attributed to; take the %s out, because a second copy"+
					" of that version is one `make record-udr` would leave behind",
					test, measurement.Version, measuredOn)
			}
			if !slices.Contains(measurements, measurement) {
				measurements = append(measurements, measurement)
			}
		}
	}
	// Newest first, as the version columns are, and by what was measured where
	// two came off the same firmware — so the published order is the facts'
	// rather than the order tests happen to be sorted in.
	slices.SortFunc(measurements, func(a, b compat.Measurement) int {
		if by := compat.CompareVersions(b.Version, a.Version); by != 0 {
			return by
		}
		return strings.Compare(a.What, b.What)
	})
	return measurements, nil
}

// agree checks the configuration against the suite in both directions: every
// area names tests that exist, and every test file is named by an area. The
// second half is the one that matters — without it, a new area of behaviour
// could be tested for months without ever reaching the table.
func agree(cfg compat.Config, s suite) error {
	claimed := map[string]bool{}
	for _, area := range cfg.Areas {
		if len(s.testsByFile[area.Tests]) == 0 {
			return fmt.Errorf("the area %q names %s, which holds no tests", area.Name, area.Tests)
		}
		claimed[area.Tests] = true
	}
	var missing []string
	for _, file := range s.files() {
		if !claimed[file] {
			missing = append(missing, file)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s holds tests no area in compatibility.yaml names, so the table would not cover"+
			" them: %s", suiteDir, strings.Join(missing, ", "))
	}
	return nil
}

func allPassed(run results, tests []string) bool {
	if len(tests) == 0 {
		return false
	}
	for _, test := range tests {
		if run.tests[test] != passed {
			return false
		}
	}
	return true
}

// readRecording asks the committed recording which Controller version it came
// off. Nothing configures this: the recording is the evidence, so the version
// it holds is the one the table attributes the replayed areas to, and
// re-recording from a router on a different firmware moves the table with it.
func readRecording(path string) (compat.Recording, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return compat.Recording{}, fmt.Errorf("reading the recording's system information: %w", err)
	}
	var answered struct {
		Data []struct {
			Version string `json:"version"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &answered); err != nil {
		return compat.Recording{}, fmt.Errorf("reading %s: %w", path, err)
	}
	if len(answered.Data) == 0 || answered.Data[0].Version == "" {
		return compat.Recording{}, fmt.Errorf("%s does not say which Controller version it was recorded from", path)
	}
	return compat.Recording{Version: answered.Data[0].Version, Source: recordingDir}, nil
}
