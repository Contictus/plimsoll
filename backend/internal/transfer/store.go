package transfer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/ledger"
	"github.com/Contictus/plimsoll/backend/internal/quality"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"
)

// DefaultRules are what the matcher runs with until a user tunes them.
//
// Six hours because a chain confirmation plus an exchange's own credit delay is hours rather
// than minutes, and two percent because a network fee on a small withdrawal can be a
// surprisingly large fraction of it. Both are deliberately loose: a miss goes to a queue a
// human reads, while a wrong link is a statement the system makes on its own.
var DefaultRules = Rules{Window: 6 * time.Hour, FeeTolerance: decimal.New(2, -2)}

// Reconcile matches this account's unlinked legs and records what it found.
//
// It writes links and findings; it writes no ledger row and changes no balance. That is the
// property the exit test asserts, because a milestone whose success is mostly the absence of
// change would otherwise be passed by a matcher that did nothing at all (K49, K57).
func Reconcile(
	ctx context.Context, db tenancy.Beginner, accountID uuid.UUID, rules Rules, now time.Time,
) (Result, error) {
	var result Result
	err := tenancy.InTx(ctx, db, accountID, func(q *store.Queries) error {
		rows, err := q.ListUnlinkedTransferLegs(ctx, accountID)
		if err != nil {
			return fmt.Errorf("transfer: list unlinked legs: %w", err)
		}

		var outs, ins []Leg
		for _, r := range rows {
			leg := Leg{
				EventID:       strconv.FormatInt(r.Seq, 10),
				IntegrationID: r.IntegrationID,
				Venue:         r.Exchange,
				Asset:         r.CanonicalSymbol,
				EventTime:     r.EventTime,
			}
			if r.Quantity.Valid {
				leg.Quantity = r.Quantity.Decimal
			}
			leg.TxID = txIDFrom(r.Raw)

			if r.EventType == string(ledger.TypeWithdrawal) {
				outs = append(outs, leg)
			} else {
				ins = append(ins, leg)
			}
		}

		result = Match(outs, ins, rules)

		for _, link := range result.Links {
			outSeq, inSeq, err := seqPair(link)
			if err != nil {
				return err
			}
			if err := q.InsertTransferLink(ctx, store.InsertTransferLinkParams{
				AccountID: accountID,
				OutSeq:    outSeq,
				InSeq:     inSeq,
				Method:    link.Method,
			}); err != nil {
				// A unique violation means another writer claimed a leg between the read and
				// the write. Not an error worth failing the run: the leg is linked, which is
				// the outcome this was trying to produce.
				if isUniqueViolation(err) {
					continue
				}
				return fmt.Errorf("transfer: link %s to %s: %w",
					link.Out.EventID, link.In.EventID, err)
			}
		}

		return recordUnmatched(ctx, q, accountID, result, now)
	})
	return result, err
}

func seqPair(link Link) (int64, int64, error) {
	outSeq, err := strconv.ParseInt(link.Out.EventID, 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("transfer: leg id %q is not a ledger seq: %w", link.Out.EventID, err)
	}
	inSeq, err := strconv.ParseInt(link.In.EventID, 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("transfer: leg id %q is not a ledger seq: %w", link.In.EventID, err)
	}
	return outSeq, inSeq, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// txIDFrom pulls the chain transaction out of the payload the venue sent (L15).
//
// It reads `raw` rather than a column because the two venues spell it differently -- Binance
// sends `txId` and Bybit sends `txID` -- and a column would have had to pick one spelling at
// ingest time, before either normalizer existed. An absent or unreadable txid is empty, not an
// error: an internal movement between two exchange accounts never touches a chain, and that is
// the common case rather than a fault.
func txIDFrom(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ""
	}
	// Exact keys, in order of preference. Not a case-insensitive struct tag: encoding/json
	// matches field names case-insensitively, which is how one field once got filled by
	// another's value (F20).
	for _, key := range []string{"txId", "txID", "txid"} {
		value, ok := payload[key]
		if !ok {
			continue
		}
		var id string
		if err := json.Unmarshal(value, &id); err == nil && id != "" {
			return id
		}
	}
	return ""
}

// recordUnmatched puts the leftovers in the data-quality register.
//
// A deposit with no withdrawal to match is usually not a matcher failure at all: Binance does
// not publish the enums that would let its withdrawals be normalized (F5, B2), so the outbound
// half of a Binance-to-Bybit move was never ingestible. Left silent, that documentation gap
// would look like a balance. Named, it is a known limit with a venue attached (K57).
func recordUnmatched(
	ctx context.Context, q *store.Queries, accountID uuid.UUID, result Result, now time.Time,
) error {
	byIntegration := map[uuid.UUID][]quality.Finding{}
	seen := map[uuid.UUID]struct{}{}

	add := func(leg Leg, direction string) {
		seen[leg.IntegrationID] = struct{}{}
		byIntegration[leg.IntegrationID] = append(byIntegration[leg.IntegrationID], quality.Finding{
			Kind:     quality.KindUnmatchedTransfer,
			Subject:  leg.EventID,
			Severity: quality.SeverityWarn,
			Detail: fmt.Sprintf(
				"a %s of %s %s on %s at %s has no other half; unmatched, it reads as a %s",
				direction, leg.Quantity, leg.Asset, leg.Venue,
				leg.EventTime.Format(time.RFC3339), consequence(direction)),
			Delta: decimal.NewNullDecimal(leg.Quantity),
		})
	}
	for _, leg := range result.UnmatchedOut {
		add(leg, "withdrawal")
	}
	for _, leg := range result.UnmatchedIn {
		add(leg, "deposit")
	}

	// Every integration that has legs at all is swept, so an integration whose last unmatched
	// leg was just linked has its finding closed rather than left open forever.
	for _, leg := range append(append([]Leg{}, result.UnmatchedOut...), result.UnmatchedIn...) {
		seen[leg.IntegrationID] = struct{}{}
	}
	for _, link := range result.Links {
		seen[link.Out.IntegrationID] = struct{}{}
		seen[link.In.IntegrationID] = struct{}{}
	}

	for integrationID := range seen {
		if err := quality.Record(ctx, q, accountID, integrationID, quality.Pass{
			At:    now,
			Owns:  quality.TransferKinds,
			Found: byIntegration[integrationID],
		}); err != nil {
			return fmt.Errorf("transfer: record unmatched legs for %s: %w", integrationID, err)
		}
	}
	return nil
}

// consequence spells out what the unmatched leg is mistaken for, because that -- not the
// absence of a link -- is what costs the user money.
func consequence(direction string) string {
	if direction == "withdrawal" {
		return "disposal, inventing a realized loss"
	}
	return "purchase at no cost, inventing a cost basis it never had"
}
