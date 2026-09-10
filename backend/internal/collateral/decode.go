package collateral

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/shopspring/decimal"
)

// ErrHedgeMode means a position row reports a side V1 does not model. Same refusal the fill
// normalizer makes, for the same reason: one position per instrument, and two sides folded
// into it report a flat account that is carrying two live exposures.
var ErrHedgeMode = errors.New("collateral: hedge mode is not modelled in V1 (one-way only)")

const positionSideOneWay = "BOTH"

// DecodeAccount reads the margin totals out of a /fapi/v3/account payload.
//
// This endpoint rather than positionRisk because the maintenance requirement is only here
// (F14) -- and the maintenance requirement is half of the margin buffer, which is the number
// the whole page exists for.
func DecodeAccount(raw json.RawMessage) (Snapshot, error) {
	// Every field is a string on the wire and stays one until decimal: a margin balance
	// decoded through float64 comes back with different digits (L1).
	var payload struct {
		TotalMaintMargin      string `json:"totalMaintMargin"`
		TotalWalletBalance    string `json:"totalWalletBalance"`
		TotalUnrealizedProfit string `json:"totalUnrealizedProfit"`
		TotalMarginBalance    string `json:"totalMarginBalance"`
		AvailableBalance      string `json:"availableBalance"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return Snapshot{}, fmt.Errorf("collateral: decode futures account: %w", err)
	}

	var s Snapshot
	for _, f := range []struct {
		name string
		raw  string
		into *decimal.Decimal
	}{
		{"totalMarginBalance", payload.TotalMarginBalance, &s.MarginBalance},
		{"totalMaintMargin", payload.TotalMaintMargin, &s.MaintenanceMargin},
		{"totalWalletBalance", payload.TotalWalletBalance, &s.WalletBalance},
		{"totalUnrealizedProfit", payload.TotalUnrealizedProfit, &s.UnrealizedPnL},
		{"availableBalance", payload.AvailableBalance, &s.AvailableBalance},
	} {
		v, err := parse(f.raw)
		if err != nil {
			return Snapshot{}, fmt.Errorf("collateral: futures account %s: %w", f.name, err)
		}
		*f.into = v
	}
	return s, nil
}

// DecodePositions reads the open positions out of a /fapi/v3/positionRisk payload.
//
// A zero positionAmt is NO POSITION rather than a position of zero. The endpoint may return
// a row for every symbol -- the catalog page says "all symbols" and the endpoint page did
// not render far enough to confirm (F14's open question) -- so the safe reading is that most
// rows are flat. Storing them would fill the risk view with hundreds of rows at zero, each
// with no liquidation distance, and bury the one position that matters.
func DecodePositions(raw json.RawMessage) ([]PositionRisk, error) {
	var rows []struct {
		Symbol           string `json:"symbol"`
		PositionAmt      string `json:"positionAmt"`
		EntryPrice       string `json:"entryPrice"`
		MarkPrice        string `json:"markPrice"`
		LiquidationPrice string `json:"liquidationPrice"`
		Notional         string `json:"notional"`
		Leverage         string `json:"leverage"`
		PositionSide     string `json:"positionSide"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("collateral: decode position risk: %w", err)
	}

	out := make([]PositionRisk, 0, len(rows))
	for _, row := range rows {
		if row.PositionSide != "" && row.PositionSide != positionSideOneWay {
			return nil, fmt.Errorf("%w: %s reports positionSide %q",
				ErrHedgeMode, row.Symbol, row.PositionSide)
		}

		quantity, err := parse(row.PositionAmt)
		if err != nil {
			return nil, fmt.Errorf("collateral: %s positionAmt: %w", row.Symbol, err)
		}
		if quantity.IsZero() {
			continue
		}

		p := PositionRisk{Symbol: row.Symbol, Quantity: quantity}
		for _, f := range []struct {
			name string
			raw  string
			into *decimal.Decimal
		}{
			{"entryPrice", row.EntryPrice, &p.EntryPrice},
			{"markPrice", row.MarkPrice, &p.MarkPrice},
			{"liquidationPrice", row.LiquidationPrice, &p.LiquidationPrice},
			{"notional", row.Notional, &p.Notional},
			{"leverage", row.Leverage, &p.Leverage},
		} {
			v, err := parse(f.raw)
			if err != nil {
				return nil, fmt.Errorf("collateral: %s %s: %w", row.Symbol, f.name, err)
			}
			*f.into = v
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Symbol < out[j].Symbol })
	return out, nil
}

// DecodeBrackets reads the maintenance-margin tier table, keyed by exchange symbol and
// ordered by notional floor.
//
// The order is not cosmetic: MaintenanceAt walks the table with a half-open [floor, cap)
// test, so a table sorted by anything else matches the wrong tier for a notional sitting
// near a boundary -- and near a boundary is exactly where a scenario shock puts one.
func DecodeBrackets(raw json.RawMessage) (map[string][]Bracket, error) {
	// The numbers here arrive as JSON NUMBERS rather than as strings, unlike every other
	// money field on this venue. json.Number keeps their digits: decoding 0.005 through
	// float64 yields 0.005000000000000000104..., and that multiplied by a large notional is
	// off in the digits a margin call is decided by (L1).
	var rows []struct {
		Symbol   string `json:"symbol"`
		Brackets []struct {
			Bracket          int         `json:"bracket"`
			NotionalFloor    json.Number `json:"notionalFloor"`
			NotionalCap      json.Number `json:"notionalCap"`
			MaintMarginRatio json.Number `json:"maintMarginRatio"`
			Cum              json.Number `json:"cum"`
		} `json:"brackets"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("collateral: decode leverage brackets: %w", err)
	}

	out := make(map[string][]Bracket, len(rows))
	for _, row := range rows {
		if row.Symbol == "" {
			return nil, fmt.Errorf("collateral: a bracket table names no symbol")
		}
		table := make([]Bracket, 0, len(row.Brackets))
		for _, b := range row.Brackets {
			bracket := Bracket{Bracket: b.Bracket}
			for _, f := range []struct {
				name string
				raw  json.Number
				into *decimal.Decimal
			}{
				{"notionalFloor", b.NotionalFloor, &bracket.NotionalFloor},
				{"notionalCap", b.NotionalCap, &bracket.NotionalCap},
				{"maintMarginRatio", b.MaintMarginRatio, &bracket.MaintMarginRatio},
				{"cum", b.Cum, &bracket.Cum},
			} {
				v, err := parse(f.raw.String())
				if err != nil {
					return nil, fmt.Errorf("collateral: %s bracket %d %s: %w",
						row.Symbol, b.Bracket, f.name, err)
				}
				*f.into = v
			}
			table = append(table, bracket)
		}
		sort.Slice(table, func(i, j int) bool {
			return table[i].NotionalFloor.LessThan(table[j].NotionalFloor)
		})
		out[row.Symbol] = table
	}
	return out, nil
}

// parse turns one venue number into a decimal. An empty field is zero rather than an error:
// the venue omits a field it has nothing to say about, and refusing the whole capture over
// an absent leverage would trade the margin buffer for a detail.
func parse(s string) (decimal.Decimal, error) {
	if s == "" {
		return decimal.Zero, nil
	}
	v, err := decimal.NewFromString(s)
	if err != nil {
		return decimal.Decimal{}, fmt.Errorf("%q is not a number: %w", s, err)
	}
	return v, nil
}
