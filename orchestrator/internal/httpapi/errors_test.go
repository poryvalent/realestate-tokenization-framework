package httpapi

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// domainPackagesWithSentinels are the packages whose errors reach a client over HTTP.
//
// Listed explicitly rather than discovered, because not every package's errors are answers to a request:
// a chain client's dial failure is an operational fault, not something to explain to a caller.
var domainPackagesWithSentinels = []string{
	"../offer",
	"../period",
	"../bidbook",
	"../settlement",
	"../adjustment",
}

// TestEveryDomainSentinelIsClassified is the reason errorClasses is a table.
//
// It reads the domain packages' source, collects every `ErrX = errors.New("...")`, and checks each message
// appears in the classification table. A sentinel missing from the table is not a compile error and not a
// visibly broken handler: it is a caller being told "internal error" when the system knew exactly what was
// wrong and why. That is the failure this catches.
//
// Matching is by message rather than by name because the table holds error values, and a value's message is
// the only thing a test can compare against source it has parsed.
func TestEveryDomainSentinelIsClassified(t *testing.T) {
	declared := scanSentinels(t)
	if len(declared) == 0 {
		t.Fatal("scanned zero sentinels; the package layout changed and this check is no longer checking anything")
	}

	classified := make(map[string]bool, len(errorClasses))
	for _, c := range errorClasses {
		classified[c.err.Error()] = true
	}

	var missing []string
	for msg, name := range declared {
		if !classified[msg] {
			missing = append(missing, fmt.Sprintf("%s (%q)", name, msg))
		}
	}
	sort.Strings(missing)

	if len(missing) > 0 {
		t.Errorf("%d domain sentinel(s) are not classified, so each one reaches a client as a bare 500:\n  %s\n\n"+
			"Add them to errorClasses in errors.go with the status and contract code that describe what they mean.",
			len(missing), strings.Join(missing, "\n  "))
	}
}

// TestClassificationTableHasNoDeadEntries catches the opposite drift.
//
// An entry whose sentinel no longer exists in the domain cannot be reached. Left in place it reads as
// coverage that is not there, which is how an unmapped sentinel hides.
func TestClassificationTableHasNoDeadEntries(t *testing.T) {
	declared := scanSentinels(t)

	for _, c := range errorClasses {
		if _, ok := declared[c.err.Error()]; !ok {
			t.Errorf("errorClasses maps %q, but no domain package declares that sentinel any more", c.err.Error())
		}
	}
}

// TestClassifyNeverReturnsInternalForADomainError asserts the behaviour the table is for.
//
// The table being complete and classify reading it correctly are two different claims. This one exercises
// classify itself, including that a wrapped sentinel is still recognised, which is how domain code actually
// returns these: fmt.Errorf("...: %w", ErrX).
func TestClassifyNeverReturnsInternalForADomainError(t *testing.T) {
	for _, c := range errorClasses {
		t.Run(c.err.Error(), func(t *testing.T) {
			wrapped := fmt.Errorf("while doing the thing: %w", c.err)

			status, code, msg := classify(wrapped)

			if status == http.StatusInternalServerError || code == CodeInternal {
				t.Fatalf("a wrapped domain sentinel classified as internal: status=%d code=%s", status, code)
			}
			if status != c.status || code != c.code {
				t.Fatalf("classify gave status=%d code=%s, table says status=%d code=%s",
					status, code, c.status, c.code)
			}
			// The domain's own words must survive, including the caller's context around them.
			if !strings.Contains(msg, c.err.Error()) {
				t.Fatalf("the message lost the domain's explanation: got %q, want it to contain %q", msg, c.err.Error())
			}
			if !strings.Contains(msg, "while doing the thing") {
				t.Fatalf("the message lost the wrapping context: got %q", msg)
			}
		})
	}
}

// TestClassifyRedactsUnrecognisedErrors is the other half of the contract.
//
// An error nobody has considered might carry a connection string or somebody's data, so its text must not
// reach a client.
func TestClassifyRedactsUnrecognisedErrors(t *testing.T) {
	secret := errors.New("dial tcp 10.0.0.5:5432: password=hunter2 database=acresync")

	status, code, msg := classify(secret)

	if status != http.StatusInternalServerError || code != CodeInternal {
		t.Fatalf("an unrecognised error should be a bare 500, got status=%d code=%s", status, code)
	}
	if strings.Contains(msg, "hunter2") || strings.Contains(msg, "10.0.0.5") {
		t.Fatalf("an unrecognised error's text leaked into the response: %q", msg)
	}
	if msg != "internal error" {
		t.Fatalf("want the fixed redacted message, got %q", msg)
	}
}

// TestStatusErrorsClassifyToTheirOwnStatus covers the request-level failures.
func TestStatusErrorsClassifyToTheirOwnStatus(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"notFound", notFound("offer"), http.StatusNotFound, CodeNotFound},
		{"badRequest", badRequest("offerId is not a uuid", errors.New("bad uuid")), http.StatusBadRequest, CodeValidationFailed},
		{"unauthorized", unauthorized("a bearer token is required"), http.StatusUnauthorized, CodeUnauthorized},
		{"forbidden", forbidden("this endpoint requires the TRUSTEE role"), http.StatusForbidden, CodeForbidden},
		{"conflict", conflict(CodeIdempotencyConflict, "this key was used with a different body"), http.StatusConflict, CodeIdempotencyConflict},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, code, msg := classify(tc.err)
			if status != tc.wantStatus || code != tc.wantCode {
				t.Fatalf("got status=%d code=%s, want status=%d code=%s", status, code, tc.wantStatus, tc.wantCode)
			}
			if msg == "" {
				t.Fatal("a status error must explain itself")
			}
		})
	}
}

// TestStatusErrorClassificationSurvivesWrapping guards the errors.As path.
//
// A handler that adds context with fmt.Errorf must not turn a 404 into a 500.
func TestStatusErrorClassificationSurvivesWrapping(t *testing.T) {
	wrapped := fmt.Errorf("loading the offer: %w", notFound("offer"))

	status, code, _ := classify(wrapped)

	if status != http.StatusNotFound || code != CodeNotFound {
		t.Fatalf("a wrapped status error lost its classification: status=%d code=%s", status, code)
	}
}

// TestEveryCodeInTheTableIsAContractCode stops the table inventing codes.
//
// The contract publishes a closed enum. A code that is not in it is a code no client can branch on, and
// the frontend would have to discover it from a failure in production.
func TestEveryCodeInTheTableIsAContractCode(t *testing.T) {
	published := map[string]bool{
		CodePreconditionFailed: true, CodeNotFeasible: true, CodeAnchorNotConfirmed: true,
		CodeCeremonyOrder: true, CodeUnitsIssued: true, CodeSettlementIncomplete: true,
		CodeValidationFailed: true, CodeIdempotencyConflict: true, CodePaused: true,
		CodeNotFound: true, CodeUnauthorized: true, CodeForbidden: true, CodeInternal: true,
	}

	for _, c := range errorClasses {
		if !published[c.code] {
			t.Errorf("errorClasses uses %q, which is not in the contract's error code enum", c.code)
		}
		if c.status < 400 || c.status > 599 {
			t.Errorf("%q maps to status %d, which is not an error status", c.err.Error(), c.status)
		}
	}
}

// scanSentinels parses the domain packages and returns message -> qualified name.
//
// Keyed by message because that is what the table can be compared against. A duplicate message across two
// packages would collide, so the test fails loudly on one rather than silently accepting half a mapping.
func scanSentinels(t *testing.T) map[string]string {
	t.Helper()

	found := make(map[string]string)

	for _, dir := range domainPackagesWithSentinels {
		fset := token.NewFileSet()
		pkgs, err := parser.ParseDir(fset, dir, func(fi fs.FileInfo) bool {
			return !strings.HasSuffix(fi.Name(), "_test.go")
		}, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", dir, err)
		}
		if len(pkgs) == 0 {
			t.Fatalf("no package found in %s; the layout changed", dir)
		}

		pkgName := filepath.Base(dir)

		for _, pkg := range pkgs {
			for _, file := range pkg.Files {
				ast.Inspect(file, func(n ast.Node) bool {
					spec, ok := n.(*ast.ValueSpec)
					if !ok {
						return true
					}
					for i, name := range spec.Names {
						if !strings.HasPrefix(name.Name, "Err") || !name.IsExported() {
							continue
						}
						if i >= len(spec.Values) {
							continue
						}
						msg, ok := errorsNewLiteral(spec.Values[i])
						if !ok {
							continue
						}
						qualified := pkgName + "." + name.Name
						if prior, dup := found[msg]; dup {
							t.Errorf("%s and %s share the message %q, so they cannot be told apart on the wire",
								prior, qualified, msg)
						}
						found[msg] = qualified
					}
					return true
				})
			}
		}
	}

	return found
}

// errorsNewLiteral returns the string literal from an errors.New call.
func errorsNewLiteral(expr ast.Expr) (string, bool) {
	call, ok := expr.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return "", false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "New" {
		return "", false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "errors" {
		return "", false
	}
	lit, ok := call.Args[0].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	msg, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return msg, true
}
