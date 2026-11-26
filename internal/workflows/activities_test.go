package workflows

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"flightbooking/internal/booking"
)

// fakeUpdater records Update requests and returns canned errors. The mutex
// guards its fields because a test activity may run on another goroutine.
type fakeUpdater struct {
	mu        sync.Mutex
	sendErr   error // returned by UpdateWorkflow
	getErr    error // returned by the handle's Get
	failFirst int   // fail this many calls with a retryable error first
	got       client.UpdateWorkflowOptions
	ids       []string // Update ID of every call
}

func (f *fakeUpdater) UpdateWorkflow(_ context.Context, opts client.UpdateWorkflowOptions) (client.WorkflowUpdateHandle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = opts
	f.ids = append(f.ids, opts.UpdateID)
	if len(f.ids) <= f.failFirst {
		return nil, errors.New("connection reset")
	}
	if f.sendErr != nil {
		return nil, f.sendErr
	}
	return fakeHandle{err: f.getErr}, nil
}

type fakeHandle struct{ err error }

func (fakeHandle) WorkflowID() string               { return "" }
func (fakeHandle) RunID() string                    { return "" }
func (fakeHandle) UpdateID() string                 { return "" }
func (h fakeHandle) Get(context.Context, any) error { return h.err }

func TestSeatActivities(t *testing.T) {
	t.Parallel()

	seatTaken := temporal.NewNonRetryableApplicationError("seat 1A: seat unavailable", ErrTypeSeatUnavailable, nil)
	hold := HoldRequest{OrderID: "o1", Seats: []string{"1A"}}

	tests := []struct {
		name          string
		run           func(a *Activities, env *testsuite.TestActivityEnvironment) error
		sendErr       error
		getErr        error
		wantUpdate    string
		wantArg       any
		wantErrType   string // "" for success
		wantRetryable bool
	}{
		{
			name: "hold succeeds",
			run: func(a *Activities, env *testsuite.TestActivityEnvironment) error {
				_, err := env.ExecuteActivity(a.HoldSeats, HoldSeatsInput{FlightID: "LY001", Hold: hold})
				return err
			},
			wantUpdate: UpdateHold, wantArg: hold,
		},
		{
			name: "release succeeds",
			run: func(a *Activities, env *testsuite.TestActivityEnvironment) error {
				_, err := env.ExecuteActivity(a.ReleaseSeats, ReleaseSeatsInput{FlightID: "LY001", OrderID: "o1"})
				return err
			},
			wantUpdate: UpdateRelease, wantArg: "o1",
		},
		{
			name: "confirm succeeds",
			run: func(a *Activities, env *testsuite.TestActivityEnvironment) error {
				_, err := env.ExecuteActivity(a.ConfirmSeats, ConfirmSeatsInput{FlightID: "LY001", Confirm: ConfirmRequest{"o1", []string{"1A"}}})
				return err
			},
			wantUpdate: UpdateConfirm, wantArg: ConfirmRequest{"o1", []string{"1A"}},
		},
		{
			name: "flight rejection is not retried",
			run: func(a *Activities, env *testsuite.TestActivityEnvironment) error {
				_, err := env.ExecuteActivity(a.HoldSeats, HoldSeatsInput{FlightID: "LY001", Hold: hold})
				return err
			},
			getErr:     seatTaken,
			wantUpdate: UpdateHold, wantArg: hold,
			wantErrType: ErrTypeSeatUnavailable, wantRetryable: false,
		},
		{
			name: "rejection while sending is not retried",
			run: func(a *Activities, env *testsuite.TestActivityEnvironment) error {
				_, err := env.ExecuteActivity(a.HoldSeats, HoldSeatsInput{FlightID: "LY001", Hold: hold})
				return err
			},
			sendErr:    seatTaken,
			wantUpdate: UpdateHold, wantArg: hold,
			wantErrType: ErrTypeSeatUnavailable, wantRetryable: false,
		},
		{
			name: "unreachable flight is retried",
			run: func(a *Activities, env *testsuite.TestActivityEnvironment) error {
				_, err := env.ExecuteActivity(a.HoldSeats, HoldSeatsInput{FlightID: "LY001", Hold: hold})
				return err
			},
			sendErr:    errors.New("connection refused"),
			wantUpdate: UpdateHold, wantArg: hold,
			wantRetryable: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var s testsuite.WorkflowTestSuite
			env := s.NewTestActivityEnvironment()
			fake := &fakeUpdater{sendErr: tt.sendErr, getErr: tt.getErr}
			a := &Activities{Client: fake}
			env.RegisterActivity(a)

			err := tt.run(a, env)

			if fake.got.WorkflowID != "flight-LY001" || fake.got.UpdateName != tt.wantUpdate {
				t.Errorf("sent %s to %s, want %s to flight-LY001", fake.got.UpdateName, fake.got.WorkflowID, tt.wantUpdate)
			}
			if len(fake.got.Args) != 1 || !argEqual(fake.got.Args[0], tt.wantArg) {
				t.Errorf("args = %v, want [%v]", fake.got.Args, tt.wantArg)
			}
			if fake.got.UpdateID == "" || fake.got.WaitForStage != client.WorkflowUpdateStageCompleted {
				t.Errorf("UpdateID = %q, WaitForStage = %v; want an ID and Completed", fake.got.UpdateID, fake.got.WaitForStage)
			}
			checkActivityErr(t, err, tt.sendErr == nil && tt.getErr == nil, tt.wantErrType, tt.wantRetryable)
			// The flight's message reaches the user, so it must pass through
			// as written, without the error chain's decorations.
			if appErr, ok := errors.AsType[*temporal.ApplicationError](err); ok && tt.wantErrType != "" && appErr.Message() != "seat 1A: seat unavailable" {
				t.Errorf("message = %q, want %q", appErr.Message(), "seat 1A: seat unavailable")
			}
		})
	}
}

// argEqual compares Update arguments, which are request structs or strings.
func argEqual(got, want any) bool {
	switch w := want.(type) {
	case HoldRequest:
		g, ok := got.(HoldRequest)
		return ok && g.OrderID == w.OrderID && g.Until.Equal(w.Until) && slices.Equal(g.Seats, w.Seats)
	case ConfirmRequest:
		g, ok := got.(ConfirmRequest)
		return ok && g.OrderID == w.OrderID && slices.Equal(g.Seats, w.Seats)
	default:
		return got == want
	}
}

// TestUpdateIDStableAcrossRetries checks that a retried activity sends the
// same Update ID, which is what lets the flight deduplicate it.
func TestUpdateIDStableAcrossRetries(t *testing.T) {
	t.Parallel()

	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	fake := &fakeUpdater{failFirst: 2}
	a := &Activities{Client: fake}
	env.RegisterActivity(a)
	in := HoldSeatsInput{FlightID: "LY001", Hold: HoldRequest{OrderID: "o1", Seats: []string{"1A"}}}

	// A workflow runs the activity so Temporal's retry policy really retries it.
	holdWithRetries := func(ctx workflow.Context) error {
		ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
			StartToCloseTimeout: time.Minute,
			RetryPolicy:         &temporal.RetryPolicy{InitialInterval: time.Second, MaximumAttempts: 3},
		})
		return workflow.ExecuteActivity(ctx, a.HoldSeats, in).Get(ctx, nil)
	}
	env.RegisterWorkflow(holdWithRetries)

	env.ExecuteWorkflow(holdWithRetries)

	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error = %v", err)
	}
	if len(fake.ids) != 3 {
		t.Fatalf("got %d attempts, want 3", len(fake.ids))
	}
	for _, id := range fake.ids[1:] {
		if id != fake.ids[0] {
			t.Errorf("Update IDs differ between attempts: %q", fake.ids)
			break
		}
	}
}

func TestChargePayment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		roll          float64
		code          string
		wantErrType   string
		wantRetryable bool
	}{
		{"approved", 0.9, "12345", "", false},
		{"declined is retried", 0.01, "12345", ErrTypePaymentDeclined, true},
		{"invalid code is not retried", 0.9, "12a45", ErrTypeInvalidPaymentCode, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			payments, err := booking.NewPaymentSimulator(0.15, func() float64 { return tt.roll })
			if err != nil {
				t.Fatalf("NewPaymentSimulator() error = %v", err)
			}
			a := &Activities{Payments: payments}
			var s testsuite.WorkflowTestSuite
			env := s.NewTestActivityEnvironment()
			env.RegisterActivity(a)

			_, err = env.ExecuteActivity(a.ChargePayment, tt.code)

			checkActivityErr(t, err, tt.wantErrType == "", tt.wantErrType, tt.wantRetryable)
		})
	}
}

func TestSendConfirmation(t *testing.T) {
	t.Parallel()

	var s testsuite.WorkflowTestSuite
	env := s.NewTestActivityEnvironment()
	a := &Activities{}
	env.RegisterActivity(a)

	_, err := env.ExecuteActivity(a.SendConfirmation, booking.Order{ID: "o1", FlightID: "LY001", Seats: []string{"1A"}})
	if err != nil {
		t.Errorf("SendConfirmation() error = %v", err)
	}
}

// checkActivityErr checks an activity's error: nil when wantOK, otherwise an
// ApplicationError of wantType (when set) that is retryable as expected.
func checkActivityErr(t *testing.T, err error, wantOK bool, wantType string, wantRetryable bool) {
	t.Helper()
	if wantOK {
		if err != nil {
			t.Errorf("error = %v, want nil", err)
		}
		return
	}
	if err == nil {
		t.Fatal("error = nil, want an error")
	}
	appErr, ok := errors.AsType[*temporal.ApplicationError](err)
	if !ok {
		t.Fatalf("error = %v, want an ApplicationError", err)
	}
	if wantType != "" && appErr.Type() != wantType {
		t.Errorf("error type = %q, want %q", appErr.Type(), wantType)
	}
	if appErr.NonRetryable() == wantRetryable {
		t.Errorf("NonRetryable() = %v, want %v", appErr.NonRetryable(), !wantRetryable)
	}
}
