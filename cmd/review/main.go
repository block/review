// Command review reviews a git revision range with a Goose review panel.
//
//	review review --base main            review HEAD against main
//	review reviewbench                   run under the ReviewBench agent contract
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/block/review/review"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var err error
	switch os.Args[1] {
	case "review":
		err = reviewCmd(ctx, os.Args[2:])
	case "reviewbench":
		err = reviewBenchCmd(ctx)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "review:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: review review --base <rev> [flags] | review reviewbench")
	os.Exit(2)
}

func reviewCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("review", flag.ExitOnError)
	repo := fs.String("repo", ".", "git checkout to review")
	base := fs.String("base", "", "base revision (required); the review covers merge-base(base, head)..head")
	head := fs.String("head", "HEAD", "head revision")
	provider := fs.String("provider", "", "Goose model provider (default: Goose's configured provider)")
	model := fs.String("model", "", "model for every pass (default: Goose's configured model)")
	effort := fs.String("effort", "", "reasoning effort for every pass: low, medium, or high")
	variants := fs.String("panel-variants", "", "run each lens once per variant, e.g. sol,opus")
	roleProviders := fs.String("role-providers", "", "per-pass providers, e.g. sol=openai,opus=anthropic")
	roleModels := fs.String("role-models", "", "per-pass models, e.g. behavior_state_data=a,coordinator=b,judge=c")
	roleEfforts := fs.String("role-efforts", "", "per-pass efforts, e.g. coordinator=medium")
	intent := fs.String("intent", "", "author's description of the change (untrusted evidence)")
	noGate := fs.Bool("all", false, "report every finding, including P3 and confidence below 0.8")
	asJSON := fs.Bool("json", false, "print the full result as JSON")
	fs.Parse(args)
	if *base == "" {
		return errors.New("--base is required")
	}
	cfg := review.Config{
		RepoDir: *repo, BaseSHA: *base, HeadSHA: *head, Intent: *intent,
		Goose:         review.GooseConfig{Provider: *provider, Model: *model, Effort: *effort},
		PanelVariants: parseList(*variants),
		RoleProviders: parsePairs(*roleProviders),
		RoleModels:    parsePairs(*roleModels),
		RoleEfforts:   parsePairs(*roleEfforts),
	}
	if *noGate {
		cfg.Gate = review.NoGate
	}
	res, err := review.Run(ctx, cfg, review.Options{OnPass: logPass})
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}
	for _, f := range res.Findings {
		fmt.Printf("[P%d] %s\n  %s:%d-%d\n  %s\n\n", f.Priority, f.Title, f.File, f.StartLine, f.EndLine, strings.ReplaceAll(f.Body, "\n", "\n  "))
	}
	fmt.Printf("%s: %s\n", res.OverallCorrectness, res.OverallExplanation)
	return nil
}

func logPass(p review.PassReport) {
	msg := fmt.Sprintf("review: %s %s in %s", p.Role, p.Status, p.Duration.Round(time.Second))
	if p.Error != "" {
		msg += ": " + firstLine(p.Error)
	}
	fmt.Fprintln(os.Stderr, msg)
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// reviewBenchCmd implements https://github.com/review-bench/ReviewBench/blob/main/AGENT_CONTRACT.md.
// The container is the sandbox, and the whole review must fit the 15-minute
// limit per pull request: 7 minutes for the panel, then the coordinator and
// judge share the rest of a 14-minute budget. Each pass holds back its last
// two minutes to report what it has if it runs long.
func reviewBenchCmd(ctx context.Context) error {
	env := func(k string) string { return strings.TrimSpace(os.Getenv(k)) }
	for _, k := range []string{"RB_NWO", "RB_PR_NUMBER", "RB_BASE", "RB_HEAD", "RB_OUT"} {
		if env(k) == "" {
			return fmt.Errorf("missing %s", k)
		}
	}
	agent := orDefault(env("RB_AGENT"), "review")
	repo := orDefault(env("RB_REPO"), "/work/repo")
	intent := ""
	if raw, err := os.ReadFile(orDefault(env("RB_PR_JSON"), "/work/pr/pr.json")); err == nil {
		var pr struct{ Title, Body string }
		if json.Unmarshal(raw, &pr) == nil {
			intent = strings.TrimSpace(pr.Title + "\n\n" + pr.Body)
		}
	}
	provider := orDefault(env("RB_CONFIG_PROVIDER"), "openai")
	goose := review.GooseConfig{
		Bin:      env("REVIEW_GOOSE_BIN"),
		Provider: provider,
		Model:    orDefault(env("RB_CONFIG_MODEL"), "gpt-5.6-sol"),
		Effort:   orDefault(env("RB_CONFIG_EFFORT"), "high"),
		StateDir: env("REVIEW_STATE_DIR"),
	}
	roleProviders := parsePairs(env("RB_CONFIG_ROLE_PROVIDERS"))
	usesOpenAI := provider == "openai"
	for _, p := range roleProviders {
		usesOpenAI = usesOpenAI || p == "openai"
	}
	if usesOpenAI {
		goose.Env = os.Environ()
		if provider == "openai" {
			baseURL := orDefault(env("RB_MODEL_BASE_URL"), "https://api.openai.com/v1")
			goose.Env = append(goose.Env, "OPENAI_HOST="+strings.TrimSuffix(strings.TrimSuffix(baseURL, "/"), "/v1"))
		}
		// Goose sends models it does not recognize, such as gpt-6.1-sol, to
		// Chat Completions, where those models cannot call tools.
		if env("OPENAI_BASE_PATH") == "" {
			goose.Env = append(goose.Env, "OPENAI_BASE_PATH=v1/responses")
		}
	}
	cfg := review.Config{
		RepoDir: repo, BaseSHA: env("RB_BASE"), HeadSHA: env("RB_HEAD"), Intent: intent,
		Goose:              goose,
		PanelVariants:      parseList(env("RB_CONFIG_PANEL_VARIANTS")),
		RoleProviders:      roleProviders,
		RoleModels:         parsePairs(env("RB_CONFIG_ROLE_MODELS")),
		RoleEfforts:        parsePairs(env("RB_CONFIG_ROLE_EFFORTS")),
		PanelistTimeout:    7 * time.Minute,
		CoordinatorTimeout: 6 * time.Minute,
		JudgeTimeout:       4 * time.Minute,
		Budget:             14 * time.Minute,
		JudgeReserve:       90 * time.Second,
	}
	for name, d := range map[string]*time.Duration{
		"REVIEW_PANELIST_TIMEOUT": &cfg.PanelistTimeout, "REVIEW_COORDINATOR_TIMEOUT": &cfg.CoordinatorTimeout,
		"REVIEW_JUDGE_TIMEOUT": &cfg.JudgeTimeout, "REVIEW_BUDGET": &cfg.Budget, "REVIEW_JUDGE_RESERVE": &cfg.JudgeReserve,
	} {
		if v := env(name); v != "" {
			parsed, err := time.ParseDuration(v)
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			*d = parsed
		}
	}
	if env("RB_CONFIG_GATE") == "off" {
		cfg.Gate = review.NoGate
	}
	fmt.Fprintf(os.Stderr, "review: provider=%s model=%s effort=%s gate=%s panel_variants=%s role_providers=%s role_models=%s role_efforts=%s\n",
		provider, goose.Model, goose.Effort, orDefault(env("RB_CONFIG_GATE"), "on"), env("RB_CONFIG_PANEL_VARIANTS"),
		env("RB_CONFIG_ROLE_PROVIDERS"), env("RB_CONFIG_ROLE_MODELS"), env("RB_CONFIG_ROLE_EFFORTS"))

	res, err := review.Run(ctx, cfg, review.Options{OnPass: logPass})
	if path := env("REVIEW_RESULT"); path != "" && res != nil {
		if payload, err := json.MarshalIndent(res, "", "  "); err == nil {
			os.WriteFile(path, payload, 0o644)
		}
	}
	if err != nil {
		return err
	}
	type finding struct {
		File      string `json:"file"`
		StartLine int    `json:"start_line"`
		EndLine   int    `json:"end_line"`
		Message   string `json:"message"`
		Producer  string `json:"producer"`
	}
	findings := []finding{}
	for _, f := range res.Findings {
		findings = append(findings, finding{f.File, f.StartLine, f.EndLine, fmt.Sprintf("[P%d] %s\n\n%s", f.Priority, f.Title, f.Body), agent})
	}
	var prNumber int
	fmt.Sscan(env("RB_PR_NUMBER"), &prNumber)
	out := map[string]any{
		"pr":       map[string]any{"repo": "https://github.com/" + env("RB_NWO"), "pr_number": prNumber, "base": env("RB_BASE"), "head": env("RB_HEAD")},
		"agent":    agent,
		"findings": findings,
	}
	payload, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	restored := 0
	for _, f := range res.Ungated {
		if f.Stage == review.RoleJudge {
			restored++
		}
	}
	fmt.Fprintf(os.Stderr, "review: %d finding(s) reported, %d returned (%d restored by the judge), %d dropped by the coordinator\n",
		len(res.Findings), len(res.Ungated), restored, len(res.Dropped))
	return os.WriteFile(env("RB_OUT"), payload, 0o644)
}

func parsePairs(spec string) map[string]string {
	pairs := map[string]string{}
	for _, pair := range strings.Split(spec, ",") {
		if k, v, ok := strings.Cut(strings.TrimSpace(pair), "="); ok && k != "" && v != "" {
			pairs[k] = v
		}
	}
	return pairs
}

func parseList(spec string) []string {
	var out []string
	for _, v := range strings.Split(spec, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}
