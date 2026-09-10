package collateral

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/instrument"
)

// Source is the slice of the exchange adapter a capture needs. An interface rather than
// *binance.Client so the capture is testable against an account that exists only in memory:
// what these tests have to prove is about tearing and ordering, not about HTTP.
type Source interface {
	FuturesAccount(ctx context.Context) (json.RawMessage, error)
	PositionRisk(ctx context.Context) (json.RawMessage, error)
	LeverageBracket(ctx context.Context) (json.RawMessage, error)
}

// InstrumentResolver turns the venue's symbol into an instrument id, as of the capture's own
// instant. A raw symbol is never a key (L8, K10) -- and "BTCUSDT" would be the spot pair as
// readily as the perp.
type InstrumentResolver interface {
	Instrument(ctx context.Context, market instrument.Market, symbol string, at time.Time) (int64, error)
}

// Capture asks the venue for the two halves of a collateral picture and pairs them.
//
// The account call and the positionRisk call are made back to back with nothing between
// them, because maintenance margin is on one and the liquidation price is on the other
// (F14) and the pair has to describe one moment. NewSnapshot then refuses the pair if the
// clock moved further than tolerance between them, and stamps as_of with the EARLIER of the
// two -- a snapshot is only as fresh as its stalest half.
//
// The bracket table is fetched afterwards and is deliberately outside that pairing: it is
// the venue's tier schedule rather than a fact about this instant, so a slow response to it
// does not tear the snapshot.
func Capture(
	ctx context.Context,
	src Source,
	r InstrumentResolver,
	now func() time.Time,
	tolerance time.Duration,
) (Snapshot, error) {
	accountRaw, err := src.FuturesAccount(ctx)
	if err != nil {
		return Snapshot{}, fmt.Errorf("collateral: futures account: %w", err)
	}
	accountAt := now().UTC()

	positionsRaw, err := src.PositionRisk(ctx)
	if err != nil {
		return Snapshot{}, fmt.Errorf("collateral: position risk: %w", err)
	}
	positionsAt := now().UTC()

	s, err := NewSnapshot(accountAt, positionsAt, tolerance)
	if err != nil {
		return Snapshot{}, err
	}

	totals, err := DecodeAccount(accountRaw)
	if err != nil {
		return Snapshot{}, err
	}
	s.MarginBalance = totals.MarginBalance
	s.WalletBalance = totals.WalletBalance
	s.UnrealizedPnL = totals.UnrealizedPnL
	s.MaintenanceMargin = totals.MaintenanceMargin
	s.AvailableBalance = totals.AvailableBalance

	positions, err := DecodePositions(positionsRaw)
	if err != nil {
		return Snapshot{}, err
	}

	bracketsRaw, err := src.LeverageBracket(ctx)
	if err != nil {
		return Snapshot{}, fmt.Errorf("collateral: leverage brackets: %w", err)
	}
	brackets, err := DecodeBrackets(bracketsRaw)
	if err != nil {
		return Snapshot{}, err
	}

	// Resolved as of the snapshot's own instant, never time.Now() at write time (L8).
	s.Brackets = make(map[string][]Bracket, len(positions))
	for i := range positions {
		id, err := r.Instrument(ctx, instrument.MarketUSDM, positions[i].Symbol, s.AsOf)
		if err != nil {
			return Snapshot{}, fmt.Errorf("collateral: resolve %s: %w", positions[i].Symbol, err)
		}
		positions[i].InstrumentID = id
		positions[i].MaintMargin, err = MaintenanceAt(brackets[positions[i].Symbol], positions[i].Notional)
		if err != nil {
			return Snapshot{}, fmt.Errorf("collateral: %s: %w", positions[i].Symbol, err)
		}
		s.Brackets[positions[i].Symbol] = brackets[positions[i].Symbol]
	}
	s.Positions = positions
	return s, nil
}
