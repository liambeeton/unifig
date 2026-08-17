package main

import (
	"encoding/json"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// What a recording looks like coming off a real router, before anything has
// been taken out of it. Every value here is one this repository must never
// hold: an ISP account name, the address that account was handed, the
// household's own subnets and SSIDs, and the identifiers that name one
// console. The tests below are the statement that the scrub takes each of
// them out — and, just as much, that it leaves everything else alone.
const rawNetworkconf = `{
  "meta": { "rc": "ok" },
  "data": [
    {
      "_id": "65f1c0a1d4e2b30af1c00a01",
      "site_id": "65f1c0a1d4e2b30af1c00000",
      "name": "Default",
      "purpose": "corporate",
      "networkgroup": "LAN",
      "enabled": true,
      "ip_subnet": "192.168.4.1/24",
      "domain_name": "example.home",
      "dhcpd_start": "192.168.4.6",
      "dhcpd_stop": "192.168.4.254"
    },
    {
      "_id": "65f1c0a1d4e2b30af1c00a02",
      "site_id": "65f1c0a1d4e2b30af1c00000",
      "name": "Guest",
      "purpose": "guest",
      "enabled": true,
      "vlan": 30,
      "vlan_enabled": true,
      "ip_subnet": "10.20.30.1/24"
    },
    {
      "_id": "65f1c0a1d4e2b30af1c00a03",
      "site_id": "65f1c0a1d4e2b30af1c00000",
      "name": "Fibre Co Gigabit",
      "purpose": "wan",
      "enabled": true,
      "attr_no_delete": true,
      "attr_hidden_id": "WAN",
      "external_id": "7c9e6679-7425-40de-944b-e07fc1f90ae7",
      "wan_networkgroup": "WAN",
      "wan_type": "pppoe",
      "wan_username": "user@fibreco.example",
      "x_wan_password": "hunter2-off-the-router",
      "wan_pppoe_username_enabled": true,
      "wan_pppoe_password_enabled": true,
      "wan_ip": "100.64.11.23",
      "wan_netmask": "255.255.255.0",
      "wan_gateway": "100.64.11.1",
      "wan_dns_preference": "manual",
      "wan_dns1": "1.1.1.1",
      "wan_dns2": "9.9.9.9",
      "wan_vlan_enabled": true,
      "wan_vlan": 911,
      "wan_failover_priority": 1,
      "wan_load_balance_type": "failover-only",
      "mac_override": "78:45:58:1a:2b:3c",
      "wan_dhcp_options": [],
      "wan_type_v6": "dhcpv6",
      "wan_ipv6": "2c0f:fe38:1234:5678::1",
      "wan_gateway_v6": "2c0f:fe38:1234:5678::2",
      "ipv6_wan_delegation_type": "prefix-delegation",
      "report_wan_event": true,
      "setting_preference": "manual"
    },
    {
      "_id": "65f1c0a1d4e2b30af1c00a04",
      "site_id": "65f1c0a1d4e2b30af1c00000",
      "name": "LTE Failover",
      "purpose": "wan",
      "enabled": true,
      "wan_networkgroup": "WAN_LTE_FAILOVER",
      "wan_type": "dhcp",
      "wan_username": "",
      "x_wan_password": "",
      "wan_pppoe_username_enabled": false,
      "wan_pppoe_password_enabled": false,
      "wan_ip": "0.0.0.0",
      "wan_failover_priority": 2
    }
  ]
}`

const rawSysinfo = `{
  "meta": { "rc": "ok" },
  "data": [
    {
      "version": "10.0.162",
      "build": "atag_10.0.162_32076",
      "name": "Example Home Console",
      "hostname": "example-udr",
      "timezone": "Africa/Johannesburg",
      "ubnt_device_type": "UDR",
      "udm_version": "4.3.9",
      "anonymous_controller_id": "3f2504e0-4f89-11d3-9a0c-0305e82c3301",
      "sso_app_id": "d1b0f2a4-9b3c-4a51-8f4d-2c0e6a5f1b77",
      "ip_addrs": ["192.168.4.1", "100.64.11.23"],
      "uptime": 604800,
      "https_port": 443,
      "update_available": false,
      "data_retention_days": 90
    }
  ]
}`

const rawWlanconf = `{
  "meta": { "rc": "ok" },
  "data": [
    {
      "_id": "65f1c0a1d4e2b30af1c00b01",
      "site_id": "65f1c0a1d4e2b30af1c00000",
      "name": "Example Family",
      "enabled": true,
      "security": "wpapsk",
      "x_passphrase": "the-family-wifi-password",
      "networkconf_id": "65f1c0a1d4e2b30af1c00a01"
    },
    {
      "_id": "65f1c0a1d4e2b30af1c00b02",
      "site_id": "65f1c0a1d4e2b30af1c00000",
      "name": "Example Guests",
      "enabled": true,
      "security": "wpapsk",
      "x_passphrase": "the-guest-wifi-password",
      "networkconf_id": "65f1c0a1d4e2b30af1c00a02"
    }
  ]
}`

// The settings response is the whole console's configuration, and unifig reads
// one key out of it. Everything here that is not that key is a reason the scrub
// keeps only the one it reads.
const rawSetting = `{
  "meta": { "rc": "ok" },
  "data": [
    {
      "_id": "65f1c0a1d4e2b30af1c00c01",
      "site_id": "65f1c0a1d4e2b30af1c00000",
      "key": "super_mail",
      "provider": "smtp",
      "smtp_username": "postmaster@example.invalid",
      "x_smtp_password": "the-mail-password"
    },
    {
      "_id": "65f1c0a1d4e2b30af1c00c02",
      "site_id": "65f1c0a1d4e2b30af1c00000",
      "key": "doh",
      "state": "custom",
      "server_names": [],
      "custom_servers": [
        {
          "enabled": true,
          "server_name": "Example Resolver",
          "sdns_stamp": "sdns://AgcAAAAAAAAABzEuMi4zLjSgnou0mUYSbFcvIGxNAgNzc2VjcmV0LWVuZHBvaW50"
        }
      ]
    },
    {
      "_id": "65f1c0a1d4e2b30af1c00c03",
      "site_id": "65f1c0a1d4e2b30af1c00000",
      "key": "mgmt",
      "x_ssh_password": "the-device-password"
    }
  ]
}`

// The committed recording the scrub takes its LAN from — the one already in
// e2e/testdata/udr, which came from the dockerized Controller and so describes
// nobody's house.
const committedNetworkconf = `{
  "meta": { "rc": "ok" },
  "data": [
    {
      "_id": "6613a1f0c4b2d90a5e1f0001",
      "site_id": "6613a1f0c4b2d90a5e1f0000",
      "name": "Default",
      "purpose": "corporate",
      "networkgroup": "LAN",
      "enabled": true,
      "attr_no_delete": true,
      "attr_hidden_id": "LAN",
      "ip_subnet": "192.168.1.1/24",
      "domain_name": "localdomain"
    }
  ]
}`

const committedSite = "6613a1f0c4b2d90a5e1f0000"

func TestScrubKeepsTheUplinksAndEverythingUnifigDoesNotModel(t *testing.T) {
	out := scrubbed(t)

	slots := slotNames(out.networkconf)
	if want := []string{"WAN", "WAN_LTE_FAILOVER"}; !slices.Equal(slots, want) {
		t.Fatalf("the scrub kept slots %v, want %v — the uplinks are the one thing only the router can supply", slots, want)
	}

	// The fields unifig does not model are the reason a recording is worth
	// having: they are what an apply has to write back untouched.
	wan := slot(t, out.networkconf, "WAN")
	for field, want := range map[string]any{
		"wan_type":              "pppoe",
		"wan_netmask":           "255.255.255.0",
		"wan_dns_preference":    "manual",
		"wan_vlan_enabled":      true,
		"wan_vlan":              float64(911),
		"wan_failover_priority": float64(1),
		"wan_load_balance_type": "failover-only",
		"attr_hidden_id":        "WAN",
		"report_wan_event":      true,
	} {
		if wan[field] != want {
			t.Errorf("%s = %#v, want %#v — the scrub changed something that gives nothing away", field, wan[field], want)
		}
	}
}

func TestScrubTakesTheLANFromTheCommittedRecording(t *testing.T) {
	out := scrubbed(t)

	names := networkNames(out.networkconf)
	if want := []string{"Default"}; !slices.Equal(names, want) {
		t.Fatalf("the scrub kept networks %v, want %v — the LAN comes from the committed recording, not the router", names, want)
	}
	lan := network(t, out.networkconf, "Default")
	if lan["ip_subnet"] != "192.168.1.1/24" || lan["domain_name"] != "localdomain" {
		t.Errorf("the LAN is not the committed one: %#v", lan)
	}

	// The router's own topology is the thing that must not survive: not the
	// subnets, not the VLAN layout, not the guest network's existence.
	for _, gone := range []string{"Guest", "10.20.30.1/24", "192.168.4.1/24", "example.home", "192.168.4.6"} {
		if strings.Contains(string(written(t, out.networkconf)), gone) {
			t.Errorf("the scrubbed networkconf still holds %q", gone)
		}
	}
}

func TestScrubEmptiesTheWLANs(t *testing.T) {
	out := scrubbed(t)

	if len(out.wlanconf.Data) != 0 {
		t.Fatalf("the scrub kept %d WLANs, want none — an SSID is the household's name for itself", len(out.wlanconf.Data))
	}
	// Emptied, not removed: the endpoint still answers, and it answers the way
	// a Controller does.
	body := string(written(t, out.wlanconf))
	if !strings.Contains(body, `"rc": "ok"`) || !strings.Contains(body, `"data": []`) {
		t.Errorf("the emptied wlanconf is not a Controller response: %s", body)
	}
}

func TestScrubReplacesTheCredentials(t *testing.T) {
	out := scrubbed(t)
	wan := slot(t, out.networkconf, "WAN")

	if password, _ := wan["x_wan_password"].(string); password == "" || password == "hunter2-off-the-router" {
		t.Errorf("x_wan_password = %q, want a placeholder — and a non-empty one, or the recording stops describing a signed-in uplink", password)
	}
	if username, _ := wan["wan_username"].(string); username == "" || strings.Contains(username, "fibreco") {
		t.Errorf("wan_username = %q, want a placeholder that is not the operator's ISP account", username)
	}

	// A field with nothing in it has nothing to hide, and filling it in would
	// turn an uplink with no credentials into one that has them.
	failover := slot(t, out.networkconf, "WAN_LTE_FAILOVER")
	for _, field := range []string{"wan_username", "x_wan_password"} {
		if failover[field] != "" {
			t.Errorf("%s = %#v on a slot that had none, want it left empty", field, failover[field])
		}
	}
}

func TestScrubReplacesTheISPAddressing(t *testing.T) {
	out := scrubbed(t)
	wan := slot(t, out.networkconf, "WAN")

	for _, field := range []string{"wan_ip", "wan_gateway", "wan_dns1", "wan_dns2", "wan_ipv6", "wan_gateway_v6"} {
		value, _ := wan[field].(string)
		assertPlaceholderAddress(t, field, value)
	}
	// Two resolvers are two resolvers afterwards: collapsing them would change
	// what the recording says the router was configured with.
	if wan["wan_dns1"] == wan["wan_dns2"] {
		t.Errorf("two different DNS servers became one placeholder: %#v", wan["wan_dns1"])
	}
	// A netmask says how big a subnet is, not where the site is, and 0.0.0.0
	// says "none" — replacing either would lose a fact and gain no privacy.
	if wan["wan_netmask"] != "255.255.255.0" {
		t.Errorf("wan_netmask = %#v, want it left alone", wan["wan_netmask"])
	}
	if failover := slot(t, out.networkconf, "WAN_LTE_FAILOVER"); failover["wan_ip"] != "0.0.0.0" {
		t.Errorf("wan_ip = %#v, want the unspecified address left alone", failover["wan_ip"])
	}
	// The router's own MAC is the identifier an ISP has on file for the line.
	mac, _ := wan["mac_override"].(string)
	if mac == "" || strings.HasPrefix(mac, "78:45:58") {
		t.Errorf("mac_override = %q, want a documentation MAC", mac)
	}
}

func TestScrubReplacesTheConsoleIdentifiers(t *testing.T) {
	out := scrubbed(t)
	console := out.sysinfo.Data[0]

	for _, field := range []string{"anonymous_controller_id", "sso_app_id"} {
		value, _ := console[field].(string)
		if value == "" || strings.Contains(rawSysinfo, value) {
			t.Errorf("%s = %q, want a placeholder of the same shape", field, value)
		}
		if strings.Count(value, "-") != 4 {
			t.Errorf("%s = %q, want something still shaped like a UUID", field, value)
		}
	}
	if console["anonymous_controller_id"] == console["sso_app_id"] {
		t.Errorf("two identifiers became one placeholder: %#v", console["sso_app_id"])
	}
	// The console's name, its hostname and its timezone are the operator's
	// words and the operator's city.
	for field, unwanted := range map[string]string{
		"name":     "Example",
		"hostname": "example-udr",
		"timezone": "Africa",
	} {
		value, _ := console[field].(string)
		if value == "" || strings.Contains(value, unwanted) {
			t.Errorf("%s = %q, want a placeholder", field, value)
		}
	}
	// What the recording exists to state — which Controller answered — stays.
	for field, want := range map[string]any{
		"version": "10.0.162", "ubnt_device_type": "UDR", "udm_version": "4.3.9",
	} {
		if console[field] != want {
			t.Errorf("%s = %#v, want %#v", field, console[field], want)
		}
	}
	// Addresses hide inside arrays too.
	for _, addr := range console["ip_addrs"].([]any) {
		assertPlaceholderAddress(t, "ip_addrs", addr.(string))
	}
	// One site, and it is the committed recording's — the LAN came from there,
	// so a WAN entry claiming a different site would describe two routers.
	if got := slot(t, out.networkconf, "WAN")["site_id"]; got != committedSite {
		t.Errorf("site_id = %#v, want the committed recording's %q", got, committedSite)
	}
}

func TestScrubReplacesTheOperatorsNameForTheirConnection(t *testing.T) {
	out := scrubbed(t)

	// An operator renames a slot to their ISP. The slot is what the tests match
	// on, so the slot is what the entry is called afterwards.
	for _, want := range []string{"WAN", "WAN_LTE_FAILOVER"} {
		if got := slot(t, out.networkconf, want)["name"]; got != want {
			t.Errorf("the %s slot is named %#v, want %q", want, got, want)
		}
	}
}

// Every field the router sent is a field the tests can exercise, and a field
// that vanished is one they stopped exercising — silently, and only for
// whoever re-recorded.
func TestScrubNeverRemovesAField(t *testing.T) {
	raw := rawRecording(t)
	out := scrubbed(t)

	for _, before := range raw.networkconf.Data {
		if before["purpose"] != "wan" {
			continue
		}
		assertSameShape(t, "networkconf["+before["wan_networkgroup"].(string)+"]",
			before, slot(t, out.networkconf, before["wan_networkgroup"].(string)))
	}
	assertSameShape(t, "sysinfo", raw.sysinfo.Data[0], out.sysinfo.Data[0])
}

// The check that makes the rest of this file more than a list of the fields
// somebody thought of: whatever the scrub took out, it goes looking for
// afterwards, everywhere, and refuses to write a recording that still has it.
func TestScrubRefusesARecordingThatStillHoldsAValueItTookOut(t *testing.T) {
	raw := rawRecording(t)
	// A field this program has never heard of, carrying the operator's name for
	// their connection — the shape of every leak a field-by-field scrub misses.
	slot(t, raw.networkconf, "WAN")["x_isp_note"] = "Fibre Co Gigabit, installed 2024"

	_, err := scrub(raw, parse(t, committedNetworkconf))
	if err == nil {
		t.Fatalf("the scrub wrote a recording that still holds the operator's name for their connection")
	}
	if !strings.Contains(err.Error(), "x_isp_note") {
		t.Errorf("the error should name the field that still holds it, got: %v", err)
	}
}

// The check above hunts for what the router sent, and only there. A household
// behind their ISP's own router is addressed out of the same private range the
// committed LAN uses, and refusing to record that would be this program failing
// on a coincidence.
func TestScrubRecordsARouterAddressedOutOfTheCommittedLANsRange(t *testing.T) {
	raw := rawRecording(t)
	slot(t, raw.networkconf, "WAN")["wan_gateway"] = "192.168.1.1"

	out, err := scrub(raw, parse(t, committedNetworkconf))
	if err != nil {
		t.Fatalf("scrubbing a double-NATted uplink: %v", err)
	}
	assertPlaceholderAddress(t, "wan_gateway", slot(t, out.networkconf, "WAN")["wan_gateway"].(string))
	if lan := network(t, out.networkconf, "Default"); lan["ip_subnet"] != "192.168.1.1/24" {
		t.Errorf("the committed LAN was scrubbed too: %#v", lan["ip_subnet"])
	}
}

// Re-recording an unchanged router is an empty diff. That is what makes the
// diff worth reading: everything in it is a real difference.
func TestScrubIsStableAndIdempotent(t *testing.T) {
	first := scrubbed(t)
	second := scrubbed(t)
	for _, files := range []struct {
		name string
		a, b document
	}{
		{"sysinfo", first.sysinfo, second.sysinfo},
		{"networkconf", first.networkconf, second.networkconf},
	} {
		if !slices.Equal(written(t, files.a), written(t, files.b)) {
			t.Errorf("two scrubs of one recording disagree about %s:\n%s\n%s",
				files.name, written(t, files.a), written(t, files.b))
		}
	}

	// And scrubbing what the scrub already wrote changes nothing: a placeholder
	// is not something to find a placeholder for.
	again, err := scrub(first, parse(t, committedNetworkconf))
	if err != nil {
		t.Fatalf("scrubbing a scrubbed recording: %v", err)
	}
	if !slices.Equal(written(t, first.networkconf), written(t, again.networkconf)) {
		t.Errorf("scrubbing a scrubbed recording changed it:\n%s\n%s",
			written(t, first.networkconf), written(t, again.networkconf))
	}
}

// `get/setting` answers with the whole console: mail credentials, remote
// access, RADIUS secrets, guest portals. unifig reads one key out of it, and
// the recording holds that one key and nothing else.
func TestScrubKeepsNothingFromTheSettingsButEncryptedDNS(t *testing.T) {
	out := scrubbed(t)

	if len(out.setting.Data) != 1 {
		t.Fatalf("the scrubbed settings hold %d entries, want only the Encrypted DNS one: %+v",
			len(out.setting.Data), out.setting.Data)
	}
	if key := out.setting.Data[0]["key"]; key != "doh" {
		t.Errorf("the scrubbed settings kept %q, want %q", key, "doh")
	}
	for _, secret := range []string{"the-mail-password", "postmaster@example.invalid", "the-device-password"} {
		if strings.Contains(string(written(t, out.setting)), secret) {
			t.Errorf("the scrubbed settings still carry %q from a setting unifig never reads", secret)
		}
	}
}

// The stamp is the third secret unifig models, and a stamp for a private
// endpoint carries the account it belongs to. The name beside it is the
// operator's own word for their resolver, replaced for the same reason their
// name for their connection is.
func TestScrubReplacesTheDNSStampsAndTheirNames(t *testing.T) {
	servers := dohServers(t, scrubbed(t).setting)

	if len(servers) != 1 {
		t.Fatalf("the scrub kept %d custom DNS servers, want the one the router sent", len(servers))
	}
	stamp, _ := servers[0]["sdns_stamp"].(string)
	if !strings.HasPrefix(stamp, "sdns://") {
		t.Errorf("sdns_stamp = %q, want something still shaped like a stamp", stamp)
	}
	if strings.Contains(stamp, "secret-endpoint") {
		t.Errorf("sdns_stamp = %q, and it still carries the endpoint off the router", stamp)
	}
	if name, _ := servers[0]["server_name"].(string); name == "" || strings.Contains(name, "Example") {
		t.Errorf("server_name = %q, want a placeholder that names nobody", name)
	}
	// Everything the recording exists to state about the shape survives.
	if servers[0]["enabled"] != true {
		t.Errorf("the resolver's enabled flag did not survive the scrub: %+v", servers[0])
	}
	if state := doh(t, scrubbed(t).setting)["state"]; state != "custom" {
		t.Errorf("the setting's state = %v, want the one the router sent", state)
	}
}

// Two resolvers stay two resolvers, and one used twice stays one: a fixed
// placeholder would say the site has one resolver written down twice.
func TestScrubGivesTwoDNSStampsTwoPlaceholders(t *testing.T) {
	raw := rawRecording(t)
	servers, _ := doh(t, raw.setting)["custom_servers"].([]any)
	doh(t, raw.setting)["custom_servers"] = append(servers, map[string]any{
		"enabled":     false,
		"server_name": "Example Backup",
		"sdns_stamp":  "sdns://AgcAAAAAAAAABzUuNi43LjigbotherEndpointHere",
	})

	out, err := scrub(raw, parse(t, committedNetworkconf))
	if err != nil {
		t.Fatalf("scrubbing the recording: %v", err)
	}

	scrubbedServers := dohServers(t, out.setting)
	if len(scrubbedServers) != 2 {
		t.Fatalf("the scrub kept %d resolvers, want 2", len(scrubbedServers))
	}
	if scrubbedServers[0]["sdns_stamp"] == scrubbedServers[1]["sdns_stamp"] {
		t.Errorf("both resolvers were given the stamp %v, so the recording says the site has one",
			scrubbedServers[0]["sdns_stamp"])
	}
	if scrubbedServers[0]["server_name"] == scrubbedServers[1]["server_name"] {
		t.Errorf("both resolvers were given the name %v", scrubbedServers[0]["server_name"])
	}
}

// A Network version with no Encrypted DNS setting cannot supply the shape the
// DNS suite replays, and a recording written without it would leave that suite
// passing against the old fixture while the operator believed it was theirs.
func TestScrubRefusesARecordingWithNoEncryptedDNSSetting(t *testing.T) {
	raw := rawRecording(t)
	raw.setting.Data = slices.DeleteFunc(raw.setting.Data, func(entry map[string]any) bool {
		return entry["key"] == "doh"
	})

	_, err := scrub(raw, parse(t, committedNetworkconf))
	if err == nil || !strings.Contains(err.Error(), "Encrypted DNS") {
		t.Fatalf("scrubbing a recording with no doh setting returned %v, want an error saying so", err)
	}
}

func TestScrubRefusesARecordingWithNoUplinkInIt(t *testing.T) {
	raw := rawRecording(t)
	raw.networkconf.Data = slices.DeleteFunc(raw.networkconf.Data, func(entry map[string]any) bool {
		return entry["purpose"] == "wan"
	})

	_, err := scrub(raw, parse(t, committedNetworkconf))
	if err == nil || !strings.Contains(err.Error(), "uplink") {
		t.Fatalf("scrubbing a recording with no WAN entry returned %v, want an error saying so", err)
	}
}

func TestScrubRefusesWhenTheCommittedRecordingHasNoLAN(t *testing.T) {
	committed := parse(t, committedNetworkconf)
	committed.Data = nil

	_, err := scrub(rawRecording(t), committed)
	if err == nil || !strings.Contains(err.Error(), "LAN") {
		t.Fatalf("scrubbing against a recording with no LAN returned %v, want an error saying so", err)
	}
}

// The committed recording is this program's other input, so it is worth
// knowing that the one in the repository is still an input it can use.
func TestTheCommittedRecordingStillSuppliesALAN(t *testing.T) {
	// From this package's directory, which is where a Go test runs.
	body, err := os.ReadFile(filepath.Join("..", "..", recordingDir, "networkconf.json"))
	if err != nil {
		t.Fatalf("reading the committed recording: %v", err)
	}

	out, err := scrub(rawRecording(t), parse(t, string(body)))
	if err != nil {
		t.Fatalf("scrubbing against the committed recording: %v", err)
	}
	if len(networkNames(out.networkconf)) == 0 {
		t.Errorf("the scrubbed recording holds no network, so the WAN tests have nothing that is not the uplink")
	}
}

func TestTheWrittenRecordingIsJSONAControllerCouldHaveSent(t *testing.T) {
	out := scrubbed(t)

	body := written(t, out.networkconf)
	if body[len(body)-1] != '\n' {
		t.Errorf("the file does not end in a newline")
	}
	var round document
	if err := json.Unmarshal(body, &round); err != nil {
		t.Fatalf("what the scrub wrote is not JSON: %v", err)
	}
	if len(round.Data) != len(out.networkconf.Data) {
		t.Errorf("the written file holds %d entries, want %d", len(round.Data), len(out.networkconf.Data))
	}
}

// scrubbed is the raw recording above, put through the scrub against the
// committed one — what almost every test here is about.
func scrubbed(t *testing.T) recording {
	t.Helper()
	out, err := scrub(rawRecording(t), parse(t, committedNetworkconf))
	if err != nil {
		t.Fatalf("scrubbing the recording: %v", err)
	}
	return out
}

func rawRecording(t *testing.T) recording {
	t.Helper()
	return recording{
		sysinfo:     parse(t, rawSysinfo),
		networkconf: parse(t, rawNetworkconf),
		wlanconf:    parse(t, rawWlanconf),
		setting:     parse(t, rawSetting),
		zones:       parseList(t, rawFirewallZones),
		policies:    parseList(t, rawFirewallPolicies),
	}
}

func parseList(t *testing.T, body string) list {
	t.Helper()
	entries, err := readList([]byte(body))
	if err != nil {
		t.Fatalf("reading a recorded list: %v", err)
	}
	return entries
}

// The firewall as a real router hands it over: the Controller's own built-in
// zones and the default policy between two of them, alongside a zone and a
// policy the operator made and named after their household.
const rawFirewallZones = `[
  {
    "_id": "65f1c0a1d4e2b30af1c00b01",
    "site_id": "65f1c0a1d4e2b30af1c00000",
    "name": "Internal",
    "attr_no_delete": true,
    "network_ids": ["65f1c0a1d4e2b30af1c00a01", "65f1c0a1d4e2b30af1c00a02"]
  },
  {
    "_id": "65f1c0a1d4e2b30af1c00b02",
    "site_id": "65f1c0a1d4e2b30af1c00000",
    "name": "External",
    "attr_no_delete": true,
    "network_ids": ["65f1c0a1d4e2b30af1c00a03"]
  },
  {
    "_id": "65f1c0a1d4e2b30af1c00b03",
    "site_id": "65f1c0a1d4e2b30af1c00000",
    "name": "Ollie's room",
    "network_ids": ["65f1c0a1d4e2b30af1c00a02"]
  }
]`

const rawFirewallPolicies = `[
  {
    "_id": "65f1c0a1d4e2b30af1c00c01",
    "site_id": "65f1c0a1d4e2b30af1c00000",
    "name": "Internal to External",
    "action": "ALLOW",
    "predefined": true,
    "enabled": true,
    "protocol": "all",
    "source": { "zone_id": "65f1c0a1d4e2b30af1c00b01", "matching_target": "ANY" },
    "destination": { "zone_id": "65f1c0a1d4e2b30af1c00b02", "matching_target": "ANY" }
  },
  {
    "_id": "65f1c0a1d4e2b30af1c00c02",
    "site_id": "65f1c0a1d4e2b30af1c00000",
    "name": "Ollie off the internet at bedtime",
    "action": "BLOCK",
    "enabled": true,
    "protocol": "all",
    "source": { "zone_id": "65f1c0a1d4e2b30af1c00b03", "matching_target": "ANY" },
    "destination": { "zone_id": "65f1c0a1d4e2b30af1c00b02", "matching_target": "ANY" }
  }
]`

// The firewall is recorded on the same terms as everything else: what only a
// router can supply is the set of zones and policies the Controller ships for
// itself, which is exactly what unifig's built-in exemption reads. A zone the
// operator made is named after their household and is dropped rather than
// scrubbed — the tests that want a custom zone seed their own.
func TestScrubKeepsTheControllersOwnZonesAndDropsTheOperatorsOwn(t *testing.T) {
	out := scrubbed(t)

	var kept []string
	for _, zone := range out.zones {
		name, _ := zone["name"].(string)
		kept = append(kept, name)
	}
	slices.Sort(kept)
	if !slices.Equal(kept, []string{"External", "Internal"}) {
		t.Errorf("the recording holds zones %v, want just the Controller's own", kept)
	}

	for _, zone := range out.zones {
		if zone["attr_no_delete"] != true {
			t.Errorf("a kept zone is not marked undeletable, so unifig's exemption would not spare it: %v", zone)
		}
	}
}

func TestScrubKeepsTheControllersOwnPoliciesAndDropsTheOperatorsOwn(t *testing.T) {
	out := scrubbed(t)

	if len(out.policies) != 1 {
		t.Fatalf("the recording holds %d policies, want just the Controller's own: %v", len(out.policies), out.policies)
	}
	if out.policies[0]["name"] != "Internal to External" {
		t.Errorf("the kept policy is %v, want the predefined one", out.policies[0]["name"])
	}
	if out.policies[0]["predefined"] != true {
		t.Errorf("the kept policy is not marked predefined, so unifig's exemption would not spare it: %v", out.policies[0])
	}
}

// A zone's membership survives as a reference that resolves. The router's own
// LANs are dropped in favour of the committed one, so a member that named one of
// them is pointed at the LAN this recording does hold — otherwise every test
// about what a zone holds would be testing a dangling id.
func TestScrubPointsAZonesMembershipAtTheNetworksTheRecordingKeeps(t *testing.T) {
	out := scrubbed(t)

	held := map[string]bool{}
	for _, entry := range out.networkconf.Data {
		held[idOf(entry)] = true
	}

	for _, zone := range out.zones {
		ids, _ := zone["network_ids"].([]any)
		if len(ids) == 0 {
			t.Errorf("the %v zone came back holding nothing, so it says less than the router did", zone["name"])
		}
		for _, raw := range ids {
			id, _ := raw.(string)
			if !held[id] {
				t.Errorf("the %v zone holds %q, which is not a network in this recording", zone["name"], id)
			}
		}
	}
}

// The two built-ins hold different things, and the difference is the whole point
// of the fixture: Internal holds a LAN, External holds the uplink. A scrub that
// folded both onto the same network would leave the suite unable to state what
// unifig does with a zone member it cannot name.
func TestScrubKeepsTheUplinkInTheExternalZoneAndTheLANInTheInternalOne(t *testing.T) {
	out := scrubbed(t)

	uplinks := map[string]bool{}
	for _, entry := range out.networkconf.Data {
		if entry["purpose"] == "wan" {
			uplinks[idOf(entry)] = true
		}
	}

	for _, zone := range out.zones {
		ids, _ := zone["network_ids"].([]any)
		if len(ids) != 1 {
			t.Fatalf("the %v zone holds %d networks, want 1", zone["name"], len(ids))
		}
		id, _ := ids[0].(string)
		switch zone["name"] {
		case "External":
			if !uplinks[id] {
				t.Errorf("the External zone holds %q, which is not this recording's uplink", id)
			}
		case "Internal":
			if uplinks[id] {
				t.Errorf("the Internal zone holds the uplink, which is not what the router said")
			}
		}
	}
}

// A policy names its zones by id, and those ids have to be the ones the zones
// were recorded under. One substitution table does that for free — this states
// it, because a policy pointing at a zone that is not in the recording would
// describe a firewall nobody has.
func TestScrubKeepsAPolicyPointingAtTheZonesItGoverns(t *testing.T) {
	out := scrubbed(t)

	zones := map[string]string{}
	for _, zone := range out.zones {
		name, _ := zone["name"].(string)
		zones[idOf(zone)] = name
	}

	policy := out.policies[0]
	if got := zones[zoneEnd(policy, "source")]; got != "Internal" {
		t.Errorf("the policy's source zone is %q, want Internal", got)
	}
	if got := zones[zoneEnd(policy, "destination")]; got != "External" {
		t.Errorf("the policy's destination zone is %q, want External", got)
	}
}

// The household's own words are what this program exists to keep out, and a
// custom zone is full of them.
func TestScrubWritesNothingTheOperatorNamedTheirFirewallAfter(t *testing.T) {
	out := scrubbed(t)

	written, err := json.Marshal(map[string]any{"zones": out.zones, "policies": out.policies})
	if err != nil {
		t.Fatalf("encoding the scrubbed firewall: %v", err)
	}
	for _, theirs := range []string{"Ollie", "bedtime"} {
		if strings.Contains(string(written), theirs) {
			t.Errorf("the recording still holds %q, which is the operator's own word for their firewall:\n%s",
				theirs, written)
		}
	}
}

// doh is the Encrypted DNS setting in a scrubbed settings response — the only
// entry there is meant to be left in one.
func doh(t *testing.T, doc document) map[string]any {
	t.Helper()
	entry, held := dohIn(doc)
	if !held {
		t.Fatalf("the recording holds no Encrypted DNS setting")
	}
	return entry
}

// dohServers is the custom resolvers on a scrubbed Encrypted DNS setting.
func dohServers(t *testing.T, doc document) []map[string]any {
	t.Helper()
	raw, _ := doh(t, doc)["custom_servers"].([]any)

	servers := make([]map[string]any, 0, len(raw))
	for _, server := range raw {
		entry, ok := server.(map[string]any)
		if !ok {
			t.Fatalf("a custom DNS server came out of the scrub as %T", server)
		}
		servers = append(servers, entry)
	}
	return servers
}

func parse(t *testing.T, body string) document {
	t.Helper()
	doc, err := read([]byte(body))
	if err != nil {
		t.Fatalf("reading a recorded response: %v", err)
	}
	return doc
}

func written(t *testing.T, doc document) []byte {
	t.Helper()
	body, err := doc.bytes()
	if err != nil {
		t.Fatalf("writing a recorded response: %v", err)
	}
	return body
}

func slot(t *testing.T, doc document, name string) map[string]any {
	t.Helper()
	for _, entry := range doc.Data {
		if entry["purpose"] == "wan" && entry["wan_networkgroup"] == name {
			return entry
		}
	}
	t.Fatalf("the recording holds no %s slot", name)
	return nil
}

func network(t *testing.T, doc document, name string) map[string]any {
	t.Helper()
	for _, entry := range doc.Data {
		if entry["purpose"] != "wan" && entry["name"] == name {
			return entry
		}
	}
	t.Fatalf("the recording holds no network named %q", name)
	return nil
}

func slotNames(doc document) []string {
	var names []string
	for _, entry := range doc.Data {
		if entry["purpose"] == "wan" {
			names = append(names, entry["wan_networkgroup"].(string))
		}
	}
	return names
}

func networkNames(doc document) []string {
	var names []string
	for _, entry := range doc.Data {
		if entry["purpose"] != "wan" {
			names = append(names, entry["name"].(string))
		}
	}
	return names
}

// assertPlaceholderAddress is what "a placeholder of the same type" means for
// an address: still an address, and one of the ranges the RFCs set aside for
// documentation, which belong to nobody.
func assertPlaceholderAddress(t *testing.T, field, value string) {
	t.Helper()
	addr, err := netip.ParseAddr(value)
	if err != nil {
		t.Errorf("%s = %q, which is not an address any more", field, value)
		return
	}
	if !documentationAddress(addr) {
		t.Errorf("%s = %q, want an address from a documentation range", field, value)
	}
}

// assertSameShape fails for any field that lost its place or changed type
// between the recording and the scrub of it.
func assertSameShape(t *testing.T, where string, before, after map[string]any) {
	t.Helper()
	for _, name := range slices.Sorted(maps.Keys(before)) {
		got, ok := after[name]
		if !ok {
			t.Errorf("%s.%s is gone from the scrubbed recording", where, name)
			continue
		}
		switch was := before[name].(type) {
		case map[string]any:
			nested, ok := got.(map[string]any)
			if !ok {
				t.Errorf("%s.%s is %T after the scrub, was an object", where, name, got)
				continue
			}
			assertSameShape(t, where+"."+name, was, nested)
		default:
			if wantType, gotType := typeOf(before[name]), typeOf(got); wantType != gotType {
				t.Errorf("%s.%s is a %s after the scrub, was a %s", where, name, gotType, wantType)
			}
		}
	}
}

func typeOf(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	default:
		return "object"
	}
}

// Every shape needs a rule, because the failure mode of a missing one is a
// field replaced by nothing — the one thing this program promises never to do.
func TestEveryShapeOfValueHasAPlaceholder(t *testing.T) {
	for k := shape(0); k < shapeCount; k++ {
		r, ok := rules[k]
		if !ok {
			t.Errorf("shape %d has no rule, so a field of that shape would be emptied", k)
			continue
		}
		given := 0
		for _, has := range []bool{r.fixed != "", r.counted != "", r.site} {
			if has {
				given++
			}
		}
		if given != 1 {
			t.Errorf("shape %d has %d placeholders, want exactly one: %+v", k, given, r)
		}
	}
}

// The envelope comes off the router like everything else, so it is swept and
// checked like everything else. It says `{"rc": "ok"}` today, and today is not
// a guarantee — ADR-0011 promises the check covers everywhere, not everywhere
// anyone has looked.
func TestScrubReachesIntoTheResponseEnvelope(t *testing.T) {
	raw := rawRecording(t)
	raw.sysinfo.Meta["from"] = "100.64.11.23"

	out, err := scrub(raw, parse(t, committedNetworkconf))
	if err != nil {
		t.Fatalf("scrubbing a recording with more in its envelope: %v", err)
	}
	if out.sysinfo.Meta["rc"] != "ok" {
		t.Errorf("the envelope stopped saying the request succeeded: %#v", out.sysinfo.Meta)
	}
	assertPlaceholderAddress(t, "meta.from", out.sysinfo.Meta["from"].(string))

	// And the backstop reaches it too: a value taken out of an uplink, sitting
	// in the envelope, stops the whole recording.
	raw = rawRecording(t)
	raw.networkconf.Meta["served_by"] = "Fibre Co Gigabit"
	if _, err := scrub(raw, parse(t, committedNetworkconf)); err == nil {
		t.Errorf("the envelope escaped the check that everything else gets")
	}
}

// The backstop hunts for what identifies somebody, and a three-letter value is
// not that. A console whose hostname is its device type used to refuse the
// whole recording, and the operator could do nothing about it.
func TestScrubRecordsAConsoleWhoseHostnameCollidesWithSomethingHarmless(t *testing.T) {
	raw := rawRecording(t)
	raw.sysinfo.Data[0]["hostname"] = "UDR"

	out, err := scrub(raw, parse(t, committedNetworkconf))
	if err != nil {
		t.Fatalf("scrubbing a console named after its own device type: %v", err)
	}
	if out.sysinfo.Data[0]["ubnt_device_type"] != "UDR" {
		t.Errorf("the device type went missing: %#v", out.sysinfo.Data[0]["ubnt_device_type"])
	}
	if out.sysinfo.Data[0]["hostname"] != placeholderHostname {
		t.Errorf("hostname = %#v, want it replaced anyway", out.sysinfo.Data[0]["hostname"])
	}
}

// A mask is kept because of the field it is in, not because of how it reads.
// 255.0.0.0 in wan_ip is somebody's address.
func TestScrubReplacesAMaskShapedAddressThatIsNotAMask(t *testing.T) {
	raw := rawRecording(t)
	slot(t, raw.networkconf, "WAN")["wan_ip"] = "255.0.0.0"

	out, err := scrub(raw, parse(t, committedNetworkconf))
	if err != nil {
		t.Fatalf("scrubbing: %v", err)
	}
	wan := slot(t, out.networkconf, "WAN")
	assertPlaceholderAddress(t, "wan_ip", wan["wan_ip"].(string))
	if wan["wan_netmask"] != "255.255.255.0" {
		t.Errorf("wan_netmask = %#v, want the mask left alone", wan["wan_netmask"])
	}
}

// An identifier does not need a field name this program recognises in order to
// be one. A real UDR's WAN entry carries `single_network_lan`, holding the id
// of a network on that console — a field nobody here had heard of, holding an
// identifier that the name table therefore let straight through into a public
// repository.
//
// So the shape is caught as well as the name, the way an address is. Anything
// shaped like one of the Controller's own object ids is one.
func TestScrubReplacesAnIdentifierInAFieldItHasNeverHeardOf(t *testing.T) {
	raw := rawRecording(t)
	slot(t, raw.networkconf, "WAN")["single_network_lan"] = "7a1c3e5209bd4f6081c2d3e4"

	out, err := scrub(raw, parse(t, committedNetworkconf))
	if err != nil {
		t.Fatalf("scrubbing: %v", err)
	}

	got, _ := slot(t, out.networkconf, "WAN")["single_network_lan"].(string)
	if got == "7a1c3e5209bd4f6081c2d3e4" {
		t.Errorf("single_network_lan = %q — a real console's object id reached the recording", got)
	}
	if !strings.HasPrefix(got, documentationIDs) {
		t.Errorf("single_network_lan = %q, want an object id of this program's own making", got)
	}
	// Still an id, so the entry still describes the same shape of thing.
	if len(got) != len("7a1c3e5209bd4f6081c2d3e4") {
		t.Errorf("single_network_lan = %q, want something still shaped like an object id", got)
	}
}

// The shape is narrow on purpose: 24 hex characters and nothing else. A build
// string or a version is not an identifier, and replacing one would lose the
// fact the recording exists to state.
func TestScrubLeavesValuesThatMerelyLookTechnicalAlone(t *testing.T) {
	raw := rawRecording(t)
	raw.sysinfo.Data[0]["udm_version"] = "UDR.mt7622.v5.1.19.3fbc1da.260613.0957"
	raw.sysinfo.Data[0]["build"] = "atag_10.5.67_35187"

	out, err := scrub(raw, parse(t, committedNetworkconf))
	if err != nil {
		t.Fatalf("scrubbing: %v", err)
	}
	for field, want := range map[string]any{
		"udm_version": "UDR.mt7622.v5.1.19.3fbc1da.260613.0957",
		"build":       "atag_10.5.67_35187",
	} {
		if out.sysinfo.Data[0][field] != want {
			t.Errorf("%s = %#v, want %#v — that is which Controller answered, not who owns it",
				field, out.sysinfo.Data[0][field], want)
		}
	}
}
