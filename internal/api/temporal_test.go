package api

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"

	"flightbooking/internal/booking"
	"flightbooking/internal/workflows"
)

// fakeClient stands in for the Temporal client. Each call returns err for
// its kind of call and result as the decoded value, and records its input.
type fakeClient struct {
	startErr, updateErr, getErr, queryErr error
	result                                any

	startOpts  []client.StartWorkflowOptions
	startArgs  []any
	updateOpts client.UpdateWorkflowOptions
	queryID    string
	queryType  string
	queryArgs  []any
}

func (f *fakeClient) ExecuteWorkflow(_ context.Context, opts client.StartWorkflowOptions, _ any, args ...any) (client.WorkflowRun, error) {
	f.startOpts = append(f.startOpts, opts)
	f.startArgs = append(f.startArgs, args...)
	return nil, f.startErr
}

func (f *fakeClient) UpdateWorkflow(_ context.Context, opts client.UpdateWorkflowOptions) (client.WorkflowUpdateHandle, error) {
	f.updateOpts = opts
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	return fakeHandle{err: f.getErr, result: f.result}, nil
}

func (f *fakeClient) QueryWorkflow(_ context.Context, workflowID, _, queryType string, args ...any) (converter.EncodedValue, error) {
	f.queryID, f.queryType, f.queryArgs = workflowID, queryType, args
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	return fakeValue{v: f.result}, nil
}

type fakeHandle struct {
	client.WorkflowUpdateHandle // only Get is used
	err                         error
	result                      any
}

func (h fakeHandle) Get(_ context.Context, ptr any) error {
	if h.err != nil || ptr == nil {
		return h.err
	}
	return roundTrip(h.result, ptr)
}

type fakeValue struct{ v any }

func (fakeValue) HasValue() bool      { return true }
func (v fakeValue) Get(ptr any) error { return roundTrip(v.v, ptr) }

// roundTrip decodes v into ptr through JSON, as Temporal's converter does.
func roundTrip(v, ptr any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, ptr)
}

func TestCreateOrder(t *testing.T) {
	t.Parallel()

	fake := &fakeClient{}
	b := NewTemporalBooking(fake)

	o1, err := b.CreateOrder(context.Background(), "FB100")
	if err != nil {
		t.Fatalf("CreateOrder() error = %v", err)
	}
	o2, err := b.CreateOrder(context.Background(), "FB100")
	if err != nil {
		t.Fatalf("CreateOrder() error = %v", err)
	}

	if o1.ID == "" || o1.ID == o2.ID {
		t.Errorf("order IDs = %q, %q; want two different non-empty IDs", o1.ID, o2.ID)
	}
	if o1.Status != booking.StatusCreated || o1.FlightID != "FB100" || o1.Seats == nil {
		t.Errorf("order = %+v, want CREATED on FB100 with an empty seat list", o1)
	}
	opts := fake.startOpts[0]
	if opts.ID != workflows.OrderWorkflowID(o1.ID) || opts.TaskQueue != workflows.TaskQueue {
		t.Errorf("start options = %+v", opts)
	}
	want := workflows.OrderInput{OrderID: o1.ID, FlightID: "FB100"}
	if fake.startArgs[0] != want {
		t.Errorf("start input = %+v, want %+v", fake.startArgs[0], want)
	}
}

func TestCreateOrderStartFails(t *testing.T) {
	t.Parallel()

	b := NewTemporalBooking(&fakeClient{startErr: errors.New("unavailable")})

	_, err := b.CreateOrder(context.Background(), "FB100")

	if _, ok := errors.AsType[*Error](err); ok || err == nil {
		t.Errorf("CreateOrder() error = %v, want an internal error", err)
	}
}

func TestUpdates(t *testing.T) {
	t.Parallel()

	held := booking.Order{ID: "o1", FlightID: "FB100", Status: booking.StatusSeatsHeld, Seats: []string{"01A"}}
	taken := temporal.NewNonRetryableApplicationError("seat 01A: seat unavailable", workflows.ErrTypeSeatUnavailable, nil)

	t.Run("select seats", func(t *testing.T) {
		t.Parallel()
		fake := &fakeClient{result: held}
		got, err := NewTemporalBooking(fake).SelectSeats(context.Background(), "o1", []string{"01A"})
		if err != nil || got.Status != booking.StatusSeatsHeld || !slices.Equal(got.Seats, []string{"01A"}) {
			t.Errorf("SelectSeats() = %+v, %v", got, err)
		}
		checkUpdate(t, fake.updateOpts, workflows.UpdateSelectSeats, []string{"01A"})
	})

	t.Run("pay", func(t *testing.T) {
		t.Parallel()
		fake := &fakeClient{}
		if err := NewTemporalBooking(fake).Pay(context.Background(), "o1", "12345"); err != nil {
			t.Errorf("Pay() error = %v", err)
		}
		checkUpdate(t, fake.updateOpts, workflows.UpdatePay, "12345")
	})

	errTests := []struct {
		name     string
		fake     fakeClient
		wantKind Kind
	}{
		{"rejected by the validator", fakeClient{updateErr: taken}, KindConflict},
		{"failed in the handler", fakeClient{getErr: taken}, KindConflict},
		{"unknown or closed order", fakeClient{updateErr: serviceerror.NewNotFound("workflow execution already completed")}, KindNotFound},
	}
	for _, tt := range errTests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := tt.fake
			_, err := NewTemporalBooking(&fake).SelectSeats(context.Background(), "o1", []string{"01A"})
			apiErr, ok := errors.AsType[*Error](err)
			if !ok || apiErr.Kind != tt.wantKind {
				t.Errorf("SelectSeats() error = %v, want kind %d", err, tt.wantKind)
			}
		})
	}
}

func checkUpdate(t *testing.T, opts client.UpdateWorkflowOptions, name string, arg any) {
	t.Helper()
	if opts.WorkflowID != workflows.OrderWorkflowID("o1") || opts.UpdateName != name ||
		opts.WaitForStage != client.WorkflowUpdateStageCompleted || len(opts.Args) != 1 {
		t.Errorf("update options = %+v", opts)
		return
	}
	got, _ := json.Marshal(opts.Args[0])
	want, _ := json.Marshal(arg)
	if string(got) != string(want) {
		t.Errorf("update arg = %s, want %s", got, want)
	}
}

func TestOrderStatus(t *testing.T) {
	t.Parallel()

	fake := &fakeClient{result: booking.Order{ID: "o1", Status: booking.StatusConfirmed}}
	got, err := NewTemporalBooking(fake).OrderStatus(context.Background(), "o1")
	if err != nil || got.Status != booking.StatusConfirmed {
		t.Errorf("OrderStatus() = %+v, %v", got, err)
	}
	if fake.queryID != workflows.OrderWorkflowID("o1") || fake.queryType != workflows.QueryStatus {
		t.Errorf("query = %s %s", fake.queryID, fake.queryType)
	}

	missing := &fakeClient{queryErr: serviceerror.NewNotFound("workflow not found")}
	_, err = NewTemporalBooking(missing).OrderStatus(context.Background(), "nope")
	if apiErr, ok := errors.AsType[*Error](err); !ok || apiErr.Kind != KindNotFound {
		t.Errorf("OrderStatus(missing) error = %v, want not found", err)
	}
}

func TestAvailableSeats(t *testing.T) {
	t.Parallel()

	fake := &fakeClient{result: []string{"01B"}}
	before := time.Now()
	got, err := NewTemporalBooking(fake).AvailableSeats(context.Background(), "FB100")
	if err != nil || !slices.Equal(got, []string{"01B"}) {
		t.Errorf("AvailableSeats() = %v, %v", got, err)
	}
	if fake.queryID != workflows.FlightWorkflowID("FB100") || fake.queryType != workflows.QueryAvailable {
		t.Errorf("query = %s %s", fake.queryID, fake.queryType)
	}
	if len(fake.queryArgs) != 1 {
		t.Fatalf("query args = %v, want the current time", fake.queryArgs)
	}
	if now, ok := fake.queryArgs[0].(time.Time); !ok || now.Before(before) || now.After(time.Now()) {
		t.Errorf("query arg = %v, want the current time", fake.queryArgs[0])
	}

	// The handler already checked the catalog, so a missing flight
	// workflow is an internal fault, not a 404.
	missing := &fakeClient{queryErr: serviceerror.NewNotFound("workflow not found")}
	_, err = NewTemporalBooking(missing).AvailableSeats(context.Background(), "FB100")
	if _, ok := errors.AsType[*Error](err); ok || err == nil {
		t.Errorf("AvailableSeats(missing) error = %v, want an internal error", err)
	}
}

func TestRequestError(t *testing.T) {
	t.Parallel()

	appErr := func(errType string) error {
		return temporal.NewNonRetryableApplicationError("msg "+errType, errType, nil)
	}
	notFound := serviceerror.NewNotFound("workflow not found")

	tests := []struct {
		name     string
		err      error
		notFound string
		wantKind Kind // 0 means an internal error
		wantMsg  string
	}{
		{"not found", notFound, "order not found", KindNotFound, "order not found"},
		{"not found without a message", notFound, "", 0, ""},
		{"seat unavailable", appErr(workflows.ErrTypeSeatUnavailable), "", KindConflict, "msg SeatUnavailable"},
		{"invalid order state", appErr(workflows.ErrTypeInvalidOrderState), "", KindConflict, "msg InvalidOrderState"},
		{"invalid seat request", appErr(workflows.ErrTypeInvalidRequest), "", KindInvalid, "msg InvalidSeatRequest"},
		{"invalid payment code", appErr(workflows.ErrTypeInvalidPaymentCode), "", KindInvalid, "msg InvalidPaymentCode"},
		{"other application error", appErr("Panic"), "", 0, ""},
		{"plain error", errors.New("connection refused"), "x", 0, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := requestError(tt.err, tt.notFound, "act")
			apiErr, ok := errors.AsType[*Error](err)
			if tt.wantKind == 0 {
				if ok {
					t.Errorf("requestError() = %+v, want an internal error", apiErr)
				}
				if !errors.Is(err, tt.err) {
					t.Errorf("requestError() = %v, want it to wrap %v", err, tt.err)
				}
				return
			}
			if !ok || apiErr.Kind != tt.wantKind || apiErr.Message != tt.wantMsg {
				t.Errorf("requestError() = %v, want kind %d %q", err, tt.wantKind, tt.wantMsg)
			}
		})
	}
}
