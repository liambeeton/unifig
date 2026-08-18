// The warning is the only part of the compatibility promise an operator meets
// without reading anything, so what it says on each kind of untested version
// is pinned down here. What it must never do is refuse: every case below is a
// Controller unifig still talks to.
package compat_test

import (
	"strings"
	"testing"

	"github.com/liambeeton/unifig/internal/compat"
)

// tested is a matrix with a gap in the middle of it, so that "not tested" and
// "outside the range" can be told apart.
var tested = compat.Matrix{Versions: []string{"10.5.67", "10.4.57", "10.1.84", "10.0.162"}}

func TestAVersionTheMatrixCarriesIsNotWarnedAbout(t *testing.T) {
	for _, version := range tested.Versions {
		if warning := tested.Warning(version); warning != "" {
			t.Errorf("%s is in the matrix and was warned about anyway: %s", version, warning)
		}
	}
}

// The common case, and the least alarming one: the operator is ahead of the
// matrix rather than behind it. Saying which way round it is means an operator
// knows whether to expect a fix or a release.
func TestAVersionNewerThanAnythingTestedSaysSo(t *testing.T) {
	warning := tested.Warning("10.6.9")
	assertMentions(t, warning, "10.6.9", "newer", "10.5.67")
	assertCarriesOn(t, warning)
}

func TestAVersionOlderThanAnythingTestedSaysSo(t *testing.T) {
	warning := tested.Warning("9.5.21")
	assertMentions(t, warning, "9.5.21", "older", "10.0.162")
	assertCarriesOn(t, warning)
}

// A version inside the range is not covered by the ones either side of it, so
// the warning names what was actually run rather than implying a range.
func TestAVersionBetweenTwoTestedOnesNamesTheOnesThatWereTested(t *testing.T) {
	warning := tested.Warning("10.2.7")
	assertMentions(t, warning, "10.2.7", "10.5.67", "10.4.57", "10.1.84", "10.0.162")
	assertCarriesOn(t, warning)
}

// A Controller that answered with something unifig cannot read as a version is
// not a Controller unifig refuses; it is one it cannot make the promise about.
func TestAControllerThatDidNotSayItsVersionIsStillTalkedTo(t *testing.T) {
	for _, said := range []string{"", "unknown", "10.x"} {
		warning := tested.Warning(said)
		if warning == "" {
			t.Errorf("a Controller answering %q was treated as a tested version", said)
			continue
		}
		assertCarriesOn(t, warning)
	}
}

// A matrix with nothing in it makes no promise, so it has nothing to warn
// about. This is the shape the embedded evidence takes if it ever fails to
// load: quiet, rather than a tool that will not run.
func TestAnEmptyMatrixWarnsAboutNothing(t *testing.T) {
	if warning := (compat.Matrix{}).Warning("10.6.9"); warning != "" {
		t.Errorf("an empty matrix warned anyway: %s", warning)
	}
}

// The evidence the binary ships with is generated (tools/compat) and committed,
// so nothing else in this package would notice it going missing.
func TestTheEmbeddedEvidenceLoads(t *testing.T) {
	shipped := compat.Shipped()
	if len(shipped.Versions) == 0 {
		t.Fatal("the binary ships no compatibility evidence at all")
	}
	if shipped.Recording.Version == "" {
		t.Error("the evidence does not say which Controller version the recording came from")
	}
	for _, version := range shipped.Versions {
		if warning := shipped.Warning(version); warning != "" {
			t.Errorf("UniFi Network %s is in the shipped matrix and is warned about anyway: %s", version, warning)
		}
	}
	if len(shipped.Areas) == 0 {
		t.Error("the evidence covers no areas")
	}
}

// The recording's version is attribution for four rows, not a claim that the
// suite ran against that Controller — so it earns no silence of its own. The
// two coincide today, which is why this asks the question the way round that
// notices when they stop: a warning about a version outside the container
// matrix is correct even when the recording came off it.
func TestTheRecordingsVersionIsNotAMatrixVersionInItsOwnRight(t *testing.T) {
	recorded := compat.Matrix{
		Versions:  []string{"10.5.67", "10.0.162"},
		Recording: compat.Recording{Version: "10.7.4"},
	}

	if warning := recorded.Warning("10.7.4"); warning == "" {
		t.Error("a Controller outside the container matrix was not warned about, because a recording came off it")
	}
}

func assertMentions(t *testing.T, warning string, fragments ...string) {
	t.Helper()
	if warning == "" {
		t.Fatal("no warning at all")
	}
	for _, fragment := range fragments {
		if !strings.Contains(warning, fragment) {
			t.Errorf("the warning should mention %q, got: %s", fragment, warning)
		}
	}
}

// Every warning says two things beyond the version: that unifig is going on
// anyway, and where the table it is talking about lives.
func assertCarriesOn(t *testing.T, warning string) {
	t.Helper()
	if !strings.Contains(warning, "docs/COMPATIBILITY.md") {
		t.Errorf("the warning should point at the table, got: %s", warning)
	}
	if !strings.Contains(strings.ToLower(warning), "carrying on") {
		t.Errorf("the warning should say it is not stopping, got: %s", warning)
	}
}
