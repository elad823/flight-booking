package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"flightbooking/internal/booking"
)

// noBooking fails every call; the routes test only checks which handler
// answers.
type noBooking struct{}

var errUnused = errors.New("unused")

func (noBooking) CreateOrder(context.Context, string) (booking.Order, error) {
	return booking.Order{}, errUnused
}

func (noBooking) SelectSeats(context.Context, string, []string) (booking.Order, error) {
	return booking.Order{}, errUnused
}
func (noBooking) Pay(context.Context, string, string) error { return errUnused }
func (noBooking) OrderStatus(context.Context, string) (booking.Order, error) {
	return booking.Order{}, errUnused
}

func (noBooking) AvailableSeats(context.Context, string) ([]string, error) {
	return nil, errUnused
}

func TestRoutes(t *testing.T) {
	t.Parallel()

	h := routes(noBooking{}, slog.New(slog.DiscardHandler))

	tests := []struct {
		method, path string
		wantStatus   int
		wantBody     string
	}{
		{"GET", "/", 200, "<title>Flight Booking</title>"},
		{"GET", "/api/flights", 200, `"id":"FB100"`},
		{"PUT", "/api/orders/o1/seats", 400, `"error"`},
		{"GET", "/nope", 404, ""},
	}

	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()

			h.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, strings.NewReader("")))

			if rec.Code != tt.wantStatus || !strings.Contains(rec.Body.String(), tt.wantBody) {
				t.Errorf("%s %s = %d %q, want %d containing %q", tt.method, tt.path, rec.Code, rec.Body.String(), tt.wantStatus, tt.wantBody)
			}
		})
	}
}
