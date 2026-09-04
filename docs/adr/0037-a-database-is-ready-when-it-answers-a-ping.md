# A database is ready when it answers a ping, so every run is what covers the pin

The rig waited for the database's port. `mongo:8` refuses to start at all on a
Docker host running a Linux kernel of 6.19 or newer (SERVER-121912), and the
port answered for it anyway, so the rig went on to boot the Controller and
failed there five minutes later on the Controller's own startup timeout —
naming the Controller for a database that was never up (issue #60).

The rig now waits for a mongod that answers an authenticated ping at the name,
port and credentials the Controller is about to use it under. The same database
fails the rig in two seconds and names itself.

## What the port was actually saying

`wait.ForListeningPort` is three checks and none of them is "mongod is
serving":

- **the mapped port exists** — Docker's own bookkeeping, settled before the
  container has run anything;
- **an external dial to it** — answered by Docker's port publishing, which is
  up as soon as the container starts and goes on answering while whatever is
  behind it dies;
- **an internal `/dev/tcp/localhost/27017` probe** through the container's
  shell — which the official image's *first* mongod answers: the entrypoint
  starts one on loopback to create the root user before it starts the one the
  Controller talks to.

So the comment beside that wait had it exactly backwards. It said the port was
what told the two mongod runs apart, and the loopback run is precisely what the
internal half of it reaches. What kept the check from being wrong every day is
that it also gives up when the container has exited, so which answer a broken
database got came down to a race between the dial and mongod's exit — and two
runs of the same suite on the same host produced both: one container reported
ready on its way out, and one honest failure two seconds in.

## The rule

**The rig waits for a mongod that answers an authenticated ping at the name,
port and credentials the Controller is about to use.** `mongosh` is in the
image, so it is a `docker exec` and no new dependency:

```go
wait.ForExec([]string{
	"mongosh",
	"--host", databaseHost, "--port", databasePort,
	"--username", databaseUser, "--password", databasePassword,
	"--authenticationDatabase", "admin",
	"--quiet", "--eval", "quit(db.adminCommand({ping: 1}).ok === 1 ? 0 : 1)",
})
```

Every clause of it is load-bearing:

- **a ping** is answered by a mongod or by nothing at all. Docker cannot answer
  one on a dead container's behalf, which is the whole of what went wrong. Its
  answer is turned into the exit code because that is what the wait reads, and
  mongosh exits 0 for a command the server refused politely;
- **the network alias** is the name the Controller resolves, and the loopback
  mongod the entrypoint runs during initialisation is not listening for it
  (`mongod --bind_ip 127.0.0.1`, read off the running container);
- **the credentials** are the ones the Controller is handed in `MONGO_USER` and
  `MONGO_PASS`. mongosh authenticates as it connects and exits 1 when that
  fails, so a mongod serving under other credentials is not ready either;
- **an exec** needs a container that is running. One that has exited fails the
  command outright rather than politely, which is why the answer no longer
  depends on the race.

## What covers the pin

Nothing did, which is the other half of issue #60. The matrix's columns are
Controller versions and the database beside them is one pin every column
shares, so ADR-0016 recorded it as a fact rather than a row — and a database
that could not start became a five-minute timeout attributed to a Controller.

**Every run covers it.** No test in this suite is reached until the pinned
image has answered an authenticated ping as the Controller's database, so the
pin is asserted by each `make e2e` and each matrix job in the same form as
everything else here: what a run did, rather than what a file says.

That is also what there is to say about `mongo:8.2` being a moving tag, as
`mongo:8` was. Nothing holds the fix in that series. What holds is that the day
it stops serving, the next run says so in two seconds and names the image.

## Considered Options

- **Wait for `Waiting for connections` in the log** — rejected. Both mongod
  runs print it, so it needs an occurrence count, and the count depends on
  whether the data directory was already initialised: one wait meaning two
  things depending on how the container was started. A log line is also what
  mongod said rather than what it will answer.
- **Ping the mapped port from the test process** — rejected for its cost rather
  than its meaning. It is the same claim, and it puts a MongoDB driver in
  `go.mod` for the rig alone when the image already ships a shell that speaks
  the protocol.
- **Pin the database by digest** — rejected. It answers today's failure and
  takes every later fix out with it, and a digest that has gone stale fails by
  saying nothing at all. A moving tag that stops serving is now a two-second
  failure naming the image, which is the louder of the two.
- **Publish the database in the compatibility table** — not this change, and
  the option to keep. The table is generated from what the runs did, so making
  the database evidence rather than configuration means the runs reporting
  which one they used. That is a change to the generator, and it is worth
  making on the day the pin stops being one value every column shares.
- **Shorten the Controller's startup timeout so the misattribution is cheaper**
  — rejected. Five minutes was the cost of the wrong answer, not the wrong
  answer.

## Consequences

- **The database container no longer publishes a port to the host.** Nothing
  outside the private network ever spoke to it; the mapping was there for the
  check that lied. `docker exec … mongosh` reaches it for anyone debugging a
  run.
- **A database that never became ready is a container the rig shuts down.**
  `startDatabase` holds it before it returns the error, which is what
  `startRig`'s contract already promised the caller, and the Controller is held
  the same way for the same reason. The reaper would collect either at process
  exit; this is the half that does not wait for one.
- **The failure names the database and carries what it said.** testcontainers
  prints the container's own log when a wait gives up, and mongod's fatal line
  is the last thing in it, so the run that finds a database that cannot start
  ends on `starting the Controller's database (mongo:8)` with SERVER-121912
  above it. That is the first run rather than the second.
- **Two seconds against five minutes, measured**, on Docker Desktop 29.7.2 with
  kernel 7.0.12-linuxkit, 4 September 2026: `mongo:8` fails the rig 1.9s in,
  and `mongo:8.2` answers the ping 2s after its container is created, with the
  whole suite passing behind it — 292 tests on each of the five Controller
  versions in the matrix, and the generated table unchanged by any of it.
- **A database that serves the ping and then dies is not covered by any of
  this.** Nothing watches it after the rig has started, so a Controller that
  loses its database mid-suite still fails as a Controller — which is the same
  misattribution, in the one place where the Controller really is the thing
  that failed.
