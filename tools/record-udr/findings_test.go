package main

import (
	"strings"
	"testing"
)

// ADR-0008 leaves two questions open that only a real router can answer, and
// the moment somebody has one on the other end of this program is the moment
// to ask them. Both are read-only, and both are about the recording that just
// came back — which is why they are asked before the scrub, on the raw
// response, and answered without printing the credential itself.

func TestTheAnswersSayWhetherThePasswordReadsBackPopulated(t *testing.T) {
	doc := parse(t, rawNetworkconf)

	given := answers(doc)
	if len(given) != 1 {
		t.Fatalf("asked about %d slots, want the one on PPPoE: %+v", len(given), given)
	}
	if given[0].slot != "WAN" || given[0].password != populated {
		t.Errorf("the WAN slot's password read back %v, want %v", given[0].password, populated)
	}

	// An uplink whose credential comes back empty is the case that would make
	// every WAN plan non-empty, so it is reported as its own answer rather than
	// rolled in with a slot that has none.
	slot(t, doc, "WAN")["x_wan_password"] = ""
	if given := answers(doc); given[0].password != empty {
		t.Errorf("an empty credential read back as %v, want %v", given[0].password, empty)
	}
	// And a Controller that withholds the field is a third answer again: the
	// write-only fallback ADR-0007 decided it did not need.
	delete(slot(t, doc, "WAN"), "x_wan_password")
	if given := answers(doc); given[0].password != absent {
		t.Errorf("a withheld credential read back as %v, want %v", given[0].password, absent)
	}
}

func TestTheAnswersReportThePPPoEFlagsAsTheyStand(t *testing.T) {
	doc := parse(t, rawNetworkconf)
	slot(t, doc, "WAN")["wan_pppoe_password_enabled"] = false
	delete(slot(t, doc, "WAN"), "wan_pppoe_username_enabled")

	report := reportOf(t, answers(doc), nil)

	// Whatever the router holds, verbatim: this is the fact unifig's own
	// behaviour was guessed from, so a report that tidied it up would be
	// answering the question with the guess.
	if !strings.Contains(report, "wan_pppoe_password_enabled=false") {
		t.Errorf("the report should say what the flag holds, got:\n%s", report)
	}
	if !strings.Contains(report, "wan_pppoe_username_enabled=absent") {
		t.Errorf("the report should say when the flag is not there at all, got:\n%s", report)
	}
}

func TestTheAnswersNeverPrintTheCredentialItself(t *testing.T) {
	report := reportOf(t, answers(parse(t, rawNetworkconf)), nil)

	for _, secret := range []string{"hunter2-off-the-router", "user@fibreco.example"} {
		if strings.Contains(report, secret) {
			t.Errorf("the report printed %q, and this program's whole point is that it does not:\n%s", secret, report)
		}
	}
}

// The questions are about a slot that is signed in. A router with none cannot
// answer them, and saying so is the answer — the alternative is an operator
// reading a clean report as a confirmation it never gave.
func TestTheReportSaysWhenNoUplinkCanAnswer(t *testing.T) {
	doc := parse(t, rawNetworkconf)
	slot(t, doc, "WAN")["wan_type"] = "dhcp"

	report := reportOf(t, answers(doc), nil)

	if !strings.Contains(report, "no uplink on PPPoE") {
		t.Errorf("the report should say the questions went unanswered, got:\n%s", report)
	}
	if strings.Contains(report, "ADR-0008 is closed") {
		t.Errorf("the report claimed an answer it does not have:\n%s", report)
	}
}

// ADR-0012's one deferred question, asked of the third secret unifig models.
func TestTheAnswersSayWhetherTheDNSStampReadsBackPopulated(t *testing.T) {
	setting := parse(t, rawSetting)

	given := stampAnswer(setting)
	if len(given) != 1 {
		t.Fatalf("asked about %d resolvers, want the one the router holds: %+v", len(given), given)
	}
	if given[0].password != populated {
		t.Errorf("the stamp read back %v, want %v", given[0].password, populated)
	}

	servers, _ := doh(t, setting)["custom_servers"].([]any)
	servers[0].(map[string]any)["sdns_stamp"] = ""
	if given := stampAnswer(setting); given[0].password != empty {
		t.Errorf("an empty stamp read back as %v, want %v", given[0].password, empty)
	}
}

func TestTheAnswersNeverPrintTheStampItself(t *testing.T) {
	report := reportOf(t, nil, stampAnswer(parse(t, rawSetting)))

	if strings.Contains(report, "secret-endpoint") {
		t.Errorf("the report printed the stamp, and this program's whole point is that it does not:\n%s", report)
	}
}

// A router with encrypted DNS switched off cannot answer, and saying so is the
// answer — the alternative is a clean report read as a confirmation it never
// gave.
func TestTheReportSaysWhenNoResolverCanAnswer(t *testing.T) {
	setting := parse(t, rawSetting)
	delete(doh(t, setting), "custom_servers")

	report := reportOf(t, nil, stampAnswer(setting))

	if !strings.Contains(report, "no custom DNS server") {
		t.Errorf("the report should say the question went unanswered, got:\n%s", report)
	}
}

func reportOf(t *testing.T, given []answer, stamped stamps) string {
	t.Helper()
	var out strings.Builder
	report(&out, given, stamped)
	return out.String()
}
