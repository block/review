package review

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type Result struct {
	// Findings passed the gate; Ungated is everything the review returned:
	// the coordinator's kept findings plus the judge's restored ones.
	Findings           []Finding          `json:"findings"`
	Ungated            []Finding          `json:"ungated_findings"`
	Dropped            []DroppedCandidate `json:"dropped_candidates"`
	OverallCorrectness string             `json:"overall_correctness"`
	OverallExplanation string             `json:"overall_explanation"`
	ReviewProcess      string             `json:"review_process"`
	Base               string             `json:"base"`
	Head               string             `json:"head"`
	Passes             []PassReport       `json:"passes"`
	// Degraded is true when a panelist or the judge did not complete, so the
	// review ran without one of its independent passes.
	Degraded bool `json:"degraded"`
}

// Run reviews merge-base(BaseSHA, HeadSHA)..HeadSHA in cfg.RepoDir: three
// panelists in parallel, then the coordinator, then the judge.
func Run(ctx context.Context, cfg Config, opts Options) (*Result, error) {
	cfg.applyDefaults()
	started := time.Now()
	if cfg.RepoDir == "" || cfg.BaseSHA == "" {
		return nil, errors.New("review: RepoDir and BaseSHA are required")
	}
	head, err := gitOutput(ctx, cfg.RepoDir, "rev-parse", orDefault(cfg.HeadSHA, "HEAD"))
	if err != nil {
		return nil, fmt.Errorf("review: resolve head: %w", err)
	}
	cfg.HeadSHA = head
	base, err := gitOutput(ctx, cfg.RepoDir, "merge-base", cfg.BaseSHA, head)
	if err != nil {
		if base, err = gitOutput(ctx, cfg.RepoDir, "rev-parse", cfg.BaseSHA); err != nil {
			return nil, fmt.Errorf("review: resolve base: %w", err)
		}
	}

	runner := opts.Runner
	if runner == nil {
		runner = GooseRunner{Config: cfg.Goose}
	}
	res := &Result{Base: base, Head: head, Dropped: []DroppedCandidate{}}
	var mu sync.Mutex
	run := func(role, prompt string, timeout time.Duration) (string, error) {
		start := time.Now()
		pass := Pass{Role: role, Model: cfg.RoleModels[role], Effort: cfg.RoleEfforts[role], Prompt: prompt, RepoDir: cfg.RepoDir, Timeout: timeout}
		out, err := runner.Run(ctx, pass)
		report := PassReport{Role: role, Model: pass.Model, Effort: pass.Effort, Status: "ok", Duration: time.Since(start)}
		if err != nil {
			report.Status, report.Error = "failed", err.Error()
		}
		mu.Lock()
		res.Passes = append(res.Passes, report)
		mu.Unlock()
		if opts.OnPass != nil {
			opts.OnPass(report)
		}
		return out, err
	}
	// remaining caps a stage at its own timeout, the time left in the budget,
	// and the time it must leave for later stages.
	remaining := func(timeout, reserve time.Duration) time.Duration {
		if cfg.Budget > 0 {
			if left := cfg.Budget - time.Since(started) - reserve; left < timeout {
				return left
			}
		}
		return timeout
	}

	results := runPanelists(cfg, base, run)
	for _, r := range results {
		if r.Status != "completed" {
			res.Degraded = true
		}
	}

	prompt, err := coordinatorPrompt(cfg, base, results)
	if err != nil {
		return nil, err
	}
	timeout := remaining(cfg.CoordinatorTimeout, cfg.JudgeReserve)
	if timeout < time.Minute {
		return res, fmt.Errorf("review: only %s of the %s budget is left for the coordinator", timeout.Round(time.Second), cfg.Budget)
	}
	out, err := run(RoleCoordinator, prompt, timeout)
	if err != nil {
		return res, err
	}
	rev, kept, err := parseReview(out, cfg.RepoDir)
	if err != nil {
		return res, fmt.Errorf("review: coordinator output: %w: %q", err, tail(out, 500))
	}
	res.OverallCorrectness, res.OverallExplanation, res.ReviewProcess = rev.OverallCorrectness, rev.OverallExplanation, rev.ReviewProcess
	if rev.DroppedCandidates != nil {
		res.Dropped = rev.DroppedCandidates
	}
	findings := kept

	// The judge is a recovery pass: when it cannot run or fails, the
	// coordinator's findings still stand and the review is marked degraded.
	if len(res.Dropped) > 0 {
		if timeout := remaining(cfg.JudgeTimeout, 0); timeout < time.Minute {
			res.Degraded = true
		} else if prompt, err := judgePrompt(cfg, base, kept, res.Dropped); err != nil {
			return res, err
		} else if out, err := run(RoleJudge, prompt, timeout); err != nil {
			res.Degraded = true
		} else if restored, err := parseJudge(out, cfg.RepoDir); err != nil {
			res.Degraded = true
		} else {
			findings = append(findings, restored...)
		}
	}

	res.Ungated = dedupe(findings)
	res.Findings = []Finding{}
	for _, f := range res.Ungated {
		if cfg.Gate.keep(f) {
			res.Findings = append(res.Findings, f)
		}
	}
	return res, nil
}

func runPanelists(cfg Config, base string, run func(role, prompt string, timeout time.Duration) (string, error)) []PanelistResult {
	results := make([]PanelistResult, len(panelistRoles))
	var wg sync.WaitGroup
	for i, role := range panelistRoles {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result := PanelistResult{Panelist: role.Name, Status: "completed"}
			out, err := run(role.ID, panelistPrompt(role, cfg, base), cfg.PanelistTimeout)
			if err == nil {
				result.Envelope, err = parsePanelistEnvelope(out, role)
			}
			if err != nil {
				result.Status, result.Envelope, result.Error = "failed", nil, err.Error()
			}
			results[i] = result
		}()
	}
	wg.Wait()
	return results
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).Output()
	return strings.TrimSpace(string(out)), err
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}
