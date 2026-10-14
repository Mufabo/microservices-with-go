// Package httpx provides a graceful HTTP server, health endpoints, and common
// middleware shared by every Snapvault service.
package httpx

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os/signal"
	"syscall"
	"time"
)

// UserHeader carries the verified user ID from the gateway to internal
// services. Internal services may trust it only because the gateway is the
// sole public entry point, verifies the caller before setting it, and removes
// any copy a client sends.
const UserHeader = "X-Snapvault-User"

// Serve runs h on addr and blocks until the process receives SIGINT or
// SIGTERM, then drains in-flight requests before returning.
func Serve(addr string, h http.Handler, log *slog.Logger) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      120 * time.Second, // generous: uploads can be large
		IdleTimeout:       90 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 1)
	go func() {
		log.Info("http server listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil &&
			!errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return err // failed to bind, e.g. port in use
	case <-ctx.Done():
		log.Info("shutdown signal received; draining")
		drainCtx, cancel := context.WithTimeout(context.Background(),
			20*time.Second)
		defer cancel()
		return srv.Shutdown(drainCtx)
	}
}
