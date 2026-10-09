package boards

import (
	"errors"
	"slices"
	"strconv"
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

// tshirtPoints turns sizes into numbers so velocity can add them up.
var tshirtPoints = map[string]float64{"XS": 1, "S": 2, "M": 3, "L": 5, "XL": 8}

// Points is an estimate as a number, whatever scale it came from; 0 when not estimated.
func Points(estimate *string) float64 {
	if estimate == nil {
		return 0
	}
	if p, ok := tshirtPoints[*estimate]; ok {
		return p
	}
	p, _ := strconv.ParseFloat(*estimate, 64)
	return p
}
