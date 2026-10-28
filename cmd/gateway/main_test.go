package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProbeNormalizesWildcardAddressAndIgnoresProxy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/healthz", r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(t, err)
	t.Setenv("GATEWAY_PROVIDER", "mock")
	t.Setenv("GATEWAY_HTTP_ADDR", ":"+port)
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("NO_PROXY", "")
	require.NoError(t, probe())
	t.Setenv("GATEWAY_HTTP_ADDR", "0.0.0.0:"+port)
	require.NoError(t, probe())
}
func TestProbeRejectsRedirectsAndFailures(t *testing.T) {
	for _, status := range []int{http.StatusFound, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Location", "/healthy")
				w.WriteHeader(status)
			}))
			defer server.Close()
			t.Setenv("GATEWAY_PROVIDER", "mock")
			t.Setenv("GATEWAY_HTTP_ADDR", server.Listener.Addr().String())
			require.ErrorContains(t, probe(), "unexpected status")
		})
	}
}
