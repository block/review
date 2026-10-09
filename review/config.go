// Package review runs a panel code review of a git revision range with Goose.
//
// Three read-only panelists review the change in parallel, each through a
// fixed lens. A coordinator deduplicates their candidates and tries to
// disprove each one; a judge then re-examines what the coordinator dropped
// and restores the findings that should have been kept.
//
// Goose has no read-only sandbox: run reviews in a disposable container or
// another sandbox that confines the checkout and the network.
package review

import (
	"context"
	"time"
)

// Pass roles. Panelist roles are the panelist ids in Panelists().
const (
	RoleCoordinator = "coordinator"
	RoleJudge       = "judge"
)

type Config struct {
	// RepoDir is the git checkout to review.
	RepoDir string
	// BaseSHA and HeadSHA bound the change. The review covers
	// merge-base(BaseSHA, HeadSHA)..HeadSHA.
	BaseSHA string
	HeadSHA string

	// Intent is the author's description of the change, such as a PR title and
	// body. It is passed to reviewers as untrusted evidence.
	Intent string
	// Discussion is existing review discussion, used only to avoid repeating
	// issues already raised.
	Discussion string
	// Context is any other untrusted context the caller wants reviewers to see.
	Context string

	Goose GooseConfig
	// RoleModels and RoleEfforts override the model and reasoning effort per
	// pass role: a panelist id, "coordinator", or "judge".
	RoleModels  map[string]string
	RoleEfforts map[string]string

	PanelistTimeout    time.Duration
	CoordinatorTimeout time.Duration
	JudgeTimeout       time.Duration
	// Budget, when set, caps the whole review. The coordinator and judge get
	// whatever time the earlier stages left, up to their own timeouts, and the
	// coordinator leaves JudgeReserve for the judge.
	Budget       time.Duration
	JudgeReserve time.Duration

	Gate Gate
}

// Gate drops findings below a reporting bar. Reviewers are asked to return
// every verified finding with honest priority and confidence; the gate decides
// what is reported.
type Gate struct {
	// MaxPriority is the lowest-urgency priority reported: 2 keeps P0-P2.
	MaxPriority int
	// MinConfidence is the lowest confidence_score reported.
	MinConfidence float64
}

// DefaultGate reports P0-P2 findings with confidence of at least 0.8.
var DefaultGate = Gate{MaxPriority: 2, MinConfidence: 0.8}

// NoGate reports every finding the review returned.
var NoGate = Gate{MaxPriority: 3, MinConfidence: 0}

// Runner executes one pass and returns its final message. The default runs
// Goose; embedders can supply their own to add accounting or routing.
type Runner interface {
	Run(ctx context.Context, pass Pass) (string, error)
}

// Pass is one reviewer process: a panelist, the coordinator, or the judge.
type Pass struct {
	Role string
	// Model and Effort override the runner's defaults for this pass.
	Model   string
	Effort  string
	Prompt  string
	RepoDir string
	Timeout time.Duration
}

// PassReport describes how a pass ended.
type PassReport struct {
	Role     string        `json:"role"`
	Model    string        `json:"model,omitempty"`
	Effort   string        `json:"effort,omitempty"`
	Status   string        `json:"status"`
	Duration time.Duration `json:"duration_ns"`
	Error    string        `json:"error,omitempty"`
}

type Options struct {
	// Runner overrides the default Goose runner.
	Runner Runner
	// OnPass is called after every pass finishes.
	OnPass func(PassReport)
}

func (c *Config) applyDefaults() {
	if c.PanelistTimeout == 0 {
		c.PanelistTimeout = 15 * time.Minute
	}
	if c.CoordinatorTimeout == 0 {
		c.CoordinatorTimeout = 10 * time.Minute
	}
	if c.JudgeTimeout == 0 {
		c.JudgeTimeout = 5 * time.Minute
	}
	if c.Gate == (Gate{}) {
		c.Gate = DefaultGate
	}
}
