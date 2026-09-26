package idempotency

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestAllActionsAreValid(t *testing.T) {
	for _, a := range AllActions {
		if !a.Valid() {
			t.Errorf("%s is in AllActions but Valid() reports false", a)
		}
	}
	if Action("").Valid() {
		t.Error("the empty action must not be valid")
	}
	if Action("advance_clock").Valid() {
		t.Error("actions are case sensitive; lowercase must not validate")
	}
}

func TestActionNamesAreScreamingSnakeCase(t *testing.T) {
	re := regexp.MustCompile(`^[A-Z][A-Z0-9_]*[A-Z0-9]$`)
	for _, a := range AllActions {
		if !re.MatchString(string(a)) {
			t.Errorf("%q does not match the SCREAMING_SNAKE_CASE convention used by the Postgres enum", a)
		}
	}
}

// The drift guard named in the action.go invariant comment.
//
// A Go action absent from the Postgres enum derives a key successfully and then
// fails at INSERT time, in production, on the action nobody tested. A Postgres
// value absent from Go is dead weight that suggests an operation exists when it
// does not. Both are caught here, at build time, by parsing the migration.
func TestActionsMatchDDL(t *testing.T) {
	// Every migration is read, not just the one that creates the type.
	//
	// The vocabulary can legitimately grow after 0001, and it has: the ASBA fund-movement actions were
	// added by 0012 because reserving funds and settling them turned out to need separate idempotency
	// keys. They could not be added to 0001, because that migration has already been applied and its
	// checksum is recorded, and rewriting an applied migration is precisely what the runner's drift
	// check exists to prevent.
	//
	// So the enum's current membership is the CREATE plus every subsequent ALTER, and a test that read
	// only the CREATE would force the choice between failing forever and editing history.
	dir := filepath.Join("..", "..", "..", "db", "migrations")
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		t.Fatalf("listing migrations: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no migrations found; the path or the layout changed")
	}
	slices.Sort(files)

	var ddlActions []string
	var createdIn string

	for _, path := range files {
		blob, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", filepath.Base(path), err)
		}
		sql := string(blob)

		if created, err := parseEnumValues(sql, "admin_action"); err == nil && len(created) > 0 {
			if createdIn != "" {
				t.Fatalf("admin_action is created in both %s and %s; only one CREATE TYPE may exist",
					createdIn, filepath.Base(path))
			}
			createdIn = filepath.Base(path)
			ddlActions = append(ddlActions, created...)
		}

		ddlActions = append(ddlActions, parseEnumAdditions(sql, "admin_action")...)
	}

	if createdIn == "" {
		t.Fatal("no CREATE TYPE admin_action found in any migration")
	}
	if len(ddlActions) == 0 {
		t.Fatal("parsed zero values from the admin_action enum; the parser or the migration changed shape")
	}

	// A value added twice would make the count comparison below pass for the wrong reason.
	seen := map[string]string{}
	for _, v := range ddlActions {
		if _, dup := seen[v]; dup {
			t.Errorf("%s appears more than once across the migrations", v)
		}
		seen[v] = ""
	}

	goActions := make([]string, 0, len(AllActions))
	for _, a := range AllActions {
		goActions = append(goActions, string(a))
	}

	slices.Sort(ddlActions)
	slices.Sort(goActions)

	missingInDDL := difference(goActions, ddlActions)
	missingInGo := difference(ddlActions, goActions)

	if len(missingInDDL) > 0 {
		t.Errorf("actions defined in Go but absent from the admin_action enum: %v\n"+
			"These will derive keys and then fail on INSERT. Add them in a NEW migration with\n"+
			"ALTER TYPE admin_action ADD VALUE, not by editing %s, which has already been applied.",
			missingInDDL, createdIn)
	}
	if len(missingInGo) > 0 {
		t.Errorf("values in the admin_action enum but absent from AllActions: %v\n"+
			"These are unreachable: no key can be derived for them.", missingInGo)
	}
	if len(ddlActions) != len(goActions) {
		t.Errorf("count mismatch: %d in DDL, %d in Go", len(ddlActions), len(goActions))
	}
}

// parseEnumValues extracts the quoted values from a CREATE TYPE ... AS ENUM (...)
// statement. Deliberately narrow: it looks for the specific type name and reads
// only to the closing parenthesis of that statement.
func parseEnumValues(sql, typeName string) ([]string, error) {
	head := regexp.MustCompile(`(?is)CREATE\s+TYPE\s+` + regexp.QuoteMeta(typeName) + `\s+AS\s+ENUM\s*\(`)
	loc := head.FindStringIndex(sql)
	if loc == nil {
		return nil, errNotFound{typeName}
	}

	rest := sql[loc[1]:]
	end := strings.Index(rest, ")")
	if end < 0 {
		return nil, errNotFound{typeName + " closing parenthesis"}
	}
	body := rest[:end]

	// Strip line comments so a commented-out value is not counted.
	var cleaned strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		cleaned.WriteString(line)
		cleaned.WriteString("\n")
	}

	valueRe := regexp.MustCompile(`'([^']+)'`)
	matches := valueRe.FindAllStringSubmatch(cleaned.String(), -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m[1])
	}
	return out, nil
}

type errNotFound struct{ what string }

func (e errNotFound) Error() string { return "not found in SQL: " + e.what }

func difference(a, b []string) []string {
	inB := make(map[string]struct{}, len(b))
	for _, v := range b {
		inB[v] = struct{}{}
	}
	var out []string
	for _, v := range a {
		if _, ok := inB[v]; !ok {
			out = append(out, v)
		}
	}
	return out
}

// parseEnumAdditions extracts values added by ALTER TYPE ... ADD VALUE statements.
//
// Handles the IF NOT EXISTS form, which is what a migration should use so a partially applied
// environment can be brought forward without hand-editing.
func parseEnumAdditions(sql, typeName string) []string {
	re := regexp.MustCompile(
		`(?is)ALTER\s+TYPE\s+` + regexp.QuoteMeta(typeName) +
			`\s+ADD\s+VALUE\s+(?:IF\s+NOT\s+EXISTS\s+)?'([^']+)'`)

	var out []string
	for _, m := range re.FindAllStringSubmatch(sql, -1) {
		out = append(out, m[1])
	}
	return out
}

// TestASBAActionsWereAddedByMigration records where the three ASBA actions came from.
//
// Not a tautology: it asserts they arrived through an ALTER in a later migration rather than by editing
// the CREATE in 0001. That distinction is the whole reason the parser above exists, and a future
// contributor who "tidied up" by folding them into 0001 would break every deployed database's checksum
// check while making this test still pass unless it looks for the ALTER specifically.
func TestASBAActionsWereAddedByMigration(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "db", "migrations")
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		t.Fatal(err)
	}

	added := map[string]string{}
	for _, path := range files {
		blob, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range parseEnumAdditions(string(blob), "admin_action") {
			added[v] = filepath.Base(path)
		}
	}

	for _, want := range []Action{
		ActionRequestASBABlock, ActionSettleASBABlock, ActionReleaseASBABlock,
	} {
		file, ok := added[string(want)]
		if !ok {
			t.Errorf("%s is not added by any ALTER TYPE. It must arrive in a new migration rather "+
				"than by editing an applied one", want)
			continue
		}
		if !strings.HasPrefix(file, "0001") {
			continue
		}
		t.Errorf("%s is added inside %s; an applied migration must not be edited", want, file)
	}
}
