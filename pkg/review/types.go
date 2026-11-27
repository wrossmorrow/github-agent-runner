// Package review implements headless PR review orchestration on top of `pi --mode rpc`.
package review

import "strings"

// Request defines the inputs passed from the GitHub Action step to agentrunnerd.
type Request struct {
	Workspace      string `json:"workspace"`
	EventPath      string `json:"eventPath,omitempty"`
	EventName      string `json:"eventName,omitempty"`
	Instructions   string `json:"instructions"`
	FailOn         string `json:"failOn,omitempty"` // none | warning | error
	Output         string `json:"output,omitempty"` // pr-review | suggest | comment | issue-comment | release-draft | branch-commit | revert-pr | stdout
	Repo           string `json:"repo,omitempty"`
	RunID          string `json:"runId,omitempty"`
	PRNumber       int    `json:"prNumber,omitempty"`
	IssueNumber    int    `json:"issueNumber,omitempty"`
	WorkflowRunID  int64  `json:"workflowRunId,omitempty"`
	BaseRef        string `json:"baseRef,omitempty"`
	HeadRef        string `json:"headRef,omitempty"`
	HeadSHA        string `json:"headSha,omitempty"`
	TargetBranch   string `json:"targetBranch,omitempty"`
	OpenPR         bool   `json:"openPr,omitempty"`
	Tag            string `json:"tag,omitempty"`
	Environment    string `json:"environment,omitempty"`
	EnvironmentURL string `json:"environmentUrl,omitempty"`
}

// Comment represents an inline review finding returned by pi.
type Comment struct {
	Path       string `json:"path"`
	StartLine  int    `json:"startLine,omitempty"`
	Line       int    `json:"line"`
	Severity   string `json:"severity"` // info | warning | error
	Body       string `json:"body"`
	Suggestion string `json:"suggestion,omitempty"`
}

// Result is the structured JSON payload emitted by pi at the end of a run.
type Result struct {
	Verdict  string    `json:"verdict"` // APPROVE | COMMENT | REQUEST_CHANGES
	Title    string    `json:"title,omitempty"`
	Summary  string    `json:"summary"`
	Body     string    `json:"body,omitempty"`
	Comments []Comment `json:"comments"`
}

// Outcome summarizes a completed review step for the caller.
type Outcome struct {
	Result   *Result `json:"result"`
	ExitCode int     `json:"exitCode"`
	Turns    int     `json:"turns"`
}

// ShouldFail reports whether any comment meets the configured fail-on threshold.
func (r *Result) ShouldFail(failOn string) bool {
	if r == nil {
		return false
	}
	threshold := severityRank(failOn)
	if threshold == 0 {
		return false
	}
	for _, c := range r.Comments {
		if severityRank(c.Severity) >= threshold {
			return true
		}
	}
	return false
}

func severityRank(s string) int {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "none", "":
		return 0
	case "info":
		return 1
	case "warning", "warn":
		return 2
	case "error":
		return 3
	default:
		return 3
	}
}
