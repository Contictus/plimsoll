package binance

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/shopspring/decimal"
)

// SpotBalance is one asset's holding on the spot wallet.
type SpotBalance struct {
	Asset  string
	Free   decimal.Decimal
	Locked decimal.Decimal
}

// Held is what the account actually holds: free plus locked.
//
// The balance sitting behind a resting limit order is held, not gone. Comparing a fold
// against Free alone reports every open order as a missing event -- the easiest available way
// to make reconciliation useless, and it fails in the direction that looks most like a real
// bug (F21).
func (b SpotBalance) Held() decimal.Decimal { return b.Free.Add(b.Locked) }

// SpotAccount is the answer from GET /api/v3/account: what the venue believes we hold, and
// when it believed it.
type SpotAccount struct {
	// UpdateTime is the EXCHANGE's instant, not ours. It means a snapshot does not have to be
	// timestamped by our clock, and the difference between the two is precisely the clock-skew
	// check the coherence pass makes -- obtained from a call we were making anyway (F21).
	UpdateTime time.Time

	Balances []SpotBalance
}

// DecodeSpotAccount reads balances out of a GET /api/v3/account payload (IP weight 20, signed).
//
// Every number is a string on the wire and stays one until decimal: a balance decoded through
// float64 comes back with different digits (L1).
func DecodeSpotAccount(raw json.RawMessage) (SpotAccount, error) {
	var payload struct {
		UpdateTime int64 `json:"updateTime"`
		Balances   []struct {
			Asset  string `json:"asset"`
			Free   string `json:"free"`
			Locked string `json:"locked"`
		} `json:"balances"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return SpotAccount{}, fmt.Errorf("binance: decode spot account: %w", err)
	}

	out := SpotAccount{
		UpdateTime: time.UnixMilli(payload.UpdateTime).UTC(),
		Balances:   make([]SpotBalance, 0, len(payload.Balances)),
	}
	for _, b := range payload.Balances {
		free, err := decimal.NewFromString(b.Free)
		if err != nil {
			// The asset is named and the value is not: a malformed number is never echoed,
			// because a payload field is not a place we have proven a credential cannot be.
			return SpotAccount{}, fmt.Errorf("binance: spot balance %s: free is not a number", b.Asset)
		}
		locked, err := decimal.NewFromString(b.Locked)
		if err != nil {
			return SpotAccount{}, fmt.Errorf("binance: spot balance %s: locked is not a number", b.Asset)
		}
		out.Balances = append(out.Balances, SpotBalance{Asset: b.Asset, Free: free, Locked: locked})
	}
	return out, nil
}
