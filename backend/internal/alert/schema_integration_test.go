//go:build integration

package alert_test

import (
	"context"
	"os"
	"testing"

	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

func ownerPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := store.NewPool(context.Background(), os.Getenv("PLIMSOLL_OWNER_DSN"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// insertRule writes as the owner WITH the account bound, because alert_rules carries FORCE
// ROW LEVEL SECURITY and the owner is bound by it too. Without the binding every insert here
// would fail on the policy -- and the tests below, which are about CHECK constraints, would
// pass for a reason that has nothing to do with what they claim.
func insertRule(t *testing.T, accountID uuid.UUID, metric, comparator, trigger, clear string) error {
	t.Helper()
	ctx := context.Background()
	return tenancy.InTxRaw(ctx, ownerPool(t), accountID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO alert_rules
			   (account_id, name, metric, comparator, trigger_at, clear_at)
			 VALUES ($1, $2, $3, $4, $5, $6)`,
			accountID, "probe-"+uuid.NewString(), metric, comparator, trigger, clear)
		return err
	})
}

func seedAccount(t *testing.T) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := ownerPool(t).Exec(context.Background(),
		`INSERT INTO accounts (id, email) VALUES ($1, $2)`,
		id, "alert-"+id.String()+"@example.test")
	require.NoError(t, err)
	return id
}

// A band that points the wrong way makes the rule fire and clear on the same evaluation, and
// then the hysteresis is decoration. The schema refuses it, so no writer -- the API, a
// migration, a hurried fix -- can produce one.
func TestTheSchemaRefusesABandThatPointsTheWrongWay(t *testing.T) {
	accountID := seedAccount(t)

	for _, bad := range []struct {
		name           string
		comparator     string
		trigger, clear string
	}{
		{"above with a clear line above it", "above", "3", "4"},
		{"below with a clear line below it", "below", "1000", "900"},
	} {
		t.Run(bad.name, func(t *testing.T) {
			require.Error(t,
				insertRule(t, accountID, "leverage", bad.comparator, bad.trigger, bad.clear),
				"an inverted hysteresis band was accepted")
		})
	}
}

// And the shape that is meant to work does.
func TestAWellFormedRuleIsAccepted(t *testing.T) {
	accountID := seedAccount(t)
	require.NoError(t, insertRule(t, accountID, "leverage", "above", "3", "2.5"))
}

// A metric nothing computes is a rule that never fires, and a rule that never fires is
// silence the user cannot tell from safety.
func TestTheSchemaRefusesAMetricNothingComputes(t *testing.T) {
	accountID := seedAccount(t)
	require.Error(t, insertRule(t, accountID, "vibes", "above", "3", "2.5"))
}
