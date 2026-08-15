// Package cli is unifig's thin command layer: verb dispatch, connection
// config from the environment, and wiring the Controller client to the
// engine. All behavior worth testing lives behind it and is exercised at the
// process boundary.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/filipowm/go-unifi/v2/unifi"

	"github.com/liambeeton/unifig/internal/config"
	"github.com/liambeeton/unifig/internal/export"
)

const usage = `usage: unifig <command>

commands:
  export            print the Controller's configuration as YAML on stdout
  validate [file]   check a config file offline (default: unifig.yaml)

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

// Run executes one unifig invocation and returns its process exit code:
// 0 on success, 1 on any error. Exit code 2 stays reserved for plan's
// "changes pending".
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	err := dispatch(ctx, args, stdout)
	switch {
	case errors.Is(err, errUsage):
		_, _ = fmt.Fprint(stderr, usage)
		return 1
	case err != nil:
		_, _ = fmt.Fprintf(stderr, "unifig: %v\n", err)
		return 1
	}
	return 0
}

func dispatch(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errUsage
	}
	verb, rest := args[0], args[1:]
	switch verb {
	case "export":
		if len(rest) != 0 {
			return errUsage
		}
		return runExport(ctx, stdout)
	case "validate":
		if len(rest) > 1 {
			return errUsage
		}
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
	path := config.DefaultPath
	if len(args) == 1 {
		path = args[0]
	}
	if _, err := config.Load(path); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "%s is valid\n", path)
	return nil
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
		Logger:             unifi.NewDefaultLogger(unifi.WarnLevel),
	})
}
