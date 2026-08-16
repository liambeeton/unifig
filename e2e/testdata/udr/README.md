# Recorded UDR responses

These are the Controller responses the WAN suite replays (`e2e/replay_test.go`).
They exist because a WAN slot is the one thing the dockerized Controller cannot
stand in for: it has no gateway, so it ships no WAN entries at all and the
suite's usual "seed it and read it back" loop has nothing to seed onto.

One file per endpoint unifig reaches, holding the response body verbatim:

| File               | Endpoint                                            |
| ------------------ | --------------------------------------------------- |
| `sysinfo.json`     | `/proxy/network/api/s/default/stat/sysinfo`          |
| `networkconf.json` | `/proxy/network/api/s/default/rest/networkconf`      |
| `wlanconf.json`    | `/proxy/network/api/s/default/rest/wlanconf`         |

`networkconf.json` is the starting state, not a script: the replay server holds
it in memory and a `PUT` updates its copy the way the Controller updates its
own, which is what lets a test apply a change and then plan again to prove the
apply converged.

## Provenance — read this before trusting them

**These were not captured from the physical UDR.** They were recorded from the
same dockerized Controller (Network 10.0.162) the rest of the suite runs
against, and then extended by hand with the WAN slots that Controller does not
have, using field names and values taken from the Controller's own responses to
a seeded `purpose: wan` entry and from the go-unifi field spec.

So what they prove is that unifig handles the *shape* correctly — matching a
slot, diffing the fields, writing the whole entry back, keeping what it does not
model. What they do not yet prove is that a real UDR's factory WAN slots look
like this. That gap is recorded in `docs/adr/0008-wan-slots-replay-recorded-responses.md`
and closes the moment the files below are re-recorded.

## Re-recording from a real UDR

With `UNIFIG_HOST` and `UNIFIG_API_KEY` pointing at the router (the same
variables unifig itself reads), from the repo root:

```sh
for endpoint in stat/sysinfo rest/networkconf rest/wlanconf; do
  curl -sk -H "X-API-KEY: $UNIFIG_API_KEY" \
    "$UNIFIG_HOST/proxy/network/api/s/default/$endpoint" \
  | jq '(.data[] | select(has("x_wan_password")) | .x_wan_password) = "recorded-pppoe-password"
      | (.data[] | select(has("x_passphrase"))    | .x_passphrase)    = "recorded-wlan-passphrase"' \
  > "e2e/testdata/udr/$(basename $endpoint).json"
done
```

The `jq` filter is not optional. These files are committed, and a recording
straight off a router carries the PPPoE password and every WLAN passphrase on
it. Read the diff before committing anyway: a WAN slot also records the public
IP the ISP handed out, and only you can say whether that belongs in a public
repository.

The tests seed the starting values they need onto whatever the recording holds
(`replay.seedSlot`), so a re-recording does not have to be arranged to suit
them — it only has to contain the site's real WAN slots.
