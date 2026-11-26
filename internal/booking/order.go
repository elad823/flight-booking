// Package booking holds the flight booking domain: order statuses, seats, and
// the rules between them. It does not import Temporal, so its logic is tested
// with plain unit tests and the workflows stay thin.
package booking

import "time"

// Status is the lifecycle state of an order.
type Status string

// Order statuses. The happy path is Created, SeatsHeld, Paying, Confirmed.
const (
	// StatusCreated means the order exists but no seats are held yet.
	StatusCreated Status = "CREATED"
	// StatusSeatsHeld means seats are held and the hold timer is running.
	StatusSeatsHeld Status = "SEATS_HELD"
	// StatusPaying means a payment is being processed.
	StatusPaying Status = "PAYING"
	// StatusConfirmed means payment succeeded and the seats are booked.
	StatusConfirmed Status = "CONFIRMED"
	// StatusFailed means payment failed and the seats were released.
	StatusFailed Status = "FAILED"
	// StatusExpired means the hold timer ran out and the seats were released.
	StatusExpired Status = "EXPIRED"
)

// IsTerminal reports whether an order in this status can no longer change.
func (s Status) IsTerminal() bool {
	switch s {
	case StatusConfirmed, StatusFailed, StatusExpired:
		return true
	default:
		return false
	}
}

// CanTransition reports whether an order may move from one status to another.
// SeatsHeld to SeatsHeld is allowed because changing seats restarts the hold.
func CanTransition(from, to Status) bool {
	switch from {
	case StatusCreated:
		return to == StatusSeatsHeld || to == StatusExpired
	case StatusSeatsHeld:
		return to == StatusSeatsHeld || to == StatusPaying || to == StatusExpired
	case StatusPaying:
		return to == StatusConfirmed || to == StatusFailed
	default:
		return false
	}
}

// Order is a snapshot of an order, returned to clients by the status query.
type Order struct {
	ID       string   `json:"id"`
	FlightID string   `json:"flightId"`
	Status   Status   `json:"status"`
	Seats    []string `json:"seats"`
	// ExpiresAt is when the seat hold ends. It is zero while no seats are held.
	ExpiresAt time.Time `json:"expiresAt,omitzero"`
	// Message explains the current status to the user, such as why payment failed.
	Message string `json:"message,omitempty"`
}
