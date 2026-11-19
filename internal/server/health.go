package server

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/a-hagi613/pagerless/internal/credentials"
)

// healthReadHeaderTimeout bounds how long a probe may take to send headers.
const healthReadHeaderTimeout = 5 * time.Second

// readiness reports the subsystems that are not configured yet. A degraded
// subsystem is a normal unfinished installation, not a fatal fault: the server
// serves, stays in the Service, and routes the Portal to onboarding. Only an
// unusable process fails the probe.
type readiness struct {
	// Credentials is nil when the settings store is not configured, in which
	// case every credential-dependent subsystem is reported as degraded.
	Credentials *credentials.Store
}

// degraded returns the names of the unconfigured subsystems.
func (r readiness) degraded(ctx context.Context) []string {
	names := make([]string, 0, 2)
	if r.Credentials == nil {
		return []string{"ai", "repository"}
	}
	view, err := r.Credentials.View(ctx)
	if err != nil {
		return []string{"ai", "repository"}
	}
	if !view.Fields[credentials.KeyAIBaseURL].Configured || !view.Fields[credentials.KeyAIAPIKey].Configured {
		names = append(names, "ai")
	}
	if !view.Fields[credentials.KeyGitHubRepo].Configured || !view.Fields[credentials.KeyGitHubToken].Configured {
		names = append(names, "repository")
	}
	return names
}

// handler serves /healthz and /readyz. /healthz answers whether the process is
// alive; /readyz always answers 200 once the manager runs, and its body lists
// the subsystems that still need onboarding.
func (r readiness) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, req *http.Request) {
		degraded := r.degraded(req.Context())
		if degraded == nil {
			degraded = []string{}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(struct {
			Ready    bool     `json:"ready"`
			Degraded []string `json:"degraded"`
		}{Ready: true, Degraded: degraded})
	})
	return mux
}

// serveHealth serves the probes on their own listener until the manager stops.
func serveHealth(listener net.Listener, handler http.Handler) manager.Runnable {
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: healthReadHeaderTimeout}
	return manager.RunnableFunc(func(ctx context.Context) error {
		errc := make(chan error, 1)
		go func() { errc <- srv.Serve(listener) }()
		select {
		case err := <-errc:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
			defer cancel()
			return srv.Shutdown(shutdownCtx)
		}
	})
}
