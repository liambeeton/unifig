package e2e

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// The version pin doubles as the CI compatibility promise for this suite;
// override the image via UNIFIG_TEST_CONTROLLER_IMAGE when running the matrix.
const defaultControllerImage = "jacobalberty/unifi:v10.0.162"

// rig is the process-level test rig: a real dockerized UniFi Network
// application (the Controller) fronted by a reverse proxy that emulates the
// one UniFi OS behavior the bare Network application lacks — validating the
// X-Api-Key header on Internal API requests under /proxy/network. unifig runs
// as a whole process against the proxy's base URL, exactly as it would against
// a UDR.
type rig struct {
	// controllerURL is the dockerized Network application's own base URL.
	// Rig plumbing (seeding, login) talks to it directly with a session
	// cookie; unifig itself never sees this URL.
	controllerURL string
	// proxyURL is the UniFi-OS-emulating base URL unifig is pointed at.
	proxyURL string
	// apiKey is the key the proxy accepts, standing in for a UDR API key.
	apiKey string

	binary    string // built unifig binary
	session   string // unifises session cookie for rig plumbing
	client    *http.Client
	proxy     *httptest.Server
	container testcontainers.Container
}

// insecureTransport trusts the Controller's self-signed certificate.
func insecureTransport() *http.Transport {
	return &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
}

// startRig builds the unifig binary, starts (or adopts) a Controller, logs in,
// and starts the UniFi OS emulation proxy. On error the returned rig holds
// whatever was already started, so the caller can shut it down.
func startRig(ctx context.Context) (*rig, error) {
	r := &rig{
		apiKey: "unifig-e2e-test-api-key",
		client: &http.Client{Timeout: 30 * time.Second, Transport: insecureTransport()},
	}

	if err := r.buildBinary(); err != nil {
		return r, err
	}
	if err := r.startController(ctx); err != nil {
		return r, err
	}
	if err := r.login(ctx); err != nil {
		return r, err
	}
	if err := r.startProxy(); err != nil {
		return r, err
	}
	return r, nil
}

func (r *rig) shutdown(ctx context.Context) {
	if r.proxy != nil {
		r.proxy.Close()
	}
	if r.container != nil {
		_ = r.container.Terminate(ctx)
	}
}

func (r *rig) buildBinary() error {
	dir, err := os.MkdirTemp("", "unifig-e2e-*")
	if err != nil {
		return err
	}
	r.binary = dir + "/unifig"
	cmd := exec.Command("go", "build", "-o", r.binary, "github.com/liambeeton/unifig/cmd/unifig")
	cmd.Dir = ".." // repo root
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("building unifig: %v\n%s", err, out)
	}
	return nil
}

// startController boots the pinned dockerized Controller in demo mode, or
// adopts an already-running one when UNIFIG_TEST_CONTROLLER_URL is set (a
// faster inner loop while developing the suite).
func (r *rig) startController(ctx context.Context) error {
	if u := os.Getenv("UNIFIG_TEST_CONTROLLER_URL"); u != "" {
		r.controllerURL = u
		return nil
	}

	image := os.Getenv("UNIFIG_TEST_CONTROLLER_IMAGE")
	if image == "" {
		image = defaultControllerImage
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        image,
			ExposedPorts: []string{"8443/tcp"},
			Env:          map[string]string{"UNIFI_STDOUT": "true"},
			Files: []testcontainers.ContainerFile{{
				HostFilePath:      "testdata/demo-mode",
				ContainerFilePath: "/usr/local/unifi/init.d/demo-mode",
				FileMode:          0o755,
			}},
			WaitingFor: wait.ForHTTP("/status").
				WithPort("8443/tcp").
				WithTLS(true, &tls.Config{InsecureSkipVerify: true}).
				WithStatusCodeMatcher(func(status int) bool { return status == http.StatusOK }).
				WithResponseMatcher(func(body io.Reader) bool {
					b, _ := io.ReadAll(body)
					return bytes.Contains(b, []byte(`"up":true`))
				}).
				WithPollInterval(2 * time.Second).
				WithStartupTimeout(5 * time.Minute),
		},
		Started: true,
	})
	if err != nil {
		return fmt.Errorf("starting Controller container %s: %w", image, err)
	}
	r.container = container

	endpoint, err := container.PortEndpoint(ctx, "8443/tcp", "https")
	if err != nil {
		return err
	}
	r.controllerURL = endpoint
	return nil
}

func (r *rig) login(ctx context.Context) error {
	body := strings.NewReader(`{"username":"admin","password":"admin"}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.controllerURL+"/api/login", body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("logging in to Controller: %w", err)
	}
	defer resp.Body.Close()
	for _, c := range resp.Cookies() {
		if c.Name == "unifises" {
			r.session = c.Value
			return nil
		}
	}
	return fmt.Errorf("Controller login returned no session cookie (status %d)", resp.StatusCode)
}

// startProxy starts the UniFi OS emulation in front of the Controller:
//   - GET /            -> 200 (the SDK's new-style API detection probe)
//   - /proxy/network/* -> forwarded to the Controller with the rig's admin
//     session, but only when X-Api-Key matches; 401 otherwise, mirroring
//     the UDR's OS-level API-key gate
//   - anything else    -> 404
func (r *rig) startProxy() error {
	backend, err := url.Parse(r.controllerURL)
	if err != nil {
		return err
	}
	forward := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = backend.Scheme
			pr.Out.URL.Host = backend.Host
			pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, "/proxy/network")
			pr.Out.Host = backend.Host
			pr.Out.Header.Del("X-Api-Key")
			pr.Out.Header.Set("Cookie", "unifises="+r.session)
		},
		Transport: insecureTransport(),
	}
	r.proxy = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if req.Header.Get("X-Api-Key") != r.apiKey {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":401,"message":"Unauthorized"}}`))
			return
		}
		if !strings.HasPrefix(req.URL.Path, "/proxy/network/") {
			http.NotFound(w, req)
			return
		}
		forward.ServeHTTP(w, req)
	}))
	r.proxyURL = r.proxy.URL
	return nil
}

// seedNetwork creates a networkconf entry on the live Controller through its
// own API — rig plumbing, not a code-level substitute: unifig still reads it
// back through the whole stack. Any existing entries with the same name are
// deleted first so seeding is idempotent across reused controllers.
func (r *rig) seedNetwork(t *testing.T, conf map[string]any) {
	t.Helper()
	name, _ := conf["name"].(string)
	r.deleteNetworksNamed(t, name)
	r.addNetwork(t, conf)
}

// addNetwork creates a networkconf entry without clearing same-named ones
// first, which is how a test sets up the live duplicate names that unifig has
// to refuse to choose between. The Controller allows them; unifig does not.
func (r *rig) addNetwork(t *testing.T, conf map[string]any) {
	t.Helper()
	name, _ := conf["name"].(string)

	body, err := json.Marshal(conf)
	if err != nil {
		t.Fatalf("marshaling network conf: %v", err)
	}
	resp := r.controllerDo(t, http.MethodPost, "/api/s/default/rest/networkconf", bytes.NewReader(body))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("seeding network %q: status %d: %s", name, resp.StatusCode, b)
	}
}

func (r *rig) deleteNetworksNamed(t *testing.T, name string) {
	t.Helper()
	for _, n := range r.networksNamed(t, name) {
		id, _ := n["_id"].(string)
		del := r.controllerDo(t, http.MethodDelete, "/api/s/default/rest/networkconf/"+id, nil)
		del.Body.Close()
	}
}

// liveNetwork reads one network back off the Controller as the raw JSON the
// Controller stores, which is how a test checks what an apply actually did.
// Reading it through unifig would only prove unifig agrees with itself.
func (r *rig) liveNetwork(t *testing.T, name string) map[string]any {
	t.Helper()
	found := r.networksNamed(t, name)
	if len(found) != 1 {
		t.Fatalf("the Controller has %d networks named %q, want exactly 1", len(found), name)
	}
	return found[0]
}

func (r *rig) networksNamed(t *testing.T, name string) []map[string]any {
	t.Helper()
	resp := r.controllerDo(t, http.MethodGet, "/api/s/default/rest/networkconf", nil)
	defer resp.Body.Close()

	var list struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("listing networks: %v", err)
	}

	var found []map[string]any
	for _, n := range list.Data {
		if n["name"] == name {
			found = append(found, n)
		}
	}
	return found
}

func (r *rig) controllerDo(t *testing.T, method, path string, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, r.controllerURL+path, body)
	if err != nil {
		t.Fatalf("building %s %s: %v", method, path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "unifises", Value: r.session})
	resp, err := r.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

// result captures one whole-tool run at the process boundary.
type result struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
}

// runUnifig executes the real unifig binary with a clean environment and
// nothing on stdin. extraEnv entries override the rig defaults, and setting a
// variable to "" removes it.
func (r *rig) runUnifig(t *testing.T, args []string, extraEnv map[string]string) result {
	t.Helper()
	return r.runUnifigWithInput(t, args, extraEnv, "")
}

// runUnifigWithInput is runUnifig for a verb that asks the operator something.
// stdin is a closed pipe carrying exactly what the operator typed, so an empty
// string is the operator who answered nothing at all.
func (r *rig) runUnifigWithInput(t *testing.T, args []string, extraEnv map[string]string, stdin string) result {
	t.Helper()
	env := map[string]string{
		"PATH":            os.Getenv("PATH"),
		"HOME":            os.Getenv("HOME"),
		"UNIFIG_HOST":     r.proxyURL,
		"UNIFIG_API_KEY":  r.apiKey,
		"UNIFIG_INSECURE": "true",
	}
	for k, v := range extraEnv {
		if v == "" {
			delete(env, k)
			continue
		}
		env[k] = v
	}

	cmd := exec.Command(r.binary, args...)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("running unifig %v: %v", args, err)
		}
		code = exitErr.ExitCode()
	}
	return result{ExitCode: code, Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
}
