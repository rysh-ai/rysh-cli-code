package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// startMetricsServer serves the Prometheus exporter on addr at /metrics in a
// background goroutine. The returned function gracefully shuts it down. The
// daemon's process exit also tears it down. Follow-up 3b.
func startMetricsServer(addr string, handler http.Handler) func(context.Context) error {
	mux := http.NewServeMux()
	mux.Handle("/metrics", handler)
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Warn("rysh: metrics server stopped", "addr", addr, "err", err)
		}
	}()
	slog.Info("rysh: prometheus metrics exporter listening", "addr", addr, "path", "/metrics")
	return srv.Shutdown
}
