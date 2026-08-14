// Package cli is unifig's thin command layer: verb dispatch, connection
// config from the environment, and wiring the Controller client to the
// engine. All behavior worth testing lives behind it and is exercised at the
// process boundary.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/filipowm/go-unifi/v2/unifi"

	"github.com/liambeeton/unifig/internal/export"
)

const usage = `usage: unifig <command>

commands:
  export    print the Controller's configuration as YAML on stdout

connection (environment variables):
  UNIFIG_HOST      Controller host or base URL, e.g. https://192.168.1.1
  UNIFIG_API_KEY   Controller API key
  UNIFIG_INSECURE  set to true to skip TLS certificate verification
`

// site is fixed: a config tree manages a single Controller, single site.
const site = "default"

// Run executes one unifig invocation and returns its process exit code:
// 0 on success, 1 on any error. Exit code 2 stays reserved for plan's
// "changes pending".
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 || args[0] != "export" {
		_, _ = fmt.Fprint(stderr, usage)
		return 1
	}
	if err := runExport(ctx, stdout); err != nil {
		_, _ = fmt.Fprintf(stderr, "unifig: %v\n", err)
		return 1
	}
	return 0
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
	return export.WriteYAML(stdout, cfg)
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
