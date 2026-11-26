package workflows

import (
	"errors"
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"flightbooking/internal/booking"
)

// Names of the OrderWorkflow's Updates and Query.
const (
	UpdateSelectSeats = "selectSeats"
	UpdatePay         = "pay"
	QueryStatus       = "status"
)

// ErrTypeInvalidOrderState is the ApplicationError type for a request the
// order cannot accept in its current status, such as paying twice.
const ErrTypeInvalidOrderState = "InvalidOrderState"

// Timing rules of an order.
const (
	// HoldDuration is how long seats stay held without payment. It also
	// limits how long an order may sit without seats.
	HoldDuration = 15 * time.Minute
	// FlightHoldGrace keeps the flight's backstop hold this long past the
	// order's deadline: one minute for a payment started at the last second
	// and one more for confirming the seats.
	FlightHoldGrace = 2 * time.Minute

	paymentAttemptTimeout = 10 * time.Second
	paymentAttempts       = 3
	paymentTimeout        = time.Minute
)

// OrderWorkflowID returns the workflow ID of an order's OrderWorkflow.
func OrderWorkflowID(orderID string) string {
	return "order-" + orderID
}

// OrderInput is the input of OrderWorkflow.
type OrderInput struct {
	OrderID  string `json:"orderId"`
	FlightID string `json:"flightId"`
}

// order is the OrderWorkflow's state. Handlers and the main loop share it;
// mu makes each request finish before the next starts, because handlers
// wait on activities and would otherwise interleave.
type order struct {
	booking.Order
	deadline time.Time // when the order expires unless payment starts
	code     string    // payment code, set when payment starts
	mu       workflow.Mutex
}

// OrderWorkflow runs one order: seat selection with a 15-minute timer that
// restarts on every change, then payment, then confirmation. Clicking pay
// stops the timer; the payment then confirms or fails the order. It returns
// the final order; a failed or expired order is a normal result, not a
// workflow error.
func OrderWorkflow(ctx workflow.Context, in OrderInput) (booking.Order, error) {
	o := &order{
		Order: booking.Order{
			ID:       in.OrderID,
			FlightID: in.FlightID,
			Status:   booking.StatusCreated,
			Seats:    []string{},
		},
		deadline: workflow.Now(ctx).Add(HoldDuration),
		mu:       workflow.NewMutex(ctx),
	}
	if err := o.registerHandlers(ctx); err != nil {
		return o.Order, err
	}

	for !o.Status.IsTerminal() {
		var err error
		if o.Status == booking.StatusPaying {
			err = o.pay(ctx)
		} else {
			err = o.waitForChangeOrExpiry(ctx)
		}
		if err != nil {
			return o.Order, err
		}
	}

	// Let any handler still holding the result finish before returning.
	if err := workflow.Await(ctx, func() bool { return workflow.AllHandlersFinished(ctx) }); err != nil {
		return o.Order, fmt.Errorf("wait for handlers: %w", err)
	}
	return o.Order, nil
}

func (o *order) registerHandlers(ctx workflow.Context) error {
	err := workflow.SetQueryHandler(ctx, QueryStatus, func() (booking.Order, error) {
		return o.Order, nil
	})
	if err != nil {
		return fmt.Errorf("register %s query: %w", QueryStatus, err)
	}

	err = workflow.SetUpdateHandlerWithOptions(ctx, UpdateSelectSeats, o.selectSeats,
		workflow.UpdateHandlerOptions{
			Validator: func(_ []string) error {
				return o.checkTransition(booking.StatusSeatsHeld)
			},
		})
	if err != nil {
		return fmt.Errorf("register %s update: %w", UpdateSelectSeats, err)
	}

	err = workflow.SetUpdateHandlerWithOptions(ctx, UpdatePay, o.startPayment,
		workflow.UpdateHandlerOptions{
			Validator: func(code string) error {
				if err := booking.ValidatePaymentCode(code); err != nil {
					return temporal.NewNonRetryableApplicationError(err.Error(), ErrTypeInvalidPaymentCode, err)
				}
				return o.checkTransition(booking.StatusPaying)
			},
		})
	if err != nil {
		return fmt.Errorf("register %s update: %w", UpdatePay, err)
	}
	return nil
}

// checkTransition returns an InvalidOrderState error unless the order may
// move to status to.
func (o *order) checkTransition(to booking.Status) error {
	if booking.CanTransition(o.Status, to) {
		return nil
	}
	msg := fmt.Sprintf("order is %s and cannot move to %s", o.Status, to)
	return temporal.NewNonRetryableApplicationError(msg, ErrTypeInvalidOrderState, nil)
}

// selectSeats holds seats on the flight and restarts the 15-minute timer.
// It returns the updated order so the caller sees the new deadline.
func (o *order) selectSeats(ctx workflow.Context, seats []string) (booking.Order, error) {
	if err := o.mu.Lock(ctx); err != nil {
		return o.Order, fmt.Errorf("lock order: %w", err)
	}
	defer o.mu.Unlock()

	// The validator ran before this handler waited for the lock; check again.
	if err := o.checkTransition(booking.StatusSeatsHeld); err != nil {
		return o.Order, err
	}

	deadline := workflow.Now(ctx).Add(HoldDuration)
	hold := HoldSeatsInput{FlightID: o.FlightID, Hold: HoldRequest{
		OrderID: o.ID,
		Seats:   seats,
		Until:   deadline.Add(FlightHoldGrace),
	}}
	var a *Activities
	if err := workflow.ExecuteActivity(seatActivityContext(ctx), a.HoldSeats, hold).Get(ctx, nil); err != nil {
		return o.Order, activityError("hold seats", err)
	}

	o.Status = booking.StatusSeatsHeld
	o.Seats = seats
	o.deadline = deadline
	o.ExpiresAt = deadline
	o.Message = ""
	return o.Order, nil
}

// startPayment stops the timer and hands the payment to the main loop. It
// returns as soon as the order is PAYING; clients poll the status for the
// result.
func (o *order) startPayment(ctx workflow.Context, code string) error {
	if err := o.mu.Lock(ctx); err != nil {
		return fmt.Errorf("lock order: %w", err)
	}
	defer o.mu.Unlock()

	if err := o.checkTransition(booking.StatusPaying); err != nil {
		return err
	}
	o.Status = booking.StatusPaying
	o.code = code
	o.ExpiresAt = time.Time{} // the timer is stopped while paying
	o.Message = "Processing payment"
	return nil
}

// waitForChangeOrExpiry blocks until a handler changes the status or the
// deadline, or until the deadline passes, in which case the order expires.
func (o *order) waitForChangeOrExpiry(ctx workflow.Context) error {
	status, deadline := o.Status, o.deadline
	if remaining := deadline.Sub(workflow.Now(ctx)); remaining > 0 {
		changed, err := workflow.AwaitWithTimeout(ctx, remaining, func() bool {
			return o.Status != status || !o.deadline.Equal(deadline)
		})
		if err != nil {
			return fmt.Errorf("wait for order change: %w", err)
		}
		if changed {
			return nil
		}
	}

	if err := o.mu.Lock(ctx); err != nil {
		return fmt.Errorf("lock order: %w", err)
	}
	defer o.mu.Unlock()

	// A seat change may have finished while we waited for the lock.
	if o.Status != status || !o.deadline.Equal(deadline) {
		return nil
	}
	if o.Status == booking.StatusSeatsHeld {
		o.release(ctx)
	}
	o.Status = booking.StatusExpired
	o.ExpiresAt = time.Time{}
	o.Message = "Seat hold expired after 15 minutes without payment"
	return nil
}

// pay charges the payment code and, on success, books the seats and sends
// the confirmation. A failed payment releases the seats and fails the order.
func (o *order) pay(ctx workflow.Context) error {
	if err := o.mu.Lock(ctx); err != nil {
		return fmt.Errorf("lock order: %w", err)
	}
	defer o.mu.Unlock()

	var a *Activities
	if err := workflow.ExecuteActivity(paymentActivityContext(ctx), a.ChargePayment, o.code).Get(ctx, nil); err != nil {
		o.release(ctx)
		o.Status = booking.StatusFailed
		o.Message = paymentFailureMessage(err)
		return nil
	}

	confirm := ConfirmSeatsInput{FlightID: o.FlightID, Confirm: ConfirmRequest{OrderID: o.ID, Seats: o.Seats}}
	if err := workflow.ExecuteActivity(seatActivityContext(ctx), a.ConfirmSeats, confirm).Get(ctx, nil); err != nil {
		o.release(ctx)
		o.Status = booking.StatusFailed
		o.Message = "Payment succeeded but the seats could not be booked: " + rootMessage(err)
		return nil
	}

	o.Status = booking.StatusConfirmed
	o.Message = "Booking confirmed"
	if err := workflow.ExecuteActivity(seatActivityContext(ctx), a.SendConfirmation, o.Order).Get(ctx, nil); err != nil {
		// The booking stands; only the notice failed.
		workflow.GetLogger(ctx).Warn("Send confirmation failed", "orderId", o.ID, "error", err)
	}
	return nil
}

// release frees the order's seats on the flight. If it fails, the flight's
// backstop hold frees them FlightHoldGrace after the deadline anyway.
func (o *order) release(ctx workflow.Context) {
	var a *Activities
	in := ReleaseSeatsInput{FlightID: o.FlightID, OrderID: o.ID}
	if err := workflow.ExecuteActivity(seatActivityContext(ctx), a.ReleaseSeats, in).Get(ctx, nil); err != nil {
		workflow.GetLogger(ctx).Warn("Release seats failed", "orderId", o.ID, "error", err)
	}
}

// seatActivityContext sets the options for activities that talk to the
// flight. Unreachable-flight errors are retried for up to a minute;
// rejections are non-retryable and fail at once.
func seatActivityContext(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout:    10 * time.Second,
		ScheduleToCloseTimeout: time.Minute,
	})
}

// paymentActivityContext sets the spec's payment rules: each attempt has 10
// seconds, there are at most 3 attempts, and the whole payment must finish
// within a minute of the click.
func paymentActivityContext(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout:    paymentAttemptTimeout,
		ScheduleToCloseTimeout: paymentTimeout,
		RetryPolicy:            &temporal.RetryPolicy{MaximumAttempts: paymentAttempts},
	})
}

// activityError returns the ApplicationError inside an activity failure, so
// the caller sees the flight's error type and message, not the wrapper.
func activityError(action string, err error) error {
	if appErr, ok := errors.AsType[*temporal.ApplicationError](err); ok {
		return appErr
	}
	return fmt.Errorf("%s: %w", action, err)
}

// paymentFailureMessage explains a failed payment to the user. The timeout
// check comes first: a timeout wraps the last attempt's error, which may be
// a decline, but the user should hear that time ran out.
func paymentFailureMessage(err error) string {
	if _, ok := errors.AsType[*temporal.TimeoutError](err); ok {
		return "Payment did not complete in time"
	}
	if appErr, ok := errors.AsType[*temporal.ApplicationError](err); ok {
		switch appErr.Type() {
		case ErrTypePaymentDeclined:
			return fmt.Sprintf("Payment declined after %d attempts", paymentAttempts)
		case ErrTypeInvalidPaymentCode:
			return "Payment code must be exactly 5 digits"
		}
	}
	return "Payment failed: " + rootMessage(err)
}

// rootMessage returns the message of the innermost error, which is the one
// meant for people; the outer layers only say which activity failed.
func rootMessage(err error) string {
	for {
		next := errors.Unwrap(err)
		if next == nil {
			return err.Error()
		}
		err = next
	}
}
