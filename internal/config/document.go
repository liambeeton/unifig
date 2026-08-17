package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// parse reads the file's single YAML document and returns its root node.
//
// "Single" is the part worth stating: yaml.Unmarshal would quietly read the
// first document and discard the rest, so an operator who reached for `---` to
// separate their sections would have had most of their config ignored without
// a word.
func parse(source []byte) (*yaml.Node, []Problem) {
	decoder := yaml.NewDecoder(bytes.NewReader(source))

	var doc yaml.Node
	if err := decoder.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, []Problem{emptyFile()}
		}
		return nil, []Problem{yamlSyntaxProblem(err)}
	}
	if len(doc.Content) == 0 {
		return nil, []Problem{emptyFile()}
	}

	var extra yaml.Node
	if err := decoder.Decode(&extra); err == nil {
		return nil, []Problem{{
			Line:    extra.Line,
			Message: "a config file holds one YAML document; remove the `---` separator and merge these sections",
		}}
	}

	return doc.Content[0], nil
}

// emptyFile reports a file with no YAML in it at all — blank, or nothing but
// comments. That is deliberately not the same as `{}` or `networks: []`, which
// manage nothing too but say so on purpose; a file with nothing in it is far
// more likely to be one the operator has not finished writing.
func emptyFile() Problem {
	return Problem{Message: "file is empty — see examples/unifig.yaml for a starting point"}
}

// document is a parsed config file walked exactly once: the plain JSON value
// the schema validator understands, an index from any part of that value back
// to the line it came from, and a record of what the environment supplied.
//
// These three travel together because they are made together. Walking the tree
// twice — once to interpolate, once to convert — would mean two places
// deciding what `networks[1].subnet` is called, and the moment they disagreed
// the index lookups would start quietly missing.
type document struct {
	// instance is the document as plain JSON values, for schema validation.
	instance any
	// index maps an RFC 6901 JSON pointer to where that value sits in the
	// file. The schema validator reports failures against a JSON document
	// that has no idea it came from YAML; this is what turns
	// "/networks/1/subnet" back into "line 7: networks[1].subnet".
	index index
	// interpolated holds the operator-facing paths whose value came from the
	// environment, so a later complaint about one can say where it came from.
	interpolated map[string]bool
	// missing is one Problem per `${VAR}` reference that resolved to nothing.
	missing []Problem
}

// resolve walks the document, substituting every `${ENV_VAR}` reference into
// the tree in place and converting it to JSON values as it goes.
//
// A missing variable is a validation error rather than an empty string: a
// config that silently half-applies because a secret was not exported is
// exactly the failure this tool exists to prevent.
//
// Three rules keep interpolation predictable:
//
//   - Values only, never keys. The shape of the document is the operator's,
//     not the environment's.
//   - The result is always text. `${VLAN}` cannot become the integer 20 —
//     the common use is secrets, and a passphrase of "0900" or "true" turning
//     into a number or a boolean is a far nastier surprise than the schema
//     type error the alternative produces.
//   - One pass. Substituted values are never rescanned, so a secret that
//     itself contains `${...}` is inserted verbatim and cannot inject a
//     further reference.
func resolve(root *yaml.Node) document {
	doc := document{index: index{}, interpolated: map[string]bool{}}

	// An anchor makes one node reachable from several places. Expanding it
	// twice would be harmless — the reference is gone after the first pass —
	// but an *unresolved* one would be reported once per use, so each node is
	// expanded when first reached and reused thereafter.
	expanded := map[*yaml.Node]bool{}
	fromEnv := map[*yaml.Node]bool{}

	var walk func(node *yaml.Node, pointer, path string) any
	walk = func(node *yaml.Node, pointer, path string) any {
		doc.index[pointer] = location{line: node.Line, path: path}

		switch node.Kind {
		case yaml.AliasNode:
			value := walk(node.Alias, pointer, path)
			// Report the use of an anchor at the line that uses it, not at
			// the line that defined it.
			doc.index[pointer] = location{line: node.Line, path: path}
			return value

		case yaml.MappingNode:
			out := make(map[string]any, len(node.Content)/2)
			for i := 0; i+1 < len(node.Content); i += 2 {
				key := node.Content[i].Value
				out[key] = walk(node.Content[i+1],
					pointer+"/"+pointerEscaper.Replace(key), childPath(path, key))
			}
			return out

		case yaml.SequenceNode:
			out := make([]any, 0, len(node.Content))
			for i, item := range node.Content {
				out = append(out, walk(item, pointer+"/"+strconv.Itoa(i), indexPath(path, i)))
			}
			return out

		case yaml.ScalarNode:
			if !expanded[node] {
				expanded[node] = true
				substituted, missing := expand(node.Value)
				for _, name := range missing {
					doc.missing = append(doc.missing, Problem{
						Line: node.Line,
						Path: path,
						Message: fmt.Sprintf(
							"${%s} is not set in the environment; export %s before running unifig", name, name),
					})
				}
				if substituted != node.Value {
					node.Value = substituted
					node.Tag = "!!str"
					node.Style = 0
					fromEnv[node] = true
				}
			}
			if fromEnv[node] {
				doc.interpolated[path] = true
			}
			return scalarValue(node)
		}

		return nil
	}

	doc.instance = walk(root, "", "")
	return doc
}

// placeholder matches a `${NAME}` reference. The name is deliberately narrow —
// the shell's own variable-name alphabet — so that anything else containing a
// brace stays literal text rather than becoming a reference by accident.
var placeholder = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expand substitutes every resolvable reference in s and returns the names of
// those that could not be resolved, left in place so the operator sees the
// reference they wrote rather than a hole where it used to be.
func expand(s string) (string, []string) {
	var missing []string
	expanded := placeholder.ReplaceAllStringFunc(s, func(match string) string {
		name := match[2 : len(match)-1]
		value, ok := os.LookupEnv(name)
		if !ok {
			missing = append(missing, name)
			return match
		}
		return value
	})
	return expanded, missing
}

// scalarValue resolves a YAML scalar to the JSON value it stands for, falling
// back to its text for tags JSON has no equivalent of. Anything the schema
// then rejects on type is reported against what the operator can see in the
// file, which is the text.
func scalarValue(node *yaml.Node) any {
	switch node.Tag {
	case "!!null":
		return nil
	case "!!bool":
		var b bool
		if node.Decode(&b) == nil {
			return b
		}
	case "!!int":
		var i int
		if node.Decode(&i) == nil {
			return i
		}
		fallthrough
	case "!!float":
		var f float64
		if node.Decode(&f) == nil {
			return f
		}
	}
	return node.Value
}

// location is where a value in the document sits, in both the terms the schema
// validator speaks (a line to point at) and the terms an operator does.
type location struct {
	line int
	path string
}

type index map[string]location

func (i index) at(instanceLocation []string) location {
	if at, ok := i[jsonPointer(instanceLocation)]; ok {
		return at
	}
	// Only reachable if a value was validated that the walk never saw.
	return location{path: strings.Join(instanceLocation, ".")}
}

// field locates one field of one entry, e.g. wlans[2].network — how the
// cross-reference checks ask where something was.
func (i index) field(section string, entry int, name string) location {
	return i.at([]string{section, strconv.Itoa(entry), name})
}

// nestedField is the same for a list that sits inside a section rather than at
// the top of the file, e.g. encrypted-dns.servers[0].name. Both go through at,
// so there is one place that knows how a location is addressed.
func (i index) nestedField(section, list string, entry int, name string) location {
	return i.at([]string{section, list, strconv.Itoa(entry), name})
}

func jsonPointer(tokens []string) string {
	var b strings.Builder
	for _, token := range tokens {
		b.WriteByte('/')
		b.WriteString(pointerEscaper.Replace(token))
	}
	return b.String()
}

var pointerEscaper = strings.NewReplacer("~", "~0", "/", "~1")

// childPath and indexPath are the only two places an operator-facing path is
// built, so every message everywhere spells `networks[1].subnet` the same way.
func childPath(parent, key string) string {
	if parent == "" {
		return key
	}
	return parent + "." + key
}

func indexPath(parent string, i int) string {
	return fmt.Sprintf("%s[%d]", parent, i)
}

// yamlSyntaxProblem unwraps a parse failure into unifig's own reporting shape:
// yaml.v3 already names the line, in a message of the form
// "yaml: line 4: did not find expected key".
func yamlSyntaxProblem(err error) Problem {
	text := strings.TrimPrefix(err.Error(), "yaml: ")
	problem := Problem{Message: "not valid YAML: " + text}
	if match := yamlLinePrefix.FindStringSubmatch(text); match != nil {
		if line, err := strconv.Atoi(match[1]); err == nil {
			problem.Line = line
			problem.Message = "not valid YAML: " + strings.TrimPrefix(text, match[0])
		}
	}
	return problem
}

var yamlLinePrefix = regexp.MustCompile(`^line (\d+): `)
