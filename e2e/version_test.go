package e2e

import (
	"slices"
	"strings"
	"testing"

	"github.com/liambeeton/unifig/internal/compat"
)

// The compatibility promise where an operator actually meets it: a Controller
// running something nobody has tested unifig against.
//
// What these state is that it is a warning and only a warning. unifig has no
// evidence that an untested Controller is a broken one — the matrix is the
// versions CI can boot, not the versions Ubiquiti's API works on — so refusing
// to manage a router on that basis would be unifig inventing a fault. Every
// online verb says so once, on stderr, and then does exactly what it was asked.
//
// They run against the recorded stand-in for a reason that is not the usual
// one: the version they need is by definition one the matrix does not carry, so
// there is no container that could answer with it (replay.seedVersion).

// untestedVersion is a Controller version no matrix will ever hold. Any version
// outside the matrix would do; one that cannot be released is one no future
// entry in compatibility.yaml can quietly turn into a tested version and leave
// these tests asserting nothing.
const untestedVersion = "99.0.0"

// Every warning says this, whichever way round the untested version is.
const untestedWarning = "tested against"

// aNetworkToCreate is a change on any Controller, recorded or containerised —
// enough to prove the verb went on and did its work after the warning.
const aNetworkToCreate = `networks:
  - name: Untested Version
    vlan: 143
    subnet: 10.143.0.1/24
`

func TestPlanAgainstAnUntestedControllerWarnsAndPlansAnyway(t *testing.T) {
	r := startReplay(t)
	r.seedVersion(t, untestedVersion)

	res := planEnv(t, r.env(), configFile(t, aNetworkToCreate))

	if res.ExitCode != exitChangesPending {
		t.Fatalf("plan exited %d, want %d\nstderr: %s", res.ExitCode, exitChangesPending, res.Stderr)
	}
	assertWarnsAbout(t, res.Stderr, untestedVersion)
	if !strings.Contains(string(res.Stdout), "Untested Version") {
		t.Errorf("the plan itself should still be on stdout, got:\n%s", res.Stdout)
	}
	// The warning is not part of the plan: a pipeline reading stdout is reading
	// changes, and `plan --json` has to stay JSON.
	if strings.Contains(string(res.Stdout), untestedWarning) {
		t.Errorf("the warning belongs on stderr, not in the plan:\n%s", res.Stdout)
	}
}

func TestApplyAgainstAnUntestedControllerWarnsAndAppliesAnyway(t *testing.T) {
	r := startReplay(t)
	r.seedVersion(t, untestedVersion)

	res := applyEnv(t, r.env(), configFile(t, aNetworkToCreate))

	assertWarnsAbout(t, res.Stderr, untestedVersion)
	if !slices.Contains(r.managedNetworkNames(t), "Untested Version") {
		t.Errorf("the network was not created; the Controller holds %v", r.managedNetworkNames(t))
	}
}

func TestExportAgainstAnUntestedControllerWarnsAndExportsAnyway(t *testing.T) {
	r := startReplay(t)
	r.seedVersion(t, untestedVersion)

	res := testRig.runUnifig(t, []string{"export"}, r.env())

	if res.ExitCode != 0 {
		t.Fatalf("export exited %d\nstderr: %s", res.ExitCode, res.Stderr)
	}
	assertWarnsAbout(t, res.Stderr, untestedVersion)
	if !strings.Contains(string(res.Stdout), "networks:") {
		t.Errorf("export should still have written the config, got:\n%s", res.Stdout)
	}
}

// The other half of the promise, and the one that stops the warning from being
// noise: a Controller the matrix carries is not warned about.
//
// The version comes out of the shipped matrix rather than out of the recording.
// Those two happen to be the same today, and asking the recording would make
// this test pass on that coincidence — it would keep passing if the warning
// started treating a recorded version as a tested one, which is exactly the
// overstatement Matrix.Warning is written not to make.
func TestAControllerTheMatrixCarriesIsNotWarnedAbout(t *testing.T) {
	tested := compat.Shipped().Versions
	if len(tested) == 0 {
		t.Fatal("the binary ships no compatibility evidence, so there is no tested version to run against")
	}

	for _, version := range tested {
		t.Run(version, func(t *testing.T) {
			r := startReplay(t)
			r.seedVersion(t, version)

			res := planEnv(t, r.env(), configFile(t, aNetworkToCreate))

			if strings.Contains(string(res.Stderr), untestedWarning) {
				t.Errorf("UniFi Network %s is in the compatibility table and was warned about anyway:\n%s",
					version, res.Stderr)
			}
		})
	}
}

// assertWarnsAbout checks the whole shape of the warning: which version it is
// about, where to read more, and that unifig is carrying on regardless. The
// last of those is the point of the feature, so it is asserted rather than
// assumed from the exit code.
func assertWarnsAbout(t *testing.T, stderr []byte, version string) {
	t.Helper()
	said := string(stderr)
	for _, fragment := range []string{version, untestedWarning, "docs/COMPATIBILITY.md", "Carrying on"} {
		if !strings.Contains(said, fragment) {
			t.Errorf("the warning should mention %q, got:\n%s", fragment, said)
		}
	}
}
