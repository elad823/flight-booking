package workflows

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"flightbooking/internal/booking"
)

// orderOp is one Update sent to the order at a point in workflow time.
type orderOp struct {
	at      time.Duration
	update  string
	arg     any
	wantErr string // "" for success, otherwise the ApplicationError type
	// wantRejected says the error must come from the validator, before the
	// request is accepted, rather than from the handler.
	wantRejected bool
}

// statusCheck queries the order's status at a point in workflow time.
type statusCheck struct {
	at          time.Duration
	wantStatus  booking.Status
	wantSeats   []string
	wantPaused  bool // ExpiresAt must be zero (timer stopped)
	wantExpires time.Duration
}

// flightConfig sets how the fake activities behave. It holds only plain
// values, so test tables can copy it.
type flightConfig struct {
	holdErr      error
	holdDelays   []time.Duration // delay of each HoldSeats call, in call order
	chargeErr    error
	chargeDelay  time.Duration
	confirmErr   error
	releaseDelay time.Duration
}

// fakeFlight stands in for the seat and payment activities and records how
// often each ran. Its counters are guarded by mu because activities run on
// their own goroutines.
type fakeFlight struct {
	flightConfig

	mu        sync.Mutex
	holdCalls int
	holds     []HoldSeatsInput
	charges   int
	releases  int
	confirms  int
}

func (f *fakeFlight) mock(env *testsuite.TestWorkflowEnvironment) {
	var a *Activities
	env.RegisterActivity(&Activities{})
	env.OnActivity(a.HoldSeats, mock.Anything, mock.Anything).Return(
		func(_ context.Context, in HoldSeatsInput) error {
			f.mu.Lock()
			f.holds = append(f.holds, in)
			f.mu.Unlock()
			return f.holdErr
		}).AfterFn(func() time.Duration {
		f.mu.Lock()
		defer f.mu.Unlock()
		i := f.holdCalls
		f.holdCalls++
		if i < len(f.holdDelays) {
			return f.holdDelays[i]
		}
		return 0
	})
	env.OnActivity(a.ReleaseSeats, mock.Anything, mock.Anything).Return(
		func(context.Context, ReleaseSeatsInput) error {
			f.mu.Lock()
			f.releases++
			f.mu.Unlock()
			return nil
		}).After(f.releaseDelay)
	env.OnActivity(a.ConfirmSeats, mock.Anything, mock.Anything).Return(
		func(context.Context, ConfirmSeatsInput) error {
			f.mu.Lock()
			f.confirms++
			f.mu.Unlock()
			return f.confirmErr
		})
	env.OnActivity(a.ChargePayment, mock.Anything, mock.Anything).Return(
		func(context.Context, string) error {
			f.mu.Lock()
			f.charges++
			f.mu.Unlock()
			return f.chargeErr
		}).After(f.chargeDelay)
	env.OnActivity(a.SendConfirmation, mock.Anything, mock.Anything).Return(nil)
}

func TestOrderWorkflow(t *testing.T) {
	t.Parallel()

	declined := temporal.NewApplicationError("payment declined", ErrTypePaymentDeclined, nil)
	seatTaken := temporal.NewNonRetryableApplicationError("seat 1A: seat unavailable", ErrTypeSeatUnavailable, nil)
	minute := time.Minute

	tests := []struct {
		name        string
		flight      flightConfig
		ops         []orderOp
		checks      []statusCheck
		wantStatus  booking.Status
		wantMessage string // substring of the final message
		wantEndAt   time.Duration
		wantCounts  [4]int // holds, charges, confirms, releases
	}{
		{
			name: "happy path confirms the booking",
			ops: []orderOp{
				{at: time.Second, update: UpdateSelectSeats, arg: []string{"1A", "1B"}},
				{at: 2 * time.Second, update: UpdatePay, arg: "12345"},
			},
			wantStatus: booking.StatusConfirmed, wantMessage: "confirmed",
			wantEndAt:  2 * time.Second,
			wantCounts: [4]int{1, 1, 1, 0},
		},
		{
			name: "hold expires after 15 minutes and seats are released",
			ops: []orderOp{
				{at: time.Second, update: UpdateSelectSeats, arg: []string{"1A"}},
			},
			checks: []statusCheck{
				{at: 2 * time.Second, wantStatus: booking.StatusSeatsHeld, wantSeats: []string{"1A"}, wantExpires: time.Second + 15*minute},
			},
			wantStatus: booking.StatusExpired, wantMessage: "expired",
			wantEndAt:  time.Second + 15*minute,
			wantCounts: [4]int{1, 0, 0, 1},
		},
		{
			name: "changing seats restarts the timer",
			ops: []orderOp{
				{at: time.Second, update: UpdateSelectSeats, arg: []string{"1A"}},
				{at: 10 * minute, update: UpdateSelectSeats, arg: []string{"2B"}},
			},
			checks: []statusCheck{
				{at: 16 * minute, wantStatus: booking.StatusSeatsHeld, wantSeats: []string{"2B"}, wantExpires: 25 * minute},
			},
			wantStatus: booking.StatusExpired,
			wantEndAt:  25 * minute,
			wantCounts: [4]int{2, 0, 0, 1},
		},
		{
			name: "seat change still in flight when the timer fires wins",
			// The second change starts at 15:00 and its hold takes until
			// 15:05, so the old deadline (15:01) passes while it waits.
			flight: flightConfig{holdDelays: []time.Duration{0, 5 * time.Second}},
			ops: []orderOp{
				{at: time.Second, update: UpdateSelectSeats, arg: []string{"1A"}},
				{at: 15 * minute, update: UpdateSelectSeats, arg: []string{"2B"}},
			},
			checks: []statusCheck{
				{at: 16 * minute, wantStatus: booking.StatusSeatsHeld, wantSeats: []string{"2B"}, wantExpires: 30 * minute},
			},
			wantStatus: booking.StatusExpired,
			wantEndAt:  30 * minute,
			wantCounts: [4]int{2, 0, 0, 1},
		},
		{
			name:       "order without seats expires without releasing",
			wantStatus: booking.StatusExpired,
			wantEndAt:  15 * minute,
			wantCounts: [4]int{0, 0, 0, 0},
		},
		{
			name: "paying stops the timer even at the last second",
			flight: flightConfig{
				chargeDelay: 30 * time.Second,
			},
			ops: []orderOp{
				{at: time.Second, update: UpdateSelectSeats, arg: []string{"1A"}},
				{at: 15 * minute, update: UpdatePay, arg: "12345"},
			},
			checks: []statusCheck{
				{at: 15*minute + 10*time.Second, wantStatus: booking.StatusPaying, wantSeats: []string{"1A"}, wantPaused: true},
			},
			wantStatus: booking.StatusConfirmed,
			wantEndAt:  15*minute + 30*time.Second,
			wantCounts: [4]int{1, 1, 1, 0},
		},
		{
			name:   "declined three times fails the order and releases seats",
			flight: flightConfig{chargeErr: declined},
			ops: []orderOp{
				{at: time.Second, update: UpdateSelectSeats, arg: []string{"1A"}},
				{at: 2 * time.Second, update: UpdatePay, arg: "12345"},
			},
			wantStatus: booking.StatusFailed, wantMessage: "declined after 3 attempts",
			wantCounts: [4]int{1, 3, 0, 1},
		},
		{
			name:   "seats lost after payment fails the order",
			flight: flightConfig{confirmErr: seatTaken},
			ops: []orderOp{
				{at: time.Second, update: UpdateSelectSeats, arg: []string{"1A"}},
				{at: 2 * time.Second, update: UpdatePay, arg: "12345"},
			},
			wantStatus: booking.StatusFailed, wantMessage: "could not be booked",
			wantCounts: [4]int{1, 1, 1, 1},
		},
		{
			name:   "taken seat is reported and the order stays open",
			flight: flightConfig{holdErr: seatTaken},
			ops: []orderOp{
				{at: time.Second, update: UpdateSelectSeats, arg: []string{"1A"}, wantErr: ErrTypeSeatUnavailable},
			},
			checks: []statusCheck{
				{at: 2 * time.Second, wantStatus: booking.StatusCreated, wantSeats: []string{}},
			},
			wantStatus: booking.StatusExpired,
			wantEndAt:  15 * minute,
			wantCounts: [4]int{1, 0, 0, 0},
		},
		{
			name: "invalid code is rejected and the timer keeps running",
			ops: []orderOp{
				{at: time.Second, update: UpdateSelectSeats, arg: []string{"1A"}},
				{at: 2 * time.Second, update: UpdatePay, arg: "12a45", wantErr: ErrTypeInvalidPaymentCode, wantRejected: true},
			},
			checks: []statusCheck{
				{at: 3 * time.Second, wantStatus: booking.StatusSeatsHeld, wantSeats: []string{"1A"}, wantExpires: time.Second + 15*minute},
			},
			wantStatus: booking.StatusExpired,
			wantCounts: [4]int{1, 0, 0, 1},
		},
		{
			name: "paying without seats is rejected",
			ops: []orderOp{
				{at: time.Second, update: UpdatePay, arg: "12345", wantErr: ErrTypeInvalidOrderState, wantRejected: true},
			},
			wantStatus: booking.StatusExpired,
			wantCounts: [4]int{0, 0, 0, 0},
		},
		{
			name: "seat change while paying is rejected",
			flight: flightConfig{
				chargeDelay: 5 * time.Second,
			},
			ops: []orderOp{
				{at: time.Second, update: UpdateSelectSeats, arg: []string{"1A"}},
				{at: 2 * time.Second, update: UpdatePay, arg: "12345"},
				{at: 3 * time.Second, update: UpdateSelectSeats, arg: []string{"2B"}, wantErr: ErrTypeInvalidOrderState, wantRejected: true},
			},
			wantStatus: booking.StatusConfirmed,
			wantCounts: [4]int{1, 1, 1, 0},
		},
		{
			name: "seat change queued behind expiry is refused",
			// Expiry holds the lock while releasing (15:01 to 15:06). The
			// change at 15:02 passes its validator, since the order is still
			// SEATS_HELD, then waits; after the lock it must see EXPIRED.
			flight: flightConfig{releaseDelay: 5 * time.Second},
			ops: []orderOp{
				{at: time.Second, update: UpdateSelectSeats, arg: []string{"1A"}},
				{at: 15*minute + 2*time.Second, update: UpdateSelectSeats, arg: []string{"2B"}, wantErr: ErrTypeInvalidOrderState},
			},
			wantStatus: booking.StatusExpired,
			wantCounts: [4]int{1, 0, 0, 1},
		},
		{
			name: "overlapping seat changes run one at a time",
			flight: flightConfig{
				// The first call is slower, so without the mutex its result
				// would land last and overwrite the second request's seats.
				holdDelays: []time.Duration{5 * time.Second, time.Second},
			},
			ops: []orderOp{
				{at: time.Second, update: UpdateSelectSeats, arg: []string{"1A"}},
				{at: time.Second, update: UpdateSelectSeats, arg: []string{"2B"}},
			},
			checks: []statusCheck{
				{at: minute, wantStatus: booking.StatusSeatsHeld, wantSeats: []string{"2B"}},
			},
			wantStatus: booking.StatusExpired,
			wantCounts: [4]int{2, 0, 0, 1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var s testsuite.WorkflowTestSuite
			env := s.NewTestWorkflowEnvironment()
			env.SetStartTime(start)
			flight := &fakeFlight{flightConfig: tt.flight}
			flight.mock(env)

			for i, op := range tt.ops {
				env.RegisterDelayedCallback(func() {
					env.UpdateWorkflow(op.update, fmt.Sprintf("u%d", i), &testsuite.TestUpdateCallback{
						OnAccept: func() {},
						OnReject: func(err error) {
							if !op.wantRejected {
								t.Errorf("op %d %s: rejected with %v", i, op.update, err)
								return
							}
							checkErrType(t, i, op, err)
						},
						OnComplete: func(_ any, err error) {
							if op.wantRejected {
								t.Errorf("op %d %s: accepted, want rejection", i, op.update)
								return
							}
							checkErrType(t, i, op, err)
						},
					}, op.arg)
				}, op.at)
			}
			for _, c := range tt.checks {
				env.RegisterDelayedCallback(func() { checkStatus(t, env, c) }, c.at)
			}

			env.ExecuteWorkflow(OrderWorkflow, OrderInput{OrderID: "o1", FlightID: "LY001"})

			if err := env.GetWorkflowError(); err != nil {
				t.Fatalf("workflow error = %v", err)
			}
			var got booking.Order
			if err := env.GetWorkflowResult(&got); err != nil {
				t.Fatalf("decode result: %v", err)
			}
			if got.Status != tt.wantStatus || !strings.Contains(strings.ToLower(got.Message), tt.wantMessage) {
				t.Errorf("final = %s %q, want %s containing %q", got.Status, got.Message, tt.wantStatus, tt.wantMessage)
			}
			if tt.wantEndAt != 0 && !env.Now().Equal(start.Add(tt.wantEndAt)) {
				t.Errorf("ended at %v, want %v", env.Now().Sub(start), tt.wantEndAt)
			}
			flight.mu.Lock()
			counts := [4]int{len(flight.holds), flight.charges, flight.confirms, flight.releases}
			flight.mu.Unlock()
			if counts != tt.wantCounts {
				t.Errorf("holds, charges, confirms, releases = %v, want %v", counts, tt.wantCounts)
			}
		})
	}
}

func checkErrType(t *testing.T, i int, op orderOp, err error) {
	t.Helper()
	if op.wantErr == "" {
		if err != nil {
			t.Errorf("op %d %s: unexpected error %v", i, op.update, err)
		}
		return
	}
	appErr, ok := errors.AsType[*temporal.ApplicationError](err)
	if !ok || appErr.Type() != op.wantErr {
		t.Errorf("op %d %s: error = %v, want type %s", i, op.update, err, op.wantErr)
	}
}

func checkStatus(t *testing.T, env *testsuite.TestWorkflowEnvironment, c statusCheck) {
	t.Helper()
	v, err := env.QueryWorkflow(QueryStatus)
	if err != nil {
		t.Errorf("at %v: query %s: %v", c.at, QueryStatus, err)
		return
	}
	var o booking.Order
	if err := v.Get(&o); err != nil {
		t.Errorf("at %v: decode status: %v", c.at, err)
		return
	}
	if o.Status != c.wantStatus || !slices.Equal(o.Seats, c.wantSeats) {
		t.Errorf("at %v: status %s seats %v, want %s %v", c.at, o.Status, o.Seats, c.wantStatus, c.wantSeats)
	}
	if c.wantPaused && !o.ExpiresAt.IsZero() {
		t.Errorf("at %v: ExpiresAt = %v, want zero while paying", c.at, o.ExpiresAt)
	}
	if c.wantExpires != 0 && !o.ExpiresAt.Equal(start.Add(c.wantExpires)) {
		t.Errorf("at %v: ExpiresAt = %v, want %v", c.at, o.ExpiresAt.Sub(start), c.wantExpires)
	}
}

// TestOrderHoldsSeatsPastDeadline checks the flight's backstop hold lasts
// FlightHoldGrace past the order's deadline.
func TestOrderHoldsSeatsPastDeadline(t *testing.T) {
	t.Parallel()

	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	env.SetStartTime(start)
	flight := &fakeFlight{}
	flight.mock(env)
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(UpdateSelectSeats, "a", &testsuite.TestUpdateCallback{
			OnAccept: func() {}, OnReject: func(error) {}, OnComplete: func(any, error) {},
		}, []string{"1A"})
	}, time.Second)

	env.ExecuteWorkflow(OrderWorkflow, OrderInput{OrderID: "o1", FlightID: "LY001"})

	flight.mu.Lock()
	defer flight.mu.Unlock()
	want := start.Add(time.Second + HoldDuration + FlightHoldGrace)
	if len(flight.holds) != 1 || !flight.holds[0].Hold.Until.Equal(want) {
		t.Errorf("holds = %+v, want one hold until %v", flight.holds, want)
	}
}

// TestPaymentActivityOptions checks the spec's payment limits are set: 10
// seconds per attempt, 3 attempts, and one minute in total. The test
// environment does not enforce timeouts on mocked activities, so the
// options are checked directly.
func TestPaymentActivityOptions(t *testing.T) {
	t.Parallel()

	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	checkOptions := func(ctx workflow.Context) error {
		opts := workflow.GetActivityOptions(paymentActivityContext(ctx))
		if opts.StartToCloseTimeout != 10*time.Second ||
			opts.ScheduleToCloseTimeout != time.Minute ||
			opts.RetryPolicy == nil || opts.RetryPolicy.MaximumAttempts != 3 {
			return fmt.Errorf("payment options = %+v, retry %+v", opts, opts.RetryPolicy)
		}
		return nil
	}
	env.RegisterWorkflow(checkOptions)

	env.ExecuteWorkflow(checkOptions)

	if err := env.GetWorkflowError(); err != nil {
		t.Error(err)
	}
}

func TestPaymentFailureMessage(t *testing.T) {
	t.Parallel()

	declined := temporal.NewApplicationError("payment declined", ErrTypePaymentDeclined, nil)
	invalid := temporal.NewNonRetryableApplicationError("bad code", ErrTypeInvalidPaymentCode, nil)

	tests := []struct {
		name string
		err  error
		want string
	}{
		{"declined", declined, "Payment declined after 3 attempts"},
		{"invalid code", invalid, "Payment code must be exactly 5 digits"},
		{"timeout", temporal.NewTimeoutError(enumspb.TIMEOUT_TYPE_SCHEDULE_TO_CLOSE, nil), "Payment did not complete in time"},
		{"timeout after a decline", temporal.NewTimeoutError(enumspb.TIMEOUT_TYPE_SCHEDULE_TO_CLOSE, declined), "Payment did not complete in time"},
		{"other", fmt.Errorf("activity: %w", errors.New("gateway down")), "Payment failed: gateway down"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := paymentFailureMessage(tt.err); got != tt.want {
				t.Errorf("paymentFailureMessage() = %q, want %q", got, tt.want)
			}
		})
	}
}
