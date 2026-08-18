package reconcile

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Write renders the plan for a human: what would change, what it would change
// from, and a summary to end on.
func (p Plan) Write(out io.Writer) error {
	if p.Empty() {
		// A plan with nothing in it still has to carry its caveats, and this is
		// the case they exist for: without them "No changes" is indistinguishable
		// from a site where prune was asked for and quietly did nothing.
		_, err := io.WriteString(out, "No changes. The Controller already matches the config.\n"+p.caveats())
		return err
	}

	var b strings.Builder
	for _, change := range p.Changes {
		// The mark leads each line so a long plan can be skimmed down the
		// left margin without reading a word of it.
		fmt.Fprintf(&b, "  %s\n", change.Summary())
		width := change.fieldWidth()
		for _, field := range change.Fields {
			fmt.Fprintf(&b, "      %-*s %s\n", width, field.Name+":", change.render(field))
			for _, note := range field.Notes {
				// Indented to the value column, so a note reads as belonging
				// to the field above it rather than as another field. Several
				// get a line each: running them together would put two
				// unrelated consequences in one sentence.
				fmt.Fprintf(&b, "      %-*s %s\n", width, "", note)
			}
		}
		// A Risky change says what it risks, under its fields and marked with
		// its own character, because an operator about to approve a plan should
		// not have to know which kinds are the dangerous ones. Apply asks about
		// this change on its own, and this is where that stops being a surprise.
		if change.Risk != "" {
			fmt.Fprintf(&b, "      ! %s\n", change.Risk)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "Plan: %s.\n", p.summary())
	b.WriteString(p.caveats())

	_, err := io.WriteString(out, b.String())
	return err
}

// caveats is what the plan does not do, printed after what it does so that the
// last thing read is the thing most easily missed. Empty when there is nothing
// to say, which is the ordinary case.
func (p Plan) caveats() string {
	if len(p.Caveats) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n")
	for _, caveat := range p.Caveats {
		fmt.Fprintf(&b, "! %s: %s.\n", caveat.Kind, caveat.Reason)
	}
	return b.String()
}

// WriteJSON emits the plan as JSON: the same changes with a machine on the
// other end, for a pipeline that gates on drift rather than reading prose.
//
// An empty plan is `{"changes": []}` and a change with nothing to list is
// `"fields": []`, never null, so a consumer can count and iterate without
// special-casing the quiet cases.
//
// Secret fields carry `"secret": true` with both ends null. That is not a hole
// in the output — a field is in a change only because it is changing, so the
// entry says the passphrase is being replaced, and the null says unifig will
// not be the thing that writes it into a build log.
//
// A Risky change carries `"risk"` with the same sentence the prose plan prints,
// so a pipeline can gate on the changes that can cut a site off the internet
// without keeping its own list of which kinds those are.
func (p Plan) WriteJSON(out io.Writer) error {
	if p.Changes == nil {
		p.Changes = []Change{}
	}
	if p.Caveats == nil {
		p.Caveats = []Caveat{}
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(p)
}

// Summary is the change as a single line — its mark, its kind and its name —
// and it is literally the plan's own heading line.
//
// It is exported because a change gets talked about outside a plan: apply says
// which ones it did not reach, and the command layer asks the operator about a
// Risky one by name. All of them have to be recognisably the same change, and
// the way to guarantee that is for there to be one place that says what a
// change looks like.
func (c Change) Summary() string {
	return actions[c.Action].mark + " " + c.subject()
}

// subject is what a change is about: its kind, and the name unifig matched it
// by where there is one.
//
// A singleton Setting has no name, and printing an empty pair of quotes for one
// would be reporting an identity that does not exist — there is one Encrypted
// DNS setting on a Controller, and `~ encrypted-dns` is the whole of what there
// is to say about which one this is.
func (c Change) subject() string {
	if c.Name == "" {
		return string(c.Kind)
	}
	return fmt.Sprintf("%s %q", c.Kind, c.Name)
}

// fieldWidth is how wide the field-name column has to be for this change's
// values to line up under each other — the longest name, plus the colon
// written after it.
func (c Change) fieldWidth() int {
	width := 0
	for _, field := range c.Fields {
		width = max(width, len(field.Name))
	}
	return width + len(":")
}

// render writes one field the way the action means it: a create states the
// value it will set, a delete the value being lost, and an update both ends of
// the move. Only an update gets the arrow, because only an update is a move —
// rendering a delete as `10.20.0.1/24 -> (none)` would read as a subnet being
// cleared on a network that survives.
//
// A secret says only that it is a secret, at either end and for every action. A
// field appears in a change at all only because it is changing, so `(hidden)`
// under a `~` already carries the whole message — the passphrase is being
// replaced, and unifig is not going to print it into a terminal, a CI log or a
// pasted ticket to say so.
func (c Change) render(field Field) string {
	if field.Secret {
		return "(hidden)"
	}
	switch c.Action {
	case Create:
		return value(field.To)
	case Delete:
		return value(field.From)
	default:
		return value(field.From) + " -> " + value(field.To)
	}
}

func value(v any) string {
	if v == nil {
		return "(none)"
	}
	return fmt.Sprint(v)
}

// summary counts the plan by action, naming only the actions it contains — a
// plan of pure creates should not have to mention updates to say so.
func (p Plan) summary() string {
	counts := map[Action]int{}
	for _, change := range p.Changes {
		counts[change.Action]++
	}
	var parts []string
	for _, action := range []Action{Create, Update, Delete} {
		if counts[action] > 0 {
			parts = append(parts, fmt.Sprintf("%d to %s", counts[action], action))
		}
	}
	return strings.Join(parts, ", ")
}
