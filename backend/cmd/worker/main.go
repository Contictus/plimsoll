// Command worker is the only process that will ever write to the ledger (ARCHITECTURE.md
// §1). In M0 it ingests nothing: it exists so the image, the topology and the graceful
// shutdown path are proven before M2 puts the sole ledger writer inside it.
//
// Like the api it connects as plimsoll_app, never the table owner (K15).
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/crypto"
	"github.com/Contictus/plimsoll/backend/internal/obs"
	"github.com/Contictus/plimsoll/backend/internal/store"
)

const (
	serviceName   = "plimsoll-worker"
	shutdownGrace = 15 * time.Second
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

	// Same fail-fast as cmd/api, through the same constructor, so the two binaries cannot
	// disagree about what a valid master key is (K25).
	if _, err := crypto.NewEnvFileProvider(); err != nil {
		return err
	}

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

	dsn := os.Getenv("PLIMSOLL_APP_DSN")
	if dsn == "" {
		return errors.New("PLIMSOLL_APP_DSN is not set")
	}
	// The pool is opened even though nothing uses it yet: it is what makes the container
	// fail loudly on a bad DSN or an unreachable database, which is the whole reason this
	// shell exists before there is work for it to do.
	pool, err := store.NewPool(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	log.Info("worker ready", "ingests", "nothing until M2")
	<-ctx.Done()
	log.Info("worker shutting down")
	return nil
}
