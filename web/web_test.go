package web

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandler(t *testing.T) {
	t.Parallel()

	tests := []struct {
		path       string
		wantStatus int
		wantBody   string
	}{
		{"/", 200, "<title>Flight Booking</title>"},
		{"/missing.js", 404, ""},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()

			Handler().ServeHTTP(rec, httptest.NewRequest("GET", tt.path, nil))

			if rec.Code != tt.wantStatus || !strings.Contains(rec.Body.String(), tt.wantBody) {
				t.Errorf("GET %s = %d, want %d containing %q", tt.path, rec.Code, tt.wantStatus, tt.wantBody)
			}
		})
	}
}
