package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

const healthProbeTimeout = 3 * time.Second

// healthProbeURL points at the address this process bound, not at a hard-coded port. A
// container started with a different PLIMSOLL_HTTP_ADDR would otherwise probe a port
// nothing is listening on -- or worse, one something else is.
func healthProbeURL(addr string) string {
	if addr == "" {
		addr = defaultAddr
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		// An address with no colon is not one the server could have bound either; fall
		// back so the probe fails loudly against the default rather than panicking.
		port = defaultAddr[1:]
	}
	return "http://127.0.0.1:" + port + "/healthz"
}

// probeHealth is what `plimsoll healthcheck` runs inside the container. Only 200 counts:
// /healthz answers 503 when Postgres is unreachable, and a container that stays healthy
// through that removes the one signal an operator has (L11).
func probeHealth(url string) error {
	ctx, cancel := context.WithTimeout(context.Background(), healthProbeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("health probe: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health probe: %s returned %d", url, resp.StatusCode)
	}
	return nil
}
