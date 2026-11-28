package review

import (
	"strings"
	"testing"
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
