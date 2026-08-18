// Package compat is unifig's version-compatibility promise, in the two forms
// it takes.
//
// One is configuration: compatibility.yaml says which dockerized Controller
// versions CI boots the process-level suite against, and which areas the
// published table has a row for. Adding a version to the matrix is a line in
// that file and nothing else, which is the whole point of it being read here
// rather than written into the rig, the workflow and the table separately.
//
// The other is evidence: matrix.json beside this file is generated from what
// those runs actually did (tools/compat), embedded into the binary, and is
// what decides whether the Controller in front of an operator is one unifig has
// been tested against. Nothing in this package can make that claim on its own.
package compat

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is compatibility.yaml.
type Config struct {
	// Floor is the oldest Controller version the promise reaches, e.g. "10.0".
	Floor     string    `yaml:"floor"`
	Container Container `yaml:"container"`
	// Versions are the Controller versions CI boots, newest first.
	Versions []string `yaml:"versions"`
	// Areas are the rows of the published table.
	Areas []Area `yaml:"areas"`
}

// Container is the recipe for one dockerized Controller: the image the suite
// boots, and the database the Network application needs beside it.
type Container struct {
	Image    string `yaml:"image"`
	Database string `yaml:"database"`
}

// Area is one row of the table, and the tests that are the evidence for it.
// Tests names a file in e2e/ rather than a list of test names, because the
// generator reads which tests are in it — a row cannot go stale as tests are
// added to the file it points at.
type Area struct {
	Name  string `yaml:"name"`
	Tests string `yaml:"tests"`
	// Why an area cannot be tested against a container, for the areas that
	// cannot. It is published beside the row, so that an area missing from the
	// version columns reads as a stated limit rather than as an oversight. The
	// generator requires it exactly where it applies and refuses it elsewhere,
	// since which areas those are is not this file's to decide.
	Why string `yaml:"why,omitempty"`
}

// LoadConfig reads compatibility.yaml and refuses anything the table would go
// on to publish as a promise unifig has not made: a version below the floor, a
// matrix that is not newest-first, the same version or the same tests twice.
//
// It is deliberately strict about unknown keys. A misspelled one would
// otherwise be a silently ignored version, and a version nobody notices is
// missing is exactly the failure this file exists to prevent.
func LoadConfig(path string) (Config, error) {
	body, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("reading the compatibility configuration: %w", err)
	}
	defer func() { _ = body.Close() }()

	decoder := yaml.NewDecoder(body)
	decoder.KnownFields(true)

	var cfg Config
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("%s is not valid: %w", path, err)
	}
	if err := cfg.validate(path); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) validate(path string) error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%s: %s", path, fmt.Sprintf(format, args...))
	}

	floor, ok := parseRelease(c.Floor)
	if !ok {
		return fail("floor %q is not a Controller version", c.Floor)
	}
	if c.Container.Image == "" {
		return fail("container.image says which image the suite boots and is not set")
	}
	if c.Container.Database == "" {
		return fail("container.database says which database the Network application gets and is not set")
	}
	if len(c.Versions) == 0 {
		return fail("versions is empty, so CI would run the suite against no Controller at all")
	}

	seen := map[string]bool{}
	var previous release
	for i, version := range c.Versions {
		parsed, ok := parseRelease(version)
		if !ok {
			return fail("version %q is not a Controller version", version)
		}
		if parsed.compare(floor) < 0 {
			return fail("version %q is below the floor of %s, so it is not part of the promise", version, c.Floor)
		}
		if seen[version] {
			return fail("version %q is listed twice", version)
		}
		seen[version] = true
		if i > 0 && parsed.compare(previous) > 0 {
			return fail("versions are listed oldest first; list them newest first, because the first one is what `make e2e` boots")
		}
		previous = parsed
	}

	if len(c.Areas) == 0 {
		return fail("areas is empty, so the published table would have no rows")
	}
	names, files := map[string]bool{}, map[string]bool{}
	for _, area := range c.Areas {
		switch {
		case area.Name == "":
			return fail("an area has no name")
		case area.Tests == "":
			return fail("the area %q names no tests", area.Name)
		case !strings.HasSuffix(area.Tests, "_test.go"):
			return fail("the area %q names %q, which is not a test file", area.Name, area.Tests)
		case names[area.Name]:
			return fail("two areas are called %q", area.Name)
		case files[area.Tests]:
			return fail("two areas name the tests in %q, which would publish one result twice", area.Tests)
		}
		names[area.Name], files[area.Tests] = true, true
	}
	return nil
}

// Newest is the version at the top of the matrix — what a bare `make e2e`
// boots, so that the everyday loop runs against the newest Controller the
// promise covers.
func (c Config) Newest() string {
	if len(c.Versions) == 0 {
		return ""
	}
	return c.Versions[0]
}

// ControllerImage is the image reference for one version of the matrix.
func (c Config) ControllerImage(version string) string {
	return c.Container.Image + ":" + version
}
