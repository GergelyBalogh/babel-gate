// Package smart implements the virtual "smart" model: each request is
// classified into a complexity tier, and the tier's configured targets are
// tried in order so cheap models handle easy work and strong models hard work.
package smart

import (
	"fmt"
	"strings"
)

// ModelName is the requested model that activates smart routing.
const ModelName = "smart"

// Tier is a request complexity class, ordered from cheapest to strongest.
type Tier int

const (
	TierSimple Tier = iota
	TierMedium
	TierComplex
	TierReasoning
)

// Tiers lists all tiers from cheapest to strongest.
var Tiers = []Tier{TierSimple, TierMedium, TierComplex, TierReasoning}

func (t Tier) String() string {
	switch t {
	case TierSimple:
		return "simple"
	case TierMedium:
		return "medium"
	case TierComplex:
		return "complex"
	case TierReasoning:
		return "reasoning"
	default:
		return fmt.Sprintf("tier(%d)", int(t))
	}
}

// ParseTier accepts a tier name case-insensitively.
func ParseTier(s string) (Tier, error) {
	for _, t := range Tiers {
		if strings.EqualFold(strings.TrimSpace(s), t.String()) {
			return t, nil
		}
	}
	return 0, fmt.Errorf("unknown smart tier %q (expected simple, medium, complex or reasoning)", s)
}

// IsSmartModel reports whether a requested model activates smart routing.
func IsSmartModel(model string) bool {
	return strings.EqualFold(strings.TrimSpace(model), ModelName)
}
