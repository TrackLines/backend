package boards

import (
	"errors"
	"slices"
)

// Scales are the ways a board can estimate tickets, smallest first. "none" turns estimates off.
// The frontend keeps the same list in lib/estimates.ts.
var Scales = map[string][]string{
	"none":      nil,
	"fibonacci": {"1", "2", "3", "5", "8", "13", "21"},
	"tshirt":    {"XS", "S", "M", "L", "XL"},
	"powers":    {"1", "2", "4", "8", "16"},
	"linear":    {"1", "2", "3", "4", "5"},
}

var ErrBadEstimate = errors.New("estimate isn't on this board's scale")

// ValidEstimate: "" (no estimate) always is; anything else must be on the scale.
func ValidEstimate(scale, estimate string) bool {
	return estimate == "" || slices.Contains(Scales[scale], estimate)
}
