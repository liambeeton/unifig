// Command compat runs the compatibility matrix and generates what it proves.
//
// The matrix is the whole process-level suite, run against every dockerized
// Controller version in compatibility.yaml. What comes out of those runs is the
// published table (docs/COMPATIBILITY.md) and the evidence unifig itself
// carries (internal/compat/matrix.json), which is what decides whether an
// operator's Controller earns a warning. Neither is written by hand, and CI
// fails a change that leaves either out of date.
//
//	compat versions                    print the matrix, for CI to fan out over
//	compat run [-version V] -out DIR   run the suite and keep the results
//	compat generate -results DIR       write the table and the evidence
//	compat check -results DIR          fail if either is out of date
//
// Run it from the repository root, or as `make matrix`.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/liambeeton/unifig/internal/compat"
)

// Where everything lives. These are paths rather than configuration because a
// generator that could be pointed at another repository's files would be a
// generator that could publish them.
const (
	configFile   = "compatibility.yaml"
	suiteDir     = "e2e"
	recordingDir = "e2e/testdata/udr"
	sysinfoFile  = "e2e/testdata/udr/sysinfo.json"
	matrixFile   = "internal/compat/matrix.json"
	tableFile    = "docs/COMPATIBILITY.md"
)

const usage = `usage: compat <command> [flags]

  versions                    print the matrix's Controller versions as JSON
  run [-version V] -out DIR   run the suite against every version in the matrix,
                              or one of them, keeping each run's results in DIR
  generate -results DIR       write ` + tableFile + ` and ` + matrixFile + `
  check -results DIR          fail if either is out of date

Run from the repository root.
`

func main() { os.Exit(run()) }

func run() int {
	if len(os.Args) < 2 {
		_, _ = fmt.Fprint(os.Stderr, usage)
		return 1
	}
	if err := atRepoRoot(); err != nil {
		return fail(err)
	}

	verb, args := os.Args[1], os.Args[2:]
	var err error
	switch verb {
	case "versions":
		err = printVersions()
	case "run":
		err = runMatrix(args)
	case "generate":
		err = publish(args, false)
	case "check":
		err = publish(args, true)
	default:
		_, _ = fmt.Fprint(os.Stderr, usage)
		return 1
	}
	if err != nil {
		return fail(err)
	}
	return 0
}

// printVersions writes the matrix as a JSON array, which is how CI turns
// compatibility.yaml into a job per version. It goes through the same loader as
// everything else, so a configuration CI would refuse never becomes a workflow
// that silently runs against fewer Controllers than the table claims.
func printVersions() error {
	cfg, err := compat.LoadConfig(configFile)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(cfg.Versions)
	if err != nil {
		return err
	}
	say("%s\n", encoded)
	return nil
}

// runMatrix boots each Controller version in the matrix and runs the whole
// suite against it, keeping the machine-readable result of each run and showing
// the human-readable one as it happens.
func runMatrix(args []string) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	only := flags.String("version", "", "run against this Controller version alone, rather than the whole matrix")
	out := flags.String("out", "", "directory to keep each run's results in")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return errors.New("-out says where to keep the results, and is not set")
	}

	cfg, err := compat.LoadConfig(configFile)
	if err != nil {
		return err
	}
	versions := cfg.Versions
	if *only != "" {
		if !slices.Contains(versions, *only) {
			return fmt.Errorf("UniFi Network %s is not in the matrix; the versions are %s",
				*only, strings.Join(cfg.Versions, ", "))
		}
		versions = []string{*only}
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return fmt.Errorf("making somewhere to keep the results: %w", err)
	}

	for _, version := range versions {
		image := cfg.ControllerImage(version)
		say("\n==> UniFi Network %s (%s)\n\n", version, image)
		run, err := runSuite(image, filepath.Join(*out, version+".json"))
		if err != nil {
			return err
		}
		say("\n==> UniFi Network %s: %s\n", version, run.summary())
		if !run.ok() {
			return fmt.Errorf("the suite did not pass against UniFi Network %s", version)
		}
	}
	return nil
}

// runSuite runs the e2e suite against one Controller image, writing the raw
// `go test -json` stream to path and the readable one to stdout as it arrives.
//
// The exit code of `go test` is deliberately not what decides the outcome: the
// results are, because they are also what the table is generated from. A run
// that ended badly without a failing test in it — a rig that would not start, a
// build that would not build — is caught as a broken run rather than read as
// "no tests failed".
func runSuite(image, path string) (results, error) {
	file, err := os.Create(path)
	if err != nil {
		return results{}, fmt.Errorf("keeping the results: %w", err)
	}
	defer func() { _ = file.Close() }()

	cmd := exec.Command("go", "test", "./"+suiteDir+"/...", "-timeout", "20m", "-count=1", "-json")
	cmd.Env = append(os.Environ(), "UNIFIG_TEST_CONTROLLER_IMAGE="+image)
	cmd.Stderr = os.Stderr
	stream, err := cmd.StdoutPipe()
	if err != nil {
		return results{}, err
	}
	if err := cmd.Start(); err != nil {
		return results{}, fmt.Errorf("running the suite: %w", err)
	}

	run, readErr := readResults(io.TeeReader(stream, file), os.Stdout)
	waitErr := cmd.Wait()
	switch {
	case readErr != nil:
		return results{}, readErr
	case waitErr != nil && run.ok():
		// The suite reported nothing wrong and the process still failed, which
		// is not something to publish a table on.
		return results{}, fmt.Errorf("running the suite against %s: %w", image, waitErr)
	}
	if err := file.Close(); err != nil {
		return results{}, fmt.Errorf("keeping the results: %w", err)
	}
	return run, nil
}

// publish builds the table and the evidence out of the runs, and either writes
// them or says they are out of date.
func publish(args []string, check bool) error {
	verb := "generate"
	if check {
		verb = "check"
	}
	flags := flag.NewFlagSet(verb, flag.ContinueOnError)
	from := flags.String("results", "", "directory holding one <version>.json per run")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *from == "" {
		return errors.New("-results says where the runs' results are, and is not set")
	}

	cfg, err := compat.LoadConfig(configFile)
	if err != nil {
		return err
	}
	tests, err := readSuite(suiteDir)
	if err != nil {
		return err
	}
	recording, err := readRecording(sysinfoFile)
	if err != nil {
		return err
	}
	runs, err := readRuns(*from, cfg.Versions)
	if err != nil {
		return err
	}
	matrix, err := build(cfg, tests, runs, recording)
	if err != nil {
		return err
	}

	evidence, err := json.MarshalIndent(matrix, "", "  ")
	if err != nil {
		return err
	}
	evidence = append(evidence, '\n')
	table := []byte(render(matrix))

	if check {
		return errors.Join(upToDate(matrixFile, evidence), upToDate(tableFile, table))
	}
	if err := write(matrixFile, evidence); err != nil {
		return err
	}
	if err := write(tableFile, table); err != nil {
		return err
	}
	say("Wrote %s and %s: %d Controller version(s), %d area(s).\n",
		tableFile, matrixFile, len(matrix.Versions), len(matrix.Areas))
	return nil
}

func readRuns(dir string, versions []string) (map[string]results, error) {
	runs := map[string]results{}
	for _, version := range versions {
		path := filepath.Join(dir, version+".json")
		file, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("reading the run against UniFi Network %s: %w", version, err)
		}
		run, err := readResults(file, nil)
		_ = file.Close()
		if err != nil {
			return nil, err
		}
		runs[version] = run
	}
	return runs, nil
}

// upToDate is the check CI runs: the committed file has to be the one these
// runs produce, so that the published table cannot say something no run said.
func upToDate(path string, want []byte) error {
	got, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%s is missing; run `make matrix`", path)
	}
	if string(got) == string(want) {
		return nil
	}
	return fmt.Errorf("%s is out of date — run `make matrix` and commit it.\n%s", path, firstDifference(string(got), string(want)))
}

// firstDifference is the one line worth printing about two files that should
// have been identical.
func firstDifference(got, want string) string {
	gotLines, wantLines := strings.Split(got, "\n"), strings.Split(want, "\n")
	for i := 0; i < len(gotLines) || i < len(wantLines); i++ {
		g, w := at(gotLines, i), at(wantLines, i)
		if g != w {
			return fmt.Sprintf("  line %d\n  committed: %s\n  the runs:  %s", i+1, g, w)
		}
	}
	return ""
}

func at(lines []string, i int) string {
	if i < len(lines) {
		return lines[i]
	}
	return "(end of file)"
}

func write(path string, body []byte) error {
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// atRepoRoot fails early rather than reading a suite that is not there.
func atRepoRoot() error {
	if _, err := os.Stat(configFile); err != nil {
		return fmt.Errorf("%s is not here; run this from the repository root, or as `make matrix`", configFile)
	}
	return nil
}

func fail(err error) int {
	_, _ = fmt.Fprintf(os.Stderr, "compat: %v\n", err)
	return 1
}

func say(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stdout, format, args...)
}
