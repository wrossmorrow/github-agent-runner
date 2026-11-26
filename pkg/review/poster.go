package review

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
)

const (
	OutputStdout           = "stdout"
	OutputPRReview         = "pr-review"
	OutputComment          = "comment"
	OutputIssueComment     = "issue-comment"
	OutputReleaseDraft     = "release-draft"
	OutputDeploymentStatus = "deployment-status"
	OutputBranchCommit     = "branch-commit"
	OutputRevertPR         = "revert-pr"
	OutputSuggest          = "suggest"
)

// ErrOutputNotImplemented marks planned output publishers that are not yet implemented.
var ErrOutputNotImplemented = errors.New("output mode not yet implemented")

// CheckOutputSupport validates whether req.Output is currently implemented.
// Planned future outputs wrap ErrOutputNotImplemented so callers can surface HTTP 501.
func CheckOutputSupport(output string) error {
	switch output {
	case "", OutputStdout, OutputPRReview, OutputComment, OutputIssueComment, OutputReleaseDraft, OutputSuggest:
		return nil
	case OutputDeploymentStatus, OutputBranchCommit, OutputRevertPR:
		return fmt.Errorf("%w: %q", ErrOutputNotImplemented, output)
	default:
		return fmt.Errorf("unsupported output mode %q", output)
	}
}

// Poster publishes a completed review back to GitHub.
type Poster interface {
	PostReview(ctx context.Context, req Request, res *Result) error
}

// GhPoster posts PR/issue reviews or comments via the `gh` CLI using the token
// minted by the github-auth sidecar.
type GhPoster struct {
	TokenFile string
	GHBin     string
}

// PostReview submits the structured Result to GitHub as a PR review or comment.
func (p *GhPoster) PostReview(ctx context.Context, req Request, res *Result) error {
	if req.Output == "" || req.Output == OutputStdout {
		return nil
	}
	if err := CheckOutputSupport(req.Output); err != nil {
		return err
	}
	if req.Repo == "" {
		return fmt.Errorf("repo is required to post output %q", req.Output)
	}

	ghBin := p.GHBin
	if ghBin == "" {
		ghBin = "gh"
	}

	token, err := p.readToken()
	if err != nil {
		return err
	}

	switch req.Output {
	case OutputReleaseDraft:
		return p.postReleaseDraft(ctx, ghBin, token, req, res)
	case OutputIssueComment:
		issueNum := req.IssueNumber
		if issueNum <= 0 {
			issueNum = req.PRNumber
		}
		if issueNum <= 0 {
			return fmt.Errorf("issueNumber is required to post output %q", req.Output)
		}
		args := []string{"issue", "comment", strconv.Itoa(issueNum), "--repo", req.Repo, "--body", formatReviewMarkdown(res)}
		_, err := runGH(ctx, ghBin, token, args, nil)
		return err
	case OutputComment:
		if req.PRNumber <= 0 {
			return fmt.Errorf("repo and prNumber are required to post output %q", req.Output)
		}
		args := []string{"pr", "comment", strconv.Itoa(req.PRNumber), "--repo", req.Repo, "--body", formatReviewMarkdown(res)}
		_, err := runGH(ctx, ghBin, token, args, nil)
		return err
	case OutputPRReview, OutputSuggest:
		if req.PRNumber <= 0 {
			return fmt.Errorf("repo and prNumber are required to post output %q", req.Output)
		}
		args := []string{
			"api",
			fmt.Sprintf("repos/%s/pulls/%d/reviews", req.Repo, req.PRNumber),
			"--method", "POST",
			"--input", "-",
		}
		validLines := loadPRDiffLines(ctx, req.Workspace, req.BaseRef)
		inline, offDiff := partitionComments(res.Comments, validLines)
		inlineGH := toGitHubComments(inline)

		payload, err := json.Marshal(map[string]any{
			"event":    mapEvent(res.Verdict),
			"body":     formatPRReviewBody(res, inline, offDiff),
			"comments": inlineGH,
		})
		if err != nil {
			return err
		}

		out, err := runGH(ctx, ghBin, token, args, payload)
		if err != nil && len(inlineGH) > 0 && isHTTP422(out) {
			fallbackPayload, merr := json.Marshal(map[string]any{
				"event":    mapEvent(res.Verdict),
				"body":     formatReviewMarkdown(res),
				"comments": []map[string]any{},
			})
			if merr != nil {
				return merr
			}
			_, err = runGH(ctx, ghBin, token, args, fallbackPayload)
		}
		return err
	default:
		return fmt.Errorf("unsupported output mode %q", req.Output)
	}
}

func validateReleaseTag(tag string) (string, error) {
	trimmed := strings.TrimSpace(tag)
	if trimmed == "" {
		return "", fmt.Errorf("tag is required to post output %q", OutputReleaseDraft)
	}
	if strings.HasPrefix(trimmed, "-") || strings.ContainsAny(trimmed, " \t\n\r") {
		return "", fmt.Errorf("invalid release tag %q", tag)
	}
	return trimmed, nil
}

func (p *GhPoster) postReleaseDraft(ctx context.Context, ghBin, token string, req Request, res *Result) error {
	tag, err := validateReleaseTag(req.Tag)
	if err != nil {
		return err
	}
	title := strings.TrimSpace(res.Title)
	if title == "" {
		title = tag
	}
	notes := strings.TrimSpace(res.Body)
	if notes == "" {
		notes = strings.TrimSpace(res.Summary)
	}
	if notes == "" {
		notes = formatReviewMarkdown(res)
	}

	viewArgs := []string{"release", "view", tag, "--repo", req.Repo, "--json", "isDraft"}
	if out, err := runGH(ctx, ghBin, token, viewArgs, nil); err == nil {
		var info struct {
			IsDraft bool `json:"isDraft"`
		}
		if jerr := json.Unmarshal(out, &info); jerr != nil {
			return fmt.Errorf("parsing release view response for %q: %w", tag, jerr)
		}
		if !info.IsDraft {
			return fmt.Errorf("refusing to overwrite published release %q", tag)
		}
		editArgs := []string{"release", "edit", tag, "--repo", req.Repo, "--draft", "--title", title, "--notes-file", "-"}
		_, err = runGH(ctx, ghBin, token, editArgs, []byte(notes+"\n"))
		return err
	}

	createArgs := []string{"release", "create", tag, "--repo", req.Repo, "--draft", "--verify-tag", "--title", title, "--notes-file", "-"}
	_, err = runGH(ctx, ghBin, token, createArgs, []byte(notes+"\n"))
	return err
}

func runGH(ctx context.Context, ghBin, token string, args []string, stdin []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, ghBin, args...)
	if len(stdin) > 0 {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	if token != "" {
		cmd.Env = append(os.Environ(), "GH_TOKEN="+token)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("gh %s failed: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func isHTTP422(out []byte) bool {
	s := strings.ToLower(string(out))
	return strings.Contains(s, "422") || strings.Contains(s, "unprocessable")
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
	appendCommentSection(&b, "Findings", res.Comments)
	return b.String()
}

func formatPRReviewBody(res *Result, inline, offDiff []Comment) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**Verdict:** `%s`\n\n%s\n", res.Verdict, strings.TrimSpace(res.Summary))
	appendCommentSection(&b, "Findings", inline)
	appendCommentSection(&b, "Notes outside the diff", offDiff)
	return b.String()
}

func appendCommentSection(b *strings.Builder, heading string, comments []Comment) {
	if len(comments) == 0 {
		return
	}
	fmt.Fprintf(b, "\n### %s\n", heading)
	for _, c := range comments {
		loc := "(general)"
		if c.Path != "" && c.Line > 0 {
			if c.StartLine > 0 && c.StartLine < c.Line {
				loc = fmt.Sprintf("%s:%d-%d", c.Path, c.StartLine, c.Line)
			} else {
				loc = fmt.Sprintf("%s:%d", c.Path, c.Line)
			}
		} else if c.Path != "" {
			loc = c.Path
		}
		fmt.Fprintf(b, "- `[%s]` `%s`: %s\n", strings.ToUpper(c.Severity), loc, c.Body)
	}
}

func normalizeCommentPath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.ReplaceAll(p, "\\", "/")
	for strings.HasPrefix(p, "./") || strings.HasPrefix(p, "/") {
		p = strings.TrimPrefix(p, "./")
		p = strings.TrimPrefix(p, "/")
	}
	if p == "" {
		return ""
	}
	cleaned := path.Clean(p)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return ""
	}
	return cleaned
}

func loadPRDiffLines(ctx context.Context, workspace, baseRef string) map[string]map[int]int {
	if strings.TrimSpace(workspace) == "" || strings.TrimSpace(baseRef) == "" {
		return nil
	}
	for _, ref := range []string{"origin/" + baseRef + "...HEAD", baseRef + "...HEAD"} {
		cmd := exec.CommandContext(ctx, "git", "-C", workspace, "diff", "--no-color", "-U3", ref)
		if out, err := cmd.Output(); err == nil {
			return parseDiffLines(string(out))
		}
	}
	return nil
}

func parseDiffLines(diffText string) map[string]map[int]int {
	result := map[string]map[int]int{}
	var currentFile string
	newLine := 0
	hunkID := 0
	inHunk := false

	for _, line := range strings.Split(diffText, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			currentFile = ""
			inHunk = false
		case !inHunk && line == "+++ /dev/null":
			currentFile = ""
		case !inHunk && strings.HasPrefix(line, "+++ b/"):
			currentFile = normalizeCommentPath(strings.TrimPrefix(line, "+++ b/"))
			if currentFile != "" && result[currentFile] == nil {
				result[currentFile] = map[int]int{}
			}
		case strings.HasPrefix(line, "@@ "):
			if start, ok := parseHunkNewStart(line); ok {
				newLine = start
				hunkID++
				inHunk = true
			}
		case inHunk && currentFile != "":
			if line == "" {
				continue
			}
			switch line[0] {
			case ' ', '+':
				result[currentFile][newLine] = hunkID
				newLine++
			case '-', '\\':
			default:
				inHunk = false
			}
		}
	}
	return result
}

func parseHunkNewStart(line string) (int, bool) {
	idx := strings.Index(line, " +")
	if idx < 0 {
		return 0, false
	}
	rest := line[idx+2:]
	end := strings.IndexAny(rest, ", ")
	if end < 0 {
		return 0, false
	}
	n, err := strconv.Atoi(rest[:end])
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

func partitionComments(comments []Comment, validLines map[string]map[int]int) (inline []Comment, offDiff []Comment) {
	for _, c := range comments {
		norm := normalizeCommentPath(c.Path)
		c.Path = norm
		if norm == "" || c.Line <= 0 {
			c.StartLine = 0
			offDiff = append(offDiff, c)
			continue
		}
		if c.StartLine == c.Line {
			c.StartLine = 0
		} else if c.StartLine > c.Line || c.StartLine < 0 {
			if c.Suggestion != "" {
				offDiff = append(offDiff, c)
				continue
			}
			c.StartLine = 0
		}
		if validLines != nil {
			lines, ok := validLines[norm]
			if !ok {
				offDiff = append(offDiff, c)
				continue
			}
			endHunk, ok := lines[c.Line]
			if !ok {
				offDiff = append(offDiff, c)
				continue
			}
			if c.StartLine > 0 {
				if startHunk, ok := lines[c.StartLine]; !ok || startHunk != endHunk {
					if c.Suggestion != "" {
						// Multi-line suggestions must stay within one diff hunk and remain intact.
						offDiff = append(offDiff, c)
						continue
					}
					c.StartLine = 0
				}
			}
		}
		inline = append(inline, c)
	}
	return inline, offDiff
}

func fenceSuggestion(code string) string {
	code = strings.TrimRight(code, "\r\n")
	maxRun := 0
	cur := 0
	for _, r := range code {
		if r == '`' {
			cur++
			if cur > maxRun {
				maxRun = cur
			}
		} else {
			cur = 0
		}
	}
	fenceLen := 3
	if maxRun >= 3 {
		fenceLen = maxRun + 1
	}
	fence := strings.Repeat("`", fenceLen)
	return fence + "suggestion\n" + code + "\n" + fence
}

func toGitHubComments(comments []Comment) []map[string]any {
	out := make([]map[string]any, 0, len(comments))
	for _, c := range comments {
		if c.Path == "" || c.Line <= 0 {
			continue
		}
		body := fmt.Sprintf("[%s] %s", strings.ToUpper(c.Severity), c.Body)
		if c.Suggestion != "" {
			body += "\n\n" + fenceSuggestion(c.Suggestion)
		}
		entry := map[string]any{
			"path": c.Path,
			"line": c.Line,
			"side": "RIGHT",
			"body": body,
		}
		if c.StartLine > 0 && c.StartLine < c.Line {
			entry["start_line"] = c.StartLine
			entry["start_side"] = "RIGHT"
		}
		out = append(out, entry)
	}
	return out
}
