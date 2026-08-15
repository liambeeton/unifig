package reconcile

import (
	"context"
	"fmt"
	"io"

	"github.com/filipowm/go-unifi/v2/unifi"
)

// Apply executes the plan against the Controller, reporting each change to out
// as it lands.
//
// It stops at the first failure, and there is no rollback: because reconcile
// keeps no state, the recovery for a half-applied plan is to fix the problem
// and run again. The next plan is computed from the Controller as it now
// stands, including the part that succeeded, so a re-run picks up exactly
// where this one stopped without being told anything about it.
func (p Plan) Apply(ctx context.Context, client unifi.Client, site string, out io.Writer) error {
	if p.Empty() {
		return nil
	}

	// Whatever led here — the plan, the confirmation prompt — has just been
	// printed, and what follows is the doing rather than the deciding.
	_, _ = fmt.Fprintln(out)
	for i, change := range p.Changes {
		if err := change.write(ctx, client, site); err != nil {
			// Say what did happen before saying what went wrong: the operator's
			// first question about a failed apply is what state their Controller
			// is in now.
			_, _ = fmt.Fprintf(out,
				"\nStopped after %s. Nothing further was attempted; apply is safe to run again once this is fixed.\n",
				count(i, len(p.Changes)))
			return fmt.Errorf("%s %s %q: %w", change.Action, change.Resource, change.Name, err)
		}
		_, _ = fmt.Fprintf(out, "  %s %s %q %s\n",
			actions[change.Action].mark, change.Resource, change.Name, actions[change.Action].past)
	}

	_, _ = fmt.Fprintf(out, "\nApplied %s.\n", count(len(p.Changes), len(p.Changes)))
	return nil
}

// count phrases progress as "2 of 3 changes", collapsing to "3 changes" once
// the two numbers agree.
func count(done, total int) string {
	noun := "changes"
	if total == 1 {
		noun = "change"
	}
	if done == total {
		return fmt.Sprintf("%d %s", total, noun)
	}
	return fmt.Sprintf("%d of %d %s", done, total, noun)
}
