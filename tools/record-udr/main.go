// Command record-udr re-records the Controller responses the WAN suite
// replays, from a real UniFi Dream Router, with everything that names a
// household taken out on the way through.
//
// Run it as `make record-udr`, with UNIFIG_HOST and UNIFIG_API_KEY pointing at
// the router — the same variables unifig itself reads. It is read-only against
// the Controller: three GETs, no other method anywhere in this program, so the
// worst it can do to a live site is nothing.
//
// What it does, in order:
//
//  1. asks the Controller for the three responses the recording holds;
//  2. answers the two questions ADR-0008 leaves open, out of what came back;
//  3. scrubs the recording (scrub.go — the part worth reviewing), which
//     refuses rather than writes if anything it took out survives;
//  4. writes the three files and stops, so the operator reads the diff before
//     anything is committed. Nothing here commits, and nothing here pushes.
//
// The raw responses never touch the repository: they go to a temporary
// directory outside it, and are deleted before this program exits. They carry
// the PPPoE password in the clear, which is the whole reason this program is
// not three curl commands.
package main

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The three responses, and the paths they come from. These are the paths
// e2e/replay_test.go serves back — recording anything else would produce files
// the stand-in never reaches.
var endpoints = []struct{ file, path string }{
	{"sysinfo.json", "/proxy/network/api/s/default/stat/sysinfo"},
	{"networkconf.json", "/proxy/network/api/s/default/rest/networkconf"},
	{"wlanconf.json", "/proxy/network/api/s/default/rest/wlanconf"},
}

func main() {
	os.Exit(run())
}

func run() int {
	conn, err := connectionFromEnv()
	if err != nil {
		return fail(err)
	}
	if err := atRepoRoot(); err != nil {
		return fail(err)
	}
	if err := recordingIsCommitted(); err != nil {
		return fail(err)
	}

	asking := bufio.NewScanner(os.Stdin)
	say("record-udr reads three responses from the Controller at %s and rewrites\n", conn.url)
	say("%s from them. It only ever asks the Controller for things: no\n", recordingDir)
	say("change is made to the router, and nothing is committed here.\n\n")
	if !yes(asking, "Go ahead and read the Controller") {
		say("Nothing was read.\n")
		return 1
	}

	raw, rawDir, err := record(conn)
	// Registered before the error is looked at: a recording that failed
	// half way through has half a recording on disk, and that half holds the
	// password too.
	if rawDir != "" {
		defer discard(rawDir)
	}
	if err != nil {
		return fail(err)
	}
	say("\nRead %d responses. The raw copies are in %s, and this program deletes\n", len(endpoints), rawDir)
	say("them before it exits — they hold the PPPoE password in the clear.\n\n")

	report(os.Stdout, answers(raw.networkconf))

	committed, err := readRecording("networkconf.json")
	if err != nil {
		return fail(fmt.Errorf("reading the committed recording: %w", err))
	}
	scrubbed, err := scrub(raw, committed)
	if err != nil {
		return fail(err)
	}
	if err := writeRecording(scrubbed); err != nil {
		return fail(err)
	}

	say("Written. Now the part no program can do for you.\n\n")
	if err := showDiff(); err != nil {
		return fail(err)
	}
	say("\nThe scrub replaced the credentials, the ISP's addressing, the console's\n")
	say("identifiers and your own name for your connection, took the LAN from the\n")
	say("recording already committed, and emptied the WLANs. What it cannot know\n")
	say("is a field it has never heard of. So, in the diff above:\n\n")
	say("  - does any value name your ISP, your street, your family or your site?\n")
	say("  - is any address in it one that is actually routed to you?\n")
	say("  - is there anything there you would not put on a postcard?\n\n")

	if !yes(asking, "The diff above is safe to publish") {
		say("\nLeft where it is, unpublished. To put the committed recording back:\n\n")
		say("    git restore -- %s\n\n", recordingDir)
		say("And please say what you found: the fix belongs in tools/record-udr/scrub.go,\n")
		say("where the next person gets it for free.\n")
		return 1
	}

	say("\nNothing has been committed yet. What is left:\n\n")
	say("    make e2e          # the WAN suite, now against your router's recording\n")
	say("    git add %s && git commit\n\n", recordingDir)
	say("If the suite fails, that is worth reading closely: it is the first time\n")
	say("these tests have run against a real UDR (ADR-0008).\n")
	return 0
}

// connection is where the Controller is and how to prove we may ask it — the
// same three variables unifig reads, so that whoever can run unifig can run
// this.
type connection struct {
	url      string
	apiKey   string
	insecure bool
}

func connectionFromEnv() (connection, error) {
	host := os.Getenv("UNIFIG_HOST")
	if host == "" {
		return connection{}, fmt.Errorf("UNIFIG_HOST is not set; set it to the router's URL, e.g. https://192.168.1.1")
	}
	if !strings.Contains(host, "://") {
		host = "https://" + host
	}
	apiKey := os.Getenv("UNIFIG_API_KEY")
	if apiKey == "" {
		return connection{}, fmt.Errorf("UNIFIG_API_KEY is not set; create an API key on the Controller and export it")
	}
	insecure, _ := strconv.ParseBool(os.Getenv("UNIFIG_INSECURE"))
	return connection{url: host, apiKey: apiKey, insecure: insecure}, nil
}

// record asks the Controller for the three responses. It keeps the raw bodies
// in a temporary directory outside the repository, named on stdout, so that an
// operator can look at what actually arrived while this program waits at a
// prompt; the caller deletes the directory before exiting. They carry the
// PPPoE password in the clear, so where they are kept is a decision rather
// than an implementation detail.
func record(conn connection) (recording, string, error) {
	dir, err := os.MkdirTemp("", "unifig-record-udr-")
	if err != nil {
		return recording{}, "", fmt.Errorf("making somewhere outside the repository to put the raw responses: %w", err)
	}

	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			// A UDR serves its own certificate. UNIFIG_INSECURE says the same
			// thing here that it says to unifig.
			TLSClientConfig: &tls.Config{InsecureSkipVerify: conn.insecure},
		},
	}

	// From here on the directory holds plaintext, so the ways out of this
	// program that skip its deletion have to be closed.
	deleteOnSignal(dir)

	var raw recording
	for _, endpoint := range endpoints {
		say("GET %s\n", endpoint.path)
		body, err := get(client, conn, endpoint.path)
		if err != nil {
			return recording{}, dir, err
		}
		if err := os.WriteFile(filepath.Join(dir, endpoint.file), body, 0o600); err != nil {
			return recording{}, dir, fmt.Errorf("keeping the raw %s: %w", endpoint.file, err)
		}
		doc, err := read(body)
		if err != nil {
			return recording{}, dir, fmt.Errorf("%s answered with something that is not a Controller response: %w", endpoint.path, err)
		}
		*raw.file(endpoint.file) = doc
	}
	return raw, dir, nil
}

// get is the only way this program talks to the Controller, and it is a GET.
func get(client *http.Client, conn connection, path string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, strings.TrimSuffix(conn.url, "/")+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-KEY", conn.apiKey)
	req.Header.Set("Accept", "application/json")

	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("asking the Controller for %s: %w", path, err)
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("reading the Controller's answer to %s: %w", path, err)
	}
	switch res.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("the Controller refused the API key (%s on %s)", res.Status, path)
	default:
		return nil, fmt.Errorf("the Controller answered %s on %s", res.Status, path)
	}

	// An Internal API response can carry a failure inside a 200, and a
	// recording of one would be a fixture that answers "no" to everything.
	var envelope struct {
		Meta struct {
			RC  string `json:"rc"`
			Msg string `json:"msg"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("%s answered with something that is not JSON: %w", path, err)
	}
	if envelope.Meta.RC != "ok" {
		return nil, fmt.Errorf("%s answered rc=%q %s", path, envelope.Meta.RC, envelope.Meta.Msg)
	}
	return body, nil
}

// deleteOnSignal makes Ctrl-C take the raw responses with it. Without it, the
// one way out of this program that leaves a plaintext PPPoE password on disk
// is the one an operator is most likely to take: quitting at a prompt, or
// closing the pager they piped the diff into.
func deleteOnSignal(dir string) {
	caught := make(chan os.Signal, 1)
	signal.Notify(caught, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGPIPE)
	go func() {
		signal := <-caught
		discard(dir)
		fmt.Fprintf(os.Stderr, "\nrecord-udr: stopped on %s. The raw responses have been deleted; nothing was committed.\n", signal)
		os.Exit(1)
	}()
}

// discard deletes the raw responses. It is safe to call twice, which it is:
// once on the way out, and once from whichever signal got there first.
func discard(dir string) {
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintf(os.Stderr,
			"record-udr: could not delete the raw responses in %s: %v\nDelete them yourself — they hold the PPPoE password in the clear.\n", dir, err)
	}
}

func readRecording(name string) (document, error) {
	body, err := os.ReadFile(filepath.Join(recordingDir, name))
	if err != nil {
		return document{}, err
	}
	return read(body)
}

func writeRecording(scrubbed recording) error {
	for _, endpoint := range endpoints {
		body, err := scrubbed.file(endpoint.file).bytes()
		if err != nil {
			return fmt.Errorf("writing %s: %w", endpoint.file, err)
		}
		// A recording is a committed fixture rather than a secret; what would
		// have been secret about it was taken out above.
		if err := os.WriteFile(filepath.Join(recordingDir, endpoint.file), body, 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", endpoint.file, err)
		}
	}
	return nil
}

// atRepoRoot fails early rather than writing a recording somewhere surprising.
func atRepoRoot() error {
	if info, err := os.Stat(recordingDir); err != nil || !info.IsDir() {
		return fmt.Errorf("%s is not here; run this from the repository root, or as `make record-udr`", recordingDir)
	}
	return nil
}

// recordingIsCommitted refuses to start on a recording that already has
// uncommitted edits. The whole stop below is "read this diff", and a diff
// holding somebody's half-finished changes as well is one nobody can read.
func recordingIsCommitted() error {
	out, err := exec.Command("git", "status", "--porcelain", "--", recordingDir).Output()
	if err != nil {
		return fmt.Errorf("asking git whether %s is clean: %w", recordingDir, err)
	}
	if len(strings.TrimSpace(string(out))) > 0 {
		return fmt.Errorf("%s already has uncommitted changes:\n%s\nCommit or `git restore` them first, so the diff you read afterwards is only the re-recording",
			recordingDir, out)
	}
	return nil
}

func showDiff() error {
	diff := exec.Command("git", "--no-pager", "diff", "--", recordingDir)
	diff.Stdout = os.Stdout
	diff.Stderr = os.Stderr
	if err := diff.Run(); err != nil {
		return fmt.Errorf("showing the diff: %w", err)
	}
	return nil
}

// fail says what went wrong the way unifig itself does, and is the only exit
// code this program has other than 0.
func fail(err error) int {
	_, _ = fmt.Fprintf(os.Stderr, "record-udr: %v\n", err)
	return 1
}

func say(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stdout, format, args...)
}

// yes asks a question the operator has to answer for themselves. Silence is
// not consent: an unattended run stops here, which is the point of the stop.
func yes(asking *bufio.Scanner, question string) bool {
	say("%s? [y/N] ", question)
	if !asking.Scan() {
		say("\n")
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(asking.Text()))
	return answer == "y" || answer == "yes"
}
