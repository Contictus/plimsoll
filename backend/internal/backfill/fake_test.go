//go:build integration

package backfill_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/asset"
	"github.com/Contictus/plimsoll/backend/internal/exchange/binance"
	"github.com/Contictus/plimsoll/backend/internal/instrument"
)

// errInjected is what the fake client returns when a test asks it to fail mid-walk. The
// walk must treat it the way it treats a dropped connection: stop, keep whatever it has
// already committed, and leave a cursor a rerun can resume from.
var errInjected = errors.New("fake binance: injected failure")

// fakeTrade is one spot fill the fake account made. Money stays a string here for the same
// reason it does in the normalizer: a quantity decoded through float64 comes back with
// different digits (L1).
type fakeTrade struct {
	ID    int64
	Time  time.Time
	Price string
	Qty   string
}

func (f fakeTrade) raw(symbol string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(
		`{"symbol":%q,"id":%d,"orderId":%d,"price":%q,"qty":%q,"quoteQty":"1",`+
			`"commission":"0.001","commissionAsset":"BNB","time":%d,`+
			`"isBuyer":true,"isMaker":false}`,
		symbol, f.ID, f.ID, f.Price, f.Qty, f.Time.UnixMilli()))
}

// fakeDeposit is one settled or unsettled deposit row.
type fakeDeposit struct {
	ID     string
	Time   time.Time
	Coin   string
	Amount string
	Status int
}

func (f fakeDeposit) raw() json.RawMessage {
	return json.RawMessage(fmt.Sprintf(
		`{"id":%q,"amount":%q,"coin":%q,"status":%d,"insertTime":%d}`,
		f.ID, f.Amount, f.Coin, f.Status, f.Time.UnixMilli()))
}

// fakeTransfer is one universal-transfer row: one movement, both endpoints named by its
// type (F10).
type fakeTransfer struct {
	TranID int64
	Type   string
	Time   time.Time
	Asset  string
	Amount string
	Status string
}

func (f fakeTransfer) raw() json.RawMessage {
	status := f.Status
	if status == "" {
		status = "CONFIRMED"
	}
	return json.RawMessage(fmt.Sprintf(
		`{"asset":%q,"amount":%q,"type":%q,"status":%q,"tranId":%d,"timestamp":%d}`,
		f.Asset, f.Amount, f.Type, status, f.TranID, f.Time.UnixMilli()))
}

// fakeClient is a Binance account that exists only in memory. It answers the two endpoints
// the backfill uses, records every query it was asked, and can be told to misbehave in the
// two ways that matter: failing partway through a walk, and answering fromId=0 with the
// newest trades instead of the oldest -- the unverified inference F5 rests on
// (docs/BINANCE-API-NOTES.md section 5).
type fakeClient struct {
	trades   map[string][]fakeTrade // per symbol, ascending by id
	deposits []fakeDeposit

	// newestOnZero makes fromId=0 return the most recent page. This is the behaviour the
	// documentation neither promises nor rules out, and the one that would make a walk
	// report a complete history after reading only the last page of it.
	newestOnZero bool

	// failFromID makes the page starting at this trade id fail, so a test can interrupt a
	// walk exactly where it wants to rather than by counting calls -- the probe that checks
	// F5 also costs calls, and a count would make the test depend on that number.
	failFromID *int64

	// stuckAtStart answers every paged request with the first page again, whatever fromId
	// was asked for. It models an exchange that is not paging the way the walk believes:
	// without a contiguity check the walk would loop forever re-reading the same page while
	// its cursor claimed to be advancing. Probes are left alone so the F5 check still runs.
	stuckAtStart bool

	// failOnSymbol makes every request for this symbol fail, which is how discovery is
	// interrupted partway through its sweep.
	failOnSymbol string

	transfers []fakeTransfer

	// failOnTransferType makes every request for one direction fail, which is how a
	// transfer walk is interrupted partway through its eight scopes.
	failOnTransferType string

	tradeCalls    []binance.MyTradesQuery
	depositCalls  []binance.HistoryQuery
	transferCalls []binance.TransferQuery
}

// UniversalTransferHistory answers one direction, one window, one page. It returns the
// object shape the endpoint documents -- {"total": n, "rows": [...]} -- because a fake that
// returned a bare array would let a decoder pass here and fail against the real venue.
//
// `total` is the count of the whole window, not of the page, which is what makes the
// endpoint's paging offset-based rather than keyset.
func (c *fakeClient) UniversalTransferHistory(
	_ context.Context, q binance.TransferQuery,
) (json.RawMessage, error) {
	c.transferCalls = append(c.transferCalls, q)
	if q.Type == c.failOnTransferType && c.failOnTransferType != "" {
		return nil, errInjected
	}
	if q.Type == "" {
		// The venue's own refusal, mirrored: `type` is a required parameter.
		return nil, fmt.Errorf("fake binance: transfer query with no type")
	}

	var window []fakeTransfer
	for _, tr := range c.transfers {
		if tr.Type != q.Type {
			continue
		}
		if !tr.Time.Before(q.StartTime) && tr.Time.Before(q.EndTime) {
			window = append(window, tr)
		}
	}
	sort.Slice(window, func(i, j int) bool { return window[i].TranID < window[j].TranID })
	total := len(window)

	size := q.Size
	if size <= 0 {
		size = 10 // the endpoint's documented default
	}
	page := q.Page
	if page <= 0 {
		page = 1
	}
	from := (page - 1) * size
	switch {
	case from >= len(window):
		window = nil
	case from+size > len(window):
		window = window[from:]
	default:
		window = window[from : from+size]
	}

	rows := make([]json.RawMessage, 0, len(window))
	for _, tr := range window {
		rows = append(rows, tr.raw())
	}
	body, err := json.Marshal(rows)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(fmt.Sprintf(`{"total":%d,"rows":%s}`, total, body)), nil
}

func (c *fakeClient) MyTrades(
	_ context.Context, q binance.MyTradesQuery,
) (json.RawMessage, error) {
	c.tradeCalls = append(c.tradeCalls, q)
	if q.Symbol == c.failOnSymbol && c.failOnSymbol != "" {
		return nil, errInjected
	}
	if c.failFromID != nil && q.FromID != nil && *q.FromID == *c.failFromID {
		return nil, errInjected
	}

	windowed := !q.StartTime.IsZero() || !q.EndTime.IsZero()
	if windowed && q.FromID != nil {
		// The venue's own refusal, mirrored. rest-api.md enumerates the supported parameter
		// combinations and this is not one of them; a fake that answered it anyway would let
		// a read pass here and be rejected against the real exchange.
		return nil, fmt.Errorf("fake binance: %w", binance.ErrUnsupportedQuery)
	}

	all := append([]fakeTrade(nil), c.trades[q.Symbol]...)
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })

	limit := q.Limit
	if limit <= 0 {
		limit = 500 // Binance's documented default
	}

	if c.stuckAtStart && q.FromID != nil && *q.FromID > 0 {
		q.FromID = new(int64)
	}

	var page []fakeTrade
	switch {
	// "With startTime, returns oldest items from startTime up to the limit... With both,
	// behaves like startTime but does not exceed endTime."
	case windowed:
		for _, t := range all {
			if !q.StartTime.IsZero() && t.Time.Before(q.StartTime) {
				continue
			}
			if !q.EndTime.IsZero() && !t.Time.Before(q.EndTime) {
				continue
			}
			if len(page) < limit {
				page = append(page, t)
			}
		}

	// "If fromId is not sent, the most recent trades are returned" -- and, when
	// newestOnZero is set, fromId=0 is answered the same way.
	case q.FromID == nil || (c.newestOnZero && *q.FromID == 0):
		if len(all) > limit {
			page = all[len(all)-limit:]
		} else {
			page = all
		}

	default:
		for _, t := range all {
			if t.ID >= *q.FromID && len(page) < limit {
				page = append(page, t)
			}
		}
	}

	rows := make([]json.RawMessage, 0, len(page))
	for _, t := range page {
		rows = append(rows, t.raw(q.Symbol))
	}
	return json.Marshal(rows)
}

func (c *fakeClient) DepositHistory(
	_ context.Context, q binance.HistoryQuery,
) (json.RawMessage, error) {
	c.depositCalls = append(c.depositCalls, q)

	var window []fakeDeposit
	for _, d := range c.deposits {
		if !d.Time.Before(q.StartTime) && d.Time.Before(q.EndTime) {
			window = append(window, d)
		}
	}
	sort.Slice(window, func(i, j int) bool { return window[i].Time.Before(window[j].Time) })

	if q.Offset >= len(window) {
		window = nil
	} else {
		window = window[q.Offset:]
	}
	if q.Limit > 0 && len(window) > q.Limit {
		window = window[:q.Limit]
	}

	rows := make([]json.RawMessage, 0, len(window))
	for _, d := range window {
		rows = append(rows, d.raw())
	}
	return json.Marshal(rows)
}

// fakeRegistry resolves symbols and coins without a database. The registries themselves are
// tested in M1; what these tests need to know is that the walk asked at all, and with which
// instant (L8).
type fakeRegistry struct {
	instruments map[string]int64
	assets      map[string]int64
}

func (r fakeRegistry) Instrument(
	_ context.Context, _ instrument.Market, symbol string, _ time.Time,
) (int64, error) {
	id, ok := r.instruments[symbol]
	if !ok {
		return 0, fmt.Errorf("fake registry: no instrument for %s", symbol)
	}
	return id, nil
}

// Asset wraps asset.ErrUnknownSymbol for a ticker it does not hold, because that sentinel
// is the contract rather than a detail: the normalizer swallows exactly that error to keep
// a fill whose fee asset is uncurated, and treats every other one as fatal. A fake that
// returned a plain error would make the two indistinguishable and the test would be
// exercising the fatal path while claiming to exercise the other.
func (r fakeRegistry) Asset(_ context.Context, symbol string, _ time.Time) (int64, error) {
	id, ok := r.assets[symbol]
	if !ok {
		return 0, fmt.Errorf("%w: fake registry has no asset for %s", asset.ErrUnknownSymbol, symbol)
	}
	return id, nil
}
