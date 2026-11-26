package api

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"

	"flightbooking/internal/booking"
	"flightbooking/internal/workflows"
)

// temporalClient is the part of client.Client that TemporalBooking uses.
type temporalClient interface {
	ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, workflow any, args ...any) (client.WorkflowRun, error)
	UpdateWorkflow(ctx context.Context, options client.UpdateWorkflowOptions) (client.WorkflowUpdateHandle, error)
	QueryWorkflow(ctx context.Context, workflowID, runID, queryType string, args ...any) (converter.EncodedValue, error)
}

// TemporalBooking implements Booking with the order and flight workflows.
type TemporalBooking struct {
	client temporalClient
}

// NewTemporalBooking returns a Booking that talks to Temporal through c.
func NewTemporalBooking(c temporalClient) *TemporalBooking {
	return &TemporalBooking{client: c}
}

// CreateOrder starts an OrderWorkflow under a new random order ID.
func (b *TemporalBooking) CreateOrder(ctx context.Context, flightID string) (booking.Order, error) {
	id := rand.Text()
	opts := client.StartWorkflowOptions{
		ID:        workflows.OrderWorkflowID(id),
		TaskQueue: workflows.TaskQueue,
	}
	in := workflows.OrderInput{OrderID: id, FlightID: flightID}
	if _, err := b.client.ExecuteWorkflow(ctx, opts, workflows.OrderWorkflow, in); err != nil {
		return booking.Order{}, fmt.Errorf("start order: %w", err)
	}
	// The workflow starts in this state; returning it saves a Query.
	return booking.Order{ID: id, FlightID: flightID, Status: booking.StatusCreated, Seats: []string{}}, nil
}

// SelectSeats replaces the order's seats and returns the updated order.
func (b *TemporalBooking) SelectSeats(ctx context.Context, orderID string, seats []string) (booking.Order, error) {
	var o booking.Order
	err := b.updateOrder(ctx, orderID, workflows.UpdateSelectSeats, seats, &o)
	return o, err
}

// Pay starts payment. It returns once the order accepts it, not when the
// payment finishes.
func (b *TemporalBooking) Pay(ctx context.Context, orderID, code string) error {
	return b.updateOrder(ctx, orderID, workflows.UpdatePay, code, nil)
}

// OrderStatus returns the order's current state. It works for finished
// orders too, so the user sees how an order ended.
func (b *TemporalBooking) OrderStatus(ctx context.Context, orderID string) (booking.Order, error) {
	v, err := b.client.QueryWorkflow(ctx, workflows.OrderWorkflowID(orderID), "", workflows.QueryStatus)
	if err != nil {
		return booking.Order{}, requestError(err, "order not found", "query order status")
	}
	var o booking.Order
	if err := v.Get(&o); err != nil {
		return booking.Order{}, fmt.Errorf("decode order status: %w", err)
	}
	return o, nil
}

// AvailableSeats returns the flight's free seats as of now.
func (b *TemporalBooking) AvailableSeats(ctx context.Context, flightID string) ([]string, error) {
	// The Query takes the caller's clock; see FlightWorkflow for why.
	v, err := b.client.QueryWorkflow(ctx, workflows.FlightWorkflowID(flightID), "", workflows.QueryAvailable, time.Now())
	if err != nil {
		// The handler checked the catalog, so a missing flight workflow is
		// a deployment fault, not the user's: no notFound message.
		return nil, requestError(err, "", "query available seats")
	}
	var seats []string
	if err := v.Get(&seats); err != nil {
		return nil, fmt.Errorf("decode available seats: %w", err)
	}
	return seats, nil
}

// updateOrder sends an Update to the order and waits for its result, which
// is decoded into result unless result is nil.
func (b *TemporalBooking) updateOrder(ctx context.Context, orderID, name string, arg, result any) error {
	h, err := b.client.UpdateWorkflow(ctx, client.UpdateWorkflowOptions{
		WorkflowID:   workflows.OrderWorkflowID(orderID),
		UpdateName:   name,
		Args:         []any{arg},
		WaitForStage: client.WorkflowUpdateStageCompleted,
	})
	if err == nil {
		err = h.Get(ctx, result)
	}
	if err != nil {
		// The server answers NotFound both for an unknown order and for one
		// that has finished; only the message text tells them apart.
		return requestError(err, "order not found or already closed", name)
	}
	return nil
}

// requestError turns the errors a user can act on into *Error: NotFound
// (when notFound is set) and the workflows' own error types. Anything else
// is wrapped with action and stays an internal error.
func requestError(err error, notFound, action string) error {
	if _, ok := errors.AsType[*serviceerror.NotFound](err); ok && notFound != "" {
		return &Error{Kind: KindNotFound, Message: notFound}
	}
	if appErr, ok := errors.AsType[*temporal.ApplicationError](err); ok {
		switch appErr.Type() {
		case workflows.ErrTypeInvalidRequest, workflows.ErrTypeInvalidPaymentCode:
			return &Error{Kind: KindInvalid, Message: appErr.Message()}
		case workflows.ErrTypeSeatUnavailable, workflows.ErrTypeInvalidOrderState:
			return &Error{Kind: KindConflict, Message: appErr.Message()}
		}
	}
	return fmt.Errorf("%s: %w", action, err)
}
