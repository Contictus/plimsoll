// Package valuation turns prices into a number in one currency, and shows its work.
//
// Everything here is pure (L4): rates and the peg set are inputs, there is no clock and no
// database. That is what lets M7.5's scenario shock re-price a whole portfolio under a
// hypothetical without touching Postgres, and what keeps these tests Docker-free.
//
// The unit is USD (K17). No venue in V1 quotes USD, so every path ends in one assumed hop —
// see PegSet for what that costs and why it is said out loud rather than hidden.
package valuation

import (
	"errors"
	"time"

	"github.com/shopspring/decimal"
)

// moneyScale matches NUMERIC(38,18), the scale the rest of the system rounds at. Division
// rounds here so the engine and Postgres hold the same number (L1).
const moneyScale = 18

// ErrNoRoute means no sequence of known pairs reaches an assumed asset.
//
// An error rather than a zero, because zero is a number a dashboard will happily add up.
// The caller turns this into "your portfolio is worth X, minus the part we could not price",
// which is a true sentence; a zero would make "worth X" a false one.
var ErrNoRoute = errors.New("valuation: no price path to the numeraire")

// Pair is one tradeable edge with a price on it. Price is quote per base: for BTCUSDT it is
// how many USDT one BTC costs.
type Pair struct {
	InstrumentID int64
	BaseAssetID  int64
	QuoteAssetID int64
	Price        decimal.Decimal
	ObservedAt   time.Time
}

// PegSet is the assets whose USD value is assumed rather than observed, and the rate
// assumed for each.
//
// It is configuration, not a constant, and that is the whole of K17. Assuming USDT is 1.00
// while USDCUSDT is trading would give the product a blind spot shaped exactly like its
// worst tail risk: during a depeg the system would report "everything is normal". Leaving
// USDT out of the set prices it through a real market instead, and the depeg shows up in
// the number.
//
// Something has to terminate the walk, because no venue in V1 quotes USD. So the smallest
// honest set is one asset, and every path ends in its assumed hop — which means AssumedPeg
// is true of every answer M4 can give. That is a truthful statement about a real limitation
// rather than a useless flag: the *path* says which asset was assumed and what the rest of
// the number rested on, and the day a real USD source is added the flag starts telling
// paths apart.
type PegSet map[int64]decimal.Decimal

// Hop is one leg of a price path. Multiplying the running amount by Rate walks it.
type Hop struct {
	From, To int64

	// InstrumentID is zero on the assumed hop, which is not a market and has no instrument.
	InstrumentID int64

	// Rate is what to multiply by, already inverted if the pair was used against its
	// quotation. Storing the applied rate rather than the raw one means a reader can check
	// the arithmetic without knowing which way round the pair is quoted.
	Rate decimal.Decimal

	// Inverted records that the pair was used in the direction it is not quoted in, so the
	// audit trail distinguishes "the market said 0.1" from "the market said 10 and we
	// divided".
	Inverted bool

	// ObservedAt is zero on the assumed hop.
	ObservedAt time.Time

	// Assumed is true only on the peg hop.
	Assumed bool
}

// Priced is an asset's value in USD and the evidence for it.
type Priced struct {
	USD  decimal.Decimal
	Path []Hop

	// AssumedPeg is true when any hop fell back to an assumed rate. See PegSet: in M4 that
	// is every answer.
	AssumedPeg bool

	// OldestObservedAt is the age of the worst market leg, not the average.
	//
	// A portfolio priced through a BTC rate from one second ago and a USDC rate from an
	// hour ago is an hour-old number. Averaging would report the most important weakness in
	// the answer as a rounding difference.
	//
	// Zero means no market was consulted at all, which happens when the asset is itself
	// assumed. Callers check IsZero rather than comparing against a tolerance.
	OldestObservedAt time.Time
}
