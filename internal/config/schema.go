package config

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"golang.org/x/text/language"
	"golang.org/x/text/message"

	"github.com/liambeeton/unifig/schema"
)

// compiled is the embedded schema, compiled once. Compilation can only fail if
// the binary was built with a broken schema, so the error travels no further
// than the message it produces.
//
// Note what is *not* here: no URL loader is installed, and the schema is
// handed to the compiler by hand under the same URL as its own `$id`. The
// library's default loader reads the filesystem, never the network, and this
// schema refers only to itself — so compiling it cannot quietly turn validate
// into something that needs the internet to run.
var compiled = sync.OnceValues(func() (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schema.JSON))
	if err != nil {
		return nil, fmt.Errorf("parsing unifig's embedded JSON Schema (this is a bug in unifig): %w", err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(schema.URL, doc); err != nil {
		return nil, fmt.Errorf("loading unifig's embedded JSON Schema (this is a bug in unifig): %w", err)
	}
	return compiler.Compile(schema.URL)
})

// printer renders the library's own wording for any error kind this file does
// not rephrase. unifig is English-only, so the locale is fixed.
var printer = message.NewPrinter(language.English)

// validateSchema checks doc against the published JSON Schema and translates
// each failure into a Problem an operator can act on.
//
// The rephrasing below exists because JSON Schema's own vocabulary leaks its
// implementation: "does not match pattern ^((25[0-5]|..." tells an operator
// nothing about what to type instead. Only the kinds this schema can actually
// produce are rephrased; anything else falls back to the library's wording, so
// growing the schema degrades the message rather than losing it.
func validateSchema(doc document) ([]Problem, error) {
	sch, err := compiled()
	if err != nil {
		return nil, err
	}
	err = sch.Validate(doc.instance)
	if err == nil {
		return nil, nil
	}

	var invalid *jsonschema.ValidationError
	if !errors.As(err, &invalid) {
		return nil, fmt.Errorf("validating against unifig's JSON Schema: %w", err)
	}
	return leafProblems(invalid, doc), nil
}

// leafProblems flattens the validation error tree to its leaves. The interior
// nodes only say "something below here failed"; the leaves are the findings.
func leafProblems(invalid *jsonschema.ValidationError, doc document) []Problem {
	if len(invalid.Causes) > 0 {
		var problems []Problem
		for _, cause := range invalid.Causes {
			problems = append(problems, leafProblems(cause, doc)...)
		}
		return problems
	}
	return describe(invalid, doc)
}

// describe turns one leaf failure into Problems — plural, because a single
// additionalProperties failure names every unknown field on the object at once
// and each deserves its own line and location.
func describe(invalid *jsonschema.ValidationError, doc document) []Problem {
	at := doc.index.at(invalid.InstanceLocation)

	switch k := invalid.ErrorKind.(type) {
	case *kind.AdditionalProperties:
		problems := make([]Problem, 0, len(k.Properties))
		for _, name := range k.Properties {
			// Point at the offending field rather than the object holding it.
			where := doc.index.at(append(slices.Clone(invalid.InstanceLocation), name))
			problems = append(problems, Problem{
				Line: where.line, Path: where.path,
				Message: fmt.Sprintf("unknown field %q — check the spelling against the schema", name),
			})
		}
		return problems

	case *kind.Required:
		noun := "field"
		if len(k.Missing) > 1 {
			noun = "fields"
		}
		return []Problem{{Line: at.line, Path: at.path, Message: fmt.Sprintf(
			"missing required %s %s", noun, quoteAll(k.Missing))}}

	case *kind.Type:
		text := fmt.Sprintf("must be %s, but this is %s", strings.Join(k.Want, " or "), k.Got)
		// Say so only where interpolation actually supplied the value: an
		// operator who quoted a number by hand is not helped by being told
		// about ${ENV_VAR}, and would be misled into looking for one.
		if k.Got == "string" && !slices.Contains(k.Want, "string") && doc.interpolated[at.path] {
			text += ` — ${ENV_VAR} interpolation always produces text, so it cannot fill this field`
		}
		return []Problem{{Line: at.line, Path: at.path, Message: text}}

	case *kind.Pattern:
		// The offending value is quoted, because "10.20.0.1 is not in the
		// expected format" is what tells an operator which of the things on
		// that line was wrong — unless the field is a secret, which is printed,
		// logged and pasted into bug reports, and is none of those things. A
		// secret gets the expectation without the value, so the schema can
		// constrain one without validate leaking it.
		shape := fieldShape(at.path)
		text := "is not in the expected format"
		if !secretFields[shape] {
			text = fmt.Sprintf("%q %s", k.Got, text)
		}
		if hint, ok := patternHints[shape]; ok {
			text += "; expected " + hint
		}
		return []Problem{{Line: at.line, Path: at.path, Message: text}}

	case *kind.Enum:
		// A closed set of values, so the message is the set: an operator who
		// mistyped one needs to see what was on offer, and there is no better
		// place to learn it than the complaint itself. The value is quoted for
		// the same reason a pattern failure quotes one, and withheld for a
		// secret for the same reason too.
		text := "must be " + orJoin(k.Want)
		if !secretFields[fieldShape(at.path)] {
			text = fmt.Sprintf("%s %s", literal(k.Got), text)
		}
		return []Problem{{Line: at.line, Path: at.path, Message: text}}

	case *kind.MinLength:
		if k.Want == 1 {
			return []Problem{{Line: at.line, Path: at.path, Message: "must not be empty"}}
		}
		return []Problem{{Line: at.line, Path: at.path, Message: fmt.Sprintf(
			"must be at least %d characters", k.Want)}}

	// Length failures name the bound and never the value, so they are safe to
	// report about a passphrase without any of the care the Pattern case above
	// needs.
	case *kind.MaxLength:
		return []Problem{{Line: at.line, Path: at.path, Message: fmt.Sprintf(
			"must be at most %d characters", k.Want)}}

	case *kind.Minimum:
		return []Problem{{Line: at.line, Path: at.path, Message: fmt.Sprintf(
			"must be at least %s", k.Want.RatString())}}

	case *kind.Maximum:
		return []Problem{{Line: at.line, Path: at.path, Message: fmt.Sprintf(
			"must be at most %s", k.Want.RatString())}}

	default:
		return []Problem{{Line: at.line, Path: at.path, Message: invalid.ErrorKind.LocalizedString(printer)}}
	}
}

// patternHints supply the one thing a `pattern` failure cannot: what the
// operator should have typed. Keyed by field path with array indices stripped.
// A pattern with no hint still reports the offending value, just without the
// example — so adding a pattern to the schema can never break this file.
var patternHints = map[string]string{
	"networks.subnet":             "a gateway address and prefix length, e.g. 10.20.0.1/24",
	"wlans.passphrase":            "printable ASCII only — no accented letters, tabs or emoji",
	"port-forwards.forward-ip":    "the address of the host the traffic is sent to, e.g. 10.20.0.10",
	"port-forwards.source":        "`any`, or an address, range or CIDR block the traffic must come from, e.g. 203.0.113.4 or 203.0.113.0/24",
	"dhcp-reservations.mac":       "the client's MAC address as the Controller writes it: six hex pairs separated by colons, e.g. 00:1a:2b:3c:4d:5e",
	"dhcp-reservations.ip":        "the address to reserve for that client, e.g. 10.20.0.50",
	"wan.slot":                    "the Controller's own name for an uplink, such as WAN, WAN2 or WAN_LTE_FAILOVER",
	"wan.username":                "the username as the ISP issued it, with no quotes or spaces",
	"wan.password":                "no quotes or spaces — the Controller refuses them",
	"encrypted-dns.servers.stamp": "a DNS stamp: the single sdns:// string the resolver's own dashboard gives you",
}

// secretFields are the fields whose value must never appear in a message,
// keyed the same way as patternHints. Nothing here changes what is *accepted*;
// it changes only what a complaint is allowed to quote.
//
// It is a list rather than something read off the schema because JSON Schema
// has no way to say "secret" — and the consequence of the two drifting is bad
// enough in one direction only: a field added here that is not secret loses a
// little detail from one message, while a secret left out of it is printed.
var secretFields = map[string]bool{
	"wlans.passphrase":            true,
	"wan.password":                true,
	"encrypted-dns.servers.stamp": true,
}

// orJoin lists the values a closed field accepts the way they would be read
// aloud: `"dhcp", "pppoe" or "disabled"`.
func orJoin(values []any) string {
	literals := make([]string, 0, len(values))
	for _, value := range values {
		literals = append(literals, literal(value))
	}
	if len(literals) < 2 {
		return strings.Join(literals, "")
	}
	return strings.Join(literals[:len(literals)-1], ", ") + " or " + literals[len(literals)-1]
}

// literal renders a value as the operator would have typed it: quoted for text,
// bare for anything else, so a message about a number does not invent quotes
// around it.
func literal(value any) string {
	if text, ok := value.(string); ok {
		return fmt.Sprintf("%q", text)
	}
	return fmt.Sprint(value)
}

var arrayIndex = regexp.MustCompile(`\[\d+\]`)

// fieldShape turns `networks[1].subnet` into `networks.subnet`: which field it
// is, independent of which entry.
func fieldShape(path string) string {
	return arrayIndex.ReplaceAllString(path, "")
}
