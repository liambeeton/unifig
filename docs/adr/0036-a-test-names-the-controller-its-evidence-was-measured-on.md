# A test names the Controller its evidence was measured on, because a recording holds no writes

> **Amended on its premise, not on its decision, on 4 September 2026.** This ADR
> argued from 10.6.101 being "a version outside the matrix entirely". It is not
> one any more: `linuxserver/unifi-network-application` publishes it, the suite
> passes against it, and `compatibility.yaml` now lists it at the head of the
> matrix. The rule below is untouched, and the reason it survives is the reason
> it was written — a container on 10.6.101 has no zone-based firewall either
> (ADR-0013), so what ADR-0033 and ADR-0035 measured still came off a physical
> router and still could not have come from anywhere the suite can boot. What
> changed is only that the version string now names two different Controllers:
> a container in the columns of the first table, and the UDR the write session
> ran against, under the second. The table is harder to read for it, and every
> sentence in it is still one a run supports.

`docs/COMPATIBILITY.md` gave the firewall row one version — 10.5.67, read out of
`e2e/testdata/udr/sysinfo.json` — and by ADR-0033 that row covered tests whose
subject had only ever been exercised on 10.6.101. The row was not wrong about the
recording and was wrong about the tests. A test now names the Controller its
evidence came off where that is not the recording's, and the table publishes the
exception beside the row rather than under it.

## What was actually the matter

The row is generated, and generated honestly: the version is read out of the
recording rather than written down beside it, so `make record-udr` against newer
firmware moves the table with it. That is the whole design of the thing, and it
holds for every `GET` in `e2e/testdata/udr`.

`make record-udr` is **read-only** — one `GET` per recorded file, and no writes at
all. So nothing the stand-in models about a **write** was ever in those files. It
came from a live session against a real router, on whatever firmware that router
was running the day somebody asked it. For ADR-0019 through ADR-0032 that was
10.5.67 and the row was accidentally right. The router was upgraded, and by
ADR-0033 and ADR-0035 it was 10.6.101 — then a version outside the matrix
entirely, against which unifig printed its own untested-version warning on every
run, and since carried as a container by the note above.

Thirteen tests in `e2e/firewall_test.go` name those two sessions. More rest on
them than that — `reconcileCompanion` in the stand-in models what ADR-0035
measured, and every test that writes an allow goes through it — which is an
argument for publishing the fact rather than a count of the tests, and the reason
the citation is per test while the row is per fact. The row said 10.5.67 about
all of them.

## The rule

**A test that rests on a measurement the recording cannot hold names the
Controller it was measured on, and the generator publishes it.** Where that is the
recording's own version there is nothing to say, and the generator refuses the
declaration rather than letting a second copy of that version sit where
`make record-udr` would leave it behind.

It is read out of the suite's source, the way `startReplay` is:

```go
var onTheReorderEndpoint = measurement{
	version: "10.6.101",
	what:    "how a policy's placement is asked for: …",
}

func TestCreatingABlockingPolicyMovesItBelowTheCompanionTier(t *testing.T) {
	measuredOn(t, onTheReorderEndpoint)
	…
```

A list in `compatibility.yaml` was the alternative and is the thing this
generator exists not to do. Every other fact in the table is read from what the
suite is and what its runs did, precisely so that a claim cannot outlive the
tests behind it; a hand-maintained exception list would be the one line in the
document nobody's run supports.

**It is declared once and cited by name.** `compat.Coverage` refuses to count an
area's tests, on the grounds that a number changing every time anybody adds a
test would put half an hour of Docker in the way of a one-line change. The same
argument reaches here: what the table is a claim about is the *fact*, so the
generator publishes each distinct measurement once, however many tests cite it,
and a fourteenth test beside the thirteen regenerates nothing.

**The generator refuses four things**, each of which would be the table saying
something the suite does not:

- a measurement on a **container** area — its versions are already the columns of
  the first table, and a second, quieter claim about the same tests is a claim
  that can disagree with them;
- a measurement naming the **recording's own version**, as above;
- a declaration **no test cites** — a line in the table with nothing behind it,
  which is `agree`'s rule about unnamed test files one level down;
- a citation of a name **nothing declares**, or a phrase that is not a string
  literal: a sentence the table printed that nobody could read out of the source.

## Considered Options

- **Probe `batch-reorder` on 10.5.67** — not rejected, and not available. It is
  the answer that would make the row true as it stood, and it needs a router on
  that firmware, which nobody has. Issue #57 offered it or this as alternatives —
  "One of" — so this closes the issue; the probe is a thing somebody may still
  want to do rather than work this leaves unfinished. What it would change is
  which half of the table the fact sits in.
- **Re-record from the 10.6.101 router** — refused, and it is worth saying why,
  because it looks like the obvious move. It would put 10.6.101 in the row and say
  nothing whatever about `batch-reorder`, because the recording holds no writes to
  carry. The row would be freshly recorded and no more true about these thirteen
  tests than it was.
- **Say nothing, on the grounds that the recording's version is honest about the
  recording** — rejected. It is honest about the files and the row is not about
  the files, it is about an area and the tests behind it. A reader with a 10.5.67
  router takes that cell as covering the firewall, which is exactly the reading
  the whole document is built to support.
- **Attribute per test, naming them in the table** — rejected on `compat.Coverage`'s
  own argument. The names would churn the generated files on every test added.
- **Split the firewall into two areas** — rejected. An area is a body of behaviour
  with a test file behind it, and "the firewall, but only the placement of it" is
  not one; it would also mean moving tests between files to keep the row honest,
  which is a filing decision driven by a document.

## Consequences

- **The firewall row now points at its own exception**, and reads
  `10.5.67, and 10.6.101 where stated below`. Two lines under it say what was
  measured there, in the words of ADR-0033 and ADR-0035.
- **The table says what the exception does not mean.** Nothing here claims
  `batch-reorder` is absent below 10.6.101 — only that nobody has asked it there.
  That is the narrower claim and the true one, and it is the difference between a
  version gate and an untested version.
- **Whether `batch-reorder` exists on any Controller below 10.6.101 is still not
  established, and this ADR does not establish it.** go-unifi v2.3.0 exposes it,
  which says its authors met it somewhere and nothing about which firmware. If a
  probe ever finds it missing on 10.5.67, unifig's placement fix is version-gated
  and this table is where that would have to be said — a different sentence from
  the one published here, which claims only that nobody has asked.
- **`measuredOn` asserts nothing at runtime.** There is nothing for it to assert:
  the fact it names was established on hardware this suite cannot reach, which is
  the reason it has to be written down at all. It logs the provenance under
  `go test -v` and is otherwise read by `tools/compat`.
- **The stand-in is where these facts live, and this does not change that.** The
  prose in `e2e/replay_test.go` and `e2e/firewall_test.go` remains the account of
  what was measured; a `measurement` is the one line of it the published table
  needs, in a form a generator can carry.
- **Nothing about the untested-version warning moves.** It asks the container
  matrix and only that (`compat.Matrix.Warning`). A measurement is evidence about
  a replayed area, and letting one into that set would be the warning going quiet
  for a Controller the container areas were never run against.
- **The warning has since gone quiet for 10.6.101, and by the route this bullet
  allows.** The version is in the container matrix because the whole suite was
  run against a container on it, which is the only thing that set has ever
  admitted. The measurements below are unchanged and were not consulted: had the
  version arrived that way instead, the warning would have been claiming a run
  nobody performed.
