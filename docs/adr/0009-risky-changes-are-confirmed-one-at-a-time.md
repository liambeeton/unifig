# A Risky change is confirmed on its own, and refusing one skips it

The spec (issue #1, stories 24 and 25) asks for two things that sound like one: any WAN-affecting change must be individually confirmed even during an otherwise approved Apply, and the confirmation must warn and ask, never hard-block. Together they decide the shape of apply's second question.

**Why a second question at all.** `--auto-approve` is the operator saying they have already read the plan. That is an answer to "shall I apply my config", and it is not an answer to "shall I take the internet down while the WAN slot reconnects" — the second is a different question about a different risk, asked of the same person, and one they may well answer differently. So the two approvals are separate by construction rather than by degree: `--auto-approve` cannot cover a Risky change, and `--allow-risky` is what says yes to one in advance.

**Why refusing skips rather than cancels.** The question asked was about that one change. An operator who says no to a PPPoE switch has not withdrawn the VLAN they asked for in the same file, and cancelling the apply would answer a question nobody asked. So the refused change is dropped from the plan, the rest is applied in the order it was already in, and the output names what was left behind alongside the flag that would have applied it. Exit code 0: the operator got what they asked for, and the drift they chose to leave shows up as the next `plan` exiting 2.

**Where it is asked.** All the questions come before anything is written, not one at a time as apply reaches each change. An operator answering the second question wants to know they are still deciding, not that three changes have already landed while they read — and an apply that stops halfway to ask is an apply whose failure report ("applied 1 of 3") has to explain a pause as well as a stop.

**What makes a change Risky** is a sentence on the change itself (`Change.Risk`), not a type the command layer knows about. The engine is where it is known that a WAN slot carries the site's internet connection, the plan prints that sentence under the change, the JSON carries it so a pipeline can gate without keeping its own list of dangerous kinds, and apply asks it back as a question. Adding the next Risky area is a sentence in one planner.

## Considered Options

- **Let `--auto-approve` cover Risky changes** — rejected: it is the spec's central safety promise inverted, and it would mean the one class of change that can cut a household off the internet is the one a stray flag applies unattended.
- **Refuse to apply Risky changes without a flag, without asking** — rejected: story 25 is explicit that unifig warns and asks rather than blocking, and a tool that will not do what its operator plainly means is one they route around.
- **Cancel the whole apply when a Risky change is refused** — rejected: it makes "no" cost more than it should, and an operator who wanted everything or nothing has that already by answering no to the plan.
- **Ask at the moment each Risky change is reached** — rejected, and it was the intuitive one. It puts an unanswered prompt in the middle of a half-applied plan, which is the exact state apply's stop-on-first-error report exists to keep legible.
- **Exit non-zero when something was refused** — rejected: the operator chose it, and a failure exit would make an interactive "no" indistinguishable from a broken apply. An unattended run reaches EOF, reads it as no, and says what it did not do; the next plan exits 2, which is the channel a pipeline already gates on.

## Consequences

- An apply in CI that means to change a WAN slot needs `--auto-approve --allow-risky`. Without the second flag it applies everything else and reports the WAN change as left behind — a quiet no-op for that one change, deliberately, rather than a surprise.
- Every question in a run reads from one buffered stdin (`cli.prompt`). A reader per question would swallow whatever followed the first answer and take the silence for a no, which is a bug that only appears once there are two questions.
- The e2e suite drives both answers through the real process boundary, with the operator's keystroke on stdin, because "it asked" and "it did not apply it" are both statements about the process rather than about a function.
