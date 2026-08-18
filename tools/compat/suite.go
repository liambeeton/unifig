package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// startReplay is the e2e suite's own name for "this test's Controller is the
// recording rather than the container". A test that calls it is testing
// against a UDR's recorded responses (ADR-0008, ADR-0013), which is one
// Controller version and not the one the container is running — so the table
// cannot put it in a version column, and reading the call is how the generator
// knows without being told.
const startReplay = "startReplay"

// suite is what the e2e package holds: which tests are in which file, and
// which of them talk to the recording.
type suite struct {
	// testsByFile maps a file name (no directory) to its top-level tests,
	// sorted.
	testsByFile map[string][]string
	// replayed names the tests whose Controller is the recording.
	replayed map[string]bool
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
	found := suite{testsByFile: map[string][]string{}, replayed: map[string]bool{}}
	// Every package-level function in the suite, tests and helpers alike, so
	// that reaching the recording through a helper counts as reaching it.
	bodies := map[string]*ast.BlockStmt{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, entry.Name()), nil, 0)
		if err != nil {
			return suite{}, fmt.Errorf("reading %s: %w", entry.Name(), err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Body == nil {
				continue
			}
			if isTest(fn) {
				found.testsByFile[entry.Name()] = append(found.testsByFile[entry.Name()], fn.Name.Name)
			}
			bodies[fn.Name.Name] = fn.Body
		}
	}

	// Which functions reach the recording, helpers included. A test that got
	// its stand-in from a helper rather than calling startReplay itself would
	// otherwise be read as a container test and published with version columns
	// — a row claiming a Controller version CI never ran it against, which is
	// the one thing the table must not do.
	reaching := reachingReplay(bodies)
	for name := range bodies {
		if reaching[name] && isTestName(name) {
			found.replayed[name] = true
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

// reachingReplay is every function that reaches startReplay, directly or
// through any number of helpers. It is a fixed point rather than one hop: a
// helper that calls a helper that starts the stand-in is still a test whose
// Controller is the recording.
func reachingReplay(bodies map[string]*ast.BlockStmt) map[string]bool {
	reaching := map[string]bool{startReplay: true}
	for grew := true; grew; {
		grew = false
		for name, body := range bodies {
			if reaching[name] {
				continue
			}
			for reached := range reaching {
				if calls(body, reached) {
					reaching[name] = true
					grew = true
					break
				}
			}
		}
	}
	delete(reaching, startReplay)
	return reaching
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
// reachingReplay's results can be checked against.
func isTestName(name string) bool {
	return strings.HasPrefix(name, "Test") && name != "TestMain"
}

// calls says whether a function body calls the named function anywhere in
// itself, including inside the subtests it declares.
func calls(body *ast.BlockStmt, name string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == name {
			found = true
			return false
		}
		return true
	})
	return found
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
