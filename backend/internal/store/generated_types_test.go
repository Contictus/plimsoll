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
	}
	if checked == 0 {
		t.Fatal("no generated files were checked; the glob or the layout changed")
	}
}
