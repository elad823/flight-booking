// Command server runs the booking system's REST API and web UI. It talks to the
// workflows through Temporal and serves until it receives SIGINT or SIGTERM,
// then finishes in-flight requests before exiting.
//
// Configuration comes from environment variables:
//
//	HTTP_ADDR           address to listen on (default localhost:8080)
//	TEMPORAL_ADDRESS    Temporal frontend host:port (default localhost:7233)
//	TEMPORAL_NAMESPACE  Temporal namespace (default "default")
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.temporal.io/sdk/client"
	tlog "go.temporal.io/sdk/log"

	"flightbooking/internal/api"
	"flightbooking/web"
)

const (
	// requestTimeout bounds each request. A seat change normally takes
	// milliseconds; if the flight is unreachable the user gets a 503 instead
	// of waiting, and the next status poll shows how the change ended.
	requestTimeout = 30 * time.Second
	// shutdownTimeout is how long in-flight requests get to finish.
	shutdownTimeout = 10 * time.Second
)

func main() {
	if err := run(); err != nil {
		slog.Error("Server failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	c, err := client.Dial(client.Options{
		HostPort:  envOr("TEMPORAL_ADDRESS", client.DefaultHostPort),
		Namespace: envOr("TEMPORAL_NAMESPACE", client.DefaultNamespace),
		Logger:    tlog.NewStructuredLogger(slog.Default()),
	})
	if err != nil {
		return fmt.Errorf("connect to Temporal: %w", err)
	}
	defer c.Close()

	srv := &http.Server{
		Handler:           routes(api.NewTemporalBooking(c), slog.Default()),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      requestTimeout + 5*time.Second,
		IdleTimeout:       time.Minute,
		ErrorLog:          slog.NewLogLogger(slog.Default().Handler(), slog.LevelWarn),
	}

	// Listen before serving so a taken port fails here, before "listening"
	// is logged.
	addr := envOr("HTTP_ADDR", "localhost:8080")
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}

	// Buffered so the goroutine can exit even after run stops reading.
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	slog.Info("Server listening", "addr", ln.Addr().String())

	select {
	case err := <-serveErr:
		// Serve only returns early on a failure.
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	slog.Info("Shutting down server")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shut down: %w", err)
	}
	return nil
}

// routes returns the server's handler: the API under /api/ and the web UI
// everywhere else, each request bounded by requestTimeout.
func routes(b api.Booking, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/api/", api.NewHandler(b, log))
	mux.Handle("/", web.Handler())
	return http.TimeoutHandler(mux, requestTimeout, `{"error":"request timed out"}`)
}

// envOr returns the environment variable key, or fallback when it is unset.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
