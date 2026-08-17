package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strings"
)

// The scrub decides what reaches a public repository, which is why it is a
// program with tests rather than a filter in a README. Two questions, answered
// here once:
//
// **How much of a recording has to be real?** Only the Settings — the uplinks
// and the Encrypted DNS setting. The dockerized Controller is a real Network
// application and already covers the networks and the WLANs; what a gateway-less
// container cannot produce is a WAN entry (ADR-0008), and what no container can
// be trusted to produce is the shape of a Setting on the firmware an operator
// actually runs (ADR-0012). So those come from the router, the LAN comes from
// the recording already committed, and the WLANs are emptied. Recording the rest
// would prove nothing the container does not already prove, and would cost the
// household's subnets, VLAN layout and every SSID.
//
// The settings response is the one place this program throws most of a response
// away rather than replacing it. `get/setting` answers with the whole console's
// configuration — mail servers, RADIUS, guest portals, remote access — and
// unifig reads exactly one key out of it. Keeping only that key is the same
// decision as emptying the WLANs, taken where the stakes are higher.
//
// **What is left of what it keeps?** Everything except the parts that name a
// person: the credentials, the DNS stamps, the addressing the ISP handed out,
// the identifiers that name one console, and the operator's own names for their
// connection and their resolvers. Each is replaced by a placeholder of the same
// type rather than removed — a field that vanished is a field the tests stopped
// exercising, and nobody would find out until a UDR behaved differently from
// the recording.
//
// The last line of defence is `leaks`: whatever the scrub took out, it goes
// looking for again in what it is about to write, and refuses to write a
// recording that still holds it anywhere. That is what covers the fields this
// program has never heard of.

// recordingDir is where the recording lives, relative to the repo root.
const recordingDir = "e2e/testdata/udr"

// The placeholders. Each is the same type as the value it stands in for, and
// each is fixed rather than random, so that re-recording an unchanged router
// produces an unchanged file and the diff an operator reads holds only real
// differences.
const (
	placeholderPassword   = "recorded-pppoe-password"
	placeholderPassphrase = "recorded-wlan-passphrase"
	placeholderUsername   = "recorded-isp-username"
	placeholderHostname   = "unifi"
	placeholderTimezone   = "UTC"
	placeholderConsole    = "UniFi Console"

	// A stamp and a resolver's name are counted rather than fixed for the same
	// reason an address is: a site with two resolvers has two of each, and
	// collapsing them into one would say the site has one resolver written down
	// twice. The stamp keeps its `sdns://` prefix and the alphabet a real one
	// uses, so the recording stays a config unifig's own schema accepts.
	placeholderStamp     = "sdns://recorded-dns-stamp-%d"
	placeholderResolver  = "recorded-dns-server-%d"
	documentationStamps  = "sdns://recorded-dns-stamp-"
	documentationServers = "recorded-dns-server-"

	// Counted placeholders, one per distinct value. The addresses come from
	// the ranges the RFCs set aside for documentation (RFC 5737, RFC 3849,
	// RFC 7042): they are addresses, they are nobody's, and a reader who knows
	// the ranges can tell at a glance that the recording was scrubbed.
	placeholderIPv4    = "203.0.113.%d"
	placeholderIPv6    = "2001:db8::%x"
	placeholderMAC     = "00:00:5e:00:53:%02x"
	placeholderObject  = "6613a1f0c4b2d90a5e1f1%03d"
	placeholderUUID    = "00000000-0000-4000-8000-%012d"
	documentationMACs  = "00:00:5e:00:53:"
	documentationUUIDs = "00000000-0000-4000-8000-"
	documentationIDs   = "6613a1f0c4b2d90a5e1f1"
)

// document is one recorded response: the envelope every Internal API response
// carries, and the entries inside it. The envelope is a map rather than raw
// JSON so that it is scrubbed and leak-checked like everything else — it holds
// `{"rc": "ok"}` today, and "today" is not a guarantee worth resting on.
type document struct {
	Meta map[string]any   `json:"meta"`
	Data []map[string]any `json:"data"`
}

// recording is the responses together — the whole of what the replay stand-in
// serves, and the unit this program fetches, scrubs and writes.
type recording struct {
	sysinfo     document
	networkconf document
	wlanconf    document
	setting     document
}

// file is the response one recorded endpoint answers with, addressed by the
// file that holds it. One place knows this mapping, so adding another endpoint
// is an entry in `endpoints` and a field here.
func (r *recording) file(name string) *document {
	switch name {
	case "sysinfo.json":
		return &r.sysinfo
	case "networkconf.json":
		return &r.networkconf
	case "wlanconf.json":
		return &r.wlanconf
	case "setting.json":
		return &r.setting
	default:
		panic("no recorded response is held in " + name)
	}
}

// scrub turns a recording off a live router into the one this repository can
// hold, or refuses and says why. Nothing is written before it returns.
func scrub(raw recording, committed document) (recording, error) {
	lan := networksIn(committed)
	if len(lan) == 0 {
		return recording{}, fmt.Errorf("the committed recording holds no LAN to take, so there would be nothing in the new one that is not an uplink")
	}
	uplinks := uplinksIn(raw.networkconf)
	if len(uplinks) == 0 {
		return recording{}, fmt.Errorf("the recording holds no uplink (no networkconf entry with purpose \"wan\"), which is the one thing only a real router can supply")
	}
	if len(raw.sysinfo.Data) == 0 {
		return recording{}, fmt.Errorf("the recording holds no sysinfo, so it does not say which Controller answered")
	}
	doh, held := dohIn(raw.setting)
	if !held {
		return recording{}, fmt.Errorf("the recording holds no Encrypted DNS setting (no %q key in get/setting), which is the other shape only a real router can supply; a Network version that predates encrypted DNS cannot re-record these fixtures", dohKey)
	}

	s := &scrubber{
		site:         siteOf(committed),
		placeholders: map[seen]string{},
		counts:       map[shape]int{},
	}
	slots, err := s.uplinks(uplinks)
	if err != nil {
		return recording{}, err
	}
	console := s.sysinfo(raw.sysinfo.Data)
	// Scrubbed after the two responses that were here first, so that the
	// counted placeholders they already hold keep the numbers they already had
	// and a re-recording is still an empty diff where nothing changed.
	setting := s.object("setting["+dohKey+"]", doh)

	out := recording{
		sysinfo:     document{Meta: s.object("sysinfo.meta", raw.sysinfo.Meta), Data: console},
		networkconf: document{Meta: s.object("networkconf.meta", raw.networkconf.Meta), Data: append(slices.Clone(lan), slots...)},
		// Emptied rather than dropped: the endpoint still answers, because a
		// recording is a statement of what unifig talks to.
		wlanconf: document{Meta: s.object("wlanconf.meta", raw.wlanconf.Meta), Data: []map[string]any{}},
		// Reduced to the one key unifig reads, for the reason at the top of
		// this file: the rest of that response is the whole console.
		setting: document{Meta: s.object("setting.meta", raw.setting.Meta), Data: []map[string]any{setting}},
	}

	// Everything that came off the router, and only that. The LAN came from
	// this repository, so it cannot be holding something the router just sent
	// — and checking it would refuse the recording of anyone whose ISP happens
	// to address them out of the same private range the committed LAN uses.
	checked := []checkable{
		{"sysinfo.meta", out.sysinfo.Meta},
		{"networkconf.meta", out.networkconf.Meta},
		{"wlanconf.meta", out.wlanconf.Meta},
		{"setting.meta", out.setting.Meta},
		{"setting[" + dohKey + "]", setting},
	}
	for i, entry := range console {
		checked = append(checked, checkable{fmt.Sprintf("sysinfo[%d]", i), entry})
	}
	for _, entry := range slots {
		checked = append(checked, checkable{fmt.Sprintf("networkconf[%s]", slotOf(entry)), entry})
	}
	if leaked := s.leaks(checked); len(leaked) > 0 {
		return recording{}, fmt.Errorf("the scrub would have written values it had already taken out, so it wrote nothing:\n%s\n"+
			"That is a field this program does not know about. Report it — the fix belongs in the scrub, not in a hand-edited file",
			strings.Join(leaked, "\n"))
	}
	return out, nil
}

// uplinksIn and networksIn split a networkconf collection the way the domain
// does: WAN slots are Settings on the router, everything else is a Resource
// the dockerized Controller can hold its own copy of.
func uplinksIn(doc document) []map[string]any {
	var uplinks []map[string]any
	for _, entry := range doc.Data {
		if entry["purpose"] == "wan" {
			uplinks = append(uplinks, entry)
		}
	}
	return uplinks
}

// dohKey is the Controller's own name for the Encrypted DNS setting, and the
// key unifig picks out of the settings response.
const dohKey = "doh"

// dohIn finds the Encrypted DNS setting in a settings response — the one entry
// of that response this recording keeps.
func dohIn(doc document) (map[string]any, bool) {
	for _, entry := range doc.Data {
		if entry["key"] == dohKey {
			return entry, true
		}
	}
	return nil, false
}

func networksIn(doc document) []map[string]any {
	var networks []map[string]any
	for _, entry := range doc.Data {
		if entry["purpose"] != "wan" {
			networks = append(networks, entry)
		}
	}
	return networks
}

// siteOf is the site the committed recording describes. The LAN comes from
// there, so every entry in the new recording claims that site — one that
// claimed the router's own would describe two consoles at once.
func siteOf(committed document) string {
	for _, entry := range committed.Data {
		if site, ok := entry["site_id"].(string); ok && site != "" {
			return site
		}
	}
	return fmt.Sprintf(placeholderObject, 0)
}

// scrubber carries the substitutions made so far: one value gets one
// placeholder however many times it appears, so a resolver used twice stays
// one resolver and two resolvers stay two.
type scrubber struct {
	site         string
	placeholders map[seen]string
	counts       map[shape]int
	// taken is what has been replaced, and what `leaks` hunts for afterwards.
	taken []replacement
}

type seen struct {
	shape shape
	value string
}

type replacement struct {
	where string
	from  string
	to    string
}

// shape is what a value is, which decides what stands in for it. It is what
// survives the scrub: an address is replaced by an address, an identifier by
// an identifier of the same form.
//
// Deliberately not called a kind: `CONTEXT.md` gives that word to the managed
// type a change is about (`network`, `wlan`, `wan`), and ADR-0010 spent a
// breaking change making it mean only that.
type shape int

const (
	shapePassword shape = iota
	shapePassphrase
	shapeUsername
	shapeHostname
	shapeTimezone
	shapeConsoleName
	shapeSite
	shapeObjectID
	shapeUUID
	shapeIPv4
	shapeIPv6
	shapeMAC
	shapeStamp
	shapeResolver

	// shapeCount is how many there are, so that a test can check every one has
	// a rule below. Keep it last: a shape with no rule would empty the field it
	// was meant to replace, which is the one thing this program must never do.
	shapeCount
)

// rule is what stands in for one shape of value: a placeholder that is always
// the same, or a format the substitutes are counted out of. `written` is how a
// value this program produced earlier is recognised, so that re-recording an
// unchanged router does not renumber it.
type rule struct {
	fixed   string
	counted string
	// site says the placeholder is the committed recording's site id, which is
	// known only to a scrubber and so cannot be written down here.
	site    bool
	written func(string) bool
}

var rules = map[shape]rule{
	shapePassword:    {fixed: placeholderPassword},
	shapePassphrase:  {fixed: placeholderPassphrase},
	shapeUsername:    {fixed: placeholderUsername},
	shapeHostname:    {fixed: placeholderHostname},
	shapeTimezone:    {fixed: placeholderTimezone},
	shapeConsoleName: {fixed: placeholderConsole},
	shapeSite:        {site: true},
	shapeObjectID:    {counted: placeholderObject, written: prefixedBy(documentationIDs)},
	shapeUUID:        {counted: placeholderUUID, written: prefixedBy(documentationUUIDs)},
	shapeIPv4:        {counted: placeholderIPv4, written: writtenAddress},
	shapeIPv6:        {counted: placeholderIPv6, written: writtenAddress},
	shapeMAC:         {counted: placeholderMAC, written: prefixedBy(documentationMACs)},
	shapeStamp:       {counted: placeholderStamp, written: prefixedBy(documentationStamps)},
	shapeResolver:    {counted: placeholderResolver, written: prefixedBy(documentationServers)},
}

// byName is the table of fields replaced because of what they are called. No
// shape distinguishes a PPPoE password from any other string, so for these the
// name is the only handle there is. Applied at any depth, and to every element
// of an array under one of these names.
var byName = map[string]shape{
	"x_wan_password": shapePassword,
	"x_password":     shapePassword,
	"password":       shapePassword,
	"x_passphrase":   shapePassphrase,
	"wan_username":   shapeUsername,
	"x_username":     shapeUsername,
	"username":       shapeUsername,
	"hostname":       shapeHostname,
	// A DNS stamp for a private endpoint carries the account it belongs to, and
	// the name beside it is the operator's own word for their resolver.
	"sdns_stamp":  shapeStamp,
	"server_name": shapeResolver,
	// A timezone is where the operator lives, spelled as a city.
	"timezone": shapeTimezone,
	"site_id":  shapeSite,
	// Identifiers of one console's objects. They give nothing away on their
	// own; they are replaced because a public recording that carries a real
	// console's identifiers is one somebody could correlate with another.
	"_id":                     shapeObjectID,
	"networkconf_id":          shapeObjectID,
	"usergroup_id":            shapeObjectID,
	"ap_group_ids":            shapeObjectID,
	"external_id":             shapeUUID,
	"uuid":                    shapeUUID,
	"anonymous_controller_id": shapeUUID,
	"anonymous_device_id":     shapeUUID,
	"sso_app_id":              shapeUUID,
	"device_id":               shapeUUID,
}

// uplinks is the scrub of the WAN entries: the operator's name for the
// connection first, then every field of the entry.
func (s *scrubber) uplinks(entries []map[string]any) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(entries))
	for i, entry := range entries {
		slot := slotOf(entry)
		if slot == "" {
			return nil, fmt.Errorf("WAN entry %d has no wan_networkgroup and no attr_hidden_id, so there is no slot to record it under", i)
		}
		where := fmt.Sprintf("networkconf[%s]", slot)

		// An operator renames a slot to their ISP, and the slot is what the
		// tests match on anyway (ADR-0010), so the slot is what the entry is
		// called in the recording.
		named := maps.Clone(entry)
		if name, ok := entry["name"].(string); ok {
			named["name"] = s.substitute(where+".name", name, slot)
		}
		out = append(out, s.object(where, named))
	}
	return out, nil
}

// sysinfo is the same for the response that says which Controller answered:
// its display name is the operator's words for their own house.
func (s *scrubber) sysinfo(entries []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(entries))
	for i, entry := range entries {
		where := fmt.Sprintf("sysinfo[%d]", i)
		named := maps.Clone(entry)
		if name, ok := entry["name"].(string); ok {
			named["name"] = s.replace(where+".name", shapeConsoleName, name)
		}
		out = append(out, s.object(where, named))
	}
	return out
}

func (s *scrubber) object(where string, entry map[string]any) map[string]any {
	out := make(map[string]any, len(entry))
	// Sorted, so that the placeholders are handed out in the same order every
	// time and re-recording an unchanged router is an empty diff.
	for _, name := range slices.Sorted(maps.Keys(entry)) {
		out[name] = s.field(where+"."+name, name, entry[name])
	}
	return out
}

// field is one field of a recorded object: replaced by name where the name is
// what gives it away, and by shape otherwise.
func (s *scrubber) field(where, name string, value any) any {
	k, named := byName[name]
	switch v := value.(type) {
	case string:
		if named {
			return s.replace(where, k, v)
		}
		return s.byShape(where, name, v)
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			// The name travels into the array with the values: an array of
			// identifiers is still identifiers.
			out[i] = s.field(fmt.Sprintf("%s[%d]", where, i), name, item)
		}
		return out
	case map[string]any:
		return s.object(where, v)
	default:
		// Numbers, booleans and nulls say nothing about a household. A VLAN
		// tag on an uplink is the ISP's, not the operator's, and the tests
		// need it to prove unifig leaves unmodelled fields alone.
		return value
	}
}

// byShape catches what no name would have caught: an address, a prefix or a
// MAC in a field this program has never heard of.
func (s *scrubber) byShape(where, name, text string) string {
	if text == "" {
		return text
	}
	if addr, err := netip.ParseAddr(text); err == nil {
		return s.address(where, name, addr, text)
	}
	if prefix, err := netip.ParsePrefix(text); err == nil {
		// A prefix keeps its length: how big a subnet is says nothing about
		// where it is, and unifig's own diffing reads that number.
		addr := s.address(where, name, prefix.Addr(), prefix.Addr().String())
		return fmt.Sprintf("%s/%d", addr, prefix.Bits())
	}
	if _, err := net.ParseMAC(text); err == nil {
		return s.replace(where, shapeMAC, text)
	}
	if isObjectID(text) {
		return s.replace(where, shapeObjectID, text)
	}
	return text
}

// isObjectID reports whether a value is one of the Controller's own object
// identifiers: twenty-four hex characters and nothing else.
//
// The name table above catches the fields that are known to hold one. This
// catches the rest, and it is not hypothetical — a real UDR's WAN entry
// carries `single_network_lan`, holding the id of a network on that console,
// in a field this program had never heard of. The shape is narrow on purpose:
// a build string or a version is not an identifier, and replacing one would
// lose the fact the recording exists to state.
func isObjectID(text string) bool {
	if len(text) != 24 {
		return false
	}
	for _, c := range text {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

func (s *scrubber) address(where, name string, addr netip.Addr, text string) string {
	switch {
	// "None" is not an address, and a mask says how big a subnet is rather
	// than where it is. Replacing either loses a fact and hides nothing.
	case addr.IsUnspecified(), isMask(name, addr):
		return text
	case addr.Is4():
		return s.replace(where, shapeIPv4, text)
	default:
		return s.replace(where, shapeIPv6, text)
	}
}

// replace swaps a value for its placeholder and remembers that it did. An
// empty field has nothing to hide, and filling it in would turn an uplink with
// no credentials into one that has them.
func (s *scrubber) replace(where string, k shape, from string) string {
	if from == "" {
		return from
	}
	return s.substitute(where, from, s.placeholder(k, from))
}

// substitute records one substitution and returns what to write, unless the
// value already was the placeholder.
func (s *scrubber) substitute(where, from, to string) string {
	if from != to {
		s.taken = append(s.taken, replacement{where: where, from: from, to: to})
	}
	return to
}

func (s *scrubber) placeholder(k shape, from string) string {
	r, ok := rules[k]
	if !ok {
		// Unreachable: a test walks every shape looking for its rule. Loud
		// rather than silent, because the silent version of this is a field
		// emptied by a program that promises never to empty one.
		panic(fmt.Sprintf("no placeholder rule for shape %d", k))
	}
	switch {
	case r.site:
		return s.site
	case r.fixed != "":
		return r.fixed
	}

	// A value this program wrote is already a placeholder; finding a
	// placeholder for it would renumber the recording on every re-record.
	if r.written != nil && r.written(from) {
		return from
	}
	if to, ok := s.placeholders[seen{k, from}]; ok {
		return to
	}
	s.counts[k]++
	to := fmt.Sprintf(r.counted, s.counts[k])
	s.placeholders[seen{k, from}] = to
	return to
}

func prefixedBy(prefix string) func(string) bool {
	return func(value string) bool {
		return strings.HasPrefix(strings.ToLower(value), strings.ToLower(prefix))
	}
}

func writtenAddress(value string) bool {
	addr, err := netip.ParseAddr(value)
	return err == nil && documentationAddress(addr)
}

// documentationAddress reports whether an address is one of the ranges set
// aside for writing about networks rather than running them: RFC 5737's three
// IPv4 blocks and RFC 3849's IPv6 one.
func documentationAddress(addr netip.Addr) bool {
	for _, block := range []string{"192.0.2.0/24", "198.51.100.0/24", "203.0.113.0/24", "2001:db8::/32"} {
		if netip.MustParsePrefix(block).Contains(addr) {
			return true
		}
	}
	return false
}

// isMask reports whether a value is a subnet mask rather than an address: a
// field that says as much by name, holding a run of ones followed by a run of
// zeroes. The name is half the test on purpose — 255.0.0.0 is a mask and also
// the front of somebody's address space, and only the field it sits in says
// which one this is.
func isMask(name string, addr netip.Addr) bool {
	if !strings.Contains(strings.ToLower(name), "mask") || !addr.Is4() {
		return false
	}
	octets := addr.As4()
	bits := uint32(octets[0])<<24 | uint32(octets[1])<<16 | uint32(octets[2])<<8 | uint32(octets[3])
	// Ones then zeroes: inverted, a mask is one less than a power of two.
	inverted := ^bits
	return bits != 0 && inverted&(inverted+1) == 0
}

// checkable is one scrubbed object and where it came from, for the check below.
type checkable struct {
	where string
	entry map[string]any
}

// leaks is the check that makes the tables above more than a list of the
// fields somebody thought of: everything the scrub took out, hunted for again
// in what it is about to write. A hit means an unknown field carries a value
// this program already decided was the operator's, and the answer is to write
// nothing at all.
//
// The values themselves never appear in the result — only where they were
// taken from and where they turned up, which is what a fix needs.
func (s *scrubber) leaks(checked []checkable) []string {
	var found []string
	for _, c := range checked {
		walk(c.where, c.entry, func(where, text string) {
			for _, taken := range s.taken {
				if holds(text, taken.from) {
					found = append(found, fmt.Sprintf("  %s still holds the value taken out of %s", where, taken.where))
				}
			}
		})
	}
	return found
}

// distinctive is how long a value has to be before it is worth hunting for.
// Below it, a value collides with things that are nobody's secret — a device
// type, a version string, a VLAN tag — and the refusal that follows is one the
// operator cannot act on, because there is nothing wrong. Short values are
// still replaced wherever this program knows the field they are in; what they
// do not get is the backstop, which exists for what actually identifies
// somebody and is never four characters long.
const distinctive = 5

// holds reports whether a scrubbed value still carries one that was taken out,
// looking inside longer strings because an ISP's name reappears in the middle
// of them.
func holds(text, from string) bool {
	if len(from) < distinctive {
		return false
	}
	return strings.Contains(strings.ToLower(text), strings.ToLower(from))
}

// walk visits every string in a recorded object, naming where it found it.
func walk(where string, value any, visit func(where, text string)) {
	switch v := value.(type) {
	case string:
		visit(where, v)
	case map[string]any:
		for _, name := range slices.Sorted(maps.Keys(v)) {
			walk(where+"."+name, v[name], visit)
		}
	case []any:
		for i, item := range v {
			walk(fmt.Sprintf("%s[%d]", where, i), item, visit)
		}
	}
}

// slotOf is the slot a WAN entry occupies — the Controller's own name for the
// uplink, which is what a WAN test matches on (ADR-0010).
func slotOf(entry map[string]any) string {
	for _, field := range []string{"wan_networkgroup", "attr_hidden_id"} {
		if slot, ok := entry[field].(string); ok && slot != "" {
			return slot
		}
	}
	return ""
}

func read(body []byte) (document, error) {
	var doc document
	if err := json.Unmarshal(body, &doc); err != nil {
		return document{}, err
	}
	return doc, nil
}

// bytes is the recorded response as it goes into the repository: two-space
// JSON, one trailing newline, and no HTML escaping, so that a value with an
// ampersand in it reads the way the Controller sent it.
//
// The fields of an entry come out in alphabetical order, because a JSON object
// read into a map has no order left to preserve and a stable one makes the
// diff an operator reads worth reading.
func (d document) bytes() ([]byte, error) {
	if len(d.Meta) == 0 {
		d.Meta = map[string]any{"rc": "ok"}
	}
	if d.Data == nil {
		d.Data = []map[string]any{}
	}

	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(d); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
