// Redact is where a secret stops being a secret if it goes wrong, so it is
// tested directly rather than only through the e2e suite. It needs no
// Controller: it is a pure rewrite of a config into the config that should be
// committed instead.
package export_test

import (
	"strings"
	"testing"

	"github.com/liambeeton/unifig/internal/config"
	"github.com/liambeeton/unifig/internal/export"
)

func wlan(name, passphrase string) config.WLAN {
	return config.WLAN{Name: name, Network: "Default", Passphrase: passphrase}
}

func TestRedactReplacesEveryPassphraseWithAReferenceToTheVariableItNames(t *testing.T) {
	redacted, vars := export.Redact(config.Config{
		Networks: []config.Network{{Name: "Default", Subnet: "192.168.1.1/24"}},
		WLANs: []config.WLAN{
			wlan("Home", "correct horse battery"),
			wlan("Home Guest", "a different passphrase"),
		},
	})

	want := []string{"UNIFIG_WLAN_HOME_PASSPHRASE", "UNIFIG_WLAN_HOME_GUEST_PASSPHRASE"}
	if len(vars) != len(want) {
		t.Fatalf("redacted %d secrets, want %d: %v", len(vars), len(want), vars)
	}
	for i, name := range want {
		if vars[i] != name {
			t.Errorf("vars[%d] = %q, want %q", i, vars[i], name)
		}
		if got := redacted.WLANs[i].Passphrase; got != "${"+name+"}" {
			t.Errorf("wlans[%d].passphrase = %q, want a reference to %s", i, got, name)
		}
	}
	// Everything else survives untouched — redaction is not a filter.
	if len(redacted.Networks) != 1 || redacted.Networks[0].Subnet != "192.168.1.1/24" {
		t.Errorf("redaction changed the networks: %+v", redacted.Networks)
	}
	if redacted.WLANs[0].Name != "Home" || redacted.WLANs[0].Network != "Default" {
		t.Errorf("redaction changed a WLAN's identity: %+v", redacted.WLANs[0])
	}
}

// The config handed in is the one the caller may still want to write with
// --with-secrets, so redaction must not reach back into it.
func TestRedactLeavesTheConfigItWasGivenAlone(t *testing.T) {
	original := config.Config{WLANs: []config.WLAN{wlan("Home", "correct horse battery")}}

	if _, _ = export.Redact(original); original.WLANs[0].Passphrase != "correct horse battery" {
		t.Errorf("Redact rewrote its argument: %+v", original.WLANs[0])
	}
}

func TestRedactHasNothingToSayAboutAWLANWithNoPassphrase(t *testing.T) {
	redacted, vars := export.Redact(config.Config{WLANs: []config.WLAN{wlan("Open", "")}})

	if len(vars) != 0 {
		t.Errorf("redacted %v, but there was no secret to redact", vars)
	}
	if redacted.WLANs[0].Passphrase != "" {
		t.Errorf("passphrase = %q, want it left empty", redacted.WLANs[0].Passphrase)
	}
}

// A WLAN's name is not an identifier, and the variable name derived from it has
// to be one an operator can actually type into a shell.
func TestVariableNamesAreUsableWhateverTheWLANIsCalled(t *testing.T) {
	for _, named := range []struct{ name, want string }{
		{"Home", "UNIFIG_WLAN_HOME_PASSPHRASE"},
		{"Home Guest", "UNIFIG_WLAN_HOME_GUEST_PASSPHRASE"},
		{"Home  Wi-Fi!", "UNIFIG_WLAN_HOME_WI_FI_PASSPHRASE"},
		// A leading digit is why the prefix is not optional.
		{"5GHz", "UNIFIG_WLAN_5GHZ_PASSPHRASE"},
		{" padded ", "UNIFIG_WLAN_PADDED_PASSPHRASE"},
		// Nothing an identifier can use contributes no word, rather than an
		// empty one between two underscores.
		{"📶", "UNIFIG_WLAN_PASSPHRASE"},
	} {
		t.Run(named.name, func(t *testing.T) {
			_, vars := export.Redact(config.Config{WLANs: []config.WLAN{wlan(named.name, "correct horse battery")}})

			if len(vars) != 1 {
				t.Fatalf("redacted %v, want one variable", vars)
			}
			if vars[0] != named.want {
				t.Errorf("variable = %q, want %q", vars[0], named.want)
			}
		})
	}
}

// WLAN names are unique — they are the natural key — but the alphabet a variable
// name allows is smaller than the one a WLAN name does, so two different WLANs
// can arrive at one variable. One silently standing in for the other would put
// the wrong passphrase on a WLAN.
func TestTwoWLANNamesThatWouldShareAVariableGetOneEach(t *testing.T) {
	redacted, vars := export.Redact(config.Config{WLANs: []config.WLAN{
		wlan("Home Wi-Fi", "the first passphrase"),
		wlan("Home Wi Fi", "the second passphrase"),
	}})

	if len(vars) != 2 {
		t.Fatalf("redacted %v, want two variables", vars)
	}
	if vars[0] == vars[1] {
		t.Fatalf("both WLANs were redacted to %q, so one passphrase is unreachable", vars[0])
	}
	for i, name := range vars {
		if got := redacted.WLANs[i].Passphrase; got != "${"+name+"}" {
			t.Errorf("wlans[%d].passphrase = %q, want a reference to %s", i, got, name)
		}
	}
}

// The notices export writes are the operator's only sight of what redaction and
// scoping did, so what they say matters as much as what the file contains.
func TestTheRedactionNoticeNamesEveryVariableAndNoSecret(t *testing.T) {
	const secret = "correct horse battery"
	_, vars := export.Redact(config.Config{WLANs: []config.WLAN{
		wlan("Home", secret),
		wlan("Home Guest", "a different passphrase"),
	}})

	var notice strings.Builder
	if err := export.WriteVariables(&notice, vars); err != nil {
		t.Fatalf("writing the notice: %v", err)
	}

	written := notice.String()
	for _, name := range vars {
		if !strings.Contains(written, "export "+name+"=") {
			t.Errorf("the notice does not tell the operator to set %s:\n%s", name, written)
		}
	}
	if strings.Contains(written, secret) {
		t.Errorf("the notice printed the secret it had just redacted:\n%s", written)
	}
	if !strings.Contains(written, "2 secrets") {
		t.Errorf("the notice should count what it redacted:\n%s", written)
	}
}

func TestNothingRedactedMeansNoNoticeAtAll(t *testing.T) {
	var notice strings.Builder
	if err := export.WriteVariables(&notice, nil); err != nil {
		t.Fatalf("writing the notice: %v", err)
	}
	if notice.Len() != 0 {
		t.Errorf("--with-secrets should ask for nothing, got:\n%s", notice.String())
	}
}

// A WLAN export could not describe is left out of the file, and saying so is
// the difference between "unifig does not manage this" and "unifig forgot it".
func TestTheOmissionNoticeNamesWhatWasLeftOutAndSaysPruneWontTouchIt(t *testing.T) {
	var notice strings.Builder
	if err := export.WriteOmissions(&notice, []string{"On A WAN", "On Nothing"}); err != nil {
		t.Fatalf("writing the notice: %v", err)
	}

	written := notice.String()
	for _, fragment := range []string{`"On A WAN"`, `"On Nothing"`, "2 WLANs", "prune"} {
		if !strings.Contains(written, fragment) {
			t.Errorf("the notice should mention %q, got:\n%s", fragment, written)
		}
	}
}

func TestNothingLeftOutMeansNoOmissionNotice(t *testing.T) {
	var notice strings.Builder
	if err := export.WriteOmissions(&notice, nil); err != nil {
		t.Fatalf("writing the notice: %v", err)
	}
	if notice.Len() != 0 {
		t.Errorf("an export that left nothing out should say nothing, got:\n%s", notice.String())
	}
}

// The whole point, stated on its own: after redaction the secret is not in the
// document anywhere.
func TestNoPassphraseSurvivesRedaction(t *testing.T) {
	const secret = "correct horse battery"

	redacted, _ := export.Redact(config.Config{WLANs: []config.WLAN{
		wlan("Home", secret),
		wlan("Home Guest", secret),
	}})

	var written strings.Builder
	if err := config.WriteYAML(&written, redacted); err != nil {
		t.Fatalf("writing redacted config: %v", err)
	}
	if strings.Contains(written.String(), secret) {
		t.Errorf("the redacted config still carries the passphrase:\n%s", written.String())
	}
}
