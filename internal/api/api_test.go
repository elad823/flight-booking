package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"flightbooking/internal/booking"
)

// fakeBooking returns canned results and records each call as text.
type fakeBooking struct {
	order booking.Order
	seats []string
	err   error
	calls []string
}

func (f *fakeBooking) CreateOrder(_ context.Context, flightID string) (booking.Order, error) {
	f.calls = append(f.calls, "CreateOrder "+flightID)
	return f.order, f.err
}

func (f *fakeBooking) SelectSeats(_ context.Context, orderID string, seats []string) (booking.Order, error) {
	f.calls = append(f.calls, fmt.Sprintf("SelectSeats %s %v", orderID, seats))
	return f.order, f.err
}

func (f *fakeBooking) Pay(_ context.Context, orderID, code string) error {
	f.calls = append(f.calls, "Pay "+orderID+" "+code)
	return f.err
}

func (f *fakeBooking) OrderStatus(_ context.Context, orderID string) (booking.Order, error) {
	f.calls = append(f.calls, "OrderStatus "+orderID)
	return f.order, f.err
}

func (f *fakeBooking) AvailableSeats(_ context.Context, flightID string) ([]string, error) {
	f.calls = append(f.calls, "AvailableSeats "+flightID)
	return f.seats, f.err
}

func TestHandler(t *testing.T) {
	t.Parallel()

	held := booking.Order{
		ID: "o1", FlightID: "FB100", Status: booking.StatusSeatsHeld,
		Seats: []string{"01A"}, ExpiresAt: time.Date(2026, 1, 1, 12, 15, 0, 0, time.UTC),
	}
	seatTaken := &Error{Kind: KindConflict, Message: "seat 01A: seat unavailable"}
	internal := errors.New("dial tcp 10.0.0.7:7233: connection refused")

	tests := []struct {
		name       string
		method     string
		path       string
		body       string
		fake       fakeBooking
		wantStatus int
		wantBody   string // substring of the response body
		wantCall   string // "" means the Booking must not be called
		plainText  bool   // the router answers, not our handlers
	}{
		{
			name: "list flights", method: "GET", path: "/api/flights",
			wantStatus: 200, wantBody: `"id":"FB100"`,
		},
		{
			name: "available seats", method: "GET", path: "/api/flights/FB100/seats",
			fake:       fakeBooking{seats: []string{"01A", "01B"}},
			wantStatus: 200, wantBody: `{"available":["01A","01B"]}`,
			wantCall: "AvailableSeats FB100",
		},
		{
			name: "available seats of an unknown flight", method: "GET", path: "/api/flights/XX1/seats",
			wantStatus: 404, wantBody: "flight not found",
		},
		{
			name: "internal error is hidden", method: "GET", path: "/api/flights/FB100/seats",
			fake:       fakeBooking{err: internal},
			wantStatus: 500, wantBody: `{"error":"internal error"}`,
			wantCall: "AvailableSeats FB100",
		},
		{
			name: "create order", method: "POST", path: "/api/orders", body: `{"flightId":"FB100"}`,
			fake:       fakeBooking{order: booking.Order{ID: "o1", FlightID: "FB100", Status: booking.StatusCreated}},
			wantStatus: 201, wantBody: `"status":"CREATED"`,
			wantCall: "CreateOrder FB100",
		},
		{
			name: "create order for an unknown flight", method: "POST", path: "/api/orders", body: `{"flightId":"XX1"}`,
			wantStatus: 400, wantBody: "unknown flight",
		},
		{
			name: "malformed JSON", method: "POST", path: "/api/orders", body: `{"flightId":`,
			wantStatus: 400, wantBody: "JSON object",
		},
		{
			name: "unknown field", method: "POST", path: "/api/orders", body: `{"flightId":"FB100","seats":["01A"]}`,
			wantStatus: 400, wantBody: "JSON object",
		},
		{
			name: "body too large", method: "POST", path: "/api/orders",
			body:       `{"flightId":"` + strings.Repeat("x", maxBodyBytes) + `"}`,
			wantStatus: 400, wantBody: "JSON object",
		},
		{
			name: "order status", method: "GET", path: "/api/orders/o1",
			fake:       fakeBooking{order: held},
			wantStatus: 200, wantBody: `"expiresAt":"2026-01-01T12:15:00Z"`,
			wantCall: "OrderStatus o1",
		},
		{
			name: "unknown order", method: "GET", path: "/api/orders/nope",
			fake:       fakeBooking{err: &Error{Kind: KindNotFound, Message: "order not found"}},
			wantStatus: 404, wantBody: "order not found",
			wantCall: "OrderStatus nope",
		},
		{
			name: "select seats", method: "PUT", path: "/api/orders/o1/seats", body: `{"seats":["01A","02B"]}`,
			fake:       fakeBooking{order: held},
			wantStatus: 200, wantBody: `"status":"SEATS_HELD"`,
			wantCall: "SelectSeats o1 [01A 02B]",
		},
		{
			name: "seat taken", method: "PUT", path: "/api/orders/o1/seats", body: `{"seats":["01A"]}`,
			fake:       fakeBooking{err: seatTaken},
			wantStatus: 409, wantBody: "seat 01A: seat unavailable",
			wantCall: "SelectSeats o1 [01A]",
		},
		{
			name: "seat ID with markup", method: "PUT", path: "/api/orders/o1/seats", body: `{"seats":["<b>1A"]}`,
			wantStatus: 400, wantBody: "seat IDs must be",
		},
		{
			name: "seat ID too long", method: "PUT", path: "/api/orders/o1/seats", body: `{"seats":["123456789"]}`,
			wantStatus: 400, wantBody: "seat IDs must be",
		},
		{
			name: "empty seat ID", method: "PUT", path: "/api/orders/o1/seats", body: `{"seats":[""]}`,
			wantStatus: 400, wantBody: "seat IDs must be",
		},
		{
			name: "pay", method: "POST", path: "/api/orders/o1/payment", body: `{"code":"12345"}`,
			wantStatus: 202,
			wantCall:   "Pay o1 12345",
		},
		{
			name: "pay with an invalid code", method: "POST", path: "/api/orders/o1/payment", body: `{"code":"12a45"}`,
			fake:       fakeBooking{err: &Error{Kind: KindInvalid, Message: "payment code must be exactly 5 digits"}},
			wantStatus: 400, wantBody: "exactly 5 digits",
			wantCall: "Pay o1 12a45",
		},
		{
			name: "wrong method", method: "DELETE", path: "/api/orders/o1",
			wantStatus: 405, plainText: true,
		},
		{
			name: "unknown path", method: "GET", path: "/api/nope",
			wantStatus: 404, plainText: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := tt.fake
			h := NewHandler(&fake, slog.New(slog.DiscardHandler))
			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			rec := httptest.NewRecorder()

			h.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d (body %q)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tt.wantBody) {
				t.Errorf("body = %q, want it to contain %q", rec.Body.String(), tt.wantBody)
			}
			if ct := rec.Header().Get("Content-Type"); !tt.plainText && rec.Body.Len() > 0 && ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			var gotCall string
			if len(fake.calls) > 0 {
				gotCall = strings.Join(fake.calls, "; ")
			}
			if gotCall != tt.wantCall {
				t.Errorf("Booking calls = %q, want %q", gotCall, tt.wantCall)
			}
		})
	}
}

func TestCreateOrderLocation(t *testing.T) {
	t.Parallel()

	fake := &fakeBooking{order: booking.Order{ID: "o1", FlightID: "FB100", Status: booking.StatusCreated}}
	h := NewHandler(fake, slog.New(slog.DiscardHandler))
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/orders", strings.NewReader(`{"flightId":"FB100"}`)))

	if got := rec.Header().Get("Location"); got != "/api/orders/o1" {
		t.Errorf("Location = %q, want /api/orders/o1", got)
	}
}

func TestValidSeatID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		id   string
		want bool
	}{
		{"01A", true},
		{"12345678", true},
		{"123456789", false},
		{"", false},
		{"01a", false},
		{"01 A", false},
		{"0١A", false}, // a non-ASCII digit
	}

	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			t.Parallel()
			if got := validSeatID(tt.id); got != tt.want {
				t.Errorf("validSeatID(%q) = %v, want %v", tt.id, got, tt.want)
			}
		})
	}
}
