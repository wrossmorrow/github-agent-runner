package review

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// DefaultProtectedBranches lists branch names rejected by default unless overridden on GhPoster.ProtectedBranches.
var DefaultProtectedBranches = []string{"main", "master", "HEAD"}

// validateTargetBranch ensures the push target is a safe, non-protected branch ref name.
func validateTargetBranch(branch string, protected []string) (string, error) {
	b := strings.TrimSpace(branch)
	b = strings.TrimPrefix(b, "refs/heads/")
	if b == "" {
		return "", fmt.Errorf("target branch is required")
	}
	if strings.HasPrefix(b, "-") || strings.HasPrefix(b, "/") || strings.HasSuffix(b, "/") || strings.HasSuffix(b, ".") {
		return "", fmt.Errorf("invalid target branch %q", branch)
	}
	if strings.ContainsAny(b, " \t\n\r~^:?*[\\") || strings.Contains(b, "..") || strings.Contains(b, "@{") || strings.Contains(b, "//") {
		return "", fmt.Errorf("invalid target branch %q", branch)
	}
	branches := protected
	if branches == nil {
		branches = DefaultProtectedBranches
	}
	lowerTarget := strings.ToLower(b)
	for _, entry := range branches {
		trimmed := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(entry), "refs/heads/"))
		if trimmed != "" && trimmed == lowerTarget {
			return "", fmt.Errorf("refusing to push to protected branch %q", b)
		}
	}
	return b, nil
}

// validateChangedPaths rejects empty commits and edits touching CI workflows or CODEOWNERS.
func validateChangedPaths(paths []string) error {
	if len(paths) == 0 {
		return fmt.Errorf("no workspace changes to commit")
	}
	for _, p := range paths {
		cleaned := path.Clean(strings.ReplaceAll(strings.TrimSpace(p), "\\", "/"))
		cleaned = strings.TrimPrefix(cleaned, "./")
		lower := strings.ToLower(cleaned)
		if lower == ".github/workflows" || strings.HasPrefix(lower, ".github/workflows/") ||
			lower == "codeowners" || lower == ".github/codeowners" || strings.HasSuffix(lower, "/codeowners") {
			return fmt.Errorf("blocked path modified in workspace: %q", p)
		}
	}
	return nil
}

func listChangedPaths(ctx context.Context, workspace string) ([]string, error) {
	allPaths, _, err := inspectWorkspaceChanges(ctx, workspace)
	return allPaths, err
}

func inspectWorkspaceChanges(ctx context.Context, workspace string) (allPaths, stagePaths []string, err error) {
	out, err := runGit(ctx, workspace, "", "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return nil, nil, err
	}
	allPaths, stagePaths = parsePorcelainZStatus(string(out))
	return allPaths, stagePaths, nil
}

func parsePorcelainZ(raw string) []string {
	allPaths, _ := parsePorcelainZStatus(raw)
	return allPaths
}

func parsePorcelainZStatus(raw string) (allPaths, stagePaths []string) {
	if raw == "" {
		return nil, nil
	}
	tokens := strings.Split(raw, "\x00")
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if len(tok) < 4 {
			continue
		}
		x, y := tok[0], tok[1]
		p := tok[3:]
		if p != "" {
			allPaths = append(allPaths, p)
			// Only paths with unstaged worktree changes (y != ' ') need `git add --`;
			// already-staged entries (e.g. `git mv`/`git rm` where y == ' ') stay in the index.
			if y != ' ' {
				stagePaths = append(stagePaths, p)
			}
		}
		// Renames and copies emit the source path in the next NUL-delimited token;
		// validate it against blocked paths without passing the removed source to `git add`.
		if (x == 'R' || x == 'C' || y == 'R' || y == 'C') && i+1 < len(tokens) {
			i++
			if tokens[i] != "" {
				allPaths = append(allPaths, tokens[i])
			}
		}
	}
	return allPaths, stagePaths
}

func (p *GhPoster) postBranchCommit(ctx context.Context, ghBin, token string, req Request, res *Result) error {
	if strings.TrimSpace(req.Workspace) == "" {
		return fmt.Errorf("workspace is required for output %q", req.Output)
	}

	branchSpec := strings.TrimSpace(req.TargetBranch)
	if branchSpec == "" {
		if req.Output == OutputRevertPR {
			shortSHA := strings.TrimSpace(req.HeadSHA)
			if len(shortSHA) > 12 {
				shortSHA = shortSHA[:12]
			}
			if shortSHA == "" {
				if out, err := runGit(ctx, req.Workspace, "", "rev-parse", "--short", "HEAD"); err == nil {
					shortSHA = strings.TrimSpace(string(out))
				}
			}
			if shortSHA == "" {
				shortSHA = "latest"
			}
			branchSpec = "bot/revert-" + shortSHA
		} else if headRef := strings.TrimSpace(req.HeadRef); headRef != "" {
			branchSpec = headRef
		} else {
			out, err := runGit(ctx, req.Workspace, "", "rev-parse", "--abbrev-ref", "HEAD")
			if err != nil {
				return fmt.Errorf("resolving current branch: %w", err)
			}
			branchSpec = strings.TrimSpace(string(out))
			if branchSpec == "HEAD" {
				return fmt.Errorf("workspace is in detached HEAD state; set target branch explicitly")
			}
		}
	}

	branch, err := validateTargetBranch(branchSpec, p.ProtectedBranches)
	if err != nil {
		return err
	}

	changed, toStage, err := inspectWorkspaceChanges(ctx, req.Workspace)
	if err != nil {
		return err
	}
	if err := validateChangedPaths(changed); err != nil {
		return err
	}

	curBranchOut, err := runGit(ctx, req.Workspace, "", "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return fmt.Errorf("reading current branch: %w", err)
	}
	if strings.TrimSpace(string(curBranchOut)) != branch {
		if _, err := runGit(ctx, req.Workspace, "", "checkout", "-b", branch); err != nil {
			return fmt.Errorf("switching to target branch %q: %w", branch, err)
		}
	}

	seen := make(map[string]struct{}, len(toStage))
	uniqueStage := make([]string, 0, len(toStage))
	for _, p := range toStage {
		if _, ok := seen[p]; !ok {
			seen[p] = struct{}{}
			uniqueStage = append(uniqueStage, p)
		}
	}
	if len(uniqueStage) > 0 {
		addArgs := append([]string{"add", "--"}, uniqueStage...)
		if _, err := runGit(ctx, req.Workspace, "", addArgs...); err != nil {
			return fmt.Errorf("staging workspace changes: %w", err)
		}
	}

	title := strings.TrimSpace(res.Title)
	if title == "" {
		title = strings.TrimSpace(res.Summary)
	}
	if title == "" {
		if req.Output == OutputRevertPR {
			title = "chore: revert failing deployment"
		} else {
			title = "chore: apply automated changes"
		}
	}
	if len([]rune(title)) > 120 {
		title = string([]rune(title)[:117]) + "..."
	}

	commitMsg := title
	if summary := strings.TrimSpace(res.Summary); summary != "" && summary != title {
		commitMsg = title + "\n\n" + summary
	}

	if _, err := runGit(ctx, req.Workspace, "",
		"-c", "user.name=agentrunner-bot",
		"-c", "user.email=agentrunner-bot@users.noreply.github.com",
		"commit", "-m", commitMsg,
	); err != nil {
		return fmt.Errorf("committing changes: %w", err)
	}

	if _, err := runGit(ctx, req.Workspace, token, "push", "origin", "HEAD:refs/heads/"+branch); err != nil {
		return fmt.Errorf("pushing branch %q: %w", branch, err)
	}

	if req.Output == OutputRevertPR || req.OpenPR {
		existingPR, err := findOpenPRNumber(ctx, ghBin, token, req.Repo, branch)
		if err != nil {
			return err
		}
		if existingPR > 0 {
			res.OpenedPRNumber = existingPR
			return nil
		}
		base := strings.TrimSpace(req.BaseRef)
		if base == "" {
			base = "main"
		}
		prBody := formatReviewMarkdown(res)
		args := []string{
			"pr", "create",
			"--repo", req.Repo,
			"--base", base,
			"--head", branch,
			"--title", title,
			"--body", prBody,
		}
		out, err := runGH(ctx, ghBin, token, args, nil)
		if err != nil {
			return err
		}
		if createdPR := parsePRNumberFromURL(string(out)); createdPR > 0 {
			res.OpenedPRNumber = createdPR
		}
	}
	return nil
}

var prURLNumberRegex = regexp.MustCompile(`/pull/([0-9]+)(?:[^0-9]|$)`)

func parsePRNumberFromURL(raw string) int {
	m := prURLNumberRegex.FindStringSubmatch(raw)
	if len(m) != 2 {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

func findOpenPRNumber(ctx context.Context, ghBin, token, repo, headBranch string) (int, error) {
	args := []string{
		"pr", "list",
		"--repo", repo,
		"--head", headBranch,
		"--state", "open",
		"--json", "number",
		"--limit", "1",
	}
	out, err := runGH(ctx, ghBin, token, args, nil)
	if err != nil {
		return 0, err
	}
	var prs []struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal(out, &prs); err != nil {
		return 0, fmt.Errorf("parsing open PR check for %q: %w", headBranch, err)
	}
	if len(prs) == 0 || prs[0].Number <= 0 {
		return 0, nil
	}
	return prs[0].Number, nil
}

func runGit(ctx context.Context, workspace, githubToken string, args ...string) ([]byte, error) {
	fullArgs := append([]string{"-C", workspace}, args...)
	cmd := exec.CommandContext(ctx, "git", fullArgs...)
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if githubToken != "" {
		creds := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + githubToken))
		env = append(env,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.https://github.com/.extraHeader",
			"GIT_CONFIG_VALUE_0=Authorization: Basic "+creds,
		)
	}
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s failed: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}
