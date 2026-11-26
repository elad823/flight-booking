package workflows

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"flightbooking/internal/booking"
)

var start = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// flightOp is one Update sent to the workflow at a point in workflow time.
type flightOp struct {
	at      time.Duration // after workflow start
	update  string
	arg     any
	wantErr string // "" for success, otherwise the ApplicationError type
}

func TestFlightWorkflow(t *testing.T) {
	t.Parallel()

	until := start.Add(15 * time.Minute)

	tests := []struct {
		name string
		ops  []flightOp
		// queryAt is when the available-seats Query runs, after all ops.
		queryAt       time.Duration
		wantAvailable []string
	}{
		{
			name: "hold removes seats from available",
			ops: []flightOp{
				{at: time.Second, update: UpdateHold, arg: HoldRequest{"o1", []string{"1A", "1B"}, until}},
			},
			queryAt:       time.Minute,
			wantAvailable: []string{"1C"},
		},
		{
			name: "second order cannot take a held seat",
			ops: []flightOp{
				{at: time.Second, update: UpdateHold, arg: HoldRequest{"o1", []string{"1A"}, until}},
				{at: 2 * time.Second, update: UpdateHold, arg: HoldRequest{"o2", []string{"1A"}, until}, wantErr: ErrTypeSeatUnavailable},
			},
			queryAt:       time.Minute,
			wantAvailable: []string{"1B", "1C"},
		},
		{
			name: "unknown seat is an invalid request",
			ops: []flightOp{
				{at: time.Second, update: UpdateHold, arg: HoldRequest{"o1", []string{"9Z"}, until}, wantErr: ErrTypeInvalidRequest},
			},
			queryAt:       time.Minute,
			wantAvailable: []string{"1A", "1B", "1C"},
		},
		{
			name: "release frees the order's seats",
			ops: []flightOp{
				{at: time.Second, update: UpdateHold, arg: HoldRequest{"o1", []string{"1A", "1B"}, until}},
				{at: 2 * time.Second, update: UpdateRelease, arg: "o1"},
			},
			queryAt:       time.Minute,
			wantAvailable: []string{"1A", "1B", "1C"},
		},
		{
			name: "confirmed seats stay booked after release",
			ops: []flightOp{
				{at: time.Second, update: UpdateHold, arg: HoldRequest{"o1", []string{"1A"}, until}},
				{at: 2 * time.Second, update: UpdateConfirm, arg: ConfirmRequest{"o1", []string{"1A"}}},
				{at: 3 * time.Second, update: UpdateRelease, arg: "o1"},
			},
			queryAt:       time.Minute,
			wantAvailable: []string{"1B", "1C"},
		},
		{
			name: "confirm fails for seats the order does not hold",
			ops: []flightOp{
				{at: time.Second, update: UpdateHold, arg: HoldRequest{"o2", []string{"1A"}, until}},
				{at: 2 * time.Second, update: UpdateConfirm, arg: ConfirmRequest{"o1", []string{"1A"}}, wantErr: ErrTypeSeatUnavailable},
			},
			queryAt:       time.Minute,
			wantAvailable: []string{"1B", "1C"},
		},
		{
			name: "lapsed hold can be taken by another order",
			ops: []flightOp{
				{at: time.Second, update: UpdateHold, arg: HoldRequest{"o1", []string{"1A"}, start.Add(5 * time.Minute)}},
				{at: 6 * time.Minute, update: UpdateHold, arg: HoldRequest{"o2", []string{"1A"}, start.Add(20 * time.Minute)}},
			},
			queryAt:       7 * time.Minute,
			wantAvailable: []string{"1B", "1C"},
		},
		{
			name: "query shows a lapsed hold as available",
			ops: []flightOp{
				{at: time.Second, update: UpdateHold, arg: HoldRequest{"o1", []string{"1A"}, start.Add(5 * time.Minute)}},
			},
			queryAt:       10 * time.Minute,
			wantAvailable: []string{"1A", "1B", "1C"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var s testsuite.WorkflowTestSuite
			env := s.NewTestWorkflowEnvironment()
			env.SetStartTime(start)

			for i, op := range tt.ops {
				env.RegisterDelayedCallback(func() {
					env.UpdateWorkflow(op.update, fmt.Sprintf("u%d", i), &testsuite.TestUpdateCallback{
						OnAccept: func() {},
						OnReject: func(err error) { checkUpdateErr(t, op, err) },
						OnComplete: func(_ any, err error) {
							// Expected errors must come from the validator as a
							// rejection, which leaves no trace in history.
							if op.wantErr != "" {
								t.Errorf("%s %+v: completed with error %v, want rejection", op.update, op.arg, err)
								return
							}
							checkUpdateErr(t, op, err)
						},
					}, op.arg)
				}, op.at)
			}

			var available []string
			env.RegisterDelayedCallback(func() {
				v, err := env.QueryWorkflow(QueryAvailable, start.Add(tt.queryAt))
				if err != nil {
					t.Errorf("query %s: %v", QueryAvailable, err)
					return
				}
				if err := v.Get(&available); err != nil {
					t.Errorf("decode query result: %v", err)
				}
			}, tt.queryAt)
			env.RegisterDelayedCallback(env.CancelWorkflow, time.Hour)

			env.ExecuteWorkflow(FlightWorkflow, FlightInput{
				FlightID:  "LY001",
				Inventory: *booking.NewInventory([]string{"1A", "1B", "1C"}),
			})

			if err := env.GetWorkflowError(); !temporal.IsCanceledError(err) {
				t.Fatalf("workflow error = %v, want cancellation", err)
			}
			if !slices.Equal(available, tt.wantAvailable) {
				t.Errorf("available = %v, want %v", available, tt.wantAvailable)
			}
		})
	}
}

// checkUpdateErr reports whether an Update's outcome matches op.wantErr.
func checkUpdateErr(t *testing.T, op flightOp, err error) {
	t.Helper()
	if op.wantErr == "" {
		if err != nil {
			t.Errorf("%s %+v: unexpected error %v", op.update, op.arg, err)
		}
		return
	}
	appErr, ok := errors.AsType[*temporal.ApplicationError](err)
	if !ok || appErr.Type() != op.wantErr {
		t.Errorf("%s %+v: error = %v, want type %s", op.update, op.arg, err, op.wantErr)
	}
}

func TestFlightWorkflowContinuesAsNew(t *testing.T) {
	t.Parallel()

	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	env.SetStartTime(start)
	until := start.Add(15 * time.Minute)

	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(UpdateHold, "u1", &testsuite.TestUpdateCallback{
			OnAccept:   func() {},
			OnReject:   func(err error) { t.Errorf("hold rejected: %v", err) },
			OnComplete: func(any, error) {},
		}, HoldRequest{"o1", []string{"1A"}, until})
	}, time.Second)
	env.RegisterDelayedCallback(func() {
		env.SetContinueAsNewSuggested(true)
		// Any event makes the workflow re-check its Await condition.
		env.UpdateWorkflow(UpdateRelease, "u2", &testsuite.TestUpdateCallback{
			OnAccept:   func() {},
			OnReject:   func(err error) { t.Errorf("release rejected: %v", err) },
			OnComplete: func(any, error) {},
		}, "nobody")
	}, 2*time.Second)

	env.ExecuteWorkflow(FlightWorkflow, FlightInput{
		FlightID:  "LY001",
		Inventory: *booking.NewInventory([]string{"1A", "1B"}),
	})

	err := env.GetWorkflowError()
	canErr, ok := errors.AsType[*workflow.ContinueAsNewError](err)
	if !ok {
		t.Fatalf("workflow error = %v, want ContinueAsNewError", err)
	}
	var next FlightInput
	if err := converter.GetDefaultDataConverter().FromPayloads(canErr.Input, &next); err != nil {
		t.Fatalf("decode continue-as-new input: %v", err)
	}
	want := map[string]booking.Seat{
		"1A": {OrderID: "o1", HeldUntil: until},
		"1B": {},
	}
	if next.FlightID != "LY001" || !mapsEqual(next.Inventory.Seats, want) {
		t.Errorf("next input = %+v, want flight LY001 with seats %v", next, want)
	}
}

// mapsEqual compares seat maps with time.Time.Equal, because times decoded
// from JSON lose their monotonic clock reading and location pointer.
func mapsEqual(got, want map[string]booking.Seat) bool {
	if len(got) != len(want) {
		return false
	}
	for id, w := range want {
		g, ok := got[id]
		if !ok || g.OrderID != w.OrderID || g.Booked != w.Booked || !g.HeldUntil.Equal(w.HeldUntil) {
			return false
		}
	}
	return true
}

// fakeStarter records StartFlights' calls and fails on failOn's flight.
type fakeStarter struct {
	failOn string
	opts   []client.StartWorkflowOptions
	inputs []FlightInput
}

func (f *fakeStarter) ExecuteWorkflow(_ context.Context, opts client.StartWorkflowOptions, _ any, args ...any) (client.WorkflowRun, error) {
	f.opts = append(f.opts, opts)
	in, _ := args[0].(FlightInput)
	f.inputs = append(f.inputs, in)
	if opts.ID == FlightWorkflowID(f.failOn) {
		return nil, errors.New("server unavailable")
	}
	return nil, nil
}

func TestStartFlights(t *testing.T) {
	t.Parallel()

	flights := []booking.Flight{
		{ID: "F1", Seats: []string{"01A", "01B"}},
		{ID: "F2", Seats: []string{"01A"}},
	}

	tests := []struct {
		name        string
		failOn      string
		wantErr     bool
		wantStarted int
	}{
		{"starts every flight", "", false, 2},
		{"stops at the first failure", "F1", true, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeStarter{failOn: tt.failOn}

			err := StartFlights(context.Background(), fake, flights)

			if (err != nil) != tt.wantErr {
				t.Fatalf("StartFlights() error = %v, want error %v", err, tt.wantErr)
			}
			if len(fake.opts) != tt.wantStarted {
				t.Fatalf("started %d flights, want %d", len(fake.opts), tt.wantStarted)
			}
			for i, opts := range fake.opts {
				f := flights[i]
				if opts.ID != FlightWorkflowID(f.ID) || opts.TaskQueue != TaskQueue ||
					opts.WorkflowIDConflictPolicy != enumspb.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING {
					t.Errorf("flight %s options = %+v", f.ID, opts)
				}
				if got := fake.inputs[i]; got.FlightID != f.ID || !slices.Equal(got.Inventory.Available(start), f.Seats) {
					t.Errorf("flight %s input = %+v, want all seats free", f.ID, got)
				}
			}
		})
	}
}
