// Command worker is the only process that writes to the ledger (ARCHITECTURE.md §1). It
// lists the integrations there is anything to run for, claims each one's lease, and runs a
// supervisor per claim: the live stream, the historical backfill beside it, and the gap
// replay between them.
//
// One limiter for the whole process, because Binance's limits are per IP rather than per
// key (K24). One lease per integration, because two writers on one integration means two
// folds racing on one cursor (L6).
//
// Like the api it connects as plimsoll_app, never the table owner (K15).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/crypto"
	"github.com/Contictus/plimsoll/backend/internal/exchange/binance"
	"github.com/Contictus/plimsoll/backend/internal/obs"
	"github.com/Contictus/plimsoll/backend/internal/ratelimit"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/worker"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	serviceName   = "plimsoll-worker"
	shutdownGrace = 15 * time.Second

	// exchangeName is the alias namespace symbols resolve in. M2 is spot Binance only; a
	// second venue arrives with its own adapter and its own namespace.
	exchangeName = "binance"

	// bootstrapWeightPerMinute is the budget used for the single unsigned exchangeInfo call
	// that reads the real ceiling. Deliberately small: it is spent before we know what the
	// venue allows, and being wrong in this direction costs a wait rather than a ban.
	bootstrapWeightPerMinute = 100

	// ratelimitBackfill is ratelimit.PriorityBackfill, named here so supervise.go does not
	// have to import the package for one constant.
	ratelimitBackfill = ratelimit.PriorityBackfill
)

func main() {
	log := obs.NewLogger(os.Stdout, slog.LevelInfo)
	if err := run(log); err != nil {
		log.Error("worker exited", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownTracing, err := obs.SetupTracing(ctx, serviceName)
	if err != nil {
		return err
	}
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := shutdownTracing(flushCtx); err != nil {
			log.Error("flushing traces", "error", err)
		}
	}()

	keys, err := crypto.NewEnvFileProvider()
	if err != nil {
		return err
	}

	dsn := os.Getenv("PLIMSOLL_APP_DSN")
	if dsn == "" {
		return errors.New("PLIMSOLL_APP_DSN is not set")
	}
	pool, err := store.NewPool(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	// The assignments first, and the exchange only if there are any. A process with nothing
	// to run has no business calling a venue, and starting one that does would spend weight
	// on the shared IP budget to learn a ceiling it will not use (K24).
	assignments, err := worker.ActiveIntegrations(ctx, pool)
	if err != nil {
		return err
	}
	if len(assignments) == 0 {
		log.Info("worker ready", "integrations", 0,
			"note", "no integration has a credential yet; not contacting the exchange")
		<-ctx.Done()
		log.Info("worker shutting down")
		return nil
	}

	d, err := connect(ctx, pool, keys, log)
	if err != nil {
		return err
	}
	log.Info("worker ready", "owner_id", d.ownerID, "integrations", len(assignments),
		"symbols", len(d.symbols))

	var wg sync.WaitGroup
	for _, assignment := range assignments {
		wg.Add(1)
		go func() {
			defer wg.Done()
			supervise(ctx, d, assignment)
		}()
	}

	<-ctx.Done()
	log.Info("worker shutting down")
	wg.Wait()
	return nil
}

// connect reads the two things every supervisor in this process shares: the venue's own
// weight ceiling, and the list of symbols discovery sweeps.
//
// The ceiling is read rather than hardcoded. A hardcoded number is a number that will be
// wrong after Binance changes theirs, and the way we would find out is the ban (K24).
func connect(
	ctx context.Context, pool *pgxpool.Pool, keys crypto.KeyProvider, log *slog.Logger,
) (deps, error) {
	restURL := envOr("PLIMSOLL_BINANCE_REST_URL", "https://api.binance.com")

	// Unsigned, so no credential is needed and none is used: exchangeInfo carries no
	// account data, and signing it would mint a signature on every start for nothing.
	bootstrapLimiter, err := ratelimit.New(ratelimit.Config{
		SharedPerMinute:         bootstrapWeightPerMinute,
		PerIntegrationPerMinute: bootstrapWeightPerMinute,
	}, ratelimit.SystemClock{})
	if err != nil {
		return deps{}, err
	}
	bootstrap, err := binance.New(binance.Config{
		IntegrationID: uuid.New(), // a throwaway budget for one unsigned call
		Limiter:       bootstrapLimiter,
		BaseURL:       restURL,
	})
	if err != nil {
		return deps{}, err
	}

	info, err := bootstrap.ExchangeInfo(ctx)
	if err != nil {
		return deps{}, fmt.Errorf("read exchangeInfo: %w", err)
	}
	perMinute, err := binance.RequestWeightPerMinute(info)
	if err != nil {
		return deps{}, err
	}
	symbols, err := binance.SpotSymbols(info)
	if err != nil {
		return deps{}, err
	}

	// The per-integration ceiling defaults to the shared one, which makes that tier
	// accounting rather than a binding constraint: with a handful of integrations the real
	// limit is the IP's, and fairness between them is what the priority tiers are for. An
	// operator running many integrations on one IP raises the pressure by lowering this.
	perIntegration := perMinute
	if configured := envInt("PLIMSOLL_BINANCE_PER_INTEGRATION_WEIGHT"); configured > 0 {
		perIntegration = configured
	}
	limiter, err := ratelimit.New(ratelimit.Config{
		SharedPerMinute:         perMinute,
		PerIntegrationPerMinute: perIntegration,
	}, ratelimit.SystemClock{})
	if err != nil {
		return deps{}, err
	}

	return deps{
		pool:    pool,
		keys:    keys,
		limiter: limiter,
		symbols: symbols,
		restURL: restURL,
		wsURL:   envOr("PLIMSOLL_BINANCE_WS_URL", binance.SpotStreamURL),
		// Process-unique, minted per start. Never a hostname: two processes on one host
		// would then share an identity and each would believe it held the other's lease.
		ownerID: uuid.NewString(),
		log:     log,
	}, nil
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envInt(name string) int {
	value, err := strconv.Atoi(os.Getenv(name))
	if err != nil {
		return 0
	}
	return value
}
