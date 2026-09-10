package binance

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
)

// weightFuturesExchangeInfo is GET /fapi/v1/exchangeInfo, verified on 2026-09-10 (F14's
// sibling on the market-data page). Weight 1 (IP).
const weightFuturesExchangeInfo = 1

// FuturesBaseURL is the USD-M REST host. Separate from the spot host because they are
// separate services, not two paths on one server.
const FuturesBaseURL = "https://fapi.binance.com"

// contractPerpetual is the only contractType this project models.
//
// The four documented values are PERPETUAL, CURRENT_QUARTER, NEXT_QUARTER and
// TRADIFI_PERPETUAL. A quarterly contract DELIVERS: it closes itself on a date, and folded
// as a perpetual it stays open forever, leaving a phantom exposure the user cannot close
// because it does not exist. TRADIFI_PERPETUAL is perpetual but its underlying is an index
// rather than a coin, which the asset registry has no entry for -- and inventing one is how
// a position acquires an asset nobody can price.
const contractPerpetual = "PERPETUAL"

// statusTrading is the only status a contract is swept in. The page shows TRADING as its
// example and does not enumerate the rest, so this is a whitelist for the same reason F11's
// is: opening a walk for a settling contract spends weight on history that is closing, and
// a status this code has never seen should stop it rather than be assumed benign.
const statusTrading = "TRADING"

// IsModelledContract reports whether a contractType is one this project folds. Exported so
// the sweep and any future reconciliation check ask the same question of the same table.
func IsModelledContract(contractType string) bool {
	return contractType == contractPerpetual
}

// FuturesContract is one USD-M contract as the registry needs it: the venue's symbol and
// the three assets that decide what a fill and a funding payment move.
//
// MarginAsset is what the contract settles in, which for a USD-M perp is the quote asset
// and for a coin-margined one would not be. It is carried explicitly rather than inferred,
// because inferring it is right for every contract V1 trades and wrong for the first one it
// does not -- correct in testing, wrong in production.
type FuturesContract struct {
	Symbol      string
	BaseAsset   string
	QuoteAsset  string
	MarginAsset string
}

// FuturesContracts reads the perpetuals out of a USD-M exchangeInfo payload, sorted.
//
// It filters rather than returns the list: the futures universe includes quarterly futures
// and index perpetuals, and a sweep that opened a walk for each of them would be walking
// contracts this ledger cannot fold.
func FuturesContracts(raw json.RawMessage) ([]FuturesContract, error) {
	var payload struct {
		Symbols []struct {
			Symbol       string `json:"symbol"`
			ContractType string `json:"contractType"`
			Status       string `json:"status"`
			BaseAsset    string `json:"baseAsset"`
			QuoteAsset   string `json:"quoteAsset"`
			MarginAsset  string `json:"marginAsset"`
		} `json:"symbols"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("binance: futures exchangeInfo has no symbols array: %w", err)
	}

	out := make([]FuturesContract, 0, len(payload.Symbols))
	seen := make(map[string]bool, len(payload.Symbols))
	for i, entry := range payload.Symbols {
		if entry.Symbol == "" {
			// The same refusal SpotSymbols makes: a sweep over a list with holes in it
			// reports a complete discovery it did not do (K33).
			return nil, fmt.Errorf(
				"binance: futures exchangeInfo symbol %d has no name; a sweep over a list"+
					" with holes in it reports a complete discovery it did not do", i)
		}
		if !IsModelledContract(entry.ContractType) || entry.Status != statusTrading {
			continue
		}
		if entry.MarginAsset == "" {
			return nil, fmt.Errorf(
				"binance: perpetual %s names no margin asset, and a perp that does not say"+
					" what it settles in cannot be stored (00005)", entry.Symbol)
		}
		if seen[entry.Symbol] {
			continue
		}
		seen[entry.Symbol] = true
		out = append(out, FuturesContract{
			Symbol:      entry.Symbol,
			BaseAsset:   entry.BaseAsset,
			QuoteAsset:  entry.QuoteAsset,
			MarginAsset: entry.MarginAsset,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Symbol < out[j].Symbol })
	return out, nil
}

// FuturesExchangeInfo fetches the USD-M contract universe. Unsigned: it is public data, and
// asking for it with a key attached would put an account's identity on a request that does
// not need one (F6's rule, applied to a second endpoint).
func (c *Client) FuturesExchangeInfo(ctx context.Context) (json.RawMessage, error) {
	return c.do(ctx, request{
		path:    "/fapi/v1/exchangeInfo",
		weight:  weightFuturesExchangeInfo,
		futures: true,
	})
}
