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
// Changes run in the order the plan printed them, which is dependency order: a
// network is created before the WLAN that joins it, and a WLAN is deleted
// before the network it was on. That ordering is the whole of what lets a
// config declaring both apply in one pass.
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
			// Say what did and did not happen before saying what went wrong:
			// the operator's first question about a failed apply is what state
			// their Controller is in now, and the answer has to be exact enough
			// to act on without going and looking.
			_, _ = fmt.Fprintf(out, "\nApplied %s. These were not applied:\n", count(i, len(p.Changes)))
			for _, skipped := range p.Changes[i:] {
				_, _ = fmt.Fprintf(out, "  %s %s %q\n",
					actions[skipped.Action].mark, skipped.Resource, skipped.Name)
			}
			_, _ = fmt.Fprint(out,
				"\nNothing was rolled back; apply is safe to run again once this is fixed.\n")
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
