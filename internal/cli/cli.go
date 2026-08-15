// Package cli is unifig's thin command layer: verb dispatch, connection
// config from the environment, and wiring the Controller client to the
// engine. All behavior worth testing lives behind it and is exercised at the
// process boundary.
package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/filipowm/go-unifi/v2/unifi"

	"github.com/liambeeton/unifig/internal/config"
	"github.com/liambeeton/unifig/internal/export"
	"github.com/liambeeton/unifig/internal/reconcile"
)

const usage = `usage: unifig <command> [flags] [file]

commands:
  plan [file]       show what apply would change on the Controller
  apply [file]      change the Controller to match the config file
  export            print the Controller's configuration as YAML on stdout
  validate [file]   check a config file offline

[file] defaults to unifig.yaml in the working directory.

flags:
  --json            plan only: write the plan as JSON instead of prose
  --auto-approve    apply only: do not ask before changing the Controller

exit codes:
  0  success; for plan, the Controller already matches the config
  1  error
  2  plan only: changes are pending

connection (environment variables; not used by validate, which is offline):
  UNIFIG_HOST      Controller host or base URL, e.g. https://192.168.1.1
  UNIFIG_API_KEY   Controller API key
  UNIFIG_INSECURE  set to true to skip TLS certificate verification
`

// site is fixed: a config tree manages a single Controller, single site.
const site = "default"

// errUsage means the command line itself was wrong, which earns the usage
// text rather than a one-line error — the operator needs the whole menu.
var errUsage = errors.New("bad usage")

// errChangesPending is plan's "there is work to do". It is not a failure,
// which is why it prints nothing of its own and exits 2 rather than 1; it
// travels as an error only because that is the one channel every verb already
// has back to Run.
var errChangesPending = errors.New("changes pending")

// Run executes one unifig invocation and returns its process exit code:
// 0 on success, 1 on any error, and 2 when plan found changes pending.
func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	err := dispatch(ctx, args, stdin, stdout)
	switch {
	case errors.Is(err, errUsage):
		_, _ = fmt.Fprint(stderr, usage)
		return 1
	case errors.Is(err, errChangesPending):
		return 2
	case err != nil:
		_, _ = fmt.Fprintf(stderr, "unifig: %v\n", err)
		return 1
	}
	return 0
}

func dispatch(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 {
		return errUsage
	}
	verb, rest := args[0], args[1:]
	switch verb {
	case "plan":
		return runPlan(ctx, rest, stdout)
	case "apply":
		return runApply(ctx, rest, stdin, stdout)
	case "export":
		if len(rest) != 0 {
			return errUsage
		}
		return runExport(ctx, stdout)
	case "validate":
		return runValidate(rest, stdout)
	default:
		return errUsage
	}
}

// connection holds Controller connection config. It is read from the
// environment only — never from the resource YAML, and never written to disk.
type connection struct {
	url      string
	apiKey   string
	insecure bool
}

func connectionFromEnv() (connection, error) {
	host := os.Getenv("UNIFIG_HOST")
	if host == "" {
		return connection{}, fmt.Errorf("UNIFIG_HOST is not set; set it to the Controller URL, e.g. https://192.168.1.1")
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

// runPlan shows what apply would do and says so in its exit code, so that a
// pipeline or a git hook can gate on drift without reading the output.
func runPlan(ctx context.Context, args []string, stdout io.Writer) error {
	flags, positional, err := splitFlags(args, "json")
	if err != nil {
		return err
	}
	path, err := configPath(positional)
	if err != nil {
		return err
	}

	_, plan, err := computePlan(ctx, path)
	if err != nil {
		return err
	}

	if flags["json"] {
		err = plan.WriteJSON(stdout)
	} else {
		err = plan.Write(stdout)
	}
	if err != nil {
		return err
	}
	if !plan.Empty() {
		return errChangesPending
	}
	return nil
}

// runApply shows the plan and then executes it. It always plans first, and
// always shows what it planned: there is no way to reach the Controller
// through unifig without the changes having been printed first.
func runApply(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	flags, positional, err := splitFlags(args, "auto-approve")
	if err != nil {
		return err
	}
	path, err := configPath(positional)
	if err != nil {
		return err
	}

	client, plan, err := computePlan(ctx, path)
	if err != nil {
		return err
	}
	if err := plan.Write(stdout); err != nil {
		return err
	}
	if plan.Empty() {
		return nil
	}

	if !flags["auto-approve"] {
		approved, err := confirm(stdin, stdout)
		if err != nil {
			return err
		}
		if !approved {
			return errors.New("apply cancelled; the Controller was not changed")
		}
	}
	return plan.Apply(ctx, client, site, stdout)
}

// computePlan is the whole read-only half of a reconcile: load the config,
// connect, compare. plan and apply share it so that what apply is about to do is
// literally what plan just showed, and it hands back the client because apply
// needs to keep talking to the same Controller it just read.
func computePlan(ctx context.Context, path string) (unifi.Client, reconcile.Plan, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, reconcile.Plan{}, err
	}
	conn, err := connectionFromEnv()
	if err != nil {
		return nil, reconcile.Plan{}, err
	}
	client, err := connect(conn)
	if err != nil {
		return nil, reconcile.Plan{}, fmt.Errorf("connecting to Controller at %s: %w", conn.url, err)
	}
	plan, err := reconcile.Networks(ctx, client, site, cfg)
	if err != nil {
		return nil, reconcile.Plan{}, err
	}
	return client, plan, nil
}

// confirm asks before anything is changed. Only an explicit yes counts:
// anything else — a bare newline, a typo, or a closed stdin — means no, so an
// apply that finds itself running unattended without --auto-approve stops
// rather than guesses.
func confirm(stdin io.Reader, stdout io.Writer) (bool, error) {
	_, _ = fmt.Fprint(stdout, "\nApply these changes to the Controller? [y/N] ")
	answer, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("reading confirmation: %w", err)
	}

	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}

func runExport(ctx context.Context, stdout io.Writer) error {
	conn, err := connectionFromEnv()
	if err != nil {
		return err
	}
	client, err := connect(conn)
	if err != nil {
		return fmt.Errorf("connecting to Controller at %s: %w", conn.url, err)
	}
	cfg, err := export.Networks(ctx, client, site)
	if err != nil {
		return fmt.Errorf("exporting networks: %w", err)
	}
	return config.WriteYAML(stdout, cfg)
}

// runValidate is config.Load and nothing else. That is the point: validate's
// promise is that a file it accepts is a file the other verbs can load, and
// the only way to keep that promise is for it to be the very same call.
func runValidate(args []string, stdout io.Writer) error {
	path, err := configPath(args)
	if err != nil {
		return err
	}
	if _, err := config.Load(path); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "%s is valid\n", path)
	return nil
}

// configPath resolves the optional file argument every config-reading verb
// takes.
func configPath(positional []string) (string, error) {
	switch len(positional) {
	case 0:
		return config.DefaultPath, nil
	case 1:
		return positional[0], nil
	default:
		return "", errUsage
	}
}

// splitFlags separates the flags a verb accepts from its positional
// arguments, in whichever order they were given.
//
// unifig's flags are all plain booleans, and hand-rolling the split buys the
// one thing an operator would notice: `unifig plan unifig.yaml --json` does
// what it looks like. The standard library's flag package stops parsing at the
// first non-flag argument, so the same command there would silently ignore
// --json and print prose.
func splitFlags(args []string, accepted ...string) (map[string]bool, []string, error) {
	set := make(map[string]bool, len(accepted))
	var positional []string

	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
			continue
		}
		name := strings.TrimLeft(arg, "-")
		if !slices.Contains(accepted, name) {
			return nil, nil, errUsage
		}
		set[name] = true
	}
	return set, positional, nil
}

func connect(conn connection) (unifi.Client, error) {
	return unifi.NewClient(&unifi.ClientConfig{
		URL:           conn.url,
		APIKey:        conn.apiKey,
		SkipVerifySSL: conn.insecure,
		Timeout:       30 * time.Second,
		// ADR-0002: all resource operations use the Internal API; the
		// Integration API is not used.
		DisableOfficialAPI: true,
		// Turning the SDK's own request-body validation off is not a choice
		// so much as a workaround: its generated `validate` tag for a
		// network's wan_type spells the
		// oneof values `map-e,hubspoke map-e,jpix ...`, and the space inside
		// a value makes validator/v10 read `hubspoke` as a rule name it does
		// not have — which it reports by panicking, on every network write,
		// before the request is sent (go-unifi v2.3.0).
		//
		// Little is lost. The values unifig sends come from a file the JSON
		// Schema has already checked, and the Controller validates the rest;
		// what this turns off is a third opinion that cannot currently be
		// consulted without crashing.
		ValidationMode: unifi.DisableValidation,
		Logger:         unifi.NewDefaultLogger(unifi.WarnLevel),
	})
}
