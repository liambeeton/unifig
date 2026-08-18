package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
)

// Outcomes a test can have. A skipped test is kept apart from a passing one
// deliberately: the table's claim is that the tests ran, and a skip is a test
// that did not.
const (
	passed  = "pass"
	failed  = "fail"
	skipped = "skip"
)

// results is what one run of the suite did — one Controller version's worth of
// evidence, read from `go test -json`.
type results struct {
	// tests maps a top-level test name to how it went. Subtests are not in
	// here: a failing subtest fails its parent, so the parent is the whole
	// answer, and the areas are counted in whole tests.
	tests map[string]string
	// broken is a run that did not get as far as reporting tests — a build
	// failure, a rig that would not start, a panic in TestMain. It is not the
	// same as a failing test, and neither is publishable.
	broken bool
}

// event is one line of `go test -json`.
type event struct {
	Action string `json:"Action"`
	Test   string `json:"Test"`
	Output string `json:"Output"`
}

// readResults reads a `go test -json` stream. Anything the stream prints goes
// to echo, which is how a run stays readable while it is happening; pass nil
// to read a file that has already been written.
func readResults(r io.Reader, echo io.Writer) (results, error) {
	got := results{tests: map[string]string{}}

	scanner := bufio.NewScanner(r)
	// Test output arrives one line per event, but a single line can be long —
	// a plan with every network on it, printed by a failing assertion.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		var e event
		if err := json.Unmarshal(line, &e); err != nil {
			// A line that is not an event is the toolchain talking directly —
			// a compile error, most often. Worth showing, and worth failing on.
			if echo != nil {
				_, _ = fmt.Fprintf(echo, "%s\n", line)
			}
			got.broken = true
			continue
		}
		if e.Output != "" && echo != nil {
			_, _ = io.WriteString(echo, e.Output)
		}
		switch {
		case e.Action == "build-fail":
			got.broken = true
		case e.Test == "" || strings.Contains(e.Test, "/"):
			// Package-level and subtest events. A package that fails without a
			// failing test in it is a run that broke rather than one that
			// disagreed.
			if e.Action == failed && e.Test == "" && !got.anyFailed() {
				got.broken = true
			}
		case e.Action == passed, e.Action == failed, e.Action == skipped:
			got.tests[e.Test] = e.Action
		}
	}
	if err := scanner.Err(); err != nil {
		return results{}, fmt.Errorf("reading the test results: %w", err)
	}
	if len(got.tests) == 0 {
		got.broken = true
	}
	return got, nil
}

func (r results) anyFailed() bool {
	for _, outcome := range r.tests {
		if outcome == failed {
			return true
		}
	}
	return false
}

// ok is a run that can be published: it ran, and everything in it passed.
func (r results) ok() bool { return !r.broken && !r.anyFailed() }

// summary is what a run is worth saying about it on one line.
func (r results) summary() string {
	counts := map[string]int{}
	for _, outcome := range r.tests {
		counts[outcome]++
	}
	switch {
	case r.broken && len(r.tests) == 0:
		return "the suite did not run"
	case r.anyFailed():
		return fmt.Sprintf("%d passed, %d failed: %s", counts[passed], counts[failed], strings.Join(r.failures(), ", "))
	case r.broken:
		return fmt.Sprintf("%d passed, but the run did not finish cleanly", counts[passed])
	case counts[skipped] > 0:
		return fmt.Sprintf("%d passed, %d skipped", counts[passed], counts[skipped])
	default:
		return fmt.Sprintf("%d passed", counts[passed])
	}
}

func (r results) failures() []string {
	var names []string
	for name, outcome := range r.tests {
		if outcome == failed {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}
