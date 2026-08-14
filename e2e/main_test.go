package e2e

import (
	"context"
	"flag"
	"fmt"
	"os"
	"testing"
)

var testRig *rig

// TestMain boots the process-level rig once for the whole suite: the real
// unifig binary, a real dockerized Controller, and the UniFi OS emulation
// proxy in front of it. `go test -short` skips the suite entirely.
func TestMain(m *testing.M) {
	flag.Parse()
	if testing.Short() {
		fmt.Println("skipping e2e suite in -short mode")
		os.Exit(0)
	}

	ctx := context.Background()
	r, err := startRig(ctx)
	if err != nil {
		r.shutdown(ctx)
		fmt.Fprintf(os.Stderr, "e2e rig failed to start: %v\n", err)
		os.Exit(1)
	}
	testRig = r

	code := m.Run()
	r.shutdown(ctx)
	os.Exit(code)
}
