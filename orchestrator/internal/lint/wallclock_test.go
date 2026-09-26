// Package lint holds build-time checks that are expressed as tests so they run in
// the ordinary `go test ./...` path and block CI without extra tooling.
package lint

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The directive that permits a wall-clock read. It must be accompanied by a
// reason on the same line, so the escape hatch is self-documenting and shows up in
// review rather than spreading quietly.
const allowDirective = "//acresync:allow-wallclock"

// Functions that answer "what time is it now". Reading one of these for a business
// decision is the defect this check exists to prevent: the demo compresses a
// thirty-day offer window into seconds, and any deadline evaluated against the
// host clock silently behaves differently in a real deployment.
//
// Deliberately excluded: time.After, time.Tick, time.NewTimer, time.Sleep. Those
// express a duration rather than a point in time and are correct for transport
// timeouts and backoff, which have nothing to do with business deadlines.
var forbidden = map[string]string{
	"Now":   "use clock.Business.Now for business time, or clock.Real() if wall time is genuinely intended",
	"Since": "compute elapsed business time from two clock reads instead",
	"Until": "compute remaining business time from two clock reads instead",
}

// Packages exempt from the check.
//
// internal/clock is the implementation of the abstraction, so it must be able to
// read the wall clock. internal/lint is this checker. Nothing else is listed: the
// directive is the mechanism for a one-off exception, because a package-level
// exemption tends to outlive the reason for it.
var exemptPackages = map[string]bool{
	"internal/clock": true,
	"internal/lint":  true,
}

type violation struct {
	file string
	line int
	fn   string
	hint string
}

func TestNoWallClockInBusinessPackages(t *testing.T) {
	root, err := moduleRoot()
	if err != nil {
		t.Fatal(err)
	}

	var violations []violation
	var scanned int

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "vendor", "testdata", ".git", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		pkgDir := filepath.ToSlash(filepath.Dir(rel))
		if exemptPackages[pkgDir] {
			return nil
		}

		scanned++
		v, scanErr := scanFile(path, rel)
		if scanErr != nil {
			return scanErr
		}
		violations = append(violations, v...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if scanned == 0 {
		t.Fatal("scanned zero files; the walk root or the module layout changed and this check is no longer checking anything")
	}
	t.Logf("scanned %d non-test Go files outside exempt packages", scanned)

	if len(violations) == 0 {
		return
	}

	sort.Slice(violations, func(i, j int) bool {
		if violations[i].file != violations[j].file {
			return violations[i].file < violations[j].file
		}
		return violations[i].line < violations[j].line
	})

	var b strings.Builder
	fmt.Fprintf(&b, "%d wall-clock read(s) in business code:\n", len(violations))
	for _, v := range violations {
		fmt.Fprintf(&b, "\n  %s:%d  time.%s\n      %s", v.file, v.line, v.fn, v.hint)
	}
	b.WriteString("\n\nIf a wall-clock read is genuinely correct here, add a directive with a reason:\n")
	b.WriteString("      " + allowDirective + " <why wall time is correct at this call site>")
	t.Error(b.String())
}

func scanFile(path, rel string) ([]violation, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", rel, err)
	}

	// Resolve the local name of the time import. Checking for a bare identifier
	// called "time" would produce a false positive on any local variable of that
	// name, and would miss an aliased import entirely.
	timeNames := map[string]bool{}
	for _, imp := range file.Imports {
		p, uerr := strconv.Unquote(imp.Path.Value)
		if uerr != nil || p != "time" {
			continue
		}
		if imp.Name != nil {
			if imp.Name.Name == "_" || imp.Name.Name == "." {
				continue
			}
			timeNames[imp.Name.Name] = true
		} else {
			timeNames["time"] = true
		}
	}
	if len(timeNames) == 0 {
		return nil, nil
	}

	// Lines carrying the directive. A directive covers its own line and the line
	// immediately following, which is the usual shape for annotating a call.
	allowed := map[int]bool{}
	for _, group := range file.Comments {
		for _, c := range group.List {
			if !strings.Contains(c.Text, allowDirective) {
				continue
			}
			line := fset.Position(c.Pos()).Line
			allowed[line] = true
			allowed[line+1] = true
		}
	}

	var out []violation
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkgIdent, ok := sel.X.(*ast.Ident)
		if !ok || !timeNames[pkgIdent.Name] {
			return true
		}
		hint, forbiddenFn := forbidden[sel.Sel.Name]
		if !forbiddenFn {
			return true
		}
		line := fset.Position(sel.Pos()).Line
		if allowed[line] {
			return true
		}
		out = append(out, violation{
			file: filepath.ToSlash(rel),
			line: line,
			fn:   sel.Sel.Name,
			hint: hint,
		})
		return true
	})
	return out, nil
}

// TestDirectiveMechanismWorks guards the checker itself.
//
// A linter that silently stops detecting anything is worse than no linter, because
// it reports success. This verifies both halves: a plain call is caught, and a
// call carrying the directive is not.
func TestDirectiveMechanismWorks(t *testing.T) {
	dir := t.TempDir()

	caught := filepath.Join(dir, "caught.go")
	if err := os.WriteFile(caught, []byte(`package x

import "time"

func f() time.Time { return time.Now() }
`), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := scanFile(caught, "caught.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].fn != "Now" {
		t.Fatalf("expected exactly one Now violation, got %#v", got)
	}

	exempt := filepath.Join(dir, "exempt.go")
	if err := os.WriteFile(exempt, []byte(`package x

import "time"

func f() time.Time {
	//acresync:allow-wallclock audit metadata, not a business deadline
	return time.Now()
}
`), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err = scanFile(exempt, "exempt.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("directive should have suppressed the violation, got %#v", got)
	}

	// An aliased import must still be detected.
	aliased := filepath.Join(dir, "aliased.go")
	if err := os.WriteFile(aliased, []byte(`package x

import clk "time"

func f() clk.Time { return clk.Now() }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = scanFile(aliased, "aliased.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("aliased time import should still be detected, got %#v", got)
	}

	// A local identifier named time must not produce a false positive.
	shadow := filepath.Join(dir, "shadow.go")
	if err := os.WriteFile(shadow, []byte(`package x

import "strings"

type fake struct{}

func (fake) Now() string { return "" }

func f() string {
	time := fake{}
	return strings.TrimSpace(time.Now())
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = scanFile(shadow, "shadow.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("a local variable named time must not be flagged, got %#v", got)
	}

	// Duration-shaped helpers are permitted.
	durations := filepath.Join(dir, "durations.go")
	if err := os.WriteFile(durations, []byte(`package x

import "time"

func f() <-chan time.Time { return time.After(5 * time.Second) }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = scanFile(durations, "durations.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("time.After expresses a duration and must be permitted, got %#v", got)
	}
}

func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for range 8 {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("lint: could not locate go.mod above %s", dir)
}
