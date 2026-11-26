// Command worker runs the Temporal worker for the booking system: it starts
// one FlightWorkflow per flight in the catalog, then executes workflows and
// activities until it receives SIGINT or SIGTERM.
//
// Configuration comes from environment variables:
//
//	TEMPORAL_ADDRESS      Temporal frontend host:port (default localhost:7233)
//	TEMPORAL_NAMESPACE    Temporal namespace (default "default")
//	PAYMENT_FAILURE_RATE  share of simulated payment declines, 0 to 1 (default 0.15)
package main

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"go.temporal.io/sdk/client"
	tlog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/worker"

	"flightbooking/internal/booking"
	"flightbooking/internal/workflows"
)

// stopTimeout is how long in-flight activities get to finish on shutdown.
// Activities still running after that are retried on another worker.
const stopTimeout = 15 * time.Second

func main() {
	if err := run(); err != nil {
		slog.Error("Worker failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	rate, err := failureRate()
	if err != nil {
		return err
	}
	// math/rand is fine here: it decides simulated declines, not secrets.
	payments, err := booking.NewPaymentSimulator(rate, rand.Float64) //nolint:gosec // G404: not security sensitive
	if err != nil {
		return fmt.Errorf("payment simulator: %w", err)
	}

	c, err := client.Dial(client.Options{
		HostPort:  envOr("TEMPORAL_ADDRESS", client.DefaultHostPort),
		Namespace: envOr("TEMPORAL_NAMESPACE", client.DefaultNamespace),
		Logger:    tlog.NewStructuredLogger(slog.Default()),
	})
	if err != nil {
		return fmt.Errorf("connect to Temporal: %w", err)
	}
	defer c.Close()

	w := worker.New(c, workflows.TaskQueue, worker.Options{WorkerStopTimeout: stopTimeout})
	w.RegisterWorkflow(workflows.FlightWorkflow)
	w.RegisterWorkflow(workflows.OrderWorkflow)
	w.RegisterActivity(&workflows.Activities{Client: c, Payments: payments})

	if err := w.Start(); err != nil {
		return fmt.Errorf("start worker: %w", err)
	}
	defer w.Stop()

	if err := workflows.StartFlights(ctx, c, booking.Catalog()); err != nil {
		return err
	}
	slog.Info("Worker running", "taskQueue", workflows.TaskQueue, "paymentFailureRate", rate)

	<-ctx.Done()
	slog.Info("Shutting down worker")
	return nil
}

// failureRate reads PAYMENT_FAILURE_RATE, defaulting to the spec's 15%.
func failureRate() (float64, error) {
	v := os.Getenv("PAYMENT_FAILURE_RATE")
	if v == "" {
		return 0.15, nil
	}
	rate, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("parse PAYMENT_FAILURE_RATE %q: %w", v, err)
	}
	return rate, nil
}

// envOr returns the environment variable key, or fallback when it is unset.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
