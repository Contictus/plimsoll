package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/freshness"
	"github.com/Contictus/plimsoll/backend/internal/portfolio"
	"github.com/Contictus/plimsoll/backend/internal/valuation"
	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
)

// eventBody is one ledger event as it crosses the API. Money is a string (L1); the optional
// fields are empty strings rather than nulls, because "" and "0" are different claims and a
// client rendering a table wants both to be renderable without a branch.
type eventBody struct {
	// Seq is lineage and debugging, never a cursor: identity values are assigned before
	// commit and can be observed out of order (K20, K41).
	Seq int64 `json:"seq"`

	IntegrationID uuid.UUID `json:"integration_id"`
	VenueEventID  string    `json:"venue_event_id" doc:"the exchange's identity for this event, and ours"`
	VenueSequence int64     `json:"venue_sequence"`

	// Source is who saw it first, never identity: REST and the stream report one trade
	// under one venue_event_id (K19, L5).
	Source string `json:"source" enum:"rest,stream"`

	EventType string `json:"event_type"`
	Side      string `json:"side"`

	Instrument string `json:"instrument" doc:"set when the event names a pair"`
	Asset      string `json:"asset"      doc:"set when the event moves a single asset"`

	Quantity string `json:"quantity"`
	Price    string `json:"price"`
	Fee      string `json:"fee"`
	FeeAsset string `json:"fee_asset"`

	EventTime  time.Time `json:"event_time"`
	IngestedAt time.Time `json:"ingested_at"`
}

// stepBody is one event and the position it produced. The `resulting` half is what makes
// this endpoint an answer rather than a log: it comes from the same position.Apply the
// projector runs, so a lineage that disagrees with a position is a finding and not two
// implementations drifting apart.
type stepBody struct {
	Event     eventBody      `json:"event"`
	Resulting resultingState `json:"resulting"`

	// Raw is the exchange payload, kept forever (L15). It is here and not in the listing
	// because this is where someone is asking why a number is what it is, and the payload
	// is the last word on that.
	Raw json.RawMessage `json:"raw"`
}

type resultingState struct {
	Quantity      string `json:"quantity"`
	AvgEntryPrice string `json:"avg_entry_price"`
	RealizedPnL   string `json:"realized_pnl"`
}

type lineageBody struct {
	freshness.Envelope
	Position    positionBody `json:"position"`
	Steps       []stepBody   `json:"steps" doc:"the most recent events, oldest first"`
	TotalEvents int          `json:"total_events" doc:"how many events folded into this position in all"`

	// Prices are the legs this position is valued through, with the hops that produced each
	// one. M3 shipped this field present and empty exactly so that filling it here would not
	// change the shape a client parses (ARCHITECTURE.md section 5).
	//
	// It is the half of the audit trail the ledger cannot supply: the steps say what was
	// traded, the path says what it is worth and how we got there. Multiplying a path's rates
	// reproduces its price exactly, which is what makes it checkable rather than merely
	// shown (K11, K17).
	Prices []legPriceBody `json:"prices"`
}

// hopBody is one edge of a price path: what it converted, through which instrument, at what
// rate, and whether that rate was the instrument's own or its reciprocal.
type hopBody struct {
	FromAsset    int64      `json:"from_asset_id"`
	ToAsset      int64      `json:"to_asset_id"`
	InstrumentID int64      `json:"instrument_id" doc:"zero when the leg was an assumed peg"`
	Rate         string     `json:"rate"`
	Inverted     bool       `json:"inverted"      doc:"the instrument quotes the other direction"`
	Assumed      bool       `json:"assumed"       doc:"a peg, not a traded price (K17)"`
	ObservedAt   *time.Time `json:"observed_at"`
}

type legPriceBody struct {
	AssetID    int64      `json:"asset_id"`
	PriceUSD   string     `json:"price_usd"`
	AssumedPeg bool       `json:"assumed_peg"`
	ObservedAt *time.Time `json:"observed_at"`
	Path       []hopBody  `json:"path"`
}

type transactionsBody struct {
	freshness.Envelope
	Events []eventBody `json:"events"`

	// NextCursor is empty at the end of what exists now. "Now" is the honest qualifier: a
	// backfill inserts events behind a cursor already passed, which is why this response
	// also carries backfill_incomplete while history is loading (K41).
	NextCursor string `json:"next_cursor"`
}

func renderEvent(tx portfolio.Transaction) eventBody {
	return eventBody{
		Seq:           tx.Seq,
		IntegrationID: tx.IntegrationID,
		VenueEventID:  tx.VenueEventID,
		VenueSequence: tx.VenueSequence,
		Source:        tx.Source,
		EventType:     tx.EventType,
		Side:          tx.Side,
		Instrument:    tx.Instrument,
		Asset:         tx.Asset,
		Quantity:      tx.Quantity,
		Price:         tx.Price,
		Fee:           tx.Fee,
		FeeAsset:      tx.FeeAsset,
		EventTime:     tx.EventTime,
		IngestedAt:    tx.IngestedAt,
	}
}

type lineageInput struct {
	ID    string `path:"id"    doc:"<integration_id>.<instrument_id>"`
	Steps int    `query:"steps" doc:"how many of the most recent events to return (default 200, max 1000)"`
}

type transactionsInput struct {
	Cursor string `query:"cursor" doc:"opaque; from a previous response's next_cursor"`
	Limit  int    `query:"limit"  doc:"default 100, max 500"`
}

// registerLineage wires the two endpoints that open a number down to its events.
//
// GET /positions/{id}/lineage is the product thesis in endpoint form (ARCHITECTURE.md
// section 10, rule 6): every event that produced this position, and the state each one left
// behind, replayed through the engine that produced the stored number in the first place.
func (d Deps) registerLineage(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "get-position-lineage",
		Method:      http.MethodGet,
		Path:        "/positions/{id}/lineage",
		Summary:     "The events that produced this position, and the state each one left",
	}, func(ctx context.Context, in *lineageInput) (*struct{ Body lineageBody }, error) {
		accountID, ok := AccountFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("unauthorized")
		}

		lineage, err := portfolio.LoadLineage(ctx, d.DB, accountID, in.ID, in.Steps, d.window())
		switch {
		case errors.Is(err, portfolio.ErrMalformedID):
			return nil, huma.Error400BadRequest("malformed position id")
		case errors.Is(err, portfolio.ErrNoSuchPosition):
			// The same answer for a position that does not exist and one that is somebody
			// else's, for the same reason /positions/{id} gives it.
			return nil, huma.Error404NotFound("no such position")
		case err != nil:
			return nil, err
		}

		steps := make([]stepBody, 0, len(lineage.Steps))
		for _, s := range lineage.Steps {
			steps = append(steps, stepBody{
				Event: renderEvent(portfolio.Transaction{
					Seq:           s.Event.Seq,
					IntegrationID: s.Event.IntegrationID,
					VenueEventID:  s.Event.VenueEventID,
					VenueSequence: s.Event.VenueSequence,
					Source:        s.Event.Source,
					EventType:     string(s.Event.EventType),
					Side:          string(s.Event.Side),
					Instrument:    lineage.Position.Symbol,
					Quantity:      nullText(s.Event.Quantity),
					Price:         nullText(s.Event.Price),
					Fee:           nullText(s.Event.Fee),
					FeeAsset:      s.Event.FeeAsset,
					EventTime:     s.Event.EventTime,
					IngestedAt:    s.Event.IngestedAt,
				}),
				Resulting: resultingState{
					Quantity:      s.Quantity,
					AvgEntryPrice: s.AvgEntryPrice,
					RealizedPnL:   s.RealizedPnL,
				},
				Raw: s.Event.Raw,
			})
		}

		return &struct{ Body lineageBody }{Body: lineageBody{
			Envelope:    freshness.Envelope{AsOf: lineage.AsOf, Freshness: lineage.Freshness},
			Position:    renderPosition(lineage.Position),
			Steps:       steps,
			TotalEvents: lineage.TotalEvents,
			Prices:      renderPrices(lineage.Prices),
		}}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-transactions",
		Method:      http.MethodGet,
		Path:        "/transactions",
		Summary:     "The account's ledger, cursor-paginated in canonical order",
	}, func(ctx context.Context, in *transactionsInput) (*struct{ Body transactionsBody }, error) {
		accountID, ok := AccountFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("unauthorized")
		}

		page, err := portfolio.LoadTransactions(ctx, d.DB, accountID, in.Cursor, in.Limit,
			d.Now(), d.LeaseTTL)
		if errors.Is(err, portfolio.ErrMalformedCursor) {
			// Rejected rather than silently restarting at page one: a client paging with a
			// corrupted cursor and served the first page again would loop forever.
			return nil, huma.Error400BadRequest("malformed cursor")
		}
		if err != nil {
			return nil, err
		}

		events := make([]eventBody, 0, len(page.Events))
		for _, e := range page.Events {
			events = append(events, renderEvent(e))
		}
		return &struct{ Body transactionsBody }{Body: transactionsBody{
			Envelope:   freshness.Envelope{AsOf: page.AsOf, Freshness: page.Freshness},
			Events:     events,
			NextCursor: page.NextCursor,
		}}, nil
	})
}

// renderPrices turns the run's legs into the response's audit block. Every number is a
// string (L1), including the rates: a client parsing a hop into a float would reproduce the
// path to within a rounding error, and "within a rounding error" is not a proof.
func renderPrices(prices []valuation.RecordedPrice) []legPriceBody {
	out := make([]legPriceBody, 0, len(prices))
	for _, p := range prices {
		leg := legPriceBody{
			AssetID: p.AssetID, PriceUSD: p.USD.String(), AssumedPeg: p.AssumedPeg,
			Path: make([]hopBody, 0, len(p.Path)),
		}
		if !p.ObservedAt.IsZero() {
			observed := p.ObservedAt
			leg.ObservedAt = &observed
		}
		for _, h := range p.Path {
			hop := hopBody{
				FromAsset: h.From, ToAsset: h.To, InstrumentID: h.InstrumentID,
				Rate: h.Rate.String(), Inverted: h.Inverted, Assumed: h.Assumed,
			}
			if !h.ObservedAt.IsZero() {
				observed := h.ObservedAt
				hop.ObservedAt = &observed
			}
			leg.Path = append(leg.Path, hop)
		}
		out = append(out, leg)
	}
	return out
}
