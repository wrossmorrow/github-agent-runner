package review

import (
	"context"
	"encoding/json"
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
