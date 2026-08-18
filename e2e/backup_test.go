package e2e

import (
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// These tests are about the one thing --backup-first promises: that the
// Controller has written a backup of itself before unifig changes anything, and
// that an apply which cannot get one changes nothing at all.
//
// A backup leaves no trace on the site — the configuration is identical
// afterwards — so the assertions here are of two kinds the rest of the suite
// does not need. What was asked of the Controller and in which order comes from
// the rig's watch, because ordering is the whole of the promise; and the file
// itself is fetched back through the same base URL and the same API key, because
// a backup nobody can download is not a safety net (ADR-0017).

// backupNotice is the sentence apply prints when it has taken a backup, and the
// URL on the end of it is what these tests fetch.
const backupNotice = "Backed up the Controller first: "

func TestApplyWithBackupFirstBacksUpBeforeItChangesAnything(t *testing.T) {
	path := managedNetwork(t, `networks:
  - name: Backup First
    vlan: 170
    subnet: 10.170.0.1/24
`, "Backup First")
	watch := testRig.watchController(t)

	res := apply(t, "--backup-first", path)

	if !strings.Contains(string(res.Stdout), backupNotice) {
		t.Fatalf("apply --backup-first should say it took a backup, got:\n%s", res.Stdout)
	}

	// The order is the promise. A backup taken after the first change would
	// hold the half-applied site rather than the one to go back to.
	asked := watch.events()
	if len(asked) == 0 || asked[0] != askedBackup {
		t.Errorf("the Controller was asked for %v; the backup has to come first", asked)
	}
	if !slices.Contains(asked, askedMutation) {
		t.Errorf("the Controller was asked for %v, so this apply changed nothing and proves no ordering", asked)
	}

	// And the change itself still landed: backing up first is a thing apply
	// does as well as its job, not instead of it.
	live := testRig.liveNetwork(t, "Backup First")
	if live["ip_subnet"] != "10.170.0.1/24" {
		t.Errorf("created network subnet = %#v, want %q", live["ip_subnet"], "10.170.0.1/24")
	}
}

func TestApplyReportsABackupTheControllerActuallyServes(t *testing.T) {
	path := managedNetwork(t, `networks:
  - name: Backup Served
    vlan: 171
    subnet: 10.171.0.1/24
`, "Backup Served")

	res := apply(t, "--backup-first", path)
	where := backupURL(t, string(res.Stdout))

	// Through the same API key unifig used, because the operator this URL is
	// printed for has nothing else to fetch it with.
	req, err := http.NewRequest(http.MethodGet, where, nil)
	if err != nil {
		t.Fatalf("building a request for %s: %v", where, err)
	}
	req.Header.Set("X-Api-Key", testRig.apiKey)
	resp, err := testRig.client.Do(req)
	if err != nil {
		t.Fatalf("fetching the backup at %s: %v", where, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("fetching the backup at %s: status %d", where, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the backup at %s: %v", where, err)
	}
	if len(body) == 0 {
		t.Errorf("the backup at %s is empty, so there is nothing to restore from", where)
	}
}

// The point of a safety net is that the fall does not happen without it. A
// Controller that cannot write a backup is a Controller unifig does not touch.
func TestApplyWithBackupFirstChangesNothingWhenTheBackupFails(t *testing.T) {
	path := managedNetwork(t, `networks:
  - name: Backup Refused
    vlan: 172
    subnet: 10.172.0.1/24
`, "Backup Refused")
	watch := testRig.watchController(t)
	testRig.refuseBackups(t, refuseCommand)

	res := testRig.runUnifig(t, []string{"apply", "--auto-approve", "--backup-first", path}, nil)

	if res.ExitCode != exitError {
		t.Fatalf("apply exited %d, want %d\nstdout: %s\nstderr: %s",
			res.ExitCode, exitError, res.Stdout, res.Stderr)
	}
	if !strings.Contains(string(res.Stderr), "backing up the Controller") {
		t.Errorf("stderr should say the backup is what failed, got: %s", res.Stderr)
	}
	if !strings.Contains(string(res.Stdout), "nothing was applied") {
		t.Errorf("apply should say plainly that it changed nothing, got:\n%s", res.Stdout)
	}
	if asked := watch.events(); slices.Contains(asked, askedMutation) {
		t.Errorf("the Controller was asked for %v after the backup failed; it should have been asked for nothing", asked)
	}
	if found := testRig.networksNamed(t, "Backup Refused"); len(found) != 0 {
		t.Errorf("the Controller has %d networks named %q, want 0 — the apply was supposed to stop",
			len(found), "Backup Refused")
	}
}

// Taking a backup and confirming there is one are different questions, and a
// Controller that answers the first and not the second is one unifig has to
// stop for: what --backup-first promises is a backup to go back to, not a
// command that returned successfully.
//
// The Controller this runs against is also the one that answers a web page at
// its root for the same path (see startProxy), which is the trap: a check that
// went looking for the backup somewhere else after the real tree said no would
// be told yes by 38 bytes of HTML and would apply the plan.
func TestApplyChangesNothingWhenTheBackupCannotBeConfirmed(t *testing.T) {
	path := managedNetwork(t, `networks:
  - name: Backup Unconfirmed
    vlan: 175
    subnet: 10.175.0.1/24
`, "Backup Unconfirmed")
	watch := testRig.watchController(t)
	testRig.refuseBackups(t, refuseDownload)

	res := testRig.runUnifig(t, []string{"apply", "--auto-approve", "--backup-first", path}, nil)

	if res.ExitCode != exitError {
		t.Fatalf("apply exited %d, want %d\nstdout: %s\nstderr: %s",
			res.ExitCode, exitError, res.Stdout, res.Stderr)
	}
	if !strings.Contains(string(res.Stderr), "does not serve it back") {
		t.Errorf("stderr should say the backup could not be confirmed, got: %s", res.Stderr)
	}
	if asked := watch.events(); !slices.Contains(asked, askedBackup) {
		t.Errorf("the Controller was asked for %v; this test is about a backup that was taken", asked)
	} else if slices.Contains(asked, askedMutation) {
		t.Errorf("the Controller was asked for %v after the backup could not be confirmed", asked)
	}
	if found := testRig.networksNamed(t, "Backup Unconfirmed"); len(found) != 0 {
		t.Errorf("the Controller has %d networks named %q, want 0 — the apply was supposed to stop",
			len(found), "Backup Unconfirmed")
	}
}

// The flag is opt-in, and an apply without it asks for no backup at all. That
// is worth stating rather than assuming: a backup writes a file on somebody's
// router, and unifig does not do that unasked.
func TestApplyWithoutBackupFirstTakesNoBackup(t *testing.T) {
	path := managedNetwork(t, `networks:
  - name: Backup Unasked
    vlan: 173
    subnet: 10.173.0.1/24
`, "Backup Unasked")
	watch := testRig.watchController(t)

	res := apply(t, path)

	if strings.Contains(string(res.Stdout), backupNotice) {
		t.Errorf("apply claimed a backup nobody asked for, got:\n%s", res.Stdout)
	}
	if asked := watch.events(); slices.Contains(asked, askedBackup) {
		t.Errorf("the Controller was asked for %v; an apply without --backup-first backs nothing up", asked)
	}
}

// Nothing to apply is nothing to be safe from, so the Controller is left alone
// entirely — no backup, and a file on it that nobody asked for.
func TestApplyWithBackupFirstOnAMatchingControllerTakesNoBackup(t *testing.T) {
	path := managedNetwork(t, `networks:
  - name: Backup Matching
    vlan: 174
    subnet: 10.174.0.1/24
`, "Backup Matching")
	apply(t, "--backup-first", path)

	watch := testRig.watchController(t)
	res := apply(t, "--backup-first", path)

	if strings.Contains(string(res.Stdout), backupNotice) {
		t.Errorf("apply backed up a Controller it had nothing to change, got:\n%s", res.Stdout)
	}
	if asked := watch.events(); len(asked) != 0 {
		t.Errorf("the Controller was asked for %v, and there was nothing to do", asked)
	}
}

// backupURL reads the URL out of what apply printed, so that a test fetches the
// backup unifig actually reported rather than one it built the same way unifig
// did — which would agree with unifig by construction.
func backupURL(t *testing.T, stdout string) string {
	t.Helper()
	_, after, found := strings.Cut(stdout, backupNotice)
	if !found {
		t.Fatalf("apply --backup-first printed no backup URL:\n%s", stdout)
	}
	where, _, _ := strings.Cut(after, "\n")
	if !strings.HasPrefix(where, testRig.proxyURL) {
		t.Fatalf("apply reported a backup at %q, which is not on the Controller it was pointed at (%s)",
			where, testRig.proxyURL)
	}
	return where
}
