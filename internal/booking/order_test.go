package booking

import "testing"

func TestStatusIsTerminal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		status Status
		want   bool
	}{
		{StatusCreated, false},
		{StatusSeatsHeld, false},
		{StatusPaying, false},
		{StatusConfirmed, true},
		{StatusFailed, true},
		{StatusExpired, true},
		{Status("UNKNOWN"), false},
	}

	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			t.Parallel()
			if got := tt.status.IsTerminal(); got != tt.want {
				t.Errorf("IsTerminal() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCanTransition(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		from, to Status
		want     bool
	}{
		{"select seats", StatusCreated, StatusSeatsHeld, true},
		{"idle order expires", StatusCreated, StatusExpired, true},
		{"change seats", StatusSeatsHeld, StatusSeatsHeld, true},
		{"start payment", StatusSeatsHeld, StatusPaying, true},
		{"hold expires", StatusSeatsHeld, StatusExpired, true},
		{"payment succeeds", StatusPaying, StatusConfirmed, true},
		{"payment fails", StatusPaying, StatusFailed, true},

		{"pay without seats", StatusCreated, StatusPaying, false},
		{"skip payment", StatusSeatsHeld, StatusConfirmed, false},
		{"change seats while paying", StatusPaying, StatusSeatsHeld, false},
		{"expire while paying", StatusPaying, StatusExpired, false},
		{"reopen confirmed", StatusConfirmed, StatusSeatsHeld, false},
		{"retry failed", StatusFailed, StatusPaying, false},
		{"revive expired", StatusExpired, StatusSeatsHeld, false},
		{"unknown from", Status("UNKNOWN"), StatusSeatsHeld, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := CanTransition(tt.from, tt.to); got != tt.want {
				t.Errorf("CanTransition(%s, %s) = %v, want %v", tt.from, tt.to, got, tt.want)
			}
		})
	}
}
