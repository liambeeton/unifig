package e2e

import "testing"

// measurement is a fact a test rests on that neither Controller in this suite
// answered for.
//
// The recording is `GET`s — `make record-udr` reads one response per file and
// writes nothing — so a replayed test about a **write** rests on a live session
// against a real router, on whatever firmware it was running that day. Where
// that is the version `e2e/testdata/udr/sysinfo.json` holds there is nothing to
// say and `docs/COMPATIBILITY.md` already says it. Where it is not, the row
// would attribute the behaviour to a Controller nobody asked, and the table has
// to carry the exception rather than let the row speak for it (ADR-0036).
//
// It is a package-level declaration cited by name rather than a literal at each
// call, because one fact several tests rest on is one line in the table:
// `tools/compat` publishes each distinct measurement once, so a test added
// beside the ones already citing it changes nothing generated and needs no
// matrix run.
type measurement struct {
	// version is the Controller it was measured on.
	version string
	// what was measured, in a phrase the published table carries as written.
	what string
}

// measuredOn names the Controller a test's evidence came off, where that is not
// the one the recording came off.
//
// It asserts nothing, and there is nothing here for it to assert: the fact it
// names was established on hardware this suite cannot reach, which is the whole
// reason it has to be written down. What it does is put that provenance beside
// the test rather than in a document about it, in a form `tools/compat` reads
// out of the source the way it reads `startReplay` — so a line in the published
// table and the tests behind it cannot drift apart.
func measuredOn(t *testing.T, m measurement) {
	t.Helper()
	t.Logf("what this test asserts was measured on UniFi Network %s: %s", m.version, m.what)
}
