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
End your final response with a single JSON object matching:
{
  "verdict": "APPROVE" | "COMMENT" | "REQUEST_CHANGES",
  "summary": "concise summary",
  "comments": [
    {"path": "relative/path", "line": 1, "severity": "info"|"warning"|"error", "body": "comment"}
  ]
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
	if req.Repo != "" {
		fmt.Fprintf(&b, "- Repository: `%s`\n", req.Repo)
	}
	if req.PRNumber > 0 {
		fmt.Fprintf(&b, "- Pull Request: #%d\n", req.PRNumber)
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
