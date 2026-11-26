package booking

import (
	"slices"
	"testing"
)

func TestSeatIDs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		rows    int
		letters string
		want    []string
	}{
		{"one row", 1, "AB", []string{"01A", "01B"}},
		{"padded rows", 2, "A", []string{"01A", "02A"}},
		{"two digit rows", 10, "A", []string{"01A", "02A", "03A", "04A", "05A", "06A", "07A", "08A", "09A", "10A"}},
		{"no rows", 0, "ABC", []string{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := SeatIDs(tt.rows, tt.letters)
			if !slices.Equal(got, tt.want) {
				t.Errorf("SeatIDs(%d, %q) = %v, want %v", tt.rows, tt.letters, got, tt.want)
			}
			if !slices.IsSorted(got) {
				t.Errorf("SeatIDs(%d, %q) = %v, not in string order", tt.rows, tt.letters, got)
			}
		})
	}
}

func TestCatalog(t *testing.T) {
	t.Parallel()

	flights := Catalog()
	if len(flights) != 3 {
		t.Fatalf("len(Catalog()) = %d, want 3", len(flights))
	}
	seen := map[string]bool{}
	for _, f := range flights {
		if seen[f.ID] {
			t.Errorf("duplicate flight ID %s", f.ID)
		}
		seen[f.ID] = true
		if len(f.Seats) != 30 {
			t.Errorf("flight %s has %d seats, want 30", f.ID, len(f.Seats))
		}
	}

	// Each call returns its own copy.
	flights[0].Seats[0] = "changed"
	if Catalog()[0].Seats[0] == "changed" {
		t.Error("changing one Catalog() result changed the next one")
	}
}

func TestFindFlight(t *testing.T) {
	t.Parallel()

	tests := []struct {
		id     string
		wantOK bool
	}{
		{"FB100", true},
		{"FB300", true},
		{"NOPE", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			t.Parallel()
			f, ok := FindFlight(tt.id)
			if ok != tt.wantOK || (ok && f.ID != tt.id) {
				t.Errorf("FindFlight(%q) = %v, %v; want ok %v", tt.id, f.ID, ok, tt.wantOK)
			}
		})
	}
}
