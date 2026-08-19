package reconcile

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/filipowm/go-unifi/v2/unifi"
)

// managedUpdate is the body of a v1 update: the Controller's own ID for the
// object, and exactly the fields the config states about it. Nothing else goes
// on the wire.
//
// It exists because a **v1 PUT merges** — a field the body leaves out is left
// exactly as the Controller was holding it, measured on all four collections
// unifig updates (ADR-0023). So the Controller is the one that puts the whole
// object back, and unifig's half of ADR-0004's update rule is to say only what
// the config says.
//
// That is the opposite conclusion to the v2 firewall-policy endpoint's, and the
// opposite code: there a PUT replaces, so an update has to read the stored
// object and carry every field of it back (mergeIntoStoredPolicy, ADR-0021).
// The v2 zone endpoint is a third answer and agrees with neither's reasoning —
// it merges like these do, while refusing all but three fields, so it reaches
// the same place by having nothing else it could accept (ADR-0024,
// writableZone). Three endpoints, three measurements, and the version in the
// path predicts none of it. Nothing here was inferred from anything there.
//
// What it replaced was a go-unifi struct handed straight back, which was wrong
// in the direction nobody was watching. Nothing was dropped, because the
// Controller kept what the body left out — but most of what the struct models
// carries no `omitempty`, so every field the Controller had never stored went
// back at Go's zero and became stored. An apply changing a VLAN wrote
// eighty-three fields the plan did not print, and a plan that quietly does more
// than it prints is not a plan (ADR-0004).
type managedUpdate map[string]any

// restPath is the Controller's v1 endpoint for one object of a collection —
// `networkconf`, `wlanconf`, `portforward`, `user` — and it is the path
// go-unifi's own update method for that type would have built. Each kind names
// its own collection through a helper of its own, so an update depends on no
// endpoint a plan did not already depend on. What differs from go-unifi is the
// body, which is the whole point.
func restPath(site, collection, id string) string {
	return fmt.Sprintf("s/%s/rest/%s/%s", site, collection, id)
}

// writeManaged sends one v1 update, and reads the Controller's answer to it.
//
// The answer is decoded rather than discarded, and that is not tidiness. The
// Internal API reports a refusal in the `meta` envelope at HTTP 200, and
// go-unifi only reads that envelope when it has somewhere to decode into — so a
// nil response body here would turn `rc: error` into a successful apply. Where
// the bytes go is not interesting; that they are asked for is.
//
// What it deliberately does not check is the shape of `data`. go-unifi's own
// update methods reject an answer that does not echo exactly one object, and
// that check would fire here on a healthy Controller: a PUT that stores no
// change answers `rc: ok` with `data: []`, measured on both `networkconf` and
// `wlanconf` while ADR-0023 was being written. A body unifig is right to send
// can produce one — an operator making the same edit in the UI between the plan
// and the apply is enough — and failing the apply for it would be reporting a
// race as an error. `rc` is the Controller saying whether it refused, and that
// is the question being asked.
func writeManaged(ctx context.Context, client unifi.Client, path string, managed managedUpdate) error {
	var answered json.RawMessage
	return client.Put(ctx, path, managed, &answered)
}
