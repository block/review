package review

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// Finding is one reported issue. File is repository-relative.
type Finding struct {
	Title           string   `json:"title"`
	Body            string   `json:"body"`
	File            string   `json:"file"`
	StartLine       int      `json:"start_line"`
	EndLine         int      `json:"end_line"`
	Priority        int      `json:"priority"`
	ConfidenceScore float64  `json:"confidence_score"`
	Personas        []string `json:"personas,omitempty"`
	// Stage is "coordinator" for findings the coordinator kept and "judge"
	// for findings the judge restored.
	Stage string `json:"stage"`
}

// DroppedCandidate is a panel candidate the coordinator did not keep.
type DroppedCandidate struct {
	Title     string `json:"title"`
	Panelist  string `json:"panelist"`
	File      string `json:"file"`
	LineRange struct {
		Start int `json:"start"`
		End   int `json:"end"`
	} `json:"line_range"`
	Priority        *int     `json:"priority,omitempty"`
	ConfidenceScore *float64 `json:"confidence_score,omitempty"`
	Claim           string   `json:"claim"`
	DropReason      string   `json:"drop_reason"`
	Evidence        string   `json:"evidence"`
}

type rawFinding struct {
	Title           string   `json:"title"`
	Body            string   `json:"body"`
	ConfidenceScore *float64 `json:"confidence_score"`
	Priority        *int     `json:"priority"`
	Personas        []string `json:"personas"`
	CodeLocation    struct {
		AbsoluteFilePath string `json:"absolute_file_path"`
		LineRange        struct {
			Start int `json:"start"`
			End   int `json:"end"`
		} `json:"line_range"`
	} `json:"code_location"`
}

type rawReview struct {
	Findings               []rawFinding       `json:"findings"`
	DroppedCandidates      []DroppedCandidate `json:"dropped_candidates"`
	OverallCorrectness     string             `json:"overall_correctness"`
	OverallExplanation     string             `json:"overall_explanation"`
	OverallConfidenceScore *float64           `json:"overall_confidence_score"`
	ReviewProcess          string             `json:"review_process"`
}

var priorityPrefix = regexp.MustCompile(`^\s*\[P([0-3])\]\s*`)

// parseReview decodes the coordinator's final message.
func parseReview(raw, repoDir string) (rawReview, []Finding, error) {
	var rev rawReview
	body, err := extractJSONObject(raw)
	if err != nil {
		return rev, nil, err
	}
	if err := json.Unmarshal(body, &rev); err != nil {
		return rev, nil, fmt.Errorf("decode review: %w", err)
	}
	if rev.Findings == nil && rev.OverallCorrectness == "" {
		return rev, nil, errors.New("review has neither findings nor an overall verdict")
	}
	return rev, convertFindings(rev.Findings, repoDir, RoleCoordinator), nil
}

// parseJudge decodes the judge's bare JSON array of restored findings.
func parseJudge(raw, repoDir string) ([]Finding, error) {
	start, end := strings.Index(raw, "["), strings.LastIndex(raw, "]")
	if start < 0 || end < start {
		return nil, errors.New("no JSON array in judge output")
	}
	var restored []rawFinding
	if err := json.Unmarshal([]byte(raw[start:end+1]), &restored); err != nil {
		return nil, fmt.Errorf("decode judge output: %w", err)
	}
	return convertFindings(restored, repoDir, RoleJudge), nil
}

// convertFindings drops findings without a usable location.
func convertFindings(raw []rawFinding, repoDir, stage string) []Finding {
	var findings []Finding
	for _, f := range raw {
		file := relativePath(f.CodeLocation.AbsoluteFilePath, repoDir)
		start, end := f.CodeLocation.LineRange.Start, f.CodeLocation.LineRange.End
		if end == 0 {
			end = start
		}
		if start > end {
			start, end = end, start
		}
		if file == "" || start < 1 {
			continue
		}
		priority := 2
		if m := priorityPrefix.FindStringSubmatch(f.Title); m != nil {
			priority = int(m[1][0] - '0')
		}
		if f.Priority != nil && *f.Priority >= 0 && *f.Priority <= 3 {
			priority = *f.Priority
		}
		confidence := 1.0
		if f.ConfidenceScore != nil {
			confidence = *f.ConfidenceScore
		}
		findings = append(findings, Finding{
			Title:           strings.TrimSpace(priorityPrefix.ReplaceAllString(f.Title, "")),
			Body:            strings.TrimSpace(f.Body),
			File:            file,
			StartLine:       start,
			EndLine:         end,
			Priority:        priority,
			ConfidenceScore: confidence,
			Personas:        f.Personas,
			Stage:           stage,
		})
	}
	return findings
}

func relativePath(path, repoDir string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if filepath.IsAbs(path) {
		root, err := filepath.EvalSymlinks(repoDir)
		if err != nil {
			root = repoDir
		}
		for _, r := range []string{root, repoDir} {
			if rel, err := filepath.Rel(r, path); err == nil && !strings.HasPrefix(rel, "..") {
				return filepath.ToSlash(rel)
			}
		}
		return ""
	}
	path = filepath.ToSlash(filepath.Clean(path))
	if strings.HasPrefix(path, "../") || path == ".." {
		return ""
	}
	return path
}

// dedupe merges findings with the same normalized title at the same location,
// keeping the strongest priority. Order is preserved.
func dedupe(findings []Finding) []Finding {
	index := map[string]int{}
	var out []Finding
	for _, f := range findings {
		key := fmt.Sprintf("%s:%d:%s", f.File, f.StartLine, strings.ToLower(strings.Join(strings.Fields(f.Title), " ")))
		if i, ok := index[key]; ok {
			if f.Priority < out[i].Priority {
				out[i].Priority = f.Priority
			}
			continue
		}
		index[key] = len(out)
		out = append(out, f)
	}
	return out
}

func (g Gate) keep(f Finding) bool {
	return f.Priority <= g.MaxPriority && f.ConfidenceScore >= g.MinConfidence
}

// extractJSONObject returns the outermost JSON object in a model message,
// tolerating surrounding prose or code fences.
func extractJSONObject(raw string) ([]byte, error) {
	start, end := strings.Index(raw, "{"), strings.LastIndex(raw, "}")
	if start < 0 || end < start {
		return nil, errors.New("no JSON object in reviewer output")
	}
	body := []byte(raw[start : end+1])
	if !json.Valid(body) {
		return nil, errors.New("reviewer output is not valid JSON")
	}
	return body, nil
}
