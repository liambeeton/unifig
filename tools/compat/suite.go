package main

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/liambeeton/unifig/internal/compat"
)

// startReplay is the e2e suite's own name for "this test's Controller is the
// recording rather than the container". A test that calls it is testing
// against a UDR's recorded responses (ADR-0008, ADR-0013), which is one
// Controller version and not the one the container is running — so the table
// cannot put it in a version column, and reading the call is how the generator
// knows without being told.
const startReplay = "startReplay"

// measuredOn is the suite's name for the other half of that: "what this test
// asserts was measured on a Controller neither the container nor the recording
// is". A recording holds `GET`s, so a replayed test about a *write* rests on a
// live session against whatever firmware the router was running that day, and
// where that is not the recording's own version the row would otherwise
// attribute it to a Controller nobody asked (ADR-0036).
//
// It is read out of the source for the reason startReplay is: a list in
// configuration would be a claim the suite could stop supporting without
// anything noticing.
const measuredOn = "measuredOn"

// measurementType is the type a declaration cited that way has. The generator
// reads the literal rather than running anything, so the fields have to be
// string literals — which they are, because they are prose the table prints.
const measurementType = "measurement"

// suite is what the e2e package holds: which tests are in which file, which of
// them talk to the recording, and what any of them rest on that neither
// Controller answered for.
type suite struct {
	// testsByFile maps a file name (no directory) to its top-level tests,
	// sorted.
	testsByFile map[string][]string
	// replayed names the tests whose Controller is the recording.
	replayed map[string]bool
	// measured maps a test to the measurements it cites, directly or through a
	// helper.
	measured map[string][]compat.Measurement
}

// readSuite reads the tests out of the e2e package's source. It is the source
// rather than a list in configuration because a list would go stale the first
// time somebody adds a test, and the whole point of the table is that its rows
// say what ran.
func readSuite(dir string) (suite, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return suite{}, fmt.Errorf("reading the %s suite: %w", dir, err)
	}

	fset := token.NewFileSet()
	found := suite{
		testsByFile: map[string][]string{},
		replayed:    map[string]bool{},
		measured:    map[string][]compat.Measurement{},
	}
	// Every package-level function in the suite, tests and helpers alike, so
	// that reaching the recording through a helper counts as reaching it.
	bodies := map[string]*ast.BlockStmt{}
	// Every package-level measurement, by the name tests cite it under.
	declared := map[string]compat.Measurement{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, entry.Name()), nil, 0)
		if err != nil {
			return suite{}, fmt.Errorf("reading %s: %w", entry.Name(), err)
		}
		for _, decl := range file.Decls {
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				if decl.Recv != nil || decl.Body == nil {
					continue
				}
				if isTest(decl) {
					found.testsByFile[entry.Name()] = append(found.testsByFile[entry.Name()], decl.Name.Name)
				}
				bodies[decl.Name.Name] = decl.Body
			case *ast.GenDecl:
				if err := readMeasurements(decl, declared); err != nil {
					return suite{}, fmt.Errorf("reading %s: %w", entry.Name(), err)
				}
			}
		}
	}

	graph := newCallGraph(bodies)

	// Which functions reach the recording, helpers included. A test that got
	// its stand-in from a helper rather than calling startReplay itself would
	// otherwise be read as a container test and published with version columns
	// — a row claiming a Controller version CI never ran it against, which is
	// the one thing the table must not do.
	reaching := reaches(graph, startReplay)
	for name := range bodies {
		if reaching[name] && isTestName(name) {
			found.replayed[name] = true
		}
	}

	cited, err := citations(bodies, declared)
	if err != nil {
		return suite{}, err
	}
	spreadCitations(graph, cited)
	for name, measurements := range cited {
		if !isTestName(name) {
			continue
		}
		for _, cite := range measurements {
			found.measured[name] = append(found.measured[name], declared[cite])
		}
	}

	// A declaration nothing rests on is a claim the table would print with no
	// test behind it — the same failure as a test file no area names, which
	// `agree` refuses for the same reason.
	for _, name := range slices.Sorted(maps.Keys(declared)) {
		if !citedAnywhere(cited, name) {
			return suite{}, fmt.Errorf("the measurement %s is declared and no test cites it, so the table would"+
				" publish a fact nothing rests on: cite it with %s, or delete it", name, measuredOn)
		}
	}

	for _, tests := range found.testsByFile {
		slices.Sort(tests)
	}
	if len(found.testsByFile) == 0 {
		return suite{}, fmt.Errorf("%s holds no tests", dir)
	}
	return found, nil
}

// readMeasurements reads `var x = measurement{version: "…", what: "…"}` out of
// a declaration, and refuses one it cannot read rather than skipping it: a
// declaration the generator quietly ignored is an exception the table would not
// carry.
func readMeasurements(decl *ast.GenDecl, into map[string]compat.Measurement) error {
	if decl.Tok != token.VAR {
		return nil
	}
	for _, spec := range decl.Specs {
		value, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		for i, name := range value.Names {
			var assigned ast.Expr
			if i < len(value.Values) {
				assigned = value.Values[i]
			}
			// Anything naming the type is a measurement somebody meant, and the
			// only shape this reads is a plain composite literal. A slice of
			// them, a pointer to one, a zero value — each would otherwise be
			// dropped in silence, and a dropped declaration is a row published
			// without the exception it was written to carry.
			if !mentions(measurementType, value.Type, assigned) {
				continue
			}
			literal, ok := assigned.(*ast.CompositeLit)
			if !ok {
				return fmt.Errorf("the measurement %s is declared in a shape this generator does not read;"+
					" write it as `var %s = %s{version: \"…\", what: \"…\"}`", name.Name, name.Name, measurementType)
			}
			if ident, ok := literal.Type.(*ast.Ident); !ok || ident.Name != measurementType {
				return fmt.Errorf("the measurement %s is declared as a collection or an alias of %s, and this"+
					" generator reads one declaration per fact; write it as `var %s = %s{version: \"…\","+
					" what: \"…\"}`", name.Name, measurementType, name.Name, measurementType)
			}
			measurement, err := readMeasurement(literal)
			if err != nil {
				return fmt.Errorf("the measurement %s: %w", name.Name, err)
			}
			into[name.Name] = measurement
		}
	}
	return nil
}

func readMeasurement(literal *ast.CompositeLit) (compat.Measurement, error) {
	var measurement compat.Measurement
	for _, element := range literal.Elts {
		field, ok := element.(*ast.KeyValueExpr)
		if !ok {
			return compat.Measurement{}, errors.New("names its fields positionally; name them, so that the" +
				" table cannot print the version under `What was measured` or the other way about")
		}
		key, ok := field.Key.(*ast.Ident)
		if !ok {
			return compat.Measurement{}, errors.New("has a field this generator cannot read")
		}
		text, err := stringLiteral(field.Value)
		if err != nil {
			return compat.Measurement{}, fmt.Errorf("field %s %w", key.Name, err)
		}
		switch key.Name {
		case "version":
			measurement.Version = text
		case "what":
			measurement.What = text
		default:
			return compat.Measurement{}, fmt.Errorf("has a field %q this generator does not publish", key.Name)
		}
	}
	switch {
	case measurement.Version == "":
		return compat.Measurement{}, errors.New("does not say which Controller it was measured on")
	case measurement.What == "":
		return compat.Measurement{}, errors.New("does not say what was measured")
	}
	return measurement, nil
}

// stringLiteral is the only value either field may hold — literals, joined with
// `+` as this codebase writes every other paragraph it has to keep inside a
// line length. Anything computed would be a phrase the table printed that
// nobody could read out of the suite.
func stringLiteral(expr ast.Expr) (string, error) {
	switch expr := expr.(type) {
	case *ast.BasicLit:
		if expr.Kind != token.STRING {
			break
		}
		text, err := strconv.Unquote(expr.Value)
		if err != nil {
			return "", fmt.Errorf("cannot be read: %w", err)
		}
		return text, nil
	case *ast.BinaryExpr:
		if expr.Op != token.ADD {
			break
		}
		left, err := stringLiteral(expr.X)
		if err != nil {
			return "", err
		}
		right, err := stringLiteral(expr.Y)
		if err != nil {
			return "", err
		}
		return left + right, nil
	}
	return "", errors.New("is not a string literal, so the table would print something the suite does not say")
}

// mentions says whether any of these expressions names the given type, which is
// how a declaration meant as a measurement is told from every other `var` in the
// suite before its shape is judged.
func mentions(name string, exprs ...ast.Expr) bool {
	found := false
	for _, expr := range exprs {
		if expr == nil {
			continue
		}
		ast.Inspect(expr, func(n ast.Node) bool {
			if ident, ok := n.(*ast.Ident); ok && ident.Name == name {
				found = true
			}
			return !found
		})
	}
	return found
}

// citations is which measurements each function cites directly, by the name it
// declared them under. A citation of a name nothing declares is refused rather
// than dropped: it is a test claiming provenance the suite cannot show.
func citations(bodies map[string]*ast.BlockStmt, declared map[string]compat.Measurement) (map[string][]string, error) {
	cited := map[string][]string{}
	for _, name := range slices.Sorted(maps.Keys(bodies)) {
		body := bodies[name]
		var err error
		ast.Inspect(body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || err != nil {
				return err == nil
			}
			if ident, ok := call.Fun.(*ast.Ident); !ok || ident.Name != measuredOn {
				return true
			}
			if len(call.Args) != 2 {
				err = fmt.Errorf("%s calls %s with %d arguments, want the test and one measurement",
					name, measuredOn, len(call.Args))
				return false
			}
			cite, ok := call.Args[1].(*ast.Ident)
			if !ok {
				err = fmt.Errorf("%s cites a measurement this generator cannot name; declare it as a package-level"+
					" %s and cite it by that name", name, measurementType)
				return false
			}
			if _, known := declared[cite.Name]; !known {
				err = fmt.Errorf("%s cites the measurement %s, which nothing declares", name, cite.Name)
				return false
			}
			if !slices.Contains(cited[name], cite.Name) {
				cited[name] = append(cited[name], cite.Name)
			}
			return false
		})
		if err != nil {
			return nil, err
		}
	}
	for _, cites := range cited {
		slices.Sort(cites)
	}
	return cited, nil
}

// callGraph is who calls whom among the suite's package-level functions.
type callGraph map[string][]string

func newCallGraph(bodies map[string]*ast.BlockStmt) callGraph {
	graph := make(callGraph, len(bodies))
	for name, body := range bodies {
		graph[name] = callees(body)
	}
	return graph
}

// reaches is every function that reaches target, directly or through any number
// of helpers, target itself excluded. It is a fixed point rather than one hop:
// a helper that calls a helper that starts the stand-in is still a test whose
// Controller is the recording.
func reaches(graph callGraph, target string) map[string]bool {
	reaching := map[string]bool{target: true}
	for grew := true; grew; {
		grew = false
		for name, called := range graph {
			if reaching[name] {
				continue
			}
			for _, callee := range called {
				if reaching[callee] {
					reaching[name], grew = true, true
					break
				}
			}
		}
	}
	delete(reaching, target)
	return reaching
}

// spreadCitations carries each function's citations up to everything that calls
// it, for the same reason `reaches` exists: a test that cites its measurement
// through a helper still rests on it.
func spreadCitations(graph callGraph, cited map[string][]string) {
	for grew := true; grew; {
		grew = false
		for name, called := range graph {
			for _, callee := range called {
				for _, cite := range cited[callee] {
					if !slices.Contains(cited[name], cite) {
						cited[name] = append(cited[name], cite)
						grew = true
					}
				}
			}
		}
	}
	for _, cites := range cited {
		slices.Sort(cites)
	}
}

func citedAnywhere(cited map[string][]string, name string) bool {
	for who, cites := range cited {
		if isTestName(who) && slices.Contains(cites, name) {
			return true
		}
	}
	return false
}

// isTest is Go's own rule for what `go test` will run, minus TestMain, which
// is the suite's plumbing rather than evidence about anything.
func isTest(fn *ast.FuncDecl) bool {
	if !isTestName(fn.Name.Name) {
		return false
	}
	if fn.Type.Params == nil || len(fn.Type.Params.List) != 1 {
		return false
	}
	param, ok := fn.Type.Params.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	selector, ok := param.X.(*ast.SelectorExpr)
	return ok && selector.Sel.Name == "T"
}

// isTestName is the half of isTest that a name alone answers, which is all
// `reaches` and the citations can be checked against.
func isTestName(name string) bool {
	return strings.HasPrefix(name, "Test") && name != "TestMain"
}

// callees is every package-level function a body calls by name, including
// inside the subtests it declares.
func callees(body *ast.BlockStmt) []string {
	var called []string
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if ident, ok := call.Fun.(*ast.Ident); ok && !slices.Contains(called, ident.Name) {
			called = append(called, ident.Name)
		}
		return true
	})
	return called
}

// files names every test file that holds at least one test, sorted.
func (s suite) files() []string {
	names := make([]string, 0, len(s.testsByFile))
	for name := range s.testsByFile {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
