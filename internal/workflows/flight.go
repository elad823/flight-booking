// Package workflows holds the Temporal workflows and activities of the
// booking system. Business rules live in package booking; this package wires
// them to Temporal.
package workflows

import (
	"context"
	"errors"
	"fmt"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"flightbooking/internal/booking"
)

// TaskQueue is the Temporal task queue the worker polls and clients start
// workflows on.
const TaskQueue = "booking"

// Names of the FlightWorkflow's Updates and Query.
const (
	UpdateHold     = "hold"
	UpdateRelease  = "release"
	UpdateConfirm  = "confirm"
	QueryAvailable = "available"
)

// Error types of the ApplicationErrors FlightWorkflow returns. Callers on
// the other side of Temporal match on these, because errors.Is does not
// survive serialization.
const (
	ErrTypeSeatUnavailable = "SeatUnavailable"
	ErrTypeInvalidRequest  = "InvalidSeatRequest"
)

// FlightWorkflowID returns the workflow ID of a flight's FlightWorkflow.
func FlightWorkflowID(flightID string) string {
	return "flight-" + flightID
}

// FlightInput is the input of FlightWorkflow. Continue-As-New passes the
// current seat map to the next run in the same shape.
type FlightInput struct {
	FlightID  string            `json:"flightId"`
	Inventory booking.Inventory `json:"inventory"`
}

// HoldRequest asks to make Seats the order's complete selection until Until.
type HoldRequest struct {
	OrderID string    `json:"orderId"`
	Seats   []string  `json:"seats"`
	Until   time.Time `json:"until"`
}

// ConfirmRequest asks to book Seats for the order.
type ConfirmRequest struct {
	OrderID string   `json:"orderId"`
	Seats   []string `json:"seats"`
}

// FlightWorkflow owns the seat map of one flight. It runs until cancelled,
// serving Updates and Queries, and continues as new when the server
// suggests it so its history stays bounded.
//
// Temporal runs one workflow's code one step at a time, and no handler here
// blocks, so each Update applies atomically without a lock.
func FlightWorkflow(ctx workflow.Context, in FlightInput) error {
	inv := &in.Inventory

	// The caller passes the current time. workflow.Now only advances when the
	// workflow processes an event, and a Query is not an event, so on a flight
	// with no recent Updates workflow.Now would be stale and show lapsed holds
	// as taken. Updates use workflow.Now: they are events, so it is fresh, and
	// writes must all judge seats by the same clock.
	err := workflow.SetQueryHandler(ctx, QueryAvailable, func(now time.Time) ([]string, error) {
		return inv.Available(now), nil
	})
	if err != nil {
		return fmt.Errorf("register %s query: %w", QueryAvailable, err)
	}

	err = workflow.SetUpdateHandlerWithOptions(ctx, UpdateHold,
		func(ctx workflow.Context, req HoldRequest) error {
			return seatError(inv.Hold(req.OrderID, req.Seats, workflow.Now(ctx), req.Until))
		},
		workflow.UpdateHandlerOptions{
			Validator: func(ctx workflow.Context, req HoldRequest) error {
				return seatError(inv.CheckHold(req.OrderID, req.Seats, workflow.Now(ctx)))
			},
		})
	if err != nil {
		return fmt.Errorf("register %s update: %w", UpdateHold, err)
	}

	err = workflow.SetUpdateHandler(ctx, UpdateRelease,
		func(_ workflow.Context, orderID string) error {
			inv.Release(orderID)
			return nil
		})
	if err != nil {
		return fmt.Errorf("register %s update: %w", UpdateRelease, err)
	}

	err = workflow.SetUpdateHandlerWithOptions(ctx, UpdateConfirm,
		func(_ workflow.Context, req ConfirmRequest) error {
			return seatError(inv.Confirm(req.OrderID, req.Seats))
		},
		workflow.UpdateHandlerOptions{
			Validator: func(req ConfirmRequest) error {
				return seatError(inv.CheckConfirm(req.OrderID, req.Seats))
			},
		})
	if err != nil {
		return fmt.Errorf("register %s update: %w", UpdateConfirm, err)
	}

	// Wait for the server to suggest Continue-As-New, and for running
	// handlers to finish so no accepted Update is cut off.
	err = workflow.Await(ctx, func() bool {
		return workflow.GetInfo(ctx).GetContinueAsNewSuggested() && workflow.AllHandlersFinished(ctx)
	})
	if err != nil {
		return fmt.Errorf("wait for continue-as-new: %w", err)
	}
	return workflow.NewContinueAsNewError(ctx, FlightWorkflow, FlightInput{
		FlightID:  in.FlightID,
		Inventory: *inv,
	})
}

// workflowStarter is the part of client.Client StartFlights uses.
type workflowStarter interface {
	ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, workflow any, args ...any) (client.WorkflowRun, error)
}

// StartFlights starts a FlightWorkflow for each flight with all seats free.
// A flight whose workflow is already running keeps it, along with its seat
// map, so restarting the worker never resets bookings.
func StartFlights(ctx context.Context, c workflowStarter, flights []booking.Flight) error {
	for _, f := range flights {
		opts := client.StartWorkflowOptions{
			ID:                       FlightWorkflowID(f.ID),
			TaskQueue:                TaskQueue,
			WorkflowIDConflictPolicy: enumspb.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
		}
		in := FlightInput{FlightID: f.ID, Inventory: *booking.NewInventory(f.Seats)}
		if _, err := c.ExecuteWorkflow(ctx, opts, FlightWorkflow, in); err != nil {
			return fmt.Errorf("start flight %s: %w", f.ID, err)
		}
	}
	return nil
}

// seatError turns a booking error into a non-retryable ApplicationError
// whose type tells the caller whether the seat was taken or the request was
// wrong. The original message is kept.
func seatError(err error) error {
	if err == nil {
		return nil
	}
	errType := ErrTypeInvalidRequest
	if errors.Is(err, booking.ErrSeatUnavailable) || errors.Is(err, booking.ErrNotHeld) {
		errType = ErrTypeSeatUnavailable
	}
	return temporal.NewNonRetryableApplicationError(err.Error(), errType, err)
}
