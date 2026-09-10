package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/events"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// heartbeatEvery keeps a proxy from closing an idle stream. A quiet account is the normal
// state -- most minutes nothing changes -- and a stream that is closed for being quiet
// teaches a client to reconnect constantly.
const heartbeatEvery = 25 * time.Second

// registerStreams wires the live-update endpoints.
//
// They are chi routes rather than Huma operations, because Server-Sent Events is a response
// that never ends and Huma's model is a value returned once. The cost is that these three
// paths do their own session check; the alternative is describing an infinite response in
// OpenAPI, which would be a lie in the document rather than a gap in it.
//
// What travels is a HINT: {"topic":"portfolio"}, never numbers. A client re-reads through the
// authenticated endpoint it already uses, so there is exactly one description of what a
// portfolio is, one freshness envelope, and one place where tenancy is decided (K51).
func (d Deps) registerStreams(router chi.Router) {
	// A process wired without a subscriber simply has no streams, which is better than a
	// route that panics. The subscriber is an interface rather than a pool because holding a
	// connection is exactly what this package is forbidden to do (K15).
	if d.Events == nil {
		return
	}
	for _, topic := range []string{
		events.TopicPortfolio, events.TopicPositions, events.TopicRisk, events.TopicAlerts,
	} {
		router.Get("/stream/"+topic, d.stream(topic))
	}
}

// stream serves one topic. The subscription is per account and the filter is per topic: a
// client watching /stream/risk is not woken by every fill.
func (d Deps) stream(topic string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, ok := d.accountFromCookie(r)
		if !ok {
			http.Error(w, `{"detail":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, `{"detail":"streaming unsupported"}`, http.StatusInternalServerError)
			return
		}

		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		stream, err := d.Events.Subscribe(ctx, accountID)
		if err != nil {
			http.Error(w, `{"detail":"could not subscribe"}`, http.StatusServiceUnavailable)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		// Caddy and every other proxy in the path is told not to buffer, or the first event
		// arrives when the connection closes -- which is the moment it stops being useful.
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		heartbeat := time.NewTicker(heartbeatEvery)
		defer heartbeat.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-heartbeat.C:
				// A comment line: valid SSE, ignored by clients, and enough to keep an idle
				// connection from being reaped.
				if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
					return
				}
				flusher.Flush()
			case m, open := <-stream:
				if !open {
					// The subscription ended -- cancelled, dropped, or the connection lost.
					// Ending the response is the honest signal: the client reconnects and
					// one re-read makes it correct again.
					return
				}
				if m.Topic != topic {
					continue
				}
				payload, err := json.Marshal(m)
				if err != nil {
					continue
				}
				if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", m.Topic, payload); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	}
}

// accountFromCookie is the session check for the routes that are not Huma operations. It
// refuses exactly as requireSession does -- one 401, whatever went wrong -- so a caller
// cannot tell a missing cookie from a forged one.
func (d Deps) accountFromCookie(r *http.Request) (accountID uuid.UUID, ok bool) {
	cookie, err := r.Cookie(SessionCookieName)
	if err != nil {
		return uuid.Nil, false
	}
	id, err := d.Auth.ResolveSession(r.Context(), cookie.Value, d.Now())
	if err != nil {
		return uuid.Nil, false
	}
	return id, true
}
