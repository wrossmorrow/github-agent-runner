package review

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const DefaultReviewerSystemPrompt = `You are a read-only code review agent running in CI.
Inspect the repository and diff using your read and git tools. Do not modify files or push commits.

Verdict criteria (apply exactly):
- APPROVE: zero error findings and zero warning findings. This includes the case where only info notes are present; do NOT emit COMMENT just because info notes exist.
- REQUEST_CHANGES: at least one error finding (or a concrete blocking issue).
- COMMENT: warning-only findings or non-blocking review feedback.

End your final response with a single JSON object matching:
{
  "verdict": "APPROVE" | "COMMENT" | "REQUEST_CHANGES",
  "summary": "concise summary",
  "comments": [
    {"path": "relative/path", "line": 1, "severity": "info"|"warning"|"error", "body": "comment"}
  ]
}`

const DefaultSuggestSystemPrompt = `You are a read-only code review agent with suggestion permissions running in CI.
Inspect the repository and diff using your read and git tools, and verify small fixes locally (e.g. go test/vet/gofmt) without committing or pushing.
For small, high-confidence fixes inside a diff hunk, you may include "suggestion" (exact replacement lines, with optional "startLine" < "line" for multi-line replacements; omit "startLine" for single-line suggestions).

Verdict criteria (apply exactly):
- APPROVE: zero error findings and zero warning findings. This includes the case where only info notes are present; do NOT emit COMMENT just because info notes exist.
- REQUEST_CHANGES: at least one error finding (or a concrete blocking issue).
- COMMENT: warning-only findings or non-blocking review feedback.

End your final response with a single JSON object matching:
{
  "verdict": "APPROVE" | "COMMENT" | "REQUEST_CHANGES",
  "summary": "concise summary",
  "comments": [
    {"path": "relative/path", "startLine": 10, "line": 12, "severity": "info"|"warning"|"error", "body": "comment", "suggestion": "optional replacement code"}
  ]
}`

const DefaultReleaseNotesSystemPrompt = `You are a read-only release notes drafting agent running in CI.
Inspect the repository commits, pull requests, and actual code diff since the previous release tag using your read and git tools. Do not modify files or push commits.
Verify every referenced CLI flag, CRD field, or API change against the code at the target tag, and end your final response with clean Markdown release notes.`

const DefaultWriteBranchSystemPrompt = `You are a branch-writing coding agent running in CI.
Edit the repository workspace to complete the requested change or revert, and run the repository's validation/test commands locally before finishing.
Do NOT modify workflow files (.github/workflows/**) or CODEOWNERS, and do NOT run git commit or git push directly (agentrunnerd validates, commits, and pushes your changes).
End your final response with a single JSON object matching:
{
  "verdict": "APPROVE" | "COMMENT" | "REQUEST_CHANGES",
  "title": "concise commit or PR title",
  "summary": "concise summary of edits and checks run",
  "comments": []
}`

// AssembleAgentsMD collects *.md files across the given directories, sorted
// lexicographically by basename so numbered fragments (00-base.md,
// 10-toolchain-image.md, 15-toolchain-crd.md, 20-deployment.md) merge in order.
func AssembleAgentsMD(dirs ...string) (string, error) {
	type fragment struct {
		name    string
		content string
	}
	var collected []fragment
	for _, dir := range dirs {
		if strings.TrimSpace(dir) == "" {
			continue
		}
		entries, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("reading agents dir %s: %w", dir, err)
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				return "", fmt.Errorf("reading %s: %w", entry.Name(), err)
			}
			if trimmed := strings.TrimSpace(string(raw)); trimmed != "" {
				collected = append(collected, fragment{name: entry.Name(), content: trimmed})
			}
		}
	}

	sort.SliceStable(collected, func(i, j int) bool {
		return collected[i].name < collected[j].name
	})

	var parts []string
	for _, f := range collected {
		parts = append(parts, f.content)
	}
	return strings.Join(parts, "\n\n"), nil
}

// BuildUserPrompt constructs the initial review prompt using git diff --stat
// and metadata rather than stuffing large diffs into the context window.
func BuildUserPrompt(req Request, diffStat string, hasRepoAgentsMD bool) string {
	var b strings.Builder
	b.WriteString("## Review Request\n")
	if req.EventName != "" {
		fmt.Fprintf(&b, "- Event: `%s`\n", req.EventName)
	}
	if req.Repo != "" {
		fmt.Fprintf(&b, "- Repository: `%s`\n", req.Repo)
	}
	if req.PRNumber > 0 {
		fmt.Fprintf(&b, "- Pull Request: #%d\n", req.PRNumber)
	}
	if req.IssueNumber > 0 && req.IssueNumber != req.PRNumber {
		fmt.Fprintf(&b, "- Issue: #%d\n", req.IssueNumber)
	}
	if req.WorkflowRunID > 0 {
		fmt.Fprintf(&b, "- Workflow Run: `%d`\n", req.WorkflowRunID)
	}
	if req.Tag != "" {
		fmt.Fprintf(&b, "- Tag: `%s`\n", req.Tag)
	}
	if req.Environment != "" {
		fmt.Fprintf(&b, "- Environment: `%s`\n", req.Environment)
	}
	if req.EnvironmentURL != "" {
		fmt.Fprintf(&b, "- Environment URL: `%s`\n", req.EnvironmentURL)
	}
	if req.BaseRef != "" {
		fmt.Fprintf(&b, "- Base Ref: `%s`\n", req.BaseRef)
	}
	if req.HeadSHA != "" {
		fmt.Fprintf(&b, "- Head SHA: `%s`\n", req.HeadSHA)
	}
	if hasRepoAgentsMD {
		b.WriteString("- Repository guidance: `AGENTS.md` is present in the workspace root; read it before finalizing.\n")
	}

	b.WriteString("\n## Instructions\n")
	b.WriteString(strings.TrimSpace(req.Instructions))
	b.WriteString("\n")

	if strings.TrimSpace(diffStat) != "" {
		b.WriteString("\n## Changed Files (`git diff --stat`)\n```text\n")
		b.WriteString(strings.TrimSpace(diffStat))
		b.WriteString("\n```\n")
		b.WriteString("Use `git diff` / `read` tools to inspect relevant hunks on demand.\n")
	}

	return b.String()
}

// ExtractReleaseResult parses release notes from either structured JSON
// (containing title/body/summary) or raw Markdown emitted by the assistant.
func ExtractReleaseResult(text, fallbackTag string) (*Result, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil, fmt.Errorf("assistant returned empty release notes")
	}
	if res, err := ExtractReviewResult(trimmed); err == nil {
		if strings.TrimSpace(res.Title) == "" {
			res.Title = fallbackTag
		}
		if strings.TrimSpace(res.Body) == "" {
			if strings.TrimSpace(res.Summary) != "" {
				res.Body = strings.TrimSpace(res.Summary)
			} else {
				res.Body = trimmed
			}
		}
		if strings.TrimSpace(res.Summary) == "" {
			res.Summary = firstMarkdownSummaryLine(res.Body)
		}
		return res, nil
	}
	return &Result{
		Verdict: "COMMENT",
		Title:   fallbackTag,
		Summary: firstMarkdownSummaryLine(trimmed),
		Body:    trimmed,
	}, nil
}

func firstMarkdownSummaryLine(md string) string {
	for _, line := range strings.Split(md, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(line, "#-* "))
		if line != "" {
			if runes := []rune(line); len(runes) > 160 {
				return string(runes[:157]) + "..."
			}
			return line
		}
	}
	return "Draft release notes generated"
}

// ExtractReviewResult parses a Result from raw JSON or a ```json fenced block
// within the assistant's final text.
func ExtractReviewResult(text string) (*Result, error) {
	candidates := extractJSONCandidates(text)
	for i := len(candidates) - 1; i >= 0; i-- {
		var res Result
		if err := json.Unmarshal([]byte(candidates[i]), &res); err == nil && res.Verdict != "" {
			return &res, nil
		}
	}
	return nil, fmt.Errorf("no valid review result JSON found in assistant output")
}

func extractJSONCandidates(text string) []string {
	var out []string
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}") {
		out = append(out, trimmed)
	}

	// Scan fenced ```json ... ``` or ``` ... ``` blocks.
	rest := text
	for {
		idx := strings.Index(rest, "```")
		if idx < 0 {
			break
		}
		rest = rest[idx+3:]
		nl := strings.IndexByte(rest, '\n')
		if nl < 0 {
			break
		}
		lang := strings.TrimSpace(rest[:nl])
		rest = rest[nl+1:]
		end := strings.Index(rest, "```")
		if end < 0 {
			break
		}
		block := strings.TrimSpace(rest[:end])
		rest = rest[end+3:]
		if (lang == "" || lang == "json") && strings.HasPrefix(block, "{") {
			out = append(out, block)
		}
	}

	// Fallback: outermost braces if no fenced block succeeded.
	if first := strings.IndexByte(text, '{'); first >= 0 {
		if last := strings.LastIndexByte(text, '}'); last > first {
			out = append(out, text[first:last+1])
		}
	}
	return out
}
