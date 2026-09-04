# The compatibility matrix needs an image that is still published, so the suite moved off jacobalberty

Issue #11 asks for CI to run the whole process-level suite "against multiple
dockerized UniFi Network versions (floor 10.0)". The suite ran against
`jacobalberty/unifi`, which ADR-0013 already noted had stopped at v10.0.162.
That is not merely the newest tag it publishes: **it is the only tag it
publishes at or above 10.0.** Checked on 18 August 2026, the whole tag list
above the floor is `v10.0.162` and the moving aliases `v10.0`, `v10` and
`latest`, which are the same image under other names. The image's last push was
December 2025.

So the matrix issue #11 asks for cannot be built out of that image at all. A
matrix of one is not a matrix, and a matrix of `v10.0.162` and `v10.0` is one
version listed twice.

`linuxserver/unifi-network-application` publishes the same Network application,
was still publishing this month, and has a tag for every release above the
floor — 10.0.160, 10.0.162, 10.1.84, 10.4.57, 10.5.67. The suite now boots that
image, and the matrix in `compatibility.yaml` is four of those versions.

## What actually changed, and what did not

The Network application is Ubiquiti's, unpacked from the same `.deb` by both
images. What differs is the packaging, and only two things about it reach this
repository:

- **The application does not ship a database.** `jacobalberty/unifi` bundles
  MongoDB inside the container; this one expects one beside it. The rig starts a
  `mongo:8` container on a private network of its own and points the Controller
  at it (`e2e/rig_test.go`). Both go away with the suite.
- **Demo mode is written in a different place.** `is_simulation` and the demo
  counts still go into `system.properties` — that is the application's own
  setting, not the image's — but the file lives at `/config/data` and the image
  writes the database half of it itself, from the `MONGO_*` variables, unless it
  is already there. Nothing orders that against the `/custom-cont-init.d` script
  the rig mounts, so the script writes whichever half is missing and leaves the
  other alone (`e2e/testdata/demo-mode`).

Nothing about unifig, the proxy that emulates UniFi OS, or a single test
changed. The suite was run against `linuxserver/unifi-network-application` at
10.0.162 — the same Network version the old image pinned — before anything else
in the matrix, so the move is checked against the version it replaced rather
than only against newer ones. All 154 tests pass on each of the four versions.

## The firewall is still not there, at any version

The interesting thing the migration made cheap to check: **10.5.67 in a
container still has no zone-based firewall.** `firewall/zone` and
`firewall-policies` both answer `200 []` on a freshly created site, five minor
versions past the container ADR-0013 was written against. The feature is
provisioned by an adopted gateway and demo mode's simulated one is not adopted,
so this is not a version that was going to fix it — which is worth knowing,
because ADR-0013 left "wait for a newer container image" open as the thing that
might one day move Zones and Policies onto the container side. A newer image was
not it.

The compatibility table therefore says what ADR-0013 predicted it would have to:
the firewall rows carry the version of the recording, and no container version
at all.

## Considered Options

- **Ship a matrix of one and say the image cannot supply more** — rejected, but
  it is the honest fallback and worth stating. It fails issue #11's first
  acceptance criterion outright, and the reason would have been somebody else's
  release schedule rather than anything about unifig.
- **Add `v10.0`, `v10` and `latest` as extra matrix entries** — rejected as
  dishonest. They resolve to the digest `v10.0.162` already names, so the table
  would show four columns of the same run.
- **Lower the floor to reach jacobalberty's 9.x tags** — rejected: the floor is
  the promise, and moving it to make a matrix fit is the tail wagging the dog.
- **Run both images** — rejected as cost without evidence. It doubles the rig's
  container recipes to test the same application twice at the one version they
  overlap on, and that overlap has already been checked once, here.
- **Wait for jacobalberty to publish again** — rejected. Eight months without a
  tag is not a release cadence to plan a test suite around.

## Consequences

- The version pin left `e2e/rig_test.go`. A bare `make e2e` boots the newest
  version in `compatibility.yaml`, so the everyday loop and the matrix cannot
  drift, and the README no longer has to name two places a version lives.
- The suite now needs two containers rather than one, and a network. On a
  machine with a Docker daemon that is invisible; in CI it is one more image to
  pull per job.
- `mongo:8` is a pin the matrix does not cover: every Controller version in the
  table was tested against that one database. A Network version that wanted a
  different MongoDB would be a change to `container.database` and a fact worth
  recording here rather than a row in the table.
  **It moved to `mongo:8.2` on 4 September 2026**, and not for a Network version:
  `mongo:8` refuses to start at all on a Docker host running a Linux kernel of
  6.19 or newer (SERVER-121912), which took the whole suite with it — the
  Controller came up, could not reach `MONGO_HOST`, and timed out five minutes
  later pointing at itself. Every version in the table was re-run against 8.2 and
  passed. That this pin was covered by nothing, and that the rig's port check
  reported a dying database as ready, was issue #60. Both are ADR-0037: the rig
  waits for a mongod that answers a ping, which makes every run the thing that
  covers the pin.
- The recording is untouched by any of this. It came off a physical UDR
  (ADR-0011) and says nothing about which container the rest of the suite boots.
