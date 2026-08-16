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
// Each variable is named after the WLAN it belongs to, so an operator reading
// the file can tell at a glance which one is which, and two runs against an
// unchanged Controller produce the same names.
func Redact(cfg config.Config) (config.Config, []string) {
	redacted := cfg
	redacted.WLANs = make([]config.WLAN, len(cfg.WLANs))
	copy(redacted.WLANs, cfg.WLANs)

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

func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
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
