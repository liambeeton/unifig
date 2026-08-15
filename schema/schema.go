// Package schema publishes unifig's JSON Schema for unifig.yaml.
//
// The schema lives at the repo root rather than inside internal/ because it is
// a released artifact in its own right: editors fetch it by URL to autocomplete
// and check a config file as the operator types. This package exists only so
// that `unifig validate` embeds the very same bytes — the tool and the editor
// cannot disagree about what a valid config is, because there is one file.
package schema

import _ "embed"

// URL is where the schema is published: the value of its own `$id`, and what a
// unifig.yaml points its editor at.
const URL = "https://raw.githubusercontent.com/liambeeton/unifig/main/schema/unifig.schema.json"

// JSON is the schema document, embedded at build time.
//
//go:embed unifig.schema.json
var JSON []byte
