package booking

import (
	"errors"
	"fmt"
	"slices"
	"time"
)

// Errors returned by Inventory. Callers check them with errors.Is, because
// they are wrapped with the seat ID they refer to.
var (
	ErrMissingOrderID  = errors.New("order ID is required")
	ErrNoSeats         = errors.New("no seats requested")
	ErrDuplicateSeat   = errors.New("seat requested more than once")
	ErrUnknownSeat     = errors.New("unknown seat")
	ErrSeatUnavailable = errors.New("seat unavailable")
	ErrNotHeld         = errors.New("seat not held by this order")
)

// Seat is the state of one seat. An empty OrderID means the seat is free.
type Seat struct {
	OrderID   string    `json:"orderId,omitempty"`
	Booked    bool      `json:"booked,omitempty"`
	HeldUntil time.Time `json:"heldUntil,omitzero"`
}

// free reports whether no order owns the seat at time now. A hold counts
// as free once its HeldUntil has passed.
func (s Seat) free(now time.Time) bool {
	return !s.Booked && (s.OrderID == "" || !now.Before(s.HeldUntil))
}

// freeFor reports whether orderID may take the seat at time now: it is
// free, or the order already holds it.
func (s Seat) freeFor(orderID string, now time.Time) bool {
	return s.free(now) || (!s.Booked && s.OrderID == orderID)
}

// Inventory is the seat map of one flight.
//
// It is not safe for concurrent use. Its only owner is the flight's workflow,
// which Temporal runs one step at a time, so a lock would protect nothing.
// Methods take the current time as an argument instead of calling time.Now,
// which keeps them deterministic, as workflow code must be.
//
// Seats is exported so the inventory can be serialized, for example as
// Continue-As-New input. Change it only through the methods.
type Inventory struct {
	Seats map[string]Seat `json:"seats"`
}

// NewInventory returns an inventory where every given seat is free.
func NewInventory(seatIDs []string) *Inventory {
	seats := make(map[string]Seat, len(seatIDs))
	for _, id := range seatIDs {
		seats[id] = Seat{}
	}
	return &Inventory{Seats: seats}
}

// Hold makes seatIDs the order's complete selection, held until the given
// time. Seats the order held before that are not in seatIDs are released, so
// calling Hold again both changes the selection and refreshes the hold.
// It is all or nothing: on error, the inventory is unchanged.
func (inv *Inventory) Hold(orderID string, seatIDs []string, now, until time.Time) error {
	if orderID == "" {
		return ErrMissingOrderID
	}
	if len(seatIDs) == 0 {
		return ErrNoSeats
	}

	seen := make(map[string]bool, len(seatIDs))
	for _, id := range seatIDs {
		if seen[id] {
			return fmt.Errorf("seat %s: %w", id, ErrDuplicateSeat)
		}
		seen[id] = true

		seat, ok := inv.Seats[id]
		if !ok {
			return fmt.Errorf("seat %s: %w", id, ErrUnknownSeat)
		}
		if !seat.freeFor(orderID, now) {
			return fmt.Errorf("seat %s: %w", id, ErrSeatUnavailable)
		}
	}

	inv.Release(orderID)
	for _, id := range seatIDs {
		inv.Seats[id] = Seat{OrderID: orderID, HeldUntil: until}
	}
	return nil
}

// Release frees every seat the order holds. Booked seats stay booked.
// Releasing an order that holds nothing does nothing, so retries are safe.
func (inv *Inventory) Release(orderID string) {
	if orderID == "" {
		return
	}
	for id, seat := range inv.Seats {
		if seat.OrderID == orderID && !seat.Booked {
			inv.Seats[id] = Seat{}
		}
	}
}

// Confirm books the given seats for the order. Every seat must still carry
// the order's ID. HeldUntil is ignored: the order workflow decides whether a
// payment came in time, and a seat that still carries the order's ID was not
// taken by anyone else. It is all or nothing, and confirming the same seats
// twice succeeds, so retries are safe.
func (inv *Inventory) Confirm(orderID string, seatIDs []string) error {
	if orderID == "" {
		return ErrMissingOrderID
	}
	if len(seatIDs) == 0 {
		return ErrNoSeats
	}

	for _, id := range seatIDs {
		seat, ok := inv.Seats[id]
		if !ok {
			return fmt.Errorf("seat %s: %w", id, ErrUnknownSeat)
		}
		if seat.OrderID != orderID {
			return fmt.Errorf("seat %s: %w", id, ErrNotHeld)
		}
	}

	for _, id := range seatIDs {
		inv.Seats[id] = Seat{OrderID: orderID, Booked: true}
	}
	return nil
}

// Available returns the IDs of the seats that are free at time now, sorted.
func (inv *Inventory) Available(now time.Time) []string {
	ids := make([]string, 0, len(inv.Seats))
	for id, seat := range inv.Seats {
		if seat.free(now) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}
