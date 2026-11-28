package review

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wrossmorrow/github-agent-runner/api/v1alpha1"
)

func TestDefaultReviewerSystemPromptVerdictRules(t *testing.T) {
	for _, want := range []string{
		"APPROVE",
		"REQUEST_CHANGES",
		"COMMENT",
		"zero error findings and zero warning findings",
		"only info notes",
		"at least one error finding",
		"warning-only findings",
	} {
		if !strings.Contains(DefaultReviewerSystemPrompt, want) {
			t.Errorf("DefaultReviewerSystemPrompt missing verdict rule %q:\n%s", want, DefaultReviewerSystemPrompt)
		}
	}
}

func TestDefaultSuggestSystemPromptVerdictRules(t *testing.T) {
	for _, want := range []string{
		"APPROVE",
		"REQUEST_CHANGES",
		"COMMENT",
		"zero error findings and zero warning findings",
		"only info notes",
		"at least one error finding",
		"warning-only findings",
	} {
		if !strings.Contains(DefaultSuggestSystemPrompt, want) {
			t.Errorf("DefaultSuggestSystemPrompt missing verdict rule %q:\n%s", want, DefaultSuggestSystemPrompt)
		}
	}
}

func TestDefaultSuggestSystemPromptSuggestionDiscipline(t *testing.T) {
	for _, want := range []string{
		"rendered verbatim inside a GitHub \"suggestion\" code block",
		"exact, drop-in replacement lines with matching indentation",
		"never English prose",
		"OMIT \"suggestion\" entirely",
		"not a verbatim code edit",
		`"suggestion": "optional exact replacement code rendered verbatim (never prose; omit if not a verbatim code edit)"`,
	} {
		if !strings.Contains(DefaultSuggestSystemPrompt, want) {
			t.Errorf("DefaultSuggestSystemPrompt missing suggestion discipline %q:\n%s", want, DefaultSuggestSystemPrompt)
		}
	}
}

func TestFormatSuggestNudgePromptSuggestionDiscipline(t *testing.T) {
	for _, want := range []string{
		"rendered verbatim inside a GitHub \"suggestion\" code block",
		"exact, drop-in replacement lines with matching indentation",
		"never English prose",
		"omit \"suggestion\" entirely",
		"not a verbatim code edit",
		"optional exact replacement code rendered verbatim",
	} {
		if !strings.Contains(formatSuggestNudgePrompt, want) {
			t.Errorf("formatSuggestNudgePrompt missing suggestion discipline %q:\n%s", want, formatSuggestNudgePrompt)
		}
	}
	// The non-suggest nudge prompt must not carry the suggest-only discipline.
	if strings.Contains(formatNudgePrompt, "drop-in replacement lines") {
		t.Errorf("formatNudgePrompt should not enforce suggestion discipline:\n%s", formatNudgePrompt)
	}
}

func TestFormatNudgePromptsVerdictRules(t *testing.T) {
	for name, prompt := range map[string]string{
		"formatNudgePrompt":        formatNudgePrompt,
		"formatSuggestNudgePrompt": formatSuggestNudgePrompt,
	} {
		for _, want := range []string{
			"APPROVE",
			"REQUEST_CHANGES",
			"COMMENT",
			"zero error and zero warning findings",
			"info-only",
			"at least one error finding",
			"warning-only",
		} {
			if !strings.Contains(prompt, want) {
				t.Errorf("%s missing verdict rule %q:\n%s", name, want, prompt)
			}
		}
	}
}

func TestMapEventApprove(t *testing.T) {
	cases := map[string]string{
		"APPROVE":         "APPROVE",
		"approve":         "APPROVE",
		"  APPROVE  ":     "APPROVE",
		"REQUEST_CHANGES": "REQUEST_CHANGES",
		"request_changes": "REQUEST_CHANGES",
		"COMMENT":         "COMMENT",
		"":                "COMMENT",
		"not-a-verdict":   "COMMENT",
	}
	for verdict, want := range cases {
		if got := mapEvent(verdict); got != want {
			t.Errorf("mapEvent(%q) = %q, want %q", verdict, got, want)
		}
	}
}

func TestExtractReviewResultApprove(t *testing.T) {
	text := "Some preamble\n```json\n{\"verdict\":\"APPROVE\",\"summary\":\"Clean\",\"comments\":[{\"path\":\"a.go\",\"line\":3,\"severity\":\"info\",\"body\":\"nit\"}]}\n```"
	res, err := ExtractReviewResult(text)
	if err != nil {
		t.Fatalf("ExtractReviewResult: %v", err)
	}
	if res.Verdict != "APPROVE" {
		t.Fatalf("verdict = %q, want APPROVE", res.Verdict)
	}
	if len(res.Comments) != 1 || res.Comments[0].Severity != "info" {
		t.Fatalf("comments = %+v, want single info comment", res.Comments)
	}
	if got := mapEvent(res.Verdict); got != "APPROVE" {
		t.Fatalf("mapEvent(extracted verdict) = %q, want APPROVE", got)
	}
}

func writeAgentFragment(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAssembleSystemPromptNoFragmentsUsesDefault(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	cases := []struct {
		name   string
		mode   string
		output string
		want   string
	}{
		{"reviewer", v1alpha1.AgentModeReadOnly, OutputPRReview, DefaultReviewerSystemPrompt},
		{"suggest", v1alpha1.AgentModeSuggest, OutputPRReview, DefaultSuggestSystemPrompt},
		{"write-branch", v1alpha1.AgentModeWriteBranch, OutputBranchCommit, DefaultWriteBranchSystemPrompt},
		{"release draft", v1alpha1.AgentModeReadOnly, OutputReleaseDraft, DefaultReleaseNotesSystemPrompt},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := AssembleSystemPrompt([]string{missing, ""}, tc.mode, tc.output)
			if err != nil {
				t.Fatalf("AssembleSystemPrompt: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want built-in default %q", got, tc.want)
			}
		})
	}
}

func TestAssembleSystemPromptPrependsDefaultBeforeNonBaseFragments(t *testing.T) {
	d1 := t.TempDir()
	d2 := t.TempDir()
	writeAgentFragment(t, d1, "15-toolchain-crd.md", "toolchain hints")
	writeAgentFragment(t, d2, "10-org-policy.md", "org policy")

	got, err := AssembleSystemPrompt([]string{d1, d2}, v1alpha1.AgentModeReadOnly, OutputPRReview)
	if err != nil {
		t.Fatalf("AssembleSystemPrompt: %v", err)
	}
	want := DefaultReviewerSystemPrompt + "\n\norg policy\n\ntoolchain hints"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestAssembleSystemPromptBaseFragmentOverridesDefault(t *testing.T) {
	d1 := t.TempDir()
	d2 := t.TempDir()
	writeAgentFragment(t, d1, "15-toolchain-crd.md", "toolchain hints")
	writeAgentFragment(t, d2, "00-base.md", "base override")

	got, err := AssembleSystemPrompt([]string{d1, d2}, v1alpha1.AgentModeReadOnly, OutputPRReview)
	if err != nil {
		t.Fatalf("AssembleSystemPrompt: %v", err)
	}
	want := "base override\n\ntoolchain hints"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if strings.Contains(got, "zero error findings and zero warning findings") {
		t.Fatalf("base fragment should not duplicate the built-in default verdict rules:\n%s", got)
	}
}

func TestAssembleSystemPromptBaseFragmentOverridesDefaultAllModes(t *testing.T) {
	dir := t.TempDir()
	writeAgentFragment(t, dir, "00-base.md", "custom base prompt")

	got, err := AssembleSystemPrompt([]string{dir}, v1alpha1.AgentModeWriteBranch, OutputBranchCommit)
	if err != nil {
		t.Fatalf("AssembleSystemPrompt: %v", err)
	}
	if got != "custom base prompt" {
		t.Fatalf("got %q, want %q", got, "custom base prompt")
	}
}

func TestAssembleSystemPromptEmptyFragmentsSkipped(t *testing.T) {
	dir := t.TempDir()
	writeAgentFragment(t, dir, "15-toolchain-crd.md", "   \n\t  ")
	writeAgentFragment(t, dir, "20-deployment.md", "deployment hints")

	got, err := AssembleSystemPrompt([]string{dir}, v1alpha1.AgentModeReadOnly, OutputPRReview)
	if err != nil {
		t.Fatalf("AssembleSystemPrompt: %v", err)
	}
	want := DefaultReviewerSystemPrompt + "\n\ndeployment hints"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
