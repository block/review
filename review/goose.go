package review

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// GooseConfig runs passes with Goose's own agent loop and its developer
// extension (shell and file tools).
type GooseConfig struct {
	// Bin is the goose executable. Defaults to "goose" on PATH.
	Bin string
	// Provider and Model select the model, for example "openai" and
	// "gpt-5.6-sol". Provider settings such as OPENAI_HOST and OPENAI_API_KEY
	// come from Env.
	Provider string
	Model    string
	// Effort sets GOOSE_THINKING_EFFORT; Pass.Effort overrides it.
	Effort string
	// MaxTurns bounds the agent loop. Zero uses 300.
	MaxTurns int
	// MaxOutputTokens sets GOOSE_MAX_TOKENS. Zero uses 64000; Goose's own
	// default for Responses-API models is 4096, which truncates a review.
	MaxOutputTokens int
	// StateDir, when set, keeps each pass's Goose config, logs, and requests
	// under StateDir/<role>. Otherwise they go to a temporary directory.
	StateDir string
	// SharedHome runs every pass in the caller's Goose home instead of a fresh
	// one, for hosts that stage their own Goose config and read usage from
	// Goose's session store. StateDir is ignored.
	SharedHome bool
	// WrapUp is how much of a pass's timeout is held back for one more turn
	// when the pass runs out of time or ends without a JSON answer. Zero uses
	// two minutes, about what a long JSON answer takes to write. Passes with
	// a timeout under twice WrapUp get no wrap-up.
	WrapUp time.Duration
	// Env is the process environment. Nil inherits the caller's environment.
	Env []string
}

// GooseRunner runs each pass as a `goose run` process. By default each pass
// gets a fresh Goose home, so no user config, extensions, hints, or sessions
// leak into the review.
type GooseRunner struct {
	Config GooseConfig
}

func (r GooseRunner) args(provider, model, session string, resume bool) []string {
	c := r.Config
	if provider != "" {
		c.Provider = provider
	}
	if model != "" {
		c.Model = model
	}
	maxTurns := c.MaxTurns
	if maxTurns == 0 {
		maxTurns = 300
	}
	// Every pass records a named session so it can be resumed for a wrap-up
	// turn; without SharedHome the session lives in the pass's private home.
	args := []string{"run", "--name", session}
	if resume {
		args = append(args, "--resume")
	}
	args = append(args, "--no-profile", "--with-builtin", "developer",
		"--output-format", "json", "--max-turns", fmt.Sprint(maxTurns), "--instructions", "-")
	if c.Provider != "" {
		args = append(args, "--provider", c.Provider)
	}
	if c.Model != "" {
		args = append(args, "--model", c.Model)
	}
	return args
}

func (r GooseRunner) env(root, effort string) []string {
	env := r.Config.Env
	if env == nil {
		env = os.Environ()
	}
	overrides := map[string]string{
		"GOOSE_MODE":         "auto",
		"CONTEXT_FILE_NAMES": "[]",
	}
	if root != "" {
		overrides["GOOSE_PATH_ROOT"] = root
	}
	if effort == "" {
		effort = r.Config.Effort
	}
	if effort != "" {
		overrides["GOOSE_THINKING_EFFORT"] = effort
	}
	maxTokens := r.Config.MaxOutputTokens
	if maxTokens == 0 {
		maxTokens = 64000
	}
	overrides["GOOSE_MAX_TOKENS"] = fmt.Sprint(maxTokens)
	out := make([]string, 0, len(env)+len(overrides))
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); overrides[k] == "" {
			out = append(out, kv)
		}
	}
	for k, v := range overrides {
		out = append(out, k+"="+v)
	}
	return out
}

const (
	wrapUpTimeUp = "Time is up. Do not call any more tools. Reply now with only the final JSON object your instructions describe, covering what you have verified so far."
	wrapUpNoJSON = "Your last reply did not contain the required JSON object. Do not call any more tools. Reply now with only that JSON object."
	// A coordinator reconciling a large panel can run out of time again just
	// writing its drop list, which is the longest part of its reply.
	wrapUpCoordinator = " Return an empty dropped_candidates array to keep the reply short, and say so in review_process."
	defaultWrapUp     = 2 * time.Minute
)

var errPassDeadline = errors.New("pass deadline")

// Run runs the pass until its timeout less WrapUp. If the pass is still
// working then, or finishes without a JSON object, Run resumes its session
// for one more turn that asks for the answer, so a slow pass reports what it
// has instead of nothing.
func (r GooseRunner) Run(ctx context.Context, pass Pass) (string, error) {
	root := ""
	if r.Config.SharedHome {
		// Keep the caller's Goose home.
	} else if r.Config.StateDir != "" {
		root = filepath.Join(r.Config.StateDir, pass.Role)
		if err := os.MkdirAll(root, 0o755); err != nil {
			return "", err
		}
	} else {
		dir, err := os.MkdirTemp("", "review-goose-")
		if err != nil {
			return "", err
		}
		defer os.RemoveAll(dir)
		root = dir
	}
	if pass.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, pass.Timeout)
		defer cancel()
	}
	wrapUp := r.Config.WrapUp
	if wrapUp == 0 {
		wrapUp = defaultWrapUp
	}
	work := ctx
	if pass.Timeout > 2*wrapUp {
		var cancel context.CancelFunc
		work, cancel = context.WithTimeout(ctx, pass.Timeout-wrapUp)
		defer cancel()
	}
	session := fmt.Sprintf("review-%s-%d", pass.Role, time.Now().UnixNano())

	out, err := r.exec(work, root, session, false, pass.Prompt, pass)
	var nudge string
	switch {
	case errors.Is(err, errPassDeadline) && work != ctx && ctx.Err() == nil:
		nudge = wrapUpTimeUp
		if pass.Role == RoleCoordinator {
			nudge += wrapUpCoordinator
		}
	case errors.Is(err, errPassDeadline):
		return "", fmt.Errorf("%s pass exceeded %s", pass.Role, pass.Timeout)
	case err != nil:
		return "", err
	default:
		if _, jsonErr := extractJSONObject(out); jsonErr == nil {
			return out, nil
		}
		nudge = wrapUpNoJSON
	}

	wrapCtx, cancel := context.WithTimeout(ctx, wrapUp)
	defer cancel()
	wrapped, wrapErr := r.exec(wrapCtx, root, session, true, nudge, pass)
	if nudge == wrapUpNoJSON {
		if wrapErr != nil {
			return out, nil
		}
		return wrapped, nil
	}
	if wrapErr != nil {
		return "", fmt.Errorf("%s pass exceeded %s and its wrap-up failed: %w", pass.Role, pass.Timeout-wrapUp, wrapErr)
	}
	return wrapped, nil
}

func (r GooseRunner) exec(ctx context.Context, root, session string, resume bool, input string, pass Pass) (string, error) {
	bin := r.Config.Bin
	if bin == "" {
		bin = "goose"
	}
	cmd := exec.CommandContext(ctx, bin, r.args(pass.Provider, pass.Model, session, resume)...)
	cmd.Dir = pass.RepoDir
	cmd.Env = r.env(root, pass.Effort)
	cmd.Stdin = strings.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second

	runErr := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "", errPassDeadline
	}
	if runErr != nil {
		return "", fmt.Errorf("%s pass: goose: %w: %s", pass.Role, runErr, tail(stderr.String()+stdout.String(), 4000))
	}
	return gooseFinalMessage(stdout.String())
}

// gooseFinalMessage returns the last assistant text of a completed
// `goose run --output-format json` transcript. Goose prints a banner before
// the JSON and exits 0 even when the run stopped early, so both are checked.
func gooseFinalMessage(raw string) (string, error) {
	start := strings.Index(raw, "{\n")
	if start < 0 {
		return "", errors.New("goose printed no JSON transcript")
	}
	var transcript struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
		Metadata struct {
			Status string `json:"status"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(raw[start:]), &transcript); err != nil {
		return "", fmt.Errorf("decode goose transcript: %w", err)
	}
	if transcript.Metadata.Status != "completed" {
		return "", fmt.Errorf("goose run ended with status %q", transcript.Metadata.Status)
	}
	if n := len(transcript.Messages); n > 0 && transcript.Messages[n-1].Role == "assistant" {
		var text []string
		for _, c := range transcript.Messages[n-1].Content {
			if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
				text = append(text, c.Text)
			}
		}
		// A final message can repeat its answer across text parts separated by
		// reasoning; prefer the last part that is a complete JSON object.
		for i := len(text) - 1; i >= 0; i-- {
			if _, err := extractJSONObject(text[i]); err == nil {
				return text[i], nil
			}
		}
		if len(text) > 0 {
			final := strings.Join(text, "\n")
			// Goose reports provider failures as the final assistant text of a
			// "completed" run.
			if strings.HasPrefix(strings.TrimSpace(final), "Ran into this error:") {
				return "", errors.New(tail(final, 1000))
			}
			return final, nil
		}
	}
	return "", errors.New("goose transcript has no final assistant text")
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}
