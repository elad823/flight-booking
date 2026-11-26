package booking

import (
	"errors"
	"fmt"
)

// Payment errors. ErrInvalidPaymentCode is the user's mistake and retrying
// cannot fix it. ErrPaymentDeclined is a simulated gateway failure and a
// retry may succeed.
var (
	ErrInvalidPaymentCode = errors.New("payment code must be exactly 5 digits")
	ErrPaymentDeclined    = errors.New("payment declined")
	ErrInvalidFailureRate = errors.New("failure rate must be between 0 and 1")
)

// ValidatePaymentCode reports whether code is exactly 5 ASCII digits.
func ValidatePaymentCode(code string) error {
	if len(code) != 5 {
		return ErrInvalidPaymentCode
	}
	for i := range len(code) {
		if code[i] < '0' || code[i] > '9' {
			return ErrInvalidPaymentCode
		}
	}
	return nil
}

// PaymentSimulator stands in for a payment gateway: each charge with a valid
// code fails with probability failureRate.
//
// It is safe for concurrent use if its random source is, because its own
// fields never change after construction.
type PaymentSimulator struct {
	failureRate float64
	random      func() float64
}

// NewPaymentSimulator returns a simulator that fails charges at the given
// rate. random must return values in [0, 1) and be safe for concurrent use;
// math/rand/v2's Float64 is both.
func NewPaymentSimulator(failureRate float64, random func() float64) (*PaymentSimulator, error) {
	if !(failureRate >= 0 && failureRate <= 1) {
		return nil, fmt.Errorf("%w: got %v", ErrInvalidFailureRate, failureRate)
	}
	return &PaymentSimulator{failureRate: failureRate, random: random}, nil
}

// Charge validates the code and then simulates the gateway call.
func (p *PaymentSimulator) Charge(code string) error {
	if err := ValidatePaymentCode(code); err != nil {
		return err
	}
	if p.random() < p.failureRate {
		return ErrPaymentDeclined
	}
	return nil
}
