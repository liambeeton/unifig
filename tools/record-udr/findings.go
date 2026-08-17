package main

import (
	"fmt"
	"io"
)

// ADR-0008 records two things about a WAN slot that no container can settle,
// and ADR-0012 one more about the Encrypted DNS setting. The moment somebody
// has a real router on the other end of this program is the moment to ask. All
// are read off the responses that have just come back — nothing extra is
// requested, nothing is written — and all are answered without printing the
// secret they are about.

// readback is what the Controller did with the PPPoE password when asked for
// the slot back. The three cases are three different worlds: populated means
// ADR-0007's finding holds for the second secret unifig models, empty means
// every WAN plan would be permanently non-empty, and absent means the field is
// not there at all and a write-only fallback would be needed after all.
type readback string

const (
	populated readback = "populated"
	empty     readback = "empty"
	absent    readback = "absent"
)

// answer is what one uplink says about both questions.
type answer struct {
	slot     string
	password readback
	flags    []flag
}

type flag struct{ name, value string }

// answers asks the recording, for every uplink already configured for PPPoE.
// A slot on DHCP has nothing to say about a credential it does not use.
func answers(networkconf document) []answer {
	var given []answer
	for _, entry := range uplinksIn(networkconf) {
		if entry["wan_type"] != "pppoe" {
			continue
		}
		given = append(given, answer{
			slot:     slotOf(entry),
			password: readbackOf(entry),
			flags: []flag{
				{"wan_pppoe_username_enabled", flagValue(entry, "wan_pppoe_username_enabled")},
				{"wan_pppoe_password_enabled", flagValue(entry, "wan_pppoe_password_enabled")},
			},
		})
	}
	return given
}

func readbackOf(entry map[string]any) readback {
	return fieldReadback(entry, "x_wan_password")
}

// stamps is what the Controller did with each custom DNS server's stamp when
// asked for the setting back — ADR-0012's one deferred question, and the same
// question as the PPPoE password's, asked about the third secret unifig models.
//
// Each answer is named by its resolver rather than by its stamp, for the same
// reason the slot names the uplink: the value is the thing being asked about.
type stamps []answer

func stampAnswer(setting document) stamps {
	doh, held := dohIn(setting)
	if !held {
		return nil
	}
	servers, _ := doh["custom_servers"].([]any)

	var given stamps
	for i, server := range servers {
		entry, ok := server.(map[string]any)
		if !ok {
			continue
		}
		name, _ := entry["server_name"].(string)
		if name == "" {
			name = fmt.Sprintf("custom_servers[%d]", i)
		}
		given = append(given, answer{slot: name, password: fieldReadback(entry, "sdns_stamp")})
	}
	return given
}

func fieldReadback(entry map[string]any, name string) readback {
	value, ok := entry[name]
	switch {
	case !ok || value == nil:
		return absent
	case value == "":
		return empty
	default:
		return populated
	}
}

// flagValue is what a field holds, as text, saying so when it holds nothing at all.
// Whatever the router has, verbatim: unifig's own behaviour was reasoned out
// rather than measured, so a report that tidied the answer up would be
// confirming the guess with itself.
func flagValue(entry map[string]any, name string) string {
	value, ok := entry[name]
	if !ok {
		return string(absent)
	}
	return fmt.Sprintf("%v", value)
}

// report writes the answers where the operator running this will read them,
// in the form the ADRs want them in.
func report(w io.Writer, given []answer, stamped stamps) {
	line := func(format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }

	line("The ADRs leave questions open that only a real router can answer. This\n")
	line("recording was read, not written, and it says:\n\n")

	line("ADR-0008, about a WAN slot:\n\n")
	if len(given) == 0 {
		line("  Nothing yet: this router has no uplink on PPPoE, and both questions are\n")
		line("  about a slot that signs in. They stay open.\n\n")
	} else {
		line("  1. Does x_wan_password read back populated?\n")
		for _, a := range given {
			line("     %s: %s\n", a.slot, a.password)
		}
		line("\n  2. What do the PPPoE flags hold on a slot that works?\n")
		for _, a := range given {
			line("     %s:", a.slot)
			for _, f := range a.flags {
				line(" %s=%s", f.name, f.value)
			}
			line("\n")
		}
		line("\n  If one of those uplinks is the one this site actually connects on, that\n")
		line("  is the answer to both: record it in docs/adr/0008-wan-slots-replay-recorded-responses.md.\n\n")
	}

	line("ADR-0012, about the Encrypted DNS setting:\n\n")
	if len(stamped) == 0 {
		line("  Nothing yet: this router has no custom DNS server configured, and the\n")
		line("  question is about a stamp it would have to hold. It stays open.\n\n")
		return
	}
	line("  3. Does sdns_stamp read back populated?\n")
	for _, a := range stamped {
		line("     %s: %s\n", a.slot, a.password)
	}
	line("\n  Record it in docs/adr/0012-encrypted-dns-is-a-singleton-setting.md.\n\n")
}
