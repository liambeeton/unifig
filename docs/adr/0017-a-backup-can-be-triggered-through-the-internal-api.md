# A backup can be triggered through the Internal API, and it is the synchronous command that can say so

Issue #12 asked a question before it asked for a feature: can unifig have the Controller back itself up before an apply changes anything? It can, on a real UDR, through the same API key everything else here goes through. This records what was found, because a finding is the deliverable either way — a "no" would have closed the ticket, and a "yes" nobody wrote down would be a feature resting on somebody's memory of an afternoon.

**The command.** `POST /proxy/network/api/s/default/cmd/backup`, with `{"cmd": "backup", "days": 0}`, answers:

```
200 {"meta":{"rc":"ok"},"data":[{"url":"/dl/backup/10.5.67.unf"}]}
```

`days` is how much of the site's traffic history to fold in, and 0 is none of it. What unifig wants backed up is the configuration, because the configuration is what an apply can break.

**It is synchronous, and that is the part that matters.** On the router the call took 2.4 seconds, and the file at the URL it named grew from 273,184 to 278,592 bytes across it: the response is the Controller saying the backup is written, not that it has been started. The Controller also has an asynchronous form of the same command, and `async-backup` is precisely what unifig cannot use — it answers `200` with an empty `data`, naming no file, so there is nothing to confirm and nothing to tell an operator to restore from. "Trigger a backup and confirm it completed" is answerable at all because the first form exists.

**The file comes back through the same gate.** `GET /proxy/network/dl/backup/10.5.67.unf` with the API key answers `200 application/octet-stream`; with no key it is `401`, so UniFi OS's gate covers the download tree as well as the API tree (ADR-0003). `HEAD` answers with the length and no body, which is how unifig confirms there is a backup without pulling a copy of somebody's whole site onto a laptop that never asked for one. A name that tree does not have is a `404`, which is what makes asking it a check at all.

**Only that tree can be asked, though.** The Controller answers with a path — `/dl/backup/10.5.67.unf` — and where it hangs depends on which style of Controller is on the other end: under `/proxy/network` on a UniFi OS console, under the root on a bare Network application. Trying one and then the other, as the zones do for the v2 API (`internal/reconcile/zone.go`), is not merely unnecessary here but wrong: **a console answers `200` and 1,209 bytes of its own HTML for a path under its root it does not recognise**, that one included, so a console whose real download had failed would have its second attempt waved through by a web page. unifig therefore hands the question back to go-unifi — the Controller's own path, rebased onto the API path as `../dl/…`, which the client joins to whichever style it detected — and asks that tree and no other.

**The name is the Controller's own, and it is one slot per version.** A UDR and a container both answered `/dl/backup/<version>.unf`, and running the command again overwrites it rather than adding a file. `{"cmd":"list-backups"}` does not list it either: that answers with the console's automatic backups — nine of them on this router, monthly, back to January, the oldest still on Network 10.0.162 — and the on-demand file is not among them. So the net `--backup-first` strings up holds the site as it was before the most recent apply that asked for one, and nothing older. What stands behind that is the automatic backups, which are the console's business rather than unifig's. unifig therefore never builds that path: it uses the URL the Controller answered with, and a Controller that starts naming them differently is one this follows rather than one it starts guessing about.

**Where it was verified.** The physical UDR on 18 August 2026 — UniFi OS, Network 10.5.67, an operator-created API key, one triggered backup — and the dockerized Controller at both ends of the compatibility matrix, 10.5.67 and 10.0.162, answering the same command with the same shape. That is why this area gets a container row in the published table rather than a note about a recording. What no container can be trusted to answer for is what a Setting looks like on the firmware an operator runs (ADR-0012, ADR-0013); a command is not a Setting, and here the two kinds of Controller agree line for line. The UDR is where it was confirmed all the same, because the API-key gate is UniFi OS's own and no container has one.

## What the confirmation is worth, exactly

The `HEAD` says the Controller is serving a backup under the name it just gave. It does not say that backup is the one from thirty seconds ago, and it cannot: the slot is fixed per version, so it may already have held a backup before this apply ever ran, and the answer would be `200` either way. The download tree carries no `Last-Modified` on a UDR, the on-demand file is in no listing, and go-unifi hands back an error rather than a response, so its length is not available to compare either.

What rules out a stale answer is the shape of the command rather than the check: it does not return until the file is written, and the file it writes is at that name. The check is there for the case that leaves — the Controller reporting a backup it is not serving — and the honest reading of the pair is "the command completed and there is a backup at the name it gave", which is what `--backup-first` should be taken to promise. A Controller that answered `rc: ok` and wrote nothing while still serving last month's file would fool it, and nothing available over this API would catch that.

## What unifig does with it

`apply --backup-first`, and nothing at all without the flag: a backup writes a file on someone's router, and doing that unasked is not a default anyone opted into.

It runs after the whole-plan approval and after each Risky change has been agreed to or refused, and immediately before the first mutation. A plan nobody approved is a plan with nothing to be safe from, and a plan whose every change was refused is one that needs no net. A backup that cannot be confirmed stops the apply entirely rather than becoming a warning printed above the changes it was supposed to protect — the point of a net is that the fall does not happen without it.

Two things it deliberately does not do:

- **It does not download the backup.** The file stays on the Controller, which is where a restore looks for it, and where it is already behind the API-key gate rather than in a directory on a laptop.
- **It does not restore one.** Rollback is out of scope (issue #12), and this ADR does not smuggle it back in. The recovery for a half-applied plan is unchanged: fix the problem and run again, against the Controller as it now stands (ADR-0001). The backup is for the case where that is not enough, and spending it is a deliberate act an operator performs in the Controller's own UI, with the file already sitting where that UI looks.

## Considered Options

- **`async-backup`, then poll until the backup appears** — rejected. It names no file at all, so there is nothing to hand the operator, and nothing to poll either: the on-demand backup never shows up in `list-backups`, and the only thing left to watch would be a fixed path that already holds the previous backup — a wait that would end before the new backup existed. That is the same blind spot the section above admits to, arrived at deliberately instead of inherited: with the synchronous command it is the narrow case of a Controller that lies, and with the asynchronous one it would be the ordinary case of a Controller that is simply not finished yet.
- **Trigger the console's own backup at the UniFi OS layer** — rejected. Nothing answered under the OS paths tried (`/api/backups`, `/api/system/backups`, `/api/backup` are each 404 on the UDR), and the Network application's backup is the one holding what unifig changes.
- **Take a backup on every apply** — rejected. It writes a file on someone's router on a run they did not ask for one on, and it would quietly overwrite the previous one, which is the backup they might have wanted.
- **Trust `rc: ok` and confirm nothing** — rejected. The check is one `HEAD` against a Controller unifig is already connected to, and it is the difference between "the Controller said yes" and "there is a backup there".

## Consequences

- The compatibility table grows a **Controller backup** row with container evidence, tested against every version in the matrix. It is the first area in the suite that is not about a Resource or a Setting.
- **The recording cannot carry it**, ever: `make record-udr` is read-only by charter (ADR-0011) and this is a POST. So `--backup-first` cannot be exercised against the replay stand-in — which is exactly where WAN slots live, so the Risky changes the net matters most for are the ones the suite cannot pair it with. On a real UDR the two work together; the gap is the test rig's, and it is named here rather than left to be discovered.
- **The test rig fabricates more than auth now.** ADR-0003 says its proxy is "UniFi OS make-believe only for auth", and the backup tests widen that: it can answer the backup command with a failure, refuse to serve a backup it took, and — always — answer a console's own HTML for a download path under its root, which is what a UDR does. None of that touches the config plane, which still comes from the real Network application behind it, and each one is a Controller state no healthy Controller produces on request. The last of the three is what keeps the check above from silently regressing to the two-tree guess it must not be.
- unifig now reaches an endpoint outside the config plane. If a Controller version drops or renames the command, `--backup-first` fails loudly and applies nothing, which is the direction that failure should fall in.
- What a backup holds is the Network application's own configuration — everything unifig can change is in it — and not the console's settings around it. An operator reading `--backup-first` as "back up my router" is reading it slightly too widely, which is why the flag's own help says what it takes and where it goes.
