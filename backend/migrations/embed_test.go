package migrations_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Contictus/plimsoll/backend/migrations"
	"github.com/stretchr/testify/require"
)

// THE TEST THAT MAKES THE EMBED TRUSTWORTHY.
//
// `//go:embed *.sql` is a pattern, and a pattern can silently match less than the
// directory holds -- a migration added under a subdirectory, or one whose name stops
// matching. The failure mode is the worst kind available here: a container that starts
// cleanly, migrates to an older schema than the binary was built against, and then fails
// somewhere else entirely with a missing column.
//
// Comparing names is not enough. Two files can share a name and differ in every byte, and
// a stale embed is exactly that -- so the contents are compared too.
func TestTheEmbeddedMigrationsAreTheOnesOnDisk(t *testing.T) {
	onDisk, err := filepath.Glob("*.sql")
	require.NoError(t, err)
	require.NotEmpty(t, onDisk, "no migrations found on disk; this test is checking nothing")

	embedded, err := migrations.FS.ReadDir(".")
	require.NoError(t, err)

	var embeddedNames []string
	for _, e := range embedded {
		if strings.HasSuffix(e.Name(), ".sql") {
			embeddedNames = append(embeddedNames, e.Name())
		}
	}
	require.ElementsMatch(t, onDisk, embeddedNames,
		"the embedded migrations and the ones on disk are not the same set")

	for _, name := range onDisk {
		want, err := os.ReadFile(name)
		require.NoError(t, err)
		got, err := migrations.FS.ReadFile(name)
		require.NoError(t, err, "%s is on disk but not embedded", name)
		require.Equal(t, string(want), string(got),
			"%s is embedded with different contents than the file on disk", name)
	}
}

// goose orders by filename, so a migration whose name does not start with its number would
// be applied out of order -- and an out-of-order DDL against a schema that has already run
// is not something a later migration can undo.
func TestEveryMigrationIsNumberedAndOrdersLexicographically(t *testing.T) {
	names, err := filepath.Glob("*.sql")
	require.NoError(t, err)

	seen := map[string]string{}
	for _, name := range names {
		prefix, rest, found := strings.Cut(name, "_")
		require.True(t, found, "%s has no NNNNN_ prefix", name)
		require.Len(t, prefix, 5, "%s: the prefix must be five digits so 00010 sorts after 00009", name)
		require.NotEmpty(t, rest, "%s has a number and no name", name)
		for _, r := range prefix {
			require.True(t, r >= '0' && r <= '9', "%s: %q is not a number", name, prefix)
		}
		require.NotContains(t, seen, prefix,
			"%s and %s share the number %s; goose would apply them in an order nobody chose",
			name, seen[prefix], prefix)
		seen[prefix] = name
	}
}
