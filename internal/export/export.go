// Package export generates YAML config from live Controller state — the
// brownfield adoption path (Export in the domain glossary).
//
// The projection itself belongs to the engine (reconcile.Project), so that what
// export writes is by construction config unifig can read back. What lives here
// is the one thing only export needs: turning the secrets the Controller handed
// over into `${ENV_VAR}` references, so the file is committable the moment it is
// generated rather than after a careful read-through.
package export

import (
	"fmt"
	"io"
	"strings"

	"github.com/liambeeton/unifig/internal/config"
)

// Redact rewrites every secret in cfg as a `${VAR}` reference and returns the
// environment variables that now have to be set, in file order.
//
// Redaction is the default rather than the option because of what export is
// for. Its output goes into a git repository, and a file that arrives with a
// live passphrase in it has already been committed by the time anyone notices.
// So the safe form is the one you get by default, and the plaintext one is the
// one you have to ask for.
//
// Each variable is named after the thing it belongs to — the WLAN, the WAN slot
// — so an operator reading the file can tell at a glance which one is which,
// and two runs against an unchanged Controller produce the same names.
func Redact(cfg config.Config) (config.Config, []string) {
	redacted := cfg
	redacted.WLANs = make([]config.WLAN, len(cfg.WLANs))
	copy(redacted.WLANs, cfg.WLANs)
	redacted.WAN = make([]config.WANSlot, len(cfg.WAN))
	copy(redacted.WAN, cfg.WAN)

	var vars []string
	taken := map[string]bool{}
	for i, wlan := range redacted.WLANs {
		if wlan.Passphrase == "" {
			continue
		}
		name := unique(taken, envVar("UNIFIG_WLAN", wlan.Name, "PASSPHRASE"))
		redacted.WLANs[i].Passphrase = "${" + name + "}"
		vars = append(vars, name)
	}
	// A slot's own name needs no prefix beyond unifig's: "WAN" and "WAN2" are
	// already the Controller's names for the uplinks, so UNIFIG_WAN_PASSWORD is
	// both unambiguous and the name an operator would have picked.
	for i, slot := range redacted.WAN {
		if slot.Password == "" {
			continue
		}
		name := unique(taken, envVar("UNIFIG", slot.Slot, "PASSWORD"))
		redacted.WAN[i].Password = "${" + name + "}"
		vars = append(vars, name)
	}
	// A DNS stamp is the third secret, and the copy is one level deeper than
	// the others: the section is a pointer and its servers are a slice, so both
	// are replaced rather than shared with the config the caller handed in.
	if cfg.EncryptedDNS != nil {
		dns := *cfg.EncryptedDNS
		dns.Servers = make([]config.DNSServer, len(cfg.EncryptedDNS.Servers))
		copy(dns.Servers, cfg.EncryptedDNS.Servers)
		redacted.EncryptedDNS = &dns

		for i, server := range dns.Servers {
			if server.Stamp == "" {
				continue
			}
			name := unique(taken, envVar("UNIFIG_DNS", server.Name, "STAMP"))
			dns.Servers[i].Stamp = "${" + name + "}"
			vars = append(vars, name)
		}
	}
	return redacted, vars
}

// WriteVariables explains which environment variables redaction invented and
// what to do about them.
//
// Its caller writes it to stderr, and that is the point: `unifig export >
// unifig.yaml` has to leave stdout carrying nothing but YAML, while the
// operator still needs to be told, on their terminal, that the file they just
// made will not work until they export something.
func WriteVariables(w io.Writer, vars []string) error {
	if len(vars) == 0 {
		return nil
	}

	pronoun := "them"
	if len(vars) == 1 {
		pronoun = "it"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "\nRedacted %s. Set %s before running unifig:\n\n", count(len(vars), "secret"), pronoun)
	for _, name := range vars {
		fmt.Fprintf(&b, "  export %s=...\n", name)
	}
	b.WriteString("\nThe values are on the Controller; `unifig export --with-secrets` prints them inline instead.\n")

	_, err := io.WriteString(w, b.String())
	return err
}

// WriteOmissions names the WLANs export could not describe, so that a file
// which came back short says so.
//
// Export is the brownfield adoption path, and its promise is that adopting a
// configured Controller takes no hand-transcription. A WLAN silently missing
// from the output would break that promise quietly — the operator would believe
// the file describes their Controller, and it would not. Saying so costs two
// lines and is the difference between "unifig does not manage this" and "unifig
// forgot this".
//
// Nothing here is a failure: the WLANs named are ones unifig deliberately does
// not manage, and prune leaves them alone for the same reason. It is a notice,
// not a warning.
func WriteOmissions(w io.Writer, wlans []string) error {
	if len(wlans) == 0 {
		return nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "\nLeft out %s, which unifig does not manage: %s.\n",
		count(len(wlans), "WLAN"), quoteAll(wlans))
	b.WriteString("Each is attached to something that is not one of this site's LANs, so there is no network for unifig to name in the config. It manages nothing about them, and `--prune` will not delete them.\n")

	_, err := io.WriteString(w, b.String())
	return err
}

// WritePartialZones names the zones whose membership the config states only in
// part, so that a file which says less than it appears to says why.
//
// The shortfall is one member rather than a whole object: a zone holds something
// that is not one of this site's LANs — the WAN in the built-in External zone,
// most often — and there is no name for the config to put in the list for it. So
// the zone is in the file, unifig manages the members it can name, and the one
// it cannot stays exactly where it is. Saying so is what stops an operator
// reading `networks:` on External as the whole truth about what is in it.
func WritePartialZones(w io.Writer, zones []string) error {
	if len(zones) == 0 {
		return nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "\nWrote %s listing only part of what it holds: %s.\n",
		count(len(zones), "zone"), quoteAll(zones))
	b.WriteString("Each also holds something that is not one of this site's LANs — a WAN network, for example — which the config has no name for. unifig manages the networks listed and leaves the rest of the membership exactly as it is.\n")

	_, err := io.WriteString(w, b.String())
	return err
}

// WriteIndescribablePolicies names the firewall policies export left out
// entirely, on the same promise as WriteOmissions: a file that came back short
// says so.
//
// A policy is all three of its fields — its verdict and the zones on either end
// — so unlike a zone there is no partial way to write one. A policy whose zone
// unifig cannot name, or whose verdict it does not model, is one the config has
// no way to state at all.
func WriteIndescribablePolicies(w io.Writer, policies []string) error {
	if len(policies) == 0 {
		return nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "\nLeft out %s, which unifig cannot describe: %s.\n",
		countOf(len(policies), "firewall policy", "firewall policies"), quoteAll(policies))
	b.WriteString("Each either governs a zone that is not one of this site's, or does something to the traffic that unifig does not model. It manages nothing about them, and `--prune` will not delete them.\n")

	_, err := io.WriteString(w, b.String())
	return err
}

// WritePartialWANSlots names the WAN slots the config describes by slot alone,
// so that a file which says less than the operator expects says why.
//
// It is the same promise as WriteOmissions and a different shape of shortfall:
// the slot is in the file and unifig will match it, but the way it connects —
// static addressing, DS-Lite — is not something unifig's config can state, so
// nothing in the file describes it and nothing unifig does will change it.
func WritePartialWANSlots(w io.Writer, slots []string) error {
	if len(slots) == 0 {
		return nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "\nWrote %s with nothing but the slot: %s.\n", count(len(slots), "WAN slot"), quoteAll(slots))
	b.WriteString("Each connects in a way unifig does not model — static addressing, for example — so there is nothing for the config to say about it. unifig will match the slot and change nothing about how it connects.\n")

	_, err := io.WriteString(w, b.String())
	return err
}

// WriteNoEncryptedDNS says why the config has no `encrypted-dns:` section, for
// the same reason WriteOmissions names a WLAN it left out: a file that came back
// short says why it did.
//
// The shortfall here belongs to the Controller rather than to unifig. There is
// no Encrypted DNS setting on the other end — an older Network version, or a
// site without it — so there is nothing to describe, and an operator who went
// looking for the section deserves to know which of the two that is.
func WriteNoEncryptedDNS(w io.Writer, absent bool) error {
	if !absent {
		return nil
	}
	_, err := io.WriteString(w,
		"\nWrote no `encrypted-dns:` section: this Controller has no Encrypted DNS setting to describe.\n")
	return err
}

// WriteUnmodelledDNSState names an Encrypted DNS state unifig does not model,
// so that a section written without one says why.
//
// It is WritePartialWANSlots one level down: the section is in the file and
// unifig will manage the resolvers in it, but the mode the Controller is in is
// not one the config can state, so the file says nothing about it and neither
// will an apply. Writing it anyway would produce a file unifig's own validate
// rejects, which is the shortfall this notice exists instead of.
func WriteUnmodelledDNSState(w io.Writer, state string) error {
	if state == "" {
		return nil
	}
	_, err := fmt.Fprintf(w,
		"\nWrote the `encrypted-dns:` section with no `state`: this Controller's is %q, which unifig does not model.\nIt manages the custom servers listed there and changes nothing about which mode encrypted DNS is in.\n",
		state)
	return err
}

// count says how many of something there are, in the words that read naturally
// aloud: "1 WLAN", "3 WLANs".
func count(n int, noun string) string {
	return countOf(n, noun, noun+"s")
}

// countOf is count for a noun whose plural is not its singular with an "s" on
// the end. There is one so far — "firewall policies" — and it needs its own
// entry point rather than a rule, because English has no rule here worth
// encoding for the handful of nouns this package names.
func countOf(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func quoteAll(values []string) string {
	quoted := make([]string, len(values))
	for i, v := range values {
		quoted[i] = fmt.Sprintf("%q", v)
	}
	return strings.Join(quoted, ", ")
}

// envVar builds an environment variable name from a Resource's own name:
// UNIFIG_WLAN_HOME_GUEST_PASSPHRASE for a WLAN called "Home Guest".
//
// Anything outside an identifier's alphabet becomes a single underscore, so an
// SSID full of punctuation still produces something an operator can type into a
// shell. The fixed prefix is what makes the result valid whatever the SSID
// starts with — "5GHz" would otherwise lead with a digit, which no shell
// accepts.
func envVar(prefix, name, suffix string) string {
	var middle strings.Builder
	separated := true // nothing written yet, so a leading separator is dropped
	for _, r := range strings.ToUpper(name) {
		if r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			middle.WriteRune(r)
			separated = false
			continue
		}
		// One underscore however many characters ran together, so "Home
		// Wi-Fi!" and "Home Wi Fi" both come out as the same three words.
		if !separated {
			middle.WriteByte('_')
			separated = true
		}
	}

	// A name with nothing an identifier can use — an SSID that is all emoji —
	// contributes no word rather than an empty one between two underscores.
	// Two such names would collide, which is unique's job to notice.
	word := strings.TrimSuffix(middle.String(), "_")
	if word == "" {
		return prefix + "_" + suffix
	}
	return prefix + "_" + word + "_" + suffix
}

// unique keeps two names from colliding. Natural keys are unique, but the
// alphabet an environment variable allows is smaller than the one an SSID does,
// so "Home Wi-Fi" and "Home Wi Fi" arrive at the same name — and one silently
// standing in for the other would put the wrong passphrase on a WLAN.
func unique(taken map[string]bool, name string) string {
	candidate := name
	for n := 2; taken[candidate]; n++ {
		candidate = fmt.Sprintf("%s_%d", name, n)
	}
	taken[candidate] = true
	return candidate
}
