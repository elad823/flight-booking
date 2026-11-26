package booking

import "fmt"

// Flight describes one bookable flight.
type Flight struct {
	ID    string   `json:"id"`
	From  string   `json:"from"`
	To    string   `json:"to"`
	Seats []string `json:"seats"`
}

// SeatIDs returns the seat IDs of a cabin with the given rows and seat
// letters per row, such as "01A". Rows are zero-padded so the IDs sort in
// seat order as plain strings.
func SeatIDs(rows int, letters string) []string {
	ids := make([]string, 0, rows*len(letters))
	for row := 1; row <= rows; row++ {
		for _, letter := range letters {
			ids = append(ids, fmt.Sprintf("%02d%c", row, letter))
		}
	}
	return ids
}

// Catalog returns the flights the system sells. The list is fixed in code;
// each call builds a new copy, so callers may change what they get.
func Catalog() []Flight {
	return []Flight{
		{ID: "FB100", From: "TLV", To: "LHR", Seats: SeatIDs(10, "ABC")},
		{ID: "FB200", From: "TLV", To: "JFK", Seats: SeatIDs(10, "ABC")},
		{ID: "FB300", From: "LHR", To: "CDG", Seats: SeatIDs(10, "ABC")},
	}
}

// FindFlight returns the flight with the given ID from the catalog.
func FindFlight(id string) (Flight, bool) {
	for _, f := range Catalog() {
		if f.ID == id {
			return f, true
		}
	}
	return Flight{}, false
}
