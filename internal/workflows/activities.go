package workflows

import (
	"context"
	"errors"
	"fmt"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"

	"flightbooking/internal/booking"
)

// Error types of the payment ApplicationErrors. Only a declined payment is
// retried; a malformed code fails at once.
const (
	ErrTypeInvalidPaymentCode = "InvalidPaymentCode"
	ErrTypePaymentDeclined    = "PaymentDeclined"
)

// workflowUpdater is the part of client.Client the activities use. Declaring
// it here lets tests pass a small fake instead of a full Temporal client.
type workflowUpdater interface {
	UpdateWorkflow(ctx context.Context, options client.UpdateWorkflowOptions) (client.WorkflowUpdateHandle, error)
}

// Activities holds the dependencies of the booking activities. The worker
// registers one instance, and Temporal calls its methods concurrently, so
// its fields must be safe for concurrent use: client.Client and
// PaymentSimulator both are.
type Activities struct {
	Client   workflowUpdater
	Payments *booking.PaymentSimulator
}

// HoldSeatsInput is the input of HoldSeats.
type HoldSeatsInput struct {
	FlightID string      `json:"flightId"`
	Hold     HoldRequest `json:"hold"`
}

// ReleaseSeatsInput is the input of ReleaseSeats.
type ReleaseSeatsInput struct {
	FlightID string `json:"flightId"`
	OrderID  string `json:"orderId"`
}

// ConfirmSeatsInput is the input of ConfirmSeats.
type ConfirmSeatsInput struct {
	FlightID string         `json:"flightId"`
	Confirm  ConfirmRequest `json:"confirm"`
}

// HoldSeats asks the flight's workflow to hold seats for an order.
func (a *Activities) HoldSeats(ctx context.Context, in HoldSeatsInput) error {
	return a.updateFlight(ctx, in.FlightID, UpdateHold, in.Hold)
}

// ReleaseSeats asks the flight's workflow to release an order's seats.
func (a *Activities) ReleaseSeats(ctx context.Context, in ReleaseSeatsInput) error {
	return a.updateFlight(ctx, in.FlightID, UpdateRelease, in.OrderID)
}

// ConfirmSeats asks the flight's workflow to book an order's seats.
func (a *Activities) ConfirmSeats(ctx context.Context, in ConfirmSeatsInput) error {
	return a.updateFlight(ctx, in.FlightID, UpdateConfirm, in.Confirm)
}

// updateFlight sends one Update to a flight's workflow and waits for its
// result.
//
// The Update ID is built from the calling workflow's run ID and this
// activity's ID. Both stay the same when Temporal retries the activity, so a
// retry after a lost reply reuses the ID and the flight returns the saved
// result instead of applying the Update twice.
//
// An ApplicationError from the flight (seat taken, bad request) is returned
// as non-retryable, since retrying cannot change the answer. Any other error,
// such as the flight being unreachable, is left retryable.
func (a *Activities) updateFlight(ctx context.Context, flightID, updateName string, arg any) error {
	info := activity.GetInfo(ctx)
	handle, err := a.Client.UpdateWorkflow(ctx, client.UpdateWorkflowOptions{
		WorkflowID:   FlightWorkflowID(flightID),
		UpdateName:   updateName,
		UpdateID:     info.WorkflowExecution.RunID + "/" + info.ActivityID,
		Args:         []any{arg},
		WaitForStage: client.WorkflowUpdateStageCompleted,
	})
	if err == nil {
		err = handle.Get(ctx, nil)
	}
	if err == nil {
		return nil
	}

	var appErr *temporal.ApplicationError
	if errors.As(err, &appErr) {
		return temporal.NewNonRetryableApplicationError(appErr.Error(), appErr.Type(), err)
	}
	return fmt.Errorf("%s update on flight %s: %w", updateName, flightID, err)
}

// ChargePayment charges the payment code through the payment gateway.
func (a *Activities) ChargePayment(_ context.Context, code string) error {
	err := a.Payments.Charge(code)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, booking.ErrInvalidPaymentCode):
		return temporal.NewNonRetryableApplicationError(err.Error(), ErrTypeInvalidPaymentCode, err)
	case errors.Is(err, booking.ErrPaymentDeclined):
		return temporal.NewApplicationError(err.Error(), ErrTypePaymentDeclined, err)
	default:
		return fmt.Errorf("charge payment: %w", err)
	}
}

// SendConfirmation tells the user their booking is confirmed. This system
// has no email or SMS gateway, so it writes the confirmation to the log.
func (a *Activities) SendConfirmation(ctx context.Context, order booking.Order) error {
	activity.GetLogger(ctx).Info("Booking confirmed",
		"orderId", order.ID, "flightId", order.FlightID, "seats", order.Seats)
	return nil
}
