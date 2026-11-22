package rollover

import (
	"fmt"
	"strings"
)

// State represents the current lifecycle state of epoch rollover.
type State string

const (
	StateNormal        State = "normal"
	StateArmed         State = "armed"
	StateCloseout      State = "closeout"
	StateBootstrapping State = "bootstrapping"
)

// DocsPolicy controls whether closeout is permitted/instructed to update project READMEs/docs.
type DocsPolicy string

const (
	DocsPolicyNone      DocsPolicy = "none"
	DocsPolicyMilestone DocsPolicy = "milestone"
	DocsPolicyAlways    DocsPolicy = "always"
)

// ParseDocsPolicy parses a string into a valid DocsPolicy.
func ParseDocsPolicy(s string) (DocsPolicy, error) {
	trimmed := strings.ToLower(strings.TrimSpace(s))
	switch trimmed {
	case "", "none":
		return DocsPolicyNone, nil
	case "milestone":
		return DocsPolicyMilestone, nil
	case "always":
		return DocsPolicyAlways, nil
	default:
		return "", fmt.Errorf("invalid docs policy %q: must be 'none', 'milestone', or 'always'", s)
	}
}

// Config controls automatic and manual epoch rollover behavior.
type Config struct {
	AutoRollover    bool       // Whether to automatically trigger rollover when context reaches threshold
	SoftPct         float64    // Context utilization threshold (0.0-1.0) to arm rollover (default 0.45)
	HardPct         float64    // Context utilization threshold (0.0-1.0) to force rollover (default 0.65)
	MaxArmedRounds  int        // Maximum tool rounds to stay Armed before forcing closeout (default 8)
	CloseoutMaxSteps int       // Step budget for closeout (default 4; callers can allow 8 when doc updates active)
	MinEpochTurns   int        // Minimum user turns in an epoch before auto-rollover can re-arm (default 2)
	DocsPolicy      DocsPolicy // Documentation update policy on rollover (default DocsPolicyNone)
	MaxEpochs       int        // Maximum epochs allowed for auto-rollover (0 = unlimited)
	MaxCostUSD      float64    // Maximum cumulative session cost before pausing auto-rollover (0 = unlimited)
}

// DefaultConfig returns a Config with sensible production defaults.
func DefaultConfig() Config {
	return Config{
		AutoRollover:     false,
		SoftPct:          0.45,
		HardPct:          0.65,
		MaxArmedRounds:   8,
		CloseoutMaxSteps: 4,
		MinEpochTurns:    2,
		DocsPolicy:       DocsPolicyNone,
		MaxEpochs:        0,
		MaxCostUSD:       0,
	}
}
