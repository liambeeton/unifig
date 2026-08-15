package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// What plan and apply actually do needs a Controller, so their behavior is
// proved in the e2e suite. What is left here is the part that happens before
// either of them reaches the network — the command line, and the exit codes
// that a pipeline reads. Those are worth pinning down without Docker, and the
// unreachable Controller in the environment is what proves they never got as
// far as connecting.

// brokenConfig fails schema validation, so plan and apply both stop while
// still offline.
const brokenConfig = `networks:
  - name: IoT
    subnet: nonsense
`

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "unifig.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return path
}

// Exit 2 means "changes pending" and nothing else. A pipeline that treats it
// as "apply these" must never see it for a config that could not even be
// read, so a config error is exit 1 like any other failure.
func TestPlanOnAnUnreadableConfigIsAnErrorNotChangesPending(t *testing.T) {
	res := run(t, "plan", writeConfig(t, brokenConfig))

	if res.exitCode != 1 {
		t.Fatalf("plan exited %d, want 1\nstdout: %s\nstderr: %s", res.exitCode, res.stdout, res.stderr)
	}
	if !strings.Contains(res.stderr, "networks[0].subnet") {
		t.Errorf("stderr should report the config problem, got: %s", res.stderr)
	}
	if res.stdout != "" {
		t.Errorf("plan should print nothing on stdout when the config is bad, got: %s", res.stdout)
	}
}

func TestApplyOnAnUnreadableConfigChangesNothing(t *testing.T) {
	res := run(t, "apply", writeConfig(t, brokenConfig))

	if res.exitCode != 1 {
		t.Fatalf("apply exited %d, want 1\nstderr: %s", res.exitCode, res.stderr)
	}
	if !strings.Contains(res.stderr, "networks[0].subnet") {
		t.Errorf("stderr should report the config problem, got: %s", res.stderr)
	}
}

func TestPlanRejectsAnUnknownFlag(t *testing.T) {
	res := run(t, "plan", "--jsno")

	if res.exitCode != 1 {
		t.Errorf("exit code = %d, want 1", res.exitCode)
	}
	if !strings.Contains(res.stderr, "usage:") {
		t.Errorf("stderr should be the usage text, got: %s", res.stderr)
	}
}

// --auto-approve is apply's flag alone. Accepting it on plan would be
// meaningless at best and, to an operator who mistyped the verb, reassuring
// about an approval that never applied to anything.
func TestPlanRejectsApplysFlagAndViceVersa(t *testing.T) {
	if res := run(t, "plan", "--auto-approve"); res.exitCode != 1 {
		t.Errorf("plan --auto-approve exited %d, want 1", res.exitCode)
	}
	if res := run(t, "apply", "--json"); res.exitCode != 1 {
		t.Errorf("apply --json exited %d, want 1", res.exitCode)
	}
}

func TestPlanWithTooManyFilesPrintsUsage(t *testing.T) {
	res := run(t, "plan", "one.yaml", "two.yaml")

	if res.exitCode != 1 {
		t.Errorf("exit code = %d, want 1", res.exitCode)
	}
	if !strings.Contains(res.stderr, "usage:") {
		t.Errorf("stderr should be the usage text, got: %s", res.stderr)
	}
}

func TestUsageListsEveryVerbAndExitCode(t *testing.T) {
	res := run(t, "nonsense")

	for _, fragment := range []string{"plan", "apply", "export", "validate", "--json", "--auto-approve", "2"} {
		if !strings.Contains(res.stderr, fragment) {
			t.Errorf("usage should mention %q, got:\n%s", fragment, res.stderr)
		}
	}
}
