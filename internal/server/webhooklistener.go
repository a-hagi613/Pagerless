package server

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

const (
	// webhookCertName and webhookKeyName are the file names of the serving
	// pair inside WebhookCertDir, which are also the defaults of the
	// controller-runtime webhook server.
	webhookCertName = "tls.crt"
	webhookKeyName  = "tls.key"
	// webhookShutdownTimeout bounds how long the webhook HTTP server drains
	// when the manager stops, as long as the controller-runtime server's own.
	webhookShutdownTimeout = time.Minute
	// webhookIdleTimeout matches the keep-alive timeout of the standard
	// library's default HTTP transport, as the controller-runtime server sets.
	webhookIdleTimeout = 90 * time.Second
	// webhookMaxHeaderBytes bounds the HTTP header size of a webhook request.
	webhookMaxHeaderBytes = 1 << 20
)

// webhookServer returns the webhook server that the manager runs. When a
// pre-bound listener is configured, the webhooks are served on it; otherwise
// the controller-runtime server binds the configured host and port when the
// manager starts.
func webhookServer(cfg Config) webhook.Server {
	server := webhook.NewServer(webhook.Options{
		Host:    cfg.WebhookHost,
		Port:    cfg.WebhookPort,
		CertDir: cfg.WebhookCertDir,
	})
	if cfg.WebhookListener == nil {
		return server
	}
	return &preboundWebhookServer{Server: server, listener: cfg.WebhookListener, certDir: cfg.WebhookCertDir}
}

// preboundWebhookServer serves the webhooks of a controller-runtime webhook
// server on a listener that is already bound. The manager calls its Start as
// it starts, and everything else, such as Register, is delegated to the
// wrapped server.
type preboundWebhookServer struct {
	webhook.Server
	listener net.Listener
	certDir  string
	started  atomic.Bool
}

// Start serves the wrapped server's webhooks over TLS on the pre-bound
// listener until the context is done. The server certificate and key are read
// from the certificate directory and reloaded when they change.
func (s *preboundWebhookServer) Start(ctx context.Context) error {
	certWatcher, err := certwatcher.New(
		filepath.Join(s.certDir, webhookCertName),
		filepath.Join(s.certDir, webhookKeyName),
	)
	if err != nil {
		return err
	}
	go func() {
		if err := certWatcher.Start(ctx); err != nil {
			logf.Log.Error(err, "certificate watcher error")
		}
	}()

	// The pre-bound listener carries the port of the whole serving span, so
	// nothing can take it between binding and serving. HTTP/2 is negotiated
	// through the TLS configuration, which the standard library's HTTP server
	// enables for a TLS listener.
	listener := tls.NewListener(s.listener, &tls.Config{
		NextProtos:     []string{"h2"},
		GetCertificate: certWatcher.GetCertificate,
	})
	srv := &http.Server{
		BaseContext:       func(net.Listener) context.Context { return ctx },
		Handler:           s.WebhookMux(),
		MaxHeaderBytes:    webhookMaxHeaderBytes,
		IdleTimeout:       webhookIdleTimeout,
		ReadHeaderTimeout: readHeaderTimeout,
	}

	idleConnsClosed := make(chan struct{})
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), webhookShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logf.Log.Error(err, "error shutting down the HTTP server")
		}
		close(idleConnsClosed)
	}()

	s.started.Store(true)
	if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-idleConnsClosed
	return nil
}

// Started reports whether the webhooks are being served.
func (s *preboundWebhookServer) Started() bool {
	return s.started.Load()
}
