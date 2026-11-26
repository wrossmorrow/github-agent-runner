package review

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeCommentPath(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"./pkg/a.go", "pkg/a.go"},
		{"/pkg/a.go", "pkg/a.go"},
		{"pkg/./a.go", "pkg/a.go"},
		{`.\pkg\a.go`, "pkg/a.go"},
		{"../escape.go", ""},
		{"", ""},
		{".", ""},
	}
	for _, tc := range cases {
		if got := normalizeCommentPath(tc.in); got != tc.want {
			t.Errorf("normalizeCommentPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseDiffLines(t *testing.T) {
	diff := `diff --git a/pkg/a.go b/pkg/a.go
index 1111111..2222222 100644
--- a/pkg/a.go
+++ b/pkg/a.go
@@ -10,4 +10,5 @@ func hello() {
 	line10()
-	removed()
+	added11()
+	added12()
 	line13()
diff --git a/deleted.go b/deleted.go
deleted file mode 100644
index 1111111..0000000
--- a/deleted.go
+++ /dev/null
@@ -1,2 +0,0 @@
-package deleted
-var x = 1
`
	valid := parseDiffLines(diff)
	lines, ok := valid["pkg/a.go"]
	if !ok {
		t.Fatalf("expected pkg/a.go in parsed diff, got %#v", valid)
	}
	for _, want := range []int{10, 11, 12, 13} {
		if _, ok := lines[want]; !ok {
			t.Errorf("expected line %d to be valid in pkg/a.go, got %#v", want, lines)
		}
	}
	if _, ok := lines[14]; ok {
		t.Errorf("did not expect line 14 in pkg/a.go")
	}
	if _, ok := valid["deleted.go"]; ok {
		t.Errorf("did not expect deleted.go (/dev/null) in valid diff lines")
	}
}

func TestPartitionCommentsAndBodyFormatting(t *testing.T) {
	validLines := map[string]map[int]struct{}{
		"pkg/a.go": {12: {}},
	}
	comments := []Comment{
		{Path: "./pkg/a.go", Line: 12, Severity: "error", Body: "Inline normalized path"},
		{Path: "pkg/a.go", Line: 99, Severity: "warning", Body: "Line outside hunk"},
		{Path: "unchanged.go", Line: 5, Severity: "info", Body: "Unchanged file"},
		{Path: "pkg/a.go", Line: 0, Severity: "info", Body: "File-level note"},
	}

	inline, offDiff := partitionComments(comments, validLines)
	if len(inline) != 1 || inline[0].Path != "pkg/a.go" || inline[0].Line != 12 {
		t.Fatalf("unexpected inline comments: %+v", inline)
	}
	if len(offDiff) != 3 {
		t.Fatalf("expected 3 offDiff comments, got %d: %+v", len(offDiff), offDiff)
	}

	res := &Result{
		Verdict:  "REQUEST_CHANGES",
		Summary:  "Found issues.",
		Comments: comments,
	}
	body := formatPRReviewBody(res, inline, offDiff)
	if !strings.Contains(body, "### Findings") || !strings.Contains(body, "Inline normalized path") {
		t.Fatalf("missing Findings section in body:\n%s", body)
	}
	if !strings.Contains(body, "### Notes outside the diff") ||
		!strings.Contains(body, "`pkg/a.go:99`") ||
		!strings.Contains(body, "`unchanged.go:5`") ||
		!strings.Contains(body, "`pkg/a.go`") {
		t.Fatalf("missing Notes outside the diff entries in body:\n%s", body)
	}
}

func TestGhPoster422Fallback(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte("ghs_test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	captureFile := filepath.Join(dir, "requests.jsonl")
	ghScript := filepath.Join(dir, "fake-gh.sh")
	script := `#!/bin/sh
payload="$(cat)"
printf '%s\n' "$payload" >> "` + captureFile + `"
if printf '%s' "$payload" | grep -q '"line":99'; then
  echo "gh: Unprocessable Entity (HTTP 422)" >&2
  exit 1
fi
exit 0
`
	if err := os.WriteFile(ghScript, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	poster := &GhPoster{TokenFile: tokenFile, GHBin: ghScript}
	err := poster.PostReview(context.Background(), Request{
		Repo:     "org/repo",
		PRNumber: 7,
		Output:   "pr-review",
	}, &Result{
		Verdict: "REQUEST_CHANGES",
		Summary: "Needs changes",
		Comments: []Comment{
			{Path: "./pkg/a.go", Line: 99, Severity: "error", Body: "Outside hunk"},
		},
	})
	if err != nil {
		t.Fatalf("expected 422 fallback to succeed, got %v", err)
	}

	raw, err := os.ReadFile(captureFile)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 gh invocations (initial + 422 fallback), got %d:\n%s", len(lines), string(raw))
	}

	var retry map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &retry); err != nil {
		t.Fatalf("unmarshal retry payload: %v", err)
	}
	comments, _ := retry["comments"].([]any)
	if len(comments) != 0 {
		t.Fatalf("expected fallback retry to send empty comments[], got %#v", retry["comments"])
	}
	body, _ := retry["body"].(string)
	if !strings.Contains(body, "Outside hunk") {
		t.Fatalf("expected fallback body to preserve all findings, got:\n%s", body)
	}
}

func TestCheckOutputSupport(t *testing.T) {
	for _, okOut := range []string{"", OutputStdout, OutputPRReview, OutputComment, OutputIssueComment, OutputReleaseDraft, OutputSuggest} {
		if err := CheckOutputSupport(okOut); err != nil {
			t.Errorf("CheckOutputSupport(%q) unexpected error: %v", okOut, err)
		}
	}
	for _, futureOut := range []string{OutputDeploymentStatus, OutputBranchCommit, OutputRevertPR} {
		err := CheckOutputSupport(futureOut)
		if !errors.Is(err, ErrOutputNotImplemented) {
			t.Errorf("CheckOutputSupport(%q) = %v, want ErrOutputNotImplemented", futureOut, err)
		}
	}
	unknownErr := CheckOutputSupport("unknown-target")
	if unknownErr == nil || errors.Is(unknownErr, ErrOutputNotImplemented) {
		t.Errorf("expected plain error for unknown output, got %v", unknownErr)
	}
}

func TestGhPosterIssueComment(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte("ghs_test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	argsFile := filepath.Join(dir, "args.txt")
	ghScript := filepath.Join(dir, "fake-gh.sh")
	script := `#!/bin/sh
printf '%s\n' "$*" > "` + argsFile + `"
`
	if err := os.WriteFile(ghScript, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	poster := &GhPoster{TokenFile: tokenFile, GHBin: ghScript}
	cases := []struct {
		name string
		req  Request
		want string
	}{
		{
			name: "explicit IssueNumber",
			req: Request{
				Repo:        "org/repo",
				IssueNumber: 88,
				Output:      OutputIssueComment,
			},
			want: "issue comment 88 --repo org/repo",
		},
		{
			name: "PRNumber fallback when IssueNumber unset",
			req: Request{
				Repo:     "org/repo",
				PRNumber: 42,
				Output:   OutputIssueComment,
			},
			want: "issue comment 42 --repo org/repo",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := poster.PostReview(context.Background(), tc.req, &Result{
				Verdict: "COMMENT",
				Summary: "Reproduced bug locally.",
			})
			if err != nil {
				t.Fatalf("PostReview issue-comment failed: %v", err)
			}

			raw, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatal(err)
			}
			got := string(raw)
			if !strings.Contains(got, tc.want) {
				t.Fatalf("expected %q, got %q", tc.want, got)
			}
		})
	}
}

func TestGhPosterReleaseDraft(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte("ghs_test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	argsLog := filepath.Join(dir, "args.log")
	notesLog := filepath.Join(dir, "notes.md")
	stateFile := filepath.Join(dir, "view_state")
	ghScript := filepath.Join(dir, "fake-gh.sh")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "` + argsLog + `"
if [ "$1" = "release" ] && [ "$2" = "view" ]; then
  state="$(cat "` + stateFile + `" 2>/dev/null || echo missing)"
  if [ "$state" = "draft" ]; then
    echo '{"isDraft":true}'
    exit 0
  fi
  if [ "$state" = "published" ]; then
    echo '{"isDraft":false}'
    exit 0
  fi
  echo "release not found" >&2
  exit 1
fi
cat > "` + notesLog + `"
`
	if err := os.WriteFile(ghScript, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	poster := &GhPoster{TokenFile: tokenFile, GHBin: ghScript}
	res := &Result{
		Verdict: "COMMENT",
		Title:   "v0.1.0",
		Summary: "Initial release",
		Body:    "## Features\n- Added `release-draft`",
	}

	t.Run("creates new draft release", func(t *testing.T) {
		_ = os.Remove(argsLog)
		_ = os.WriteFile(stateFile, []byte("missing"), 0o600)
		if err := poster.PostReview(context.Background(), Request{Repo: "org/repo", Tag: "v0.1.0", Output: OutputReleaseDraft}, res); err != nil {
			t.Fatalf("PostReview create draft failed: %v", err)
		}
		argsRaw, _ := os.ReadFile(argsLog)
		if !strings.Contains(string(argsRaw), "release create v0.1.0 --repo org/repo --draft --verify-tag --title v0.1.0 --notes-file -") {
			t.Fatalf("unexpected gh create args: %q", string(argsRaw))
		}
		notesRaw, _ := os.ReadFile(notesLog)
		if !strings.Contains(string(notesRaw), "Added `release-draft`") {
			t.Fatalf("unexpected release notes stdin: %q", string(notesRaw))
		}
	})

	t.Run("edits existing draft release", func(t *testing.T) {
		_ = os.Remove(argsLog)
		_ = os.WriteFile(stateFile, []byte("draft"), 0o600)
		if err := poster.PostReview(context.Background(), Request{Repo: "org/repo", Tag: "v0.1.0", Output: OutputReleaseDraft}, res); err != nil {
			t.Fatalf("PostReview edit draft failed: %v", err)
		}
		argsRaw, _ := os.ReadFile(argsLog)
		if !strings.Contains(string(argsRaw), "release edit v0.1.0 --repo org/repo --draft --title v0.1.0 --notes-file -") {
			t.Fatalf("unexpected gh edit args: %q", string(argsRaw))
		}
	})

	t.Run("refuses to overwrite published release", func(t *testing.T) {
		_ = os.Remove(argsLog)
		_ = os.WriteFile(stateFile, []byte("published"), 0o600)
		err := poster.PostReview(context.Background(), Request{Repo: "org/repo", Tag: "v0.1.0", Output: OutputReleaseDraft}, res)
		if err == nil || !strings.Contains(err.Error(), "published release") {
			t.Fatalf("expected published release error, got %v", err)
		}
		argsRaw, _ := os.ReadFile(argsLog)
		if strings.Contains(string(argsRaw), "release edit") || strings.Contains(string(argsRaw), "release create") {
			t.Fatalf("should not call edit or create on published release: %q", string(argsRaw))
		}
	})

	t.Run("rejects leading-dash tag before invoking gh", func(t *testing.T) {
		_ = os.Remove(argsLog)
		err := poster.PostReview(context.Background(), Request{Repo: "org/repo", Tag: "--help", Output: OutputReleaseDraft}, res)
		if err == nil || !strings.Contains(err.Error(), "invalid release tag") {
			t.Fatalf("expected invalid release tag error, got %v", err)
		}
		if _, statErr := os.Stat(argsLog); !os.IsNotExist(statErr) {
			t.Fatalf("gh should not have been invoked for --help tag, stat err=%v", statErr)
		}
	})
}

func TestToGitHubCommentsSuggestions(t *testing.T) {
	validLines := map[string]map[int]struct{}{
		"pkg/a.go": {10: {}, 11: {}, 12: {}, 20: {}},
	}
	comments := []Comment{
		{
			Path:       "pkg/a.go",
			Line:       20,
			Severity:   "info",
			Body:       "Add doc comment",
			Suggestion: "// Foo bars.\nfunc Foo() {}",
		},
		{
			Path:       "pkg/a.go",
			StartLine:  10,
			Line:       12,
			Severity:   "warning",
			Body:       "Simplify snippet",
			Suggestion: "code := \"```go\\nhi\\n```\"",
		},
		{
			Path:       "pkg/a.go",
			StartLine:  5, // outside diff hunk
			Line:       12,
			Severity:   "warning",
			Body:       "Off-hunk start line",
			Suggestion: "fixed()",
		},
	}

	inline, offDiff := partitionComments(comments, validLines)
	if len(inline) != 2 || len(offDiff) != 1 {
		t.Fatalf("expected 2 inline and 1 offDiff, got inline=%d offDiff=%d", len(inline), len(offDiff))
	}
	if offDiff[0].StartLine != 5 {
		t.Fatalf("expected off-diff multi-line suggestion preserved cleanly, got %+v", offDiff[0])
	}

	ghComments := toGitHubComments(inline)
	if len(ghComments) != 2 {
		t.Fatalf("expected 2 GitHub comments, got %d", len(ghComments))
	}

	// Single-line suggestion has no start_line and uses ```suggestion fence.
	c0 := ghComments[0]
	if _, hasStart := c0["start_line"]; hasStart {
		t.Fatalf("single-line suggestion should not set start_line: %+v", c0)
	}
	body0 := c0["body"].(string)
	if !strings.Contains(body0, "```suggestion\n// Foo bars.\nfunc Foo() {}\n```") {
		t.Fatalf("unexpected single-line suggestion body:\n%s", body0)
	}

	// Multi-line suggestion with embedded triple backticks uses 4-backtick fence and start_line/start_side.
	c1 := ghComments[1]
	if c1["start_line"] != 10 || c1["start_side"] != "RIGHT" || c1["line"] != 12 || c1["side"] != "RIGHT" {
		t.Fatalf("unexpected multi-line range fields: %+v", c1)
	}
	body1 := c1["body"].(string)
	if !strings.Contains(body1, "````suggestion\n") || !strings.HasSuffix(body1, "\n````") {
		t.Fatalf("expected 4-backtick suggestion fence, got:\n%s", body1)
	}
}
