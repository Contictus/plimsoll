package reconciliation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/asset"
	"github.com/Contictus/plimsoll/backend/internal/exchange/binance"
	"github.com/Contictus/plimsoll/backend/internal/quality"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// Source is the venue, as this package needs it: one signed call, returning bytes.
//
// An interface rather than the client itself, because the comparison must be testable against
// a venue that disagrees on demand -- a reconciliation feature developed against the live API
// is one that can only be exercised when it is already broken.
type Source interface {
	Account(ctx context.Context) (json.RawMessage, error)
}

// Deps is one integration's reconciliation, assembled.
type Deps struct {
	DB                       tenancy.Beginner
	AccountID, IntegrationID uuid.UUID

	Source    Source
	Tolerance Tolerance

	// SkewTolerance is how far the venue's clock may be from ours before event ordering is
	// reported as suspect (L7).
	SkewTolerance time.Duration
}

// aliasSource is the namespace the venue's asset codes live in.
const aliasSource = "binance"

// Run asks the venue what it believes, compares it against our fold, and records the result.
//
// The comparison is keyed by our CANONICAL symbol, never the venue's code: each code is
// resolved through the alias table as of the snapshot's own instant, and one that resolves to
// nothing becomes an unresolved_asset finding rather than being compared against a guess
// (L8, K22).
//
// A failed snapshot is both recorded and returned. Both, deliberately: the error stops the
// caller treating the run as successful, and the finding stops the register from implying
// everything is fine because nobody managed to look (L11).
func Run(ctx context.Context, d Deps, now time.Time) error {
	raw, err := d.Source.Account(ctx)
	if err != nil {
		// The venue being unreachable is not evidence that a known disagreement went away, so
		// this pass owns only the kind it can speak to. Everything a previous run opened stays
		// open.
		recordErr := recordPass(ctx, d, quality.Pass{
			At:   now,
			Owns: []quality.Kind{quality.KindSnapshotFailed},
			Found: []quality.Finding{{
				Kind:     quality.KindSnapshotFailed,
				Subject:  "spot",
				Severity: quality.SeverityWarn,
				// The error is wrapped, never the credential that produced it (L13).
				Detail: fmt.Sprintf("the venue could not be asked: %v", err),
			}},
		})
		if recordErr != nil {
			return errors.Join(err, recordErr)
		}
		return fmt.Errorf("reconciliation: snapshot integration %s: %w", d.IntegrationID, err)
	}

	acct, err := binance.DecodeSpotAccount(raw)
	if err != nil {
		return fmt.Errorf("reconciliation: decode snapshot for %s: %w", d.IntegrationID, err)
	}

	err = tenancy.InTx(ctx, d.DB, d.AccountID, func(q *store.Queries) error {
		ours, evidence, err := loadOurs(ctx, q, d)
		if err != nil {
			return err
		}
		theirs, unresolved, err := resolveTheirs(ctx, q, acct)
		if err != nil {
			return err
		}

		found := make([]quality.Finding, 0, len(unresolved))
		for _, symbol := range unresolved {
			found = append(found, quality.Finding{
				Kind:     quality.KindUnresolvedAsset,
				Subject:  symbol,
				Severity: quality.SeverityWarn,
				Detail: fmt.Sprintf(
					"the venue reports %q, which maps to no asset in the registry", symbol),
				Raw: raw,
			})
		}
		for _, delta := range Compare(ours, theirs, d.Tolerance) {
			found = append(found, quality.Finding{
				Kind:     Classify(delta, evidence),
				Subject:  delta.Subject,
				Severity: quality.SeverityError,
				Detail: fmt.Sprintf("the ledger and the venue disagree about %s by %s",
					delta.Subject, delta.Delta),
				Delta: decimal.NewNullDecimal(delta.Delta),
				Raw:   raw,
			})
		}

		return quality.Record(ctx, q, d.AccountID, d.IntegrationID, quality.Pass{
			At:    now,
			Owns:  owned(),
			Found: found,
		})
	})
	if err != nil {
		return fmt.Errorf("reconciliation: record integration %s: %w", d.IntegrationID, err)
	}
	return nil
}

// owned is what a successful reconciliation pass is responsible for, and therefore all it is
// allowed to close. The coherence checks own the rest of the register.
func owned() []quality.Kind {
	return append(append([]quality.Kind{}, quality.ReconciliationKinds...), quality.KindUnresolvedAsset)
}

// recordPass writes one pass in its own transaction, for the paths with nothing else to do in
// one.
func recordPass(ctx context.Context, d Deps, p quality.Pass) error {
	return tenancy.InTx(ctx, d.DB, d.AccountID, func(q *store.Queries) error {
		return quality.Record(ctx, q, d.AccountID, d.IntegrationID, p)
	})
}

// loadOurs reads the fold and the evidence the classifier reasons from. Both come from the
// same transaction as the record, so a comparison and its explanation describe one instant.
func loadOurs(ctx context.Context, q *store.Queries, d Deps) (Ours, Evidence, error) {
	balances, err := q.ListIntegrationBalancesWithSymbol(ctx,
		store.ListIntegrationBalancesWithSymbolParams{
			AccountID:     d.AccountID,
			IntegrationID: d.IntegrationID,
		})
	if err != nil {
		return Ours{}, Evidence{}, fmt.Errorf("reconciliation: load balances: %w", err)
	}
	ours := Ours{Balances: make(map[string]decimal.Decimal, len(balances))}
	for _, b := range balances {
		ours.Balances[b.CanonicalSymbol] = b.Quantity
	}

	ev := Evidence{
		Precision:            map[string]int32{},
		UnsupportedMagnitude: map[string]decimal.Decimal{},
	}

	fees, err := q.SumUnattributedFeesByAsset(ctx, store.SumUnattributedFeesByAssetParams{
		AccountID:     d.AccountID,
		IntegrationID: d.IntegrationID,
	})
	if err != nil {
		return Ours{}, Evidence{}, fmt.Errorf("reconciliation: load unattributed fees: %w", err)
	}
	for _, f := range fees {
		if f.FeeAsset == nil {
			continue
		}
		// The fee's asset code never resolved -- that is why it is unattributed -- so the code
		// is the only name it has. Our balance is high by exactly this much, which is what
		// makes it evidence rather than a guess (K54).
		ev.UnsupportedMagnitude[*f.FeeAsset] = f.Total
	}

	dupes, err := q.ListVenueIdentitiesSeenTwice(ctx, d.AccountID)
	if err != nil {
		return Ours{}, Evidence{}, fmt.Errorf("reconciliation: load duplicate identities: %w", err)
	}
	ev.DuplicatedHere = len(dupes) > 0

	return ours, ev, nil
}

// resolveTheirs turns the venue's asset codes into our canonical symbols, as of the snapshot's
// own instant. A code that resolves to nothing is returned separately rather than compared:
// guessing which asset it meant is how a correct quantity ends up attached to the wrong one,
// and because the number looks plausible it is debugged as a price problem for weeks (K22).
func resolveTheirs(
	ctx context.Context, q *store.Queries, acct binance.SpotAccount,
) (Theirs, []string, error) {
	theirs := Theirs{Balances: make(map[string]decimal.Decimal, len(acct.Balances))}
	var unresolved []string

	for _, b := range acct.Balances {
		held := b.Held()
		assetID, err := asset.Resolve(ctx, q, aliasSource, b.Asset, acct.UpdateTime)
		if errors.Is(err, asset.ErrUnknownSymbol) {
			// An asset the venue holds none of and we cannot name tells the user nothing, and
			// it is most of a real account's balance list. One with a quantity is a different
			// matter: that is a holding we cannot account for at all.
			if !held.IsZero() {
				unresolved = append(unresolved, b.Asset)
			}
			continue
		}
		if err != nil {
			return Theirs{}, nil, err
		}
		symbol, err := q.GetAssetSymbol(ctx, assetID)
		if err != nil {
			return Theirs{}, nil, fmt.Errorf("reconciliation: name asset %d: %w", assetID, err)
		}
		theirs.Balances[symbol] = held
	}
	return theirs, unresolved, nil
}
