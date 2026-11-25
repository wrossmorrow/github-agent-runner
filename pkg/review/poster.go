package review

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Poster publishes a completed review back to GitHub.
type Poster interface {
	PostReview(ctx context.Context, req Request, res *Result) error
}

// GhPoster posts PR reviews or comments via the `gh` CLI using the token
// minted by the github-auth sidecar.
type GhPoster struct {
	TokenFile string
	GHBin     string
}

// PostReview submits the structured Result to GitHub as a PR review or comment.
func (p *GhPoster) PostReview(ctx context.Context, req Request, res *Result) error {
	if req.Output == "" || req.Output == "stdout" {
		return nil
	}
	if req.Repo == "" || req.PRNumber <= 0 {
		return fmt.Errorf("repo and prNumber are required to post output %q", req.Output)
	}

	ghBin := p.GHBin
	if ghBin == "" {
		ghBin = "gh"
	}

	token, err := p.readToken()
	if err != nil {
		return err
	}

	body := formatReviewMarkdown(res)
	var args []string
	var stdin []byte

	switch req.Output {
	case "comment":
		args = []string{"pr", "comment", strconv.Itoa(req.PRNumber), "--repo", req.Repo, "--body", body}
	case "pr-review":
		payload, err := json.Marshal(map[string]any{
			"event":    mapEvent(res.Verdict),
			"body":     body,
			"comments": toGitHubComments(res.Comments),
		})
		if err != nil {
			return err
		}
		stdin = payload
		args = []string{
			"api",
			fmt.Sprintf("repos/%s/pulls/%d/reviews", req.Repo, req.PRNumber),
			"--method", "POST",
			"--input", "-",
		}
	default:
		return fmt.Errorf("unsupported output mode %q", req.Output)
	}

	cmd := exec.CommandContext(ctx, ghBin, args...)
	if len(stdin) > 0 {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	if token != "" {
		cmd.Env = append(os.Environ(), "GH_TOKEN="+token)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh %s failed: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (p *GhPoster) readToken() (string, error) {
	if p.TokenFile == "" {
		return os.Getenv("GH_TOKEN"), nil
	}
	raw, err := os.ReadFile(p.TokenFile)
	if err != nil {
		return "", fmt.Errorf("reading github token file %s: %w", p.TokenFile, err)
	}
	return strings.TrimSpace(string(raw)), nil
}

func mapEvent(verdict string) string {
	switch strings.ToUpper(strings.TrimSpace(verdict)) {
	case "APPROVE":
		return "APPROVE"
	case "REQUEST_CHANGES":
		return "REQUEST_CHANGES"
	default:
		return "COMMENT"
	}
}

func formatReviewMarkdown(res *Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**Verdict:** `%s`\n\n%s\n", res.Verdict, strings.TrimSpace(res.Summary))
	if len(res.Comments) > 0 {
		b.WriteString("\n### Findings\n")
		for _, c := range res.Comments {
			fmt.Fprintf(&b, "- `[%s]` `%s:%d`: %s\n", strings.ToUpper(c.Severity), c.Path, c.Line, c.Body)
		}
	}
	return b.String()
}

func toGitHubComments(comments []Comment) []map[string]any {
	out := make([]map[string]any, 0, len(comments))
	for _, c := range comments {
		if c.Path == "" || c.Line <= 0 {
			continue
		}
		out = append(out, map[string]any{
			"path": c.Path,
			"line": c.Line,
			"body": fmt.Sprintf("[%s] %s", strings.ToUpper(c.Severity), c.Body),
		})
	}
	return out
}
