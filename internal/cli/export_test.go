package cli_test

import (
	"strings"
	"testing"
)

// Export's own command line, checked where no Controller is needed to check it.
// What export actually writes is proved in the e2e suite, against a real one.

// -o and --with-secrets belong to export. Accepting them elsewhere would
// promise something the other verbs do not do: plan and apply write no config,
// and validate has no secrets to redact because it never reads a Controller.
func TestExportsFlagsAreExportsAlone(t *testing.T) {
	for _, args := range [][]string{
		{"plan", "--with-secrets"},
		{"apply", "--with-secrets"},
		{"validate", "--with-secrets"},
		{"plan", "-o", "out.yaml"},
		{"validate", "-o", "out.yaml"},
	} {
		res := run(t, args...)
		if res.exitCode != 1 {
			t.Errorf("%v exited %d, want 1", args, res.exitCode)
		}
		if !strings.Contains(res.stderr, "usage:") {
			t.Errorf("%v should print the usage text, got: %s", args, res.stderr)
		}
	}
}

// A flag that swallowed the next argument, or none at all, would write the
// config somewhere the operator did not ask for. Neither is worth guessing at.
func TestExportRejectsAnOWithNothingToWriteTo(t *testing.T) {
	for _, args := range [][]string{{"export", "-o"}, {"export", "-o="}} {
		res := run(t, args...)
		if res.exitCode != 1 {
			t.Errorf("%v exited %d, want 1", args, res.exitCode)
		}
		if !strings.Contains(res.stderr, "usage:") {
			t.Errorf("%v should print the usage text, got: %s", args, res.stderr)
		}
	}
}

// Export reads the Controller, so a filename on its command line is not a file
// to read — it is an operator who meant -o, or who meant validate.
func TestExportRejectsAPositionalFile(t *testing.T) {
	res := run(t, "export", "unifig.yaml")

	if res.exitCode != 1 {
		t.Errorf("exit code = %d, want 1", res.exitCode)
	}
	if !strings.Contains(res.stderr, "usage:") {
		t.Errorf("stderr should be the usage text, got: %s", res.stderr)
	}
}

// A boolean flag given a value is an operator expecting something to happen
// with it, and nothing would.
func TestABooleanFlagGivenAValueIsAUsageError(t *testing.T) {
	res := run(t, "plan", "--json=true")

	if res.exitCode != 1 {
		t.Errorf("exit code = %d, want 1", res.exitCode)
	}
	if !strings.Contains(res.stderr, "usage:") {
		t.Errorf("stderr should be the usage text, got: %s", res.stderr)
	}
}

func TestUsageMentionsExportsFlags(t *testing.T) {
	res := run(t, "nonsense")

	for _, fragment := range []string{"-o", "--with-secrets"} {
		if !strings.Contains(res.stderr, fragment) {
			t.Errorf("usage should mention %q, got:\n%s", fragment, res.stderr)
		}
	}
}
