package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/instrument"
	"github.com/Contictus/plimsoll/backend/internal/marketdata"
	"github.com/jackc/pgx/v5/pgxpool"
)

// resnapshotEvery is how often the REST snapshot is repeated.
//
// Not an optimisation and not a heartbeat: F7 says the stream carries only symbols that
// changed, so a pair that has not traded since the last snapshot is a price ageing quietly.
// The snapshot is what puts a floor under how stale any price can get, and at weight 4 for
// the whole market it is affordable to put that floor low.
const resnapshotEvery = 15 * time.Minute

// runPrices keeps price_ticks current for as long as the process lives.
//
// One feed per process, never one per integration. Prices are the same for every account,
// and a feed per account would multiply an IP-wide budget by the number of users (K24) --
// and would connect the same public socket N times to learn the same number.
//
// It returns only when ctx is cancelled. Every failure inside is retried, because a price
// feed that gives up leaves the portfolio valuing itself on whatever it saw last, with only
// price_stale to say so.
func runPrices(ctx context.Context, pool *pgxpool.Pool, limiter marketdata.Limiter, log *slog.Logger) {
	client, err := marketdata.NewClient("", limiter, nil)
	if err != nil {
		log.Error("price feed not started", "error", err)
		return
	}

	// Resolved as of now, which is the right instant for a live feed: the aliases in force
	// are the ones the venue is streaming under (L8).
	symbols, err := marketdata.LoadSymbols(ctx, pool, exchangeName, instrument.MarketSpot, time.Now().UTC())
	if err != nil {
		log.Error("price feed not started", "error", err)
		return
	}
	if len(symbols) == 0 {
		// Not a failure. An empty registry means nothing has been curated yet, and
		// recording prices for instruments that do not exist would fill the table with
		// rows nothing can join to.
		log.Info("price feed idle", "note", "no spot instrument aliases are curated yet")
		return
	}

	stream := marketdata.NewStream(marketdata.StreamConfig{})
	recorder := marketdata.NewRecorder(client, stream, marketdata.Writer{DB: pool}, symbols)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := stream.Run(ctx); err != nil && ctx.Err() == nil {
			log.Error("price stream stopped", "error", err)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		snapshotLoop(ctx, recorder, log)
	}()

	log.Info("price feed started", "symbols", len(symbols), "url", marketdata.DefaultStreamURL)
	if err := recorder.Run(ctx); err != nil && ctx.Err() == nil {
		log.Error("price recorder stopped", "error", err)
	}

	_ = stream.Close()
	wg.Wait()
}

// snapshotLoop takes the first snapshot immediately and repeats it on a ticker, so a symbol
// that never trades still has a price with a bounded age.
func snapshotLoop(ctx context.Context, recorder *marketdata.Recorder, log *slog.Logger) {
	ticker := time.NewTicker(resnapshotEvery)
	defer ticker.Stop()

	for {
		recorded, err := recorder.Snapshot(ctx)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			// Warn rather than stop. The stream is still delivering everything that moves;
			// a failed snapshot costs the symbols that did not move, which is exactly what
			// price_stale exists to report (L11).
			log.Warn("price snapshot failed; the stream is still running", "error", err)
		default:
			log.Debug("price snapshot recorded", "instruments", recorded)
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
