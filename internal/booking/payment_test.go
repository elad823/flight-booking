package booking

import (
	"errors"
	"math"
	"testing"
)

func TestValidatePaymentCode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		code    string
		wantErr error
	}{
		{"five digits", "12345", nil},
		{"leading zeros", "00000", nil},
		{"empty", "", ErrInvalidPaymentCode},
		{"too short", "1234", ErrInvalidPaymentCode},
		{"too long", "123456", ErrInvalidPaymentCode},
		{"letter", "12a45", ErrInvalidPaymentCode},
		{"space", "12 45", ErrInvalidPaymentCode},
		{"sign", "-1234", ErrInvalidPaymentCode},
		{"non-ASCII digits", "١٢٣٤٥", ErrInvalidPaymentCode},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := ValidatePaymentCode(tt.code); !errors.Is(err, tt.wantErr) {
				t.Errorf("ValidatePaymentCode(%q) = %v, want %v", tt.code, err, tt.wantErr)
			}
		})
	}
}

func TestNewPaymentSimulator(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		rate    float64
		wantErr error
	}{
		{"zero", 0, nil},
		{"default", 0.15, nil},
		{"one", 1, nil},
		{"negative", -0.1, ErrInvalidFailureRate},
		{"above one", 1.1, ErrInvalidFailureRate},
		{"NaN", math.NaN(), ErrInvalidFailureRate},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewPaymentSimulator(tt.rate, func() float64 { return 0 })
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("NewPaymentSimulator(%v) error = %v, want %v", tt.rate, err, tt.wantErr)
			}
		})
	}
}

func TestPaymentSimulatorCharge(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		rate    float64
		roll    float64 // the value the fake random source returns
		code    string
		wantErr error
	}{
		{"roll above rate succeeds", 0.15, 0.9, "12345", nil},
		{"roll equal to rate succeeds", 0.15, 0.15, "12345", nil},
		{"roll below rate declines", 0.15, 0.14, "12345", ErrPaymentDeclined},
		{"rate zero never declines", 0, 0, "12345", nil},
		{"rate one always declines", 1, 0.999, "12345", ErrPaymentDeclined},
		{"invalid code beats a good roll", 0.15, 0.9, "1234", ErrInvalidPaymentCode},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p, err := NewPaymentSimulator(tt.rate, func() float64 { return tt.roll })
			if err != nil {
				t.Fatalf("NewPaymentSimulator() error = %v", err)
			}
			if err := p.Charge(tt.code); !errors.Is(err, tt.wantErr) {
				t.Errorf("Charge(%q) = %v, want %v", tt.code, err, tt.wantErr)
			}
		})
	}
}
