// Package ledger implements a double-entry ledger: every transaction is a set of
// postings whose amounts sum to zero per currency. Balances are derived, never edited.
package ledger

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Amount is an integer number of minor units (cents). Floats never represent money.
type Amount int64

// ParseAmount parses "1234.50" into 123450. At most two decimal places are accepted.
func ParseAmount(s string) (Amount, error) {
	s = strings.TrimSpace(s)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" || len(frac) > 2 {
		return 0, fmt.Errorf("invalid amount %q", s)
	}
	for len(frac) < 2 {
		frac += "0"
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid amount %q: %w", s, err)
	}
	f, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid amount %q: %w", s, err)
	}
	if w > (math.MaxInt64-f)/100 {
		return 0, errors.New("amount overflows")
	}
	a := Amount(w*100 + f)
	if neg {
		a = -a
	}
	return a, nil
}

func (a Amount) String() string {
	sign := ""
	v := int64(a)
	if v < 0 {
		sign, v = "-", -v
	}
	return fmt.Sprintf("%s%d.%02d", sign, v/100, v%100)
}

// add returns a+b or an error on overflow. Ledgers must never wrap around.
func add(a, b Amount) (Amount, error) {
	if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
		return 0, errors.New("amount overflow")
	}
	return a + b, nil
}
