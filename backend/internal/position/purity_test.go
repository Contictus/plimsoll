package position_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// L1: no float64 on a money path, and the engine is the money path. The lint rules keep a
// database and a clock out of this package; nothing else keeps a float out, and a float
// here would pass every test above while being wrong in the eighteenth decimal place.
//
// It scans this package's own sources rather than reaching across the tree, so the guard
// travels with the code it guards -- each engine added later gets its own copy.
func TestEngineSourceContainsNoFloat64(t *testing.T) {
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}

	checked := 0
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		if strings.Contains(string(src), "float64") {
			t.Errorf("%s contains float64; money is decimal.Decimal everywhere (L1)", path)
		}
	}
	if checked == 0 {
		t.Fatal("no engine sources were checked; the glob or the layout changed")
	}
}
