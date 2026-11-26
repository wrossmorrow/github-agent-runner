package review

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHydrateFromEventAllShapes(t *testing.T) {
	writeEvent := func(t *testing.T, body string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "event.json")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	t.Run("issues", func(t *testing.T) {
		ep := writeEvent(t, `{
			"repository": {"full_name": "org/repo"},
			"issue": {"number": 101}
		}`)
		got := HydrateFromEvent(Request{EventPath: ep, EventName: "issues"})
		if got.Repo != "org/repo" || got.IssueNumber != 101 || got.PRNumber != 0 {
			t.Fatalf("unexpected hydration for issues: %+v", got)
		}
	})

	t.Run("issue_comment on pull request", func(t *testing.T) {
		ep := writeEvent(t, `{
			"repository": {"full_name": "org/repo"},
			"issue": {"number": 55, "pull_request": {"url": "https://api.github.com/repos/org/repo/pulls/55"}}
		}`)
		got := HydrateFromEvent(Request{EventPath: ep, EventName: "issue_comment"})
		if got.IssueNumber != 55 || got.PRNumber != 55 {
			t.Fatalf("expected PRNumber=55 for PR issue_comment, got %+v", got)
		}
	})

	t.Run("workflow_run", func(t *testing.T) {
		ep := writeEvent(t, `{
			"repository": {"full_name": "org/repo"},
			"workflow_run": {
				"id": 987654321,
				"head_sha": "deadbeef",
				"pull_requests": [{"number": 12, "base": {"ref": "main"}}]
			}
		}`)
		got := HydrateFromEvent(Request{EventPath: ep, EventName: "workflow_run"})
		if got.WorkflowRunID != 987654321 || got.HeadSHA != "deadbeef" || got.PRNumber != 12 || got.BaseRef != "main" {
			t.Fatalf("unexpected hydration for workflow_run: %+v", got)
		}
	})

	t.Run("push tag", func(t *testing.T) {
		ep := writeEvent(t, `{
			"ref": "refs/tags/v1.2.3",
			"after": "cafebabe",
			"repository": {"full_name": "org/repo"}
		}`)
		got := HydrateFromEvent(Request{EventPath: ep, EventName: "push"})
		if got.Tag != "v1.2.3" || got.HeadSHA != "cafebabe" {
			t.Fatalf("unexpected hydration for tag push: %+v", got)
		}
	})

	t.Run("deployment_status without overwriting explicit fields", func(t *testing.T) {
		ep := writeEvent(t, `{
			"repository": {"full_name": "org/repo"},
			"deployment": {"sha": "112233", "environment": "staging"},
			"deployment_status": {"environment_url": "https://staging.example.com"}
		}`)
		got := HydrateFromEvent(Request{
			EventPath:   ep,
			EventName:   "deployment_status",
			Environment: "prod-override",
		})
		if got.Environment != "prod-override" || got.EnvironmentURL != "https://staging.example.com" || got.HeadSHA != "112233" {
			t.Fatalf("unexpected hydration for deployment_status: %+v", got)
		}
	})
}

func TestBuildUserPromptIncludesEventMetadata(t *testing.T) {
	prompt := BuildUserPrompt(Request{
		EventName:      "deployment_status",
		Repo:           "org/repo",
		WorkflowRunID:  4242,
		Tag:            "v0.1.0",
		Environment:    "staging",
		EnvironmentURL: "https://staging.example.com",
		Instructions:   "Verify deployment health.",
	}, "", false)

	for _, want := range []string{
		"- Event: `deployment_status`",
		"- Workflow Run: `4242`",
		"- Tag: `v0.1.0`",
		"- Environment: `staging`",
		"- Environment URL: `https://staging.example.com`",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}
