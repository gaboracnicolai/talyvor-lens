package injection

import (
	"fmt"
	"regexp"
	"sync"
)

type Action string

const (
	ActionAllow Action = "allow"
	ActionWarn  Action = "warn"
	ActionBlock Action = "block"
)

type Policy struct {
	WarnThreshold  float64
	BlockThreshold float64
}

func DefaultPolicy() Policy {
	return Policy{WarnThreshold: 0.3, BlockThreshold: 0.7}
}

type DetectionResult struct {
	Detected  bool
	Patterns  []string
	RiskScore float64
	Action    Action
}

type compiledPattern struct {
	name string
	re   *regexp.Regexp
}

type Detector struct {
	policy         Policy
	patterns       []compiledPattern
	mu             sync.RWMutex
	customPatterns []compiledPattern
}

// builtinPatterns are the production-canon detection rules. Names are
// stable identifiers so logs and metrics stay grep-able as the regexes
// evolve. Patterns are case-insensitive via the (?i) flag inline; they
// stay specific enough that legitimate prose using "ignore" or "system"
// in normal context doesn't trip them.
var builtinPatternSources = []struct {
	name, expr string
}{
	// Instruction override attempts.
	{"ignore_previous_instructions", `(?i)ignore (all |the )?(previous|above|prior) instructions?`},
	{"disregard_previous_instructions", `(?i)disregard (all |the )?(previous|above|prior) instructions?`},
	{"forget_previous_instructions", `(?i)forget (all |the )?(previous|above|prior) instructions?`},
	{"override_instructions", `(?i)override (your |all )?(previous |system )?instructions?`},

	// Role / identity manipulation.
	{"role_you_are_now", `(?i)you are now`},
	{"role_act_as_different", `(?i)act as (a |an |the )?(different|new|another)`},
	{"role_pretend", `(?i)pretend (you are|to be)`},
	{"role_true_self", `(?i)your (true|real|actual) (self|identity|purpose|goal)`},
	{"jailbreak", `(?i)jailbreak`},
	{"dan_mode", `(?i)dan mode`},

	// System prompt extraction.
	{"reveal_system_prompt", `(?i)reveal (your|the) (system |initial |original )?prompt`},
	{"show_system_prompt", `(?i)show me (your|the) (system |initial |original )?prompt`},
	{"what_are_instructions", `(?i)what (is|are) (your|the) (system |initial )?instructions?`},
	{"repeat_system_prompt", `(?i)repeat (your|the) (system |initial |original )?prompt`},

	// Prompt-boundary manipulation. Backtick fence isn't escapable inside
	// a Go raw-string literal, so build that one with concatenation.
	{"boundary_code_system", `(?i)` + "```" + `\s*system`},
	{"boundary_xml_system", `(?i)<system>`},
	{"boundary_bracket_system", `(?i)\[system\]`},
	{"boundary_md_system", `(?i)###\s*system`},
}

// builtinPatterns is compiled once at package init so Detect spends no
// time on regex compilation in the hot path.
var builtinPatterns = func() []compiledPattern {
	out := make([]compiledPattern, len(builtinPatternSources))
	for i, p := range builtinPatternSources {
		out[i] = compiledPattern{name: p.name, re: regexp.MustCompile(p.expr)}
	}
	return out
}()

func New(policy Policy) *Detector {
	// Copy so callers can't mutate the package-level slice via AddPattern.
	pats := make([]compiledPattern, len(builtinPatterns))
	copy(pats, builtinPatterns)
	return &Detector{policy: policy, patterns: pats}
}

func (d *Detector) Detect(prompt string) DetectionResult {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var matched []string
	for _, p := range d.patterns {
		if p.re.MatchString(prompt) {
			matched = append(matched, p.name)
		}
	}
	for _, p := range d.customPatterns {
		if p.re.MatchString(prompt) {
			matched = append(matched, p.name)
		}
	}

	// Score: each unique pattern adds 0.25, capped at 1.0. 4 matches
	// saturate; clamps prevent over-large scores from many custom patterns.
	score := float64(len(matched)) * 0.25
	if score > 1.0 {
		score = 1.0
	}

	action := ActionAllow
	switch {
	case score >= d.policy.BlockThreshold:
		action = ActionBlock
	case score >= d.policy.WarnThreshold:
		action = ActionWarn
	}

	return DetectionResult{
		Detected:  len(matched) > 0,
		Patterns:  matched,
		RiskScore: score,
		Action:    action,
	}
}

// AddPattern compiles the supplied regex and appends it to the custom
// pattern set. Returns the compile error so callers can surface a
// useful 400 to admin clients.
func (d *Detector) AddPattern(pattern string) error {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return fmt.Errorf("injection: compile pattern: %w", err)
	}
	d.mu.Lock()
	name := fmt.Sprintf("custom_%d", len(d.customPatterns)+1)
	d.customPatterns = append(d.customPatterns, compiledPattern{name: name, re: re})
	d.mu.Unlock()
	return nil
}
