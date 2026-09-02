// Command api serves the read-side HTTP surface. It connects as plimsoll_app -- never the
// table owner, which would bypass RLS (K15) -- and it never writes ledger events: the
// worker is the sole writer (ARCHITECTURE.md §1).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/auth"
	"github.com/Contictus/plimsoll/backend/internal/config"
	"github.com/Contictus/plimsoll/backend/internal/httpapi"
	"github.com/Contictus/plimsoll/backend/internal/obs"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

const (
	serviceName       = "plimsoll-api"
	defaultAddr       = ":8000"
	sessionTTL        = 24 * time.Hour
	readHeaderTimeout = 10 * time.Second
	shutdownGrace     = 15 * time.Second
)

func main() {
	// `plimsoll healthcheck` probes the running server and exits. It is how compose
	// health-checks a distroless image, which has no shell and no curl to do it with.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		if err := probeHealth(healthProbeURL(os.Getenv("PLIMSOLL_HTTP_ADDR"))); err != nil {
			fmt.Fprintln(os.Stderr, "plimsoll:", err)
			os.Exit(1)
		}
		return
	}

	log := obs.NewLogger(os.Stdout, slog.LevelInfo)
	if err := run(log); err != nil {
		log.Error("api exited", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	// Signal handling is installed before anything else is opened, so a Ctrl-C during
	// startup still unwinds through the same path as one during serving.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := config.CheckMasterKEK(os.Getenv("PLIMSOLL_MASTER_KEK")); err != nil {
		return err
	}

	// Tracing is installed before the pool so that a slow or failing first connection is
	// itself visible in a trace. With no collector configured this is a no-op.
	shutdownTracing, err := obs.SetupTracing(ctx, serviceName)
	if err != nil {
		return err
	}
	defer func() {
		// A fresh context: ctx is cancelled by the time this runs, and the exporter needs
		// a live one to flush the last batch.
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
	pool, err := store.NewPool(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	addr := os.Getenv("PLIMSOLL_HTTP_ADDR")
	if addr == "" {
		addr = defaultAddr
	}
	router := httpapi.NewRouter(httpapi.Deps{
		DB:   pool,
		Auth: auth.NewService(store.New(pool), pool, sessionTTL),
		Now:  time.Now,
	})

	srv := &http.Server{
		Addr: addr,
		// otelhttp wraps the whole router, so every request gets a span whether or not
		// its handler remembers to start one.
		Handler: otelhttp.NewHandler(router, serviceName),
		// Without this a client can hold a connection open indefinitely by dribbling
		// headers; it is the one timeout with no safe default.
		ReadHeaderTimeout: readHeaderTimeout,
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Info("api listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		log.Info("api shutting down")
	}

	// A fresh context: ctx is already cancelled, and Shutdown needs a live deadline to
	// let in-flight requests finish.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return <-serveErr
}
