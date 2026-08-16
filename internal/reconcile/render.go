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
		_, err := io.WriteString(out, "No changes. The Controller already matches the config.\n")
		return err
	}

	var b strings.Builder
	for _, change := range p.Changes {
		// The mark leads each line so a long plan can be skimmed down the
		// left margin without reading a word of it.
		fmt.Fprintf(&b, "  %s %s %q\n", actions[change.Action].mark, change.Resource, change.Name)
		width := change.fieldWidth()
		for _, field := range change.Fields {
			fmt.Fprintf(&b, "      %-*s %s\n", width, field.Name+":", change.render(field))
			if field.Note != "" {
				// Indented to the value column, so a note reads as belonging
				// to the field above it rather than as another field.
				fmt.Fprintf(&b, "      %-*s %s\n", width, "", field.Note)
			}
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "Plan: %s.\n", p.summary())

	_, err := io.WriteString(out, b.String())
	return err
}

// WriteJSON emits the plan as JSON: the same changes with a machine on the
// other end, for a pipeline that gates on drift rather than reading prose.
//
// An empty plan is `{"changes": []}` and a change with nothing to list is
// `"fields": []`, never null, so a consumer can count and iterate without
// special-casing the quiet cases.
func (p Plan) WriteJSON(out io.Writer) error {
	if p.Changes == nil {
		p.Changes = []Change{}
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(p)
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
func (c Change) render(field Field) string {
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
