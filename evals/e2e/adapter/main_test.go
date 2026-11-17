package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestRelayHealthcheckUsesTheContainerLoopbackListener(t *testing.T) {
	t.Parallel()
	var status atomic.Int32
	status.Store(http.StatusNoContent)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Errorf("healthcheck path = %s, want /healthz", r.URL.Path)
		}
		w.WriteHeader(int(status.Load()))
	}))
	t.Cleanup(server.Close)
	if err := run([]string{healthcheckCommand, "--url=" + server.URL + "/healthz"}); err != nil {
		t.Fatalf("healthy loopback listener: %v", err)
	}
	status.Store(http.StatusServiceUnavailable)
	if err := run([]string{healthcheckCommand, "--url=" + server.URL + "/healthz"}); err == nil {
		t.Fatal("an unhealthy relay passed its liveness check")
	}
	if err := run([]string{healthcheckCommand, "--url=http://example.com/healthz"}); err == nil {
		t.Fatal("the healthcheck accepted a remote endpoint")
	}
}
