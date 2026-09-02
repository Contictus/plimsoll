// Command api serves the read-side HTTP surface. It connects as plimsoll_app -- never the
// table owner, which would bypass RLS (K15) -- and it never writes ledger events: the
// worker is the sole writer (ARCHITECTURE.md §1).
package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/auth"
	"github.com/Contictus/plimsoll/backend/internal/httpapi"
	"github.com/Contictus/plimsoll/backend/internal/store"
)

const (
	defaultAddr       = ":8000"
	sessionTTL        = 24 * time.Hour
	readHeaderTimeout = 10 * time.Second
	shutdownGrace     = 15 * time.Second
	masterKEKBytes    = 32
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
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

	if err := checkMasterKEK(os.Getenv("PLIMSOLL_MASTER_KEK")); err != nil {
		// Fail at startup rather than at the first credential decrypt in M2. A process
		// that boots and then cannot read any integration is far harder to diagnose.
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

	addr := os.Getenv("PLIMSOLL_HTTP_ADDR")
	if addr == "" {
		addr = defaultAddr
	}
	srv := &http.Server{
		Addr: addr,
		Handler: httpapi.NewRouter(httpapi.Deps{
			DB:   pool,
			Auth: auth.NewService(store.New(pool), pool, sessionTTL),
			Now:  time.Now,
		}),
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

// checkMasterKEK validates the envelope-encryption master key without ever holding it in
// a package-level variable or logging it (K25, L13). M0 only proves it is present and
// well-formed; the key itself is used from M2.
func checkMasterKEK(encoded string) error {
	if encoded == "" {
		return errors.New("PLIMSOLL_MASTER_KEK is not set")
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		// The decode error would not carry key material, but the value is a secret and
		// nothing derived from it belongs in an error string.
		return errors.New("PLIMSOLL_MASTER_KEK is not valid base64")
	}
	if len(raw) != masterKEKBytes {
		return fmt.Errorf("PLIMSOLL_MASTER_KEK must decode to %d bytes, got %d",
			masterKEKBytes, len(raw))
	}
	return nil
}
