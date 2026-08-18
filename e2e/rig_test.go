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
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/liambeeton/unifig/internal/compat"
)

// compatibilityConfig is where the Controller versions live: the same file CI
// builds its matrix from and the published table is generated against
// (compatibility.yaml). The rig reads it rather than pinning a version of its
// own, so a bare `make e2e` runs against the newest version in the matrix and
// the two cannot drift. UNIFIG_TEST_CONTROLLER_IMAGE overrides it, which is how
// CI points one job at one version.
const compatibilityConfig = "../compatibility.yaml"

// How the Network application finds the database that starts beside it. The
// credentials are a container-to-container detail on a private network that
// lives for one suite run; nothing outside the rig can reach either.
const (
	databaseHost     = "database"
	databaseUser     = "unifig"
	databasePassword = "unifig"
	databaseName     = "unifi"
)

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

	binary  string // built unifig binary
	session string // unifises session cookie for rig plumbing
	client  *http.Client
	proxy   *httptest.Server
	// container is the Network application, database the MongoDB it needs, and
	// net the private network the two of them talk over. The Controller image
	// ships no database (see ADR-0016), so the rig starts one; unifig never
	// sees either.
	container testcontainers.Container
	database  testcontainers.Container
	net       *testcontainers.DockerNetwork
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
	// In the reverse of the order they were started, so the network is not
	// removed while something is still attached to it.
	if r.container != nil {
		_ = r.container.Terminate(ctx)
	}
	if r.database != nil {
		_ = r.database.Terminate(ctx)
	}
	if r.net != nil {
		_ = r.net.Remove(ctx)
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

// startController boots a dockerized Controller from the matrix in demo mode,
// with the database it needs beside it, or adopts an already-running one when
// UNIFIG_TEST_CONTROLLER_URL is set (a faster inner loop while developing the
// suite).
func (r *rig) startController(ctx context.Context) error {
	if u := os.Getenv("UNIFIG_TEST_CONTROLLER_URL"); u != "" {
		r.controllerURL = u
		return nil
	}

	cfg, err := compat.LoadConfig(compatibilityConfig)
	if err != nil {
		return err
	}
	image := os.Getenv("UNIFIG_TEST_CONTROLLER_IMAGE")
	if image == "" {
		image = cfg.ControllerImage(cfg.Newest())
	}

	// A network of its own, rather than the default bridge: it is what gives
	// the database a name the Controller can resolve, and it goes away with the
	// suite.
	private, err := network.New(ctx)
	if err != nil {
		return fmt.Errorf("creating the network the Controller and its database share: %w", err)
	}
	r.net = private

	if err := r.startDatabase(ctx, cfg.Container.Database); err != nil {
		return err
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        image,
			ExposedPorts: []string{"8443/tcp"},
			Networks:     []string{private.Name},
			Env: map[string]string{
				"MONGO_HOST":       databaseHost,
				"MONGO_PORT":       "27017",
				"MONGO_USER":       databaseUser,
				"MONGO_PASS":       databasePassword,
				"MONGO_DBNAME":     databaseName,
				"MONGO_AUTHSOURCE": "admin",
			},
			Files: []testcontainers.ContainerFile{{
				HostFilePath:      "testdata/demo-mode",
				ContainerFilePath: "/custom-cont-init.d/demo-mode",
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

// startDatabase starts the MongoDB the Network application stores everything
// in. It waits for the port to answer from outside the container, which is
// what tells the two mongod runs apart: the official image starts one bound to
// loopback to create the root user, and only the second is reachable at all.
func (r *rig) startDatabase(ctx context.Context, image string) error {
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        image,
			ExposedPorts: []string{"27017/tcp"},
			Networks:     []string{r.net.Name},
			NetworkAliases: map[string][]string{
				r.net.Name: {databaseHost},
			},
			Env: map[string]string{
				"MONGO_INITDB_ROOT_USERNAME": databaseUser,
				"MONGO_INITDB_ROOT_PASSWORD": databasePassword,
			},
			WaitingFor: wait.ForListeningPort("27017/tcp").WithStartupTimeout(3 * time.Minute),
		},
		Started: true,
	})
	if err != nil {
		return fmt.Errorf("starting the Controller's database (%s): %w", image, err)
	}
	r.database = container
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
	var found []map[string]any
	for _, n := range r.networks(t) {
		if n["name"] == name {
			found = append(found, n)
		}
	}
	return found
}

// networks reads every networkconf entry the Controller holds, whatever its
// purpose. WAN slots share this collection with the LANs unifig manages, so a
// test that wants to prove prune left them alone has to be able to see them.
func (r *rig) networks(t *testing.T) []map[string]any {
	t.Helper()
	resp := r.controllerDo(t, http.MethodGet, "/api/s/default/rest/networkconf", nil)
	defer resp.Body.Close()

	var list struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("listing networks: %v", err)
	}
	return list.Data
}

// managedNetworkNames names the live networkconf entries unifig manages as
// networks — the LAN purposes, and not the WAN slots or VPN purposes that share
// the collection with them. The purposes are spelled out again here rather than
// imported from the engine, so that the rig describes the Controller rather
// than agreeing with the code under test by construction.
func (r *rig) managedNetworkNames(t *testing.T) []string {
	t.Helper()
	var names []string
	for _, n := range r.networks(t) {
		switch n["purpose"] {
		case "corporate", "guest", "vlan-only":
		default:
			continue
		}
		if name, ok := n["name"].(string); ok {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// seedWLAN creates a wlanconf entry on the live Controller through its own API,
// clearing same-named entries first so seeding is idempotent across reused
// controllers. Its conf is raw Controller JSON for the same reason seedNetwork's
// is: the rig describes the Controller, not unifig's model of it.
func (r *rig) seedWLAN(t *testing.T, conf map[string]any) {
	t.Helper()
	name, _ := conf["name"].(string)
	r.deleteWLANsNamed(t, name)
	r.addWLAN(t, conf)
}

// addWLAN creates a wlanconf entry without clearing same-named ones first,
// which is how a test sets up the live duplicate names unifig has to refuse to
// choose between. The Controller allows them; unifig does not.
func (r *rig) addWLAN(t *testing.T, conf map[string]any) {
	t.Helper()
	name, _ := conf["name"].(string)

	// Two fields every wlanconf needs and no test cares about: the Controller
	// refuses a WLAN with no AP group to broadcast from, and rejects a null
	// schedule_with_duration outright.
	if _, ok := conf["ap_group_ids"]; !ok {
		conf["ap_group_ids"] = []string{r.defaultAPGroupID(t)}
	}
	conf["schedule_with_duration"] = []any{}

	body, err := json.Marshal(conf)
	if err != nil {
		t.Fatalf("marshaling WLAN conf: %v", err)
	}
	resp := r.controllerDo(t, http.MethodPost, "/api/s/default/rest/wlanconf", bytes.NewReader(body))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("seeding WLAN %q: status %d: %s", name, resp.StatusCode, b)
	}
}

func (r *rig) deleteWLANsNamed(t *testing.T, name string) {
	t.Helper()
	for _, w := range r.wlansNamed(t, name) {
		id, _ := w["_id"].(string)
		del := r.controllerDo(t, http.MethodDelete, "/api/s/default/rest/wlanconf/"+id, nil)
		del.Body.Close()
	}
}

// liveWLAN reads one WLAN back off the Controller as the raw JSON the Controller
// stores — how a test checks what an apply actually did, including whether the
// passphrase that went in is the one that came out.
func (r *rig) liveWLAN(t *testing.T, name string) map[string]any {
	t.Helper()
	found := r.wlansNamed(t, name)
	if len(found) != 1 {
		t.Fatalf("the Controller has %d WLANs named %q, want exactly 1", len(found), name)
	}
	return found[0]
}

func (r *rig) wlansNamed(t *testing.T, name string) []map[string]any {
	t.Helper()
	var found []map[string]any
	for _, w := range r.wlans(t) {
		if w["name"] == name {
			found = append(found, w)
		}
	}
	return found
}

func (r *rig) wlans(t *testing.T) []map[string]any {
	t.Helper()
	resp := r.controllerDo(t, http.MethodGet, "/api/s/default/rest/wlanconf", nil)
	defer resp.Body.Close()

	var list struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("listing WLANs: %v", err)
	}
	return list.Data
}

// seedPortForward creates a portforward entry on the live Controller through
// its own API, clearing same-named entries first so seeding is idempotent across
// reused controllers. Its conf is raw Controller JSON for the same reason
// seedNetwork's is: the rig describes the Controller, not unifig's model of it.
func (r *rig) seedPortForward(t *testing.T, conf map[string]any) {
	t.Helper()
	name, _ := conf["name"].(string)
	r.deletePortForwardsNamed(t, name)
	r.addPortForward(t, conf)
}

// addPortForward creates a portforward entry without clearing same-named ones
// first, which is how a test sets up the live duplicate names unifig has to
// refuse to choose between. The Controller allows them; unifig does not.
func (r *rig) addPortForward(t *testing.T, conf map[string]any) {
	t.Helper()
	name, _ := conf["name"].(string)

	body, err := json.Marshal(conf)
	if err != nil {
		t.Fatalf("marshaling port forward conf: %v", err)
	}
	resp := r.controllerDo(t, http.MethodPost, "/api/s/default/rest/portforward", bytes.NewReader(body))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("seeding port forward %q: status %d: %s", name, resp.StatusCode, b)
	}
}

func (r *rig) deletePortForwardsNamed(t *testing.T, name string) {
	t.Helper()
	for _, f := range r.portForwardsNamed(t, name) {
		id, _ := f["_id"].(string)
		del := r.controllerDo(t, http.MethodDelete, "/api/s/default/rest/portforward/"+id, nil)
		del.Body.Close()
	}
}

// livePortForward reads one port forward back off the Controller as the raw JSON
// the Controller stores — how a test checks what an apply actually did, down to
// the fields unifig does not model and must not have touched.
func (r *rig) livePortForward(t *testing.T, name string) map[string]any {
	t.Helper()
	found := r.portForwardsNamed(t, name)
	if len(found) != 1 {
		t.Fatalf("the Controller has %d port forwards named %q, want exactly 1", len(found), name)
	}
	return found[0]
}

func (r *rig) portForwardsNamed(t *testing.T, name string) []map[string]any {
	t.Helper()
	var found []map[string]any
	for _, f := range r.portForwards(t) {
		if f["name"] == name {
			found = append(found, f)
		}
	}
	return found
}

func (r *rig) portForwards(t *testing.T) []map[string]any {
	t.Helper()
	resp := r.controllerDo(t, http.MethodGet, "/api/s/default/rest/portforward", nil)
	defer resp.Body.Close()

	var list struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("listing port forwards: %v", err)
	}
	return list.Data
}

// seedReservation puts a client record on the live Controller through its own
// API, forgetting any record for the same MAC first so seeding is idempotent
// across reused controllers. Its conf is raw Controller JSON for the same reason
// seedNetwork's is: the rig describes the Controller, not unifig's model of it.
//
// It seeds a *client record*, which is the thing a reservation is half of. A
// conf with no `use_fixedip` is how a test sets up a client the Controller knows
// but holds no reservation for — the state unifig has to read as "there is no
// reservation here" rather than as one with a blank address.
func (r *rig) seedReservation(t *testing.T, conf map[string]any) {
	t.Helper()
	mac, _ := conf["mac"].(string)
	r.forgetClients(t, mac)

	body, err := json.Marshal(conf)
	if err != nil {
		t.Fatalf("marshaling client record: %v", err)
	}
	resp := r.controllerDo(t, http.MethodPost, "/api/s/default/rest/user", bytes.NewReader(body))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("seeding client record %q: status %d: %s", mac, resp.StatusCode, b)
	}
}

// forgetClients removes client records entirely — the Controller's own
// forget-sta, which is the only way to make a site look as though it had never
// seen a device.
//
// It is the rig's cleanup and deliberately not what unifig does. Giving up a
// reservation leaves the record behind (ADR-0015), so a test that tidied up by
// asking unifig to prune would leave the next test a client record it did not
// seed. A MAC the Controller has no record of is not an error here.
//
// The MACs are lower-cased on the way out, and that is not tidiness. The
// Controller stores every MAC in lower case, and this command matches on the
// exact string it is given: handed `AA:BB:…` it forgets nothing and answers
// `rc: ok` anyway. A test naming its client in upper case would then leave that
// client behind for whatever ran next, which is the sort of contamination the
// rest of this rig's seeding exists to rule out.
func (r *rig) forgetClients(t *testing.T, macs ...string) {
	t.Helper()
	stored := make([]string, len(macs))
	for i, mac := range macs {
		stored[i] = strings.ToLower(mac)
	}
	body, err := json.Marshal(map[string]any{"cmd": "forget-sta", "macs": stored})
	if err != nil {
		t.Fatalf("marshaling forget-sta: %v", err)
	}
	resp := r.controllerDo(t, http.MethodPost, "/api/s/default/cmd/stamgr", bytes.NewReader(body))
	resp.Body.Close()
}

// liveClient reads one client record back off the Controller as the raw JSON the
// Controller stores — how a test checks what an apply actually did, including
// the fields of the record unifig does not model and must not have touched.
func (r *rig) liveClient(t *testing.T, mac string) map[string]any {
	t.Helper()
	found := r.clientsWithMAC(t, mac)
	if len(found) != 1 {
		t.Fatalf("the Controller has %d client records for %q, want exactly 1", len(found), mac)
	}
	return found[0]
}

// clientsWithMAC finds the records for one MAC, comparing case-insensitively
// because the Controller stores every MAC in lower case whatever case it was
// given — the same normalisation unifig matches on.
func (r *rig) clientsWithMAC(t *testing.T, mac string) []map[string]any {
	t.Helper()
	var found []map[string]any
	for _, client := range r.clients(t) {
		if stored, ok := client["mac"].(string); ok && strings.EqualFold(stored, mac) {
			found = append(found, client)
		}
	}
	return found
}

// clients reads every client record the Controller holds, reservation or not.
// Most of them are not reservations — the collection is the Controller's memory
// of every device it has seen — which is exactly what a test proving unifig
// leaves the others alone has to be able to look at.
func (r *rig) clients(t *testing.T) []map[string]any {
	t.Helper()
	resp := r.controllerDo(t, http.MethodGet, "/api/s/default/rest/user", nil)
	defer resp.Body.Close()

	var list struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("listing client records: %v", err)
	}
	return list.Data
}

// networkID is the Controller ID of a live network — what a seeded WLAN needs
// in order to name the network its clients join. Tests never see this ID
// themselves; it goes straight back into a seed.
func (r *rig) networkID(t *testing.T, name string) string {
	t.Helper()
	id, _ := r.liveNetwork(t, name)["_id"].(string)
	if id == "" {
		t.Fatalf("the network %q on the Controller has no ID", name)
	}
	return id
}

// managedNetworkNamesByID is the reverse: what a test needs in order to say, in
// config terms, which network a live WLAN is already on. Only the LAN purposes
// are in it, spelled out here rather than imported from the engine for the same
// reason managedNetworkNames spells them out — so the rig describes the
// Controller rather than agreeing with the code under test by construction.
func (r *rig) managedNetworkNamesByID(t *testing.T) map[string]string {
	t.Helper()
	names := map[string]string{}
	for _, n := range r.networks(t) {
		switch n["purpose"] {
		case "corporate", "guest", "vlan-only":
		default:
			continue
		}
		id, _ := n["_id"].(string)
		name, _ := n["name"].(string)
		names[id] = name
	}
	return names
}

// defaultAPGroupID is the "All APs" group the Controller puts every new WLAN
// on. It lives under the v2 API rather than /api/s/<site>/rest, which is why it
// does not go through controllerDo's response shape.
func (r *rig) defaultAPGroupID(t *testing.T) string {
	t.Helper()
	resp := r.controllerDo(t, http.MethodGet, "/v2/api/site/default/apgroups", nil)
	defer resp.Body.Close()

	var groups []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&groups); err != nil {
		t.Fatalf("listing AP groups: %v", err)
	}
	for _, group := range groups {
		if group["attr_hidden_id"] == "default" {
			id, _ := group["_id"].(string)
			return id
		}
	}
	t.Fatalf("the Controller has no default AP group: %v", groups)
	return ""
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
