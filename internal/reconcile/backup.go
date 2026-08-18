package reconcile

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/filipowm/go-unifi/v2/unifi"
)

// backupSettingsOnly is the `days` the Controller's backup command takes: how
// much of the site's historical statistics to fold in, where 0 is none at all.
//
// unifig asks for the configuration and nothing else. The configuration is what
// an apply can break and what a restore would put back; a site's traffic history
// would make the file larger and the wait longer without making the safety net
// any wider.
const backupSettingsOnly = 0

// Backup asks the Controller to write a backup of its own configuration and
// returns the URL it serves it at.
//
// This is the safety net an operator opts into before an apply, and it lives
// here rather than in the command layer because it is one more thing said to the
// Controller, through the same client, over the same authenticated connection as
// everything else in this package.
//
// It is not part of a Plan. A backup is not a change to the site — nothing about
// the configuration is different afterwards — so it is neither planned, printed
// as a Change, nor counted in what an apply applied. What it is, is the thing
// that has to have happened before the first change does.
//
// The command it sends is the one the Controller's own UI sends, and it answers
// only when the file is written: a UDR took 2.4 seconds over it and the file at
// the URL it named had grown by the time the response arrived (ADR-0017). That
// is what makes "confirm the backup completed" a question this can answer at all
// — the Controller's asynchronous form of the same command returns immediately
// and names no file, which is exactly the shape unifig refuses below.
//
// Two things it deliberately does not do: it never downloads the backup, and it
// never restores one. The file stays on the Controller, where a restore would
// look for it, and rollback is out of scope for unifig (issue #12) — the recovery
// for a half-applied plan is still to fix the config and run again.
func Backup(ctx context.Context, client unifi.Client, site string) (string, error) {
	var answer struct {
		Data []struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	command := map[string]any{"cmd": "backup", "days": backupSettingsOnly}
	if err := client.Post(ctx, fmt.Sprintf("s/%s/cmd/backup", site), command, &answer); err != nil {
		return "", err
	}
	if len(answer.Data) == 0 || answer.Data[0].URL == "" {
		return "", errors.New("the Controller accepted the backup command but did not say where it put the backup," +
			" so there is nothing to confirm and nothing to tell you to restore from")
	}

	// The Controller names the file; unifig never builds that name. It is the
	// running version with `.unf` on the end today, on both a UDR and a
	// container, and a Controller that starts naming them differently is one
	// this still follows rather than one it starts guessing about.
	served := answer.Data[0].URL

	// Where that name hangs is a second question, because the Controller answers
	// with a path relative to a tree the client does not expose after resolving:
	// the downloads sit beside the API rather than under it, under
	// `/proxy/network` on a UniFi OS console and under the root on a bare
	// Network application.
	//
	// So the confirmation does not guess. Rebasing the Controller's own path
	// onto the API path — `../dl/…` — hands the question back to go-unifi, which
	// joins it to whichever style it detected when it connected, and this asks
	// that tree and no other. Trying both in turn would be worse than useless
	// here: a UniFi OS console answers 200 and 1,209 bytes of its own HTML for
	// anything under its root it does not recognise, `/dl/backup/…` included, so
	// a console whose real download failed would have its second try answered
	// yes by a web page (measured on the UDR, ADR-0017). Under the tree that
	// does serve backups, a name it does not have is a 404, which is what makes
	// this a check at all.
	if err := client.Do(ctx, http.MethodHead, "../"+strings.TrimPrefix(served, "/"), nil, nil); err != nil {
		return "", fmt.Errorf("the Controller reported a backup at %s but does not serve it back, so unifig"+
			" cannot confirm there is one to restore from: %w", served, err)
	}

	// Naming it for the operator is the cheap question, asked only now that the
	// backup is confirmed: a wrong answer here misprints a URL rather than
	// waving through an apply. The console is the style that can be asked,
	// because a bare Network application has no `/proxy/network` at all.
	console := strings.TrimSuffix(unifi.NewStyleAPI.ApiPath, "/api") + served
	if err := client.Do(ctx, http.MethodHead, console, nil, nil); err == nil {
		served = console
	}
	return strings.TrimSuffix(client.BaseURL(), "/") + served, nil
}
