package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// The runtime image is distroless: no shell, no curl, nothing for compose to run a probe
// with. The binary probes itself, so this is the only health check the container has --
// worth a test that it reports the two outcomes correctly.
func TestProbeHealthAcceptsOnly200(t *testing.T) {
	tests := []struct {
		name   string
		status int
		wantOK bool
	}{
		{"healthy", http.StatusOK, true},
		{"database unreachable", http.StatusServiceUnavailable, false},
		{"anything else", http.StatusInternalServerError, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(tc.status) }))
			defer srv.Close()

			err := probeHealth(srv.URL + "/healthz")
			if tc.wantOK {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestProbeHealthFailsWhenNothingIsListening(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	url := srv.URL + "/healthz"
	srv.Close() // the port is now closed; the probe must fail rather than hang

	require.Error(t, probeHealth(url))
}

// The probe has to hit the address the server actually bound, or a container with a
// non-default PLIMSOLL_HTTP_ADDR reports healthy while nothing is listening.
func TestHealthProbeURLFollowsTheConfiguredAddress(t *testing.T) {
	require.Equal(t, "http://127.0.0.1:8000/healthz", healthProbeURL(""))
	require.Equal(t, "http://127.0.0.1:9999/healthz", healthProbeURL(":9999"))
	require.Equal(t, "http://127.0.0.1:9999/healthz", healthProbeURL("0.0.0.0:9999"))
}
