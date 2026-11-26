// Package api serves the booking system's REST API. Handlers turn HTTP
// requests into calls on a Booking and its errors into status codes; they
// know nothing about Temporal.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"flightbooking/internal/booking"
)

// Booking is what the API needs from the booking system.
type Booking interface {
	CreateOrder(ctx context.Context, flightID string) (booking.Order, error)
	SelectSeats(ctx context.Context, orderID string, seats []string) (booking.Order, error)
	Pay(ctx context.Context, orderID, code string) error
	OrderStatus(ctx context.Context, orderID string) (booking.Order, error)
	AvailableSeats(ctx context.Context, flightID string) ([]string, error)
}

// Kind says what went wrong with a request, which decides its status code.
type Kind int

// Kinds of request errors.
const (
	KindInvalid  Kind = iota + 1 // 400: the request itself is wrong
	KindNotFound                 // 404: no such order or flight
	KindConflict                 // 409: valid, but the current state refuses it
)

// Error is a request error whose message is safe to show to the user. A
// Booking returns it for anything the user can act on; any other error is
// logged and answered with a plain 500.
type Error struct {
	Kind    Kind
	Message string
}

func (e *Error) Error() string { return e.Message }

const (
	// maxBodyBytes bounds request bodies; the largest valid one, a full
	// seat list, is well under this.
	maxBodyBytes = 4 << 10
	// maxSeatIDLen bounds a seat ID, which error messages repeat back.
	maxSeatIDLen = 8
)

type server struct {
	booking Booking
	log     *slog.Logger
}

// NewHandler returns the API's routes:
//
//	GET  /api/flights                 the flight catalog
//	GET  /api/flights/{id}/seats      a flight's available seats
//	POST /api/orders                  create an order for a flight
//	GET  /api/orders/{id}             an order's status
//	PUT  /api/orders/{id}/seats       replace the order's seat selection
//	POST /api/orders/{id}/payment     start payment; poll the status for the result
func NewHandler(b Booking, log *slog.Logger) http.Handler {
	s := &server{booking: b, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/flights", s.listFlights)
	mux.HandleFunc("GET /api/flights/{id}/seats", s.availableSeats)
	mux.HandleFunc("POST /api/orders", s.createOrder)
	mux.HandleFunc("GET /api/orders/{id}", s.orderStatus)
	mux.HandleFunc("PUT /api/orders/{id}/seats", s.selectSeats)
	mux.HandleFunc("POST /api/orders/{id}/payment", s.pay)
	return mux
}

func (s *server) listFlights(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, booking.Catalog())
}

type seatsResponse struct {
	Available []string `json:"available"`
}

func (s *server) availableSeats(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := booking.FindFlight(id); !ok {
		s.writeError(w, r, &Error{Kind: KindNotFound, Message: "flight not found"})
		return
	}
	seats, err := s.booking.AvailableSeats(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, seatsResponse{Available: seats})
}

func (s *server) createOrder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FlightID string `json:"flightId"`
	}
	if err := decode(w, r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if _, ok := booking.FindFlight(req.FlightID); !ok {
		s.writeError(w, r, &Error{Kind: KindInvalid, Message: "unknown flight"})
		return
	}
	o, err := s.booking.CreateOrder(r.Context(), req.FlightID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/orders/"+o.ID)
	s.writeJSON(w, http.StatusCreated, o)
}

func (s *server) orderStatus(w http.ResponseWriter, r *http.Request) {
	o, err := s.booking.OrderStatus(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, o)
}

func (s *server) selectSeats(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Seats []string `json:"seats"`
	}
	if err := decode(w, r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	for _, id := range req.Seats {
		if !validSeatID(id) {
			s.writeError(w, r, &Error{Kind: KindInvalid, Message: "seat IDs must be 1 to 8 capital letters or digits"})
			return
		}
	}
	o, err := s.booking.SelectSeats(r.Context(), r.PathValue("id"), req.Seats)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, o)
}

func (s *server) pay(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if err := decode(w, r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.booking.Pay(r.Context(), r.PathValue("id"), req.Code); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// decode reads a JSON body of at most maxBodyBytes into v. Unknown fields
// are refused, so a misspelled field fails loudly instead of being ignored.
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return &Error{Kind: KindInvalid, Message: "request body must be a small JSON object with the expected fields"}
	}
	return nil
}

// validSeatID reports whether id has the shape of a seat ID: 1 to
// maxSeatIDLen ASCII capital letters or digits. Whether the seat exists is
// the flight's call.
func validSeatID(id string) bool {
	if id == "" || len(id) > maxSeatIDLen {
		return false
	}
	for i := range len(id) {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}

type errorResponse struct {
	Error string `json:"error"`
}

// writeError answers with the status for err's Kind and its message. Any
// other error may hold internal details, so it is logged and the user gets
// a plain 500.
func (s *server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	if apiErr, ok := errors.AsType[*Error](err); ok {
		switch apiErr.Kind {
		case KindInvalid:
			s.writeJSON(w, http.StatusBadRequest, errorResponse{Error: apiErr.Message})
			return
		case KindNotFound:
			s.writeJSON(w, http.StatusNotFound, errorResponse{Error: apiErr.Message})
			return
		case KindConflict:
			s.writeJSON(w, http.StatusConflict, errorResponse{Error: apiErr.Message})
			return
		}
	}
	s.log.Error("Request failed", "method", r.Method, "path", r.URL.Path, "error", err)
	s.writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal error"})
}

func (s *server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.log.Warn("Write response failed", "error", err)
	}
}
