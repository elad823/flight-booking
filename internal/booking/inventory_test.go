package booking

import (
	"errors"
	"maps"
	"slices"
	"testing"
	"time"
)

var (
	t0    = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	later = t0.Add(15 * time.Minute)
)

func TestInventoryHold(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		initial map[string]Seat
		orderID string
		seats   []string
		now     time.Time
		wantErr error
		want    map[string]Seat
	}{
		{
			name:    "hold free seats",
			initial: map[string]Seat{"1A": {}, "1B": {}, "1C": {}},
			orderID: "o1", seats: []string{"1A", "1B"}, now: t0,
			want: map[string]Seat{
				"1A": {OrderID: "o1", HeldUntil: later},
				"1B": {OrderID: "o1", HeldUntil: later},
				"1C": {},
			},
		},
		{
			name: "change selection releases dropped seats and refreshes hold",
			initial: map[string]Seat{
				"1A": {OrderID: "o1", HeldUntil: t0},
				"1B": {OrderID: "o1", HeldUntil: t0},
				"1C": {},
			},
			orderID: "o1", seats: []string{"1B", "1C"}, now: t0,
			want: map[string]Seat{
				"1A": {},
				"1B": {OrderID: "o1", HeldUntil: later},
				"1C": {OrderID: "o1", HeldUntil: later},
			},
		},
		{
			name:    "take another order's lapsed hold",
			initial: map[string]Seat{"1A": {OrderID: "o2", HeldUntil: t0}},
			orderID: "o1", seats: []string{"1A"}, now: t0,
			want: map[string]Seat{"1A": {OrderID: "o1", HeldUntil: later}},
		},
		{
			name: "seat held by another order",
			initial: map[string]Seat{
				"1A": {},
				"1B": {OrderID: "o2", HeldUntil: later},
			},
			orderID: "o1", seats: []string{"1A", "1B"}, now: t0,
			wantErr: ErrSeatUnavailable,
		},
		{
			name: "failed change keeps the previous selection",
			initial: map[string]Seat{
				"1A": {OrderID: "o1", HeldUntil: later},
				"1B": {OrderID: "o2", HeldUntil: later},
			},
			orderID: "o1", seats: []string{"1B"}, now: t0,
			wantErr: ErrSeatUnavailable,
		},
		{
			name:    "booked seat",
			initial: map[string]Seat{"1A": {OrderID: "o2", Booked: true}},
			orderID: "o1", seats: []string{"1A"}, now: t0,
			wantErr: ErrSeatUnavailable,
		},
		{
			name:    "order's own booked seat",
			initial: map[string]Seat{"1A": {OrderID: "o1", Booked: true}},
			orderID: "o1", seats: []string{"1A"}, now: t0,
			wantErr: ErrSeatUnavailable,
		},
		{
			name:    "unknown seat",
			initial: map[string]Seat{"1A": {}},
			orderID: "o1", seats: []string{"1A", "9Z"}, now: t0,
			wantErr: ErrUnknownSeat,
		},
		{
			name:    "duplicate seat",
			initial: map[string]Seat{"1A": {}},
			orderID: "o1", seats: []string{"1A", "1A"}, now: t0,
			wantErr: ErrDuplicateSeat,
		},
		{
			name:    "no seats",
			initial: map[string]Seat{"1A": {}},
			orderID: "o1", seats: nil, now: t0,
			wantErr: ErrNoSeats,
		},
		{
			name:    "missing order ID",
			initial: map[string]Seat{"1A": {}},
			orderID: "", seats: []string{"1A"}, now: t0,
			wantErr: ErrMissingOrderID,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			inv := &Inventory{Seats: maps.Clone(tt.initial)}

			err := inv.Hold(tt.orderID, tt.seats, tt.now, later)

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Hold() error = %v, want %v", err, tt.wantErr)
			}
			want := tt.want
			if tt.wantErr != nil {
				want = tt.initial // all or nothing: a failed Hold changes nothing
			}
			if !maps.Equal(inv.Seats, want) {
				t.Errorf("seats = %v, want %v", inv.Seats, want)
			}
		})
	}
}

func TestInventoryRelease(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		initial map[string]Seat
		orderID string
		want    map[string]Seat
	}{
		{
			name: "frees the order's held seats only",
			initial: map[string]Seat{
				"1A": {OrderID: "o1", HeldUntil: later},
				"1B": {OrderID: "o2", HeldUntil: later},
			},
			orderID: "o1",
			want: map[string]Seat{
				"1A": {},
				"1B": {OrderID: "o2", HeldUntil: later},
			},
		},
		{
			name:    "keeps booked seats",
			initial: map[string]Seat{"1A": {OrderID: "o1", Booked: true}},
			orderID: "o1",
			want:    map[string]Seat{"1A": {OrderID: "o1", Booked: true}},
		},
		{
			name:    "order with no seats is a no-op",
			initial: map[string]Seat{"1A": {OrderID: "o2", HeldUntil: later}},
			orderID: "o1",
			want:    map[string]Seat{"1A": {OrderID: "o2", HeldUntil: later}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			inv := &Inventory{Seats: maps.Clone(tt.initial)}

			inv.Release(tt.orderID)

			if !maps.Equal(inv.Seats, tt.want) {
				t.Errorf("seats = %v, want %v", inv.Seats, tt.want)
			}
		})
	}
}

func TestInventoryConfirm(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		initial map[string]Seat
		orderID string
		seats   []string
		wantErr error
		want    map[string]Seat
	}{
		{
			name: "books held seats, even past HeldUntil",
			initial: map[string]Seat{
				"1A": {OrderID: "o1", HeldUntil: t0},
				"1B": {OrderID: "o1", HeldUntil: t0},
			},
			orderID: "o1", seats: []string{"1A", "1B"},
			want: map[string]Seat{
				"1A": {OrderID: "o1", Booked: true},
				"1B": {OrderID: "o1", Booked: true},
			},
		},
		{
			name:    "confirming twice succeeds",
			initial: map[string]Seat{"1A": {OrderID: "o1", Booked: true}},
			orderID: "o1", seats: []string{"1A"},
			want: map[string]Seat{"1A": {OrderID: "o1", Booked: true}},
		},
		{
			name: "one seat taken by another order",
			initial: map[string]Seat{
				"1A": {OrderID: "o1", HeldUntil: later},
				"1B": {OrderID: "o2", HeldUntil: later},
			},
			orderID: "o1", seats: []string{"1A", "1B"},
			wantErr: ErrNotHeld,
		},
		{
			name:    "unknown seat",
			initial: map[string]Seat{"1A": {OrderID: "o1", HeldUntil: later}},
			orderID: "o1", seats: []string{"1A", "9Z"},
			wantErr: ErrUnknownSeat,
		},
		{
			name:    "no seats",
			initial: map[string]Seat{"1A": {OrderID: "o1", HeldUntil: later}},
			orderID: "o1", seats: nil,
			wantErr: ErrNoSeats,
		},
		{
			name:    "missing order ID",
			initial: map[string]Seat{"1A": {}},
			orderID: "", seats: []string{"1A"},
			wantErr: ErrMissingOrderID,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			inv := &Inventory{Seats: maps.Clone(tt.initial)}

			err := inv.Confirm(tt.orderID, tt.seats)

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Confirm() error = %v, want %v", err, tt.wantErr)
			}
			want := tt.want
			if tt.wantErr != nil {
				want = tt.initial
			}
			if !maps.Equal(inv.Seats, want) {
				t.Errorf("seats = %v, want %v", inv.Seats, want)
			}
		})
	}
}

func TestInventoryAvailable(t *testing.T) {
	t.Parallel()

	inv := &Inventory{Seats: map[string]Seat{
		"1C": {},
		"1A": {},
		"1B": {OrderID: "o1", HeldUntil: later},
		"2A": {OrderID: "o2", HeldUntil: t0},
		"2B": {OrderID: "o3", Booked: true},
	}}

	got := inv.Available(t0)

	want := []string{"1A", "1C", "2A"}
	if !slices.Equal(got, want) {
		t.Errorf("Available() = %v, want %v", got, want)
	}
}

func TestNewInventory(t *testing.T) {
	t.Parallel()

	inv := NewInventory([]string{"1A", "1B"})

	if got := inv.Available(t0); !slices.Equal(got, []string{"1A", "1B"}) {
		t.Errorf("Available() = %v, want all seats", got)
	}
}
