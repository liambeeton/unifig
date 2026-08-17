package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A recording is only worth having if it came off the router untouched, and
// only safe to make if making it cannot change anything. These are the two
// statements about the trip to the Controller; everything about what is then
// kept is in scrub_test.go.

func TestRecordingOnlyEverAsksTheControllerForThings(t *testing.T) {
	var asked []string
	controller := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		asked = append(asked, req.Method+" "+req.URL.Path)
		if req.Header.Get("X-Api-Key") != "the-api-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(bodyFor(req.URL.Path)))
	}))
	defer controller.Close()

	raw, dir, err := record(connection{url: controller.URL, apiKey: "the-api-key", insecure: true})
	if dir != "" {
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
	}
	if err != nil {
		t.Fatalf("recording: %v", err)
	}

	for _, request := range asked {
		if !strings.HasPrefix(request, "GET ") {
			t.Errorf("record-udr sent %q; it may only ask a live Controller for things", request)
		}
	}
	if len(asked) != len(endpoints) {
		t.Errorf("record-udr made %d requests, want %d: %v", len(asked), len(endpoints), asked)
	}
	if len(raw.networkconf.Data) == 0 || len(raw.sysinfo.Data) == 0 {
		t.Errorf("the recording came back empty: %+v", raw)
	}

	// The raw bodies are outside the repository, and readable only by the
	// operator: they hold the PPPoE password in the clear.
	for _, endpoint := range endpoints {
		info, err := os.Stat(filepath.Join(dir, endpoint.file))
		if err != nil {
			t.Fatalf("the raw %s was not kept: %v", endpoint.file, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("the raw %s is mode %v, want 0600", endpoint.file, info.Mode().Perm())
		}
	}
	if repo, err := filepath.Abs("."); err == nil && strings.HasPrefix(dir, repo) {
		t.Errorf("the raw responses were kept inside the repository, at %s", dir)
	}
}

func TestRecordingStopsWhenTheControllerSaysNo(t *testing.T) {
	for _, refusal := range []struct {
		name   string
		status int
		body   string
		says   string
	}{
		{"an API key it will not take", http.StatusUnauthorized, `{"meta":{"rc":"error"}}`, "API key"},
		{"a failure inside a 200", http.StatusOK, `{"meta":{"rc":"error","msg":"api.err.NoSiteContext"}}`, "NoSiteContext"},
		{"an answer that is not JSON", http.StatusOK, `<html>login</html>`, "not JSON"},
	} {
		t.Run(refusal.name, func(t *testing.T) {
			controller := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(refusal.status)
				_, _ = w.Write([]byte(refusal.body))
			}))
			defer controller.Close()

			_, dir, err := record(connection{url: controller.URL, apiKey: "k", insecure: true})
			if dir != "" {
				t.Cleanup(func() { _ = os.RemoveAll(dir) })
			}
			if err == nil {
				t.Fatalf("recording a refusal succeeded, and would have written a fixture that answers no to everything")
			}
			if !strings.Contains(err.Error(), refusal.says) {
				t.Errorf("the error should say what happened (%q), got: %v", refusal.says, err)
			}
		})
	}
}

// The paths recorded here are the paths the stand-in serves back. Recording
// anything else produces a file the WAN suite never reaches, and the suite
// would go on passing against the old recording while the operator believed
// they had replaced it.
func TestTheRecordedEndpointsAreTheOnesTheStandInServes(t *testing.T) {
	standIn, err := os.ReadFile(filepath.Join("..", "..", "e2e", "replay_test.go"))
	if err != nil {
		t.Fatalf("reading the stand-in: %v", err)
	}
	for _, endpoint := range endpoints {
		if !strings.Contains(string(standIn), `"`+endpoint.path+`"`) {
			t.Errorf("e2e/replay_test.go does not serve %s, so a recording of it would never be replayed", endpoint.path)
		}
	}
}

func bodyFor(path string) string {
	switch {
	case strings.HasSuffix(path, "sysinfo"):
		return rawSysinfo
	case strings.HasSuffix(path, "networkconf"):
		return rawNetworkconf
	// The v2 tree answers with a bare array and no envelope, which is the shape
	// this fake Controller has to reproduce: a recorder that only ever met
	// enveloped responses would fail on the first real firewall endpoint.
	case strings.Contains(path, "/v2/api/"):
		return `[]`
	default:
		return rawWlanconf
	}
}
