package store_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// L1: money never crosses as float64. sqlc emits float64 for numeric unless the override
// is configured, so this asserts the override is live rather than merely written down.
// It runs as a unit test, with no Docker, so a regression is caught on every save.
func TestGeneratedCodeContainsNoFloat64(t *testing.T) {
	paths, err := filepath.Glob("*.sql.go")
	if err != nil {
		t.Fatal(err)
	}
	paths = append(paths, "models.go", "db.go")

	checked := 0
	for _, path := range paths {
		src, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		checked++
		if strings.Contains(string(src), "float64") {
			t.Errorf("%s contains float64; check the numeric override in sqlc.yaml", path)
		}
		// A nullable NUMERIC silently falls through to the driver type when the nullable
		// override is missing -- which is how it landed in the first ledger model. The
		// value is not a float, so the check above says nothing about it, and a driver
		// type in the domain layer is how a decimal stops being one field later.
		if strings.Contains(string(src), "pgtype.Numeric") {
			t.Errorf("%s contains pgtype.Numeric; money must map to shopspring/decimal (L1)", path)
		}
	}
	if checked == 0 {
		t.Fatal("no generated files were checked; the glob or the layout changed")
	}
}
