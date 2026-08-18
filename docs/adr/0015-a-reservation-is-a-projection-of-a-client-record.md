# A reservation is a projection of a client record, so giving one up is not forgetting the client

Every other Resource unifig manages is an object on the Controller that exists because somebody asked for it. A DHCP Reservation is not. The Controller keeps one record per client it has ever seen — carrying the name an operator gave the device, a note, a user group, whether it is blocked — and a reservation is two fields of that record: `use_fixedip`, and the `fixed_ip` beside it. There is no reservation object to create and none to delete.

That leaves one question, and it is the whole of this decision: **what does `--prune` do to a reservation the config stops naming?**

**It clears the fixed address and leaves the record exactly where it was.** The Resource is the reservation, so the reservation is what goes. The client keeps its name, its note, its group, its history, and takes a dynamic address from the same network's pool at its next lease.

The alternative was to forget the client — the Controller's own `forget-sta`, which is what its UI's "Forget" does. That is a bigger request than the one the file made. An operator who deletes four lines of YAML has said "stop pinning this address"; they have not said "erase what this network knows about that device", and prune is not the place to infer the second from the first. It would also be irreversible in a way nothing else prune does is: a pruned network can be written back into the file and recreated, while the name somebody typed for a laptop in 2021 cannot.

The same reading decides the rest of the lifecycle, so the type is consistent end to end:

- **What is live** is the records with a fixed address switched on. `rest/user` answers with every client record the site holds — hundreds, on a real router — and the ones without a reservation are not of a managed type at all. Prune cannot see them, export does not write them, and `dhcp-reservations: []` puts every reserved address at stake and no client record.
- **A create is not always a POST.** The reservation is what does not exist; the record underneath it may. Giving an address to a client the Controller already knows is an edit to that client's record — the Controller refuses a second record for a MAC it has (`api.err.MacUsed`) — and the plan still reads `+`, because what the operator is adding is the reservation. Which HTTP verb carries it is mechanism.
- **An update writes two fields and hands the rest back**, which is ADR-0004 applied to a record that is mostly not unifig's.

**The deletion says so on the line.** `- dhcp-reservation "aa:bb:cc:dd:ee:ff"` reads like the device being forgotten, so the change carries the sentence that says it is not, and names the client where the Controller has a name for it — a MAC address is not how anyone recognises a laptop. This is the one deletion in unifig that needs a note, and it needs one precisely because the plan line cannot be read literally.

## The reference nobody states

A reservation names no network, and unifig does not write the `network_id` the record carries. That is the Controller's design rather than a gap: it decides which network an address belongs to by whose subnet contains it. An address inside no network's subnet is refused (`api.err.InvalidFixedIP`) whatever `network_id` says, and a network with an address reserved inside it cannot be deleted — `api.err.ResourceReferredBy`, reference type `FIXED_IP_OVERLAPS_NETWORK_SUBNET`, and it fires for a reservation that names no network at all.

So ADR-0014 applies here through containment rather than through a name: a reservation holds back the network whose subnet its address falls in, and the plan says which reservation is doing the holding. This is also why a reservation sorts after the networks — the network has to exist before an address inside it can be reserved, and the deletions go the other way.

A reservation this plan is *creating* holds a network back too, and that is where the rule parts company with the one for WLANs. There, creates are left out deliberately: a config stating a WLAN states the network its clients join, so that network is named in the file and prune was never free to take it. Nothing of the sort holds here, because a reservation names no network — so a file can reserve an address inside a network it never mentions, and the plan has to see that coming rather than emitting `+ dhcp-reservation` and `- network` together, which is a run that creates the address and then has the deletion refused.

## Consequences

- Prune is safe to point at this section on a real router, which was the thing worth getting right: the collection unifig walks is the reservations, not the address book, so the blast radius of `dhcp-reservations: []` is the fixed addresses and nothing else.
- Applying a config twice leaves a client record unifig created for a device the Controller had never seen, then gave the address up for, as a bare record with no reservation. That is the honest end state — the record is the Controller's memory of a device, not unifig's — and the next plan is empty, because a record with no reservation is not a Reservation.
- The rig has to forget clients itself rather than asking unifig to (`forget-sta`), since unifig will not. Its cleanup lower-cases the MAC first: that command matches the exact string it is given and answers `rc: ok` having forgotten nothing.
- The MAC is the one natural key in unifig that is not case-sensitive, because the Controller lower-cases every MAC it stores. Matching folds case, everything written is lower case, and two entries in one file differing only in case are one reservation written twice — which `validate` reports as the duplicate it is.
- The recording keeps no client records (ADR-0011): a MAC address identifies a piece of hardware for as long as it exists, and the dockerized Controller holds client records as well as a router does. `user.json` answers only because export and prune ask, and it goes one step past the emptied WLANs and port forwards — `make record-udr` does not fetch it at all. Emptying is for a response something else in the recorder still wants; nothing wants this one, and not asking cannot be got wrong.
