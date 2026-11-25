package review

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/wrossmorrow/agentrunner/api/v1alpha1"
	"github.com/wrossmorrow/agentrunner/pkg/pirpc"
)

// Config holds runtime settings loaded by agentrunnerd from /etc/agentrunner/config.json.
type Config struct {
	PiBin           string              `json:"piBin,omitempty"`
	PiArgs          []string            `json:"piArgs,omitempty"`
	Harness         string              `json:"harness"`
	Provider        string              `json:"provider,omitempty"`
	Model           string              `json:"model"`
	GatewayEndpoint string              `json:"gatewayEndpoint"`
	Limits          v1alpha1.LimitsSpec `json:"limits,omitempty"`
	AgentsDirs      []string            `json:"agentsDirs,omitempty"`
}

// Runner executes a single headless review against `pi --mode rpc`.
type Runner struct {
	Config Config
	Poster Poster
}

// Run spawns `pi`, executes the review prompt, streams GitHub Actions log
// groups to logOut, optionally posts to GitHub, and returns the Outcome.
func (r *Runner) Run(ctx context.Context, req Request, logOut io.Writer) (*Outcome, error) {
	if logOut == nil {
		logOut = io.Discard
	}
	if strings.TrimSpace(req.Instructions) == "" {
		return nil, fmt.Errorf("instructions are required")
	}

	req = enrichFromEvent(req)

	var cancel context.CancelFunc
	if dur, err := time.ParseDuration(r.Config.Limits.MaxDuration); err == nil && dur > 0 {
		ctx, cancel = context.WithTimeout(ctx, dur)
		defer cancel()
	}

	agentsMD, err := AssembleAgentsMD(r.Config.AgentsDirs...)
	if err != nil {
		return nil, err
	}
	if agentsMD == "" {
		agentsMD = DefaultReviewerSystemPrompt
	}

	diffStat := readDiffStat(ctx, req.Workspace, req.BaseRef, req.HeadSHA)
	_, err = os.Stat(filepath.Join(req.Workspace, "AGENTS.md"))
	hasRepoAgentsMD := err == nil

	userPrompt := BuildUserPrompt(req, diffStat, hasRepoAgentsMD)

	gatewayToken := strings.TrimSpace(os.Getenv("AGENT_GATEWAY_TOKEN"))
	piDir, err := writePiConfigDir(r.Config, req, agentsMD, gatewayToken)
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(piDir) }()

	_, _ = fmt.Fprintf(logOut, "agentrunner: provider=%s model=%s gateway=%s hasGatewayToken=%v (len=%d)\n",
		providerName(r.Config), r.Config.Model, r.Config.GatewayEndpoint, gatewayToken != "", len(gatewayToken))

	bin := r.Config.PiBin
	if bin == "" {
		bin = "pi"
	}
	args := r.Config.PiArgs
	if len(args) == 0 {
		args = []string{"--mode", "rpc"}
		if r.Config.GatewayEndpoint != "" && r.Config.Model != "" {
			args = append(args, "--provider", providerName(r.Config), "--model", r.Config.Model)
		}
	}

	env := buildChildEnv(r.Config, req, piDir)
	client := pirpc.NewClient(bin, args, req.Workspace, env)
	client.Stderr = logOut
	if err := client.Start(); err != nil {
		return nil, fmt.Errorf("starting pi rpc: %w", err)
	}
	defer func() {
		_ = client.CloseStdin()
		_ = client.Stop()
	}()

	events, unsub := client.Subscribe()
	defer unsub()

	promptCmd := map[string]any{
		"type":         "prompt",
		"message":      userPrompt,
		"systemPrompt": agentsMD,
	}
	res, err := client.Send(ctx, promptCmd)
	if err != nil {
		return nil, fmt.Errorf("sending prompt: %w", err)
	}
	if !res.Success {
		return nil, fmt.Errorf("prompt rejected: %s", res.Error)
	}

	finalText, turns, err := streamUntilEnd(ctx, events, r.Config.Limits.MaxTurns, client, logOut)
	if err != nil {
		return nil, err
	}

	reviewRes, err := ExtractReviewResult(finalText)
	if err != nil {
		preview := strings.TrimSpace(finalText)
		if preview == "" {
			preview = "<empty>"
		} else if len(preview) > 400 {
			preview = preview[:400] + "..."
		}
		_, _ = fmt.Fprintf(logOut, "pi final assistant text: %s\n", preview)
		return nil, err
	}

	if r.Poster != nil {
		if err := r.Poster.PostReview(ctx, req, reviewRes); err != nil {
			return nil, fmt.Errorf("posting review: %w", err)
		}
	}

	exitCode := 0
	if reviewRes.ShouldFail(req.FailOn) {
		exitCode = 1
	}

	return &Outcome{
		Result:   reviewRes,
		ExitCode: exitCode,
		Turns:    turns,
	}, nil
}

func streamUntilEnd(
	ctx context.Context,
	events <-chan pirpc.Event,
	maxTurns int,
	client *pirpc.Client,
	logOut io.Writer,
) (string, int, error) {
	turns := 0
	var lastAssistantText string
	var lastAssistantErr string
	var inGroup bool
	closeGroup := func() {
		if inGroup {
			_, _ = fmt.Fprintln(logOut, "::endgroup::")
			inGroup = false
		}
	}
	defer closeGroup()

	for {
		select {
		case <-ctx.Done():
			return "", turns, fmt.Errorf("review timed out or cancelled: %w", ctx.Err())
		case ev, ok := <-events:
			if !ok {
				return "", turns, fmt.Errorf("pi rpc event stream closed unexpectedly")
			}
			evType, _ := ev["type"].(string)
			switch evType {
			case "turn_start":
				turns++
				if maxTurns > 0 && turns > maxTurns {
					_ = client.Stop()
					return "", turns, fmt.Errorf("exceeded maxTurns (%d)", maxTurns)
				}
			case "tool_execution_start":
				closeGroup()
				toolName, _ := ev["toolName"].(string)
				_, _ = fmt.Fprintf(logOut, "::group::Tool: %s\n", toolName)
				inGroup = true
			case "tool_execution_end":
				if out, ok := ev["result"]; ok {
					b, _ := json.Marshal(out)
					_, _ = fmt.Fprintln(logOut, string(b))
				}
				closeGroup()
			case "message_end":
				text, errMsg := extractAssistantOutcome(ev["message"])
				if text != "" {
					lastAssistantText = text
				}
				if errMsg != "" {
					lastAssistantErr = errMsg
				}
			case "agent_end":
				closeGroup()
				text, errMsg := extractLastOutcomeFromAgentEnd(ev)
				if text != "" {
					lastAssistantText = text
				}
				if errMsg != "" {
					lastAssistantErr = errMsg
				}
				if lastAssistantErr != "" && lastAssistantText == "" {
					return "", turns, fmt.Errorf("pi model error: %s", lastAssistantErr)
				}
				return lastAssistantText, turns, nil
			case "session_closed":
				return "", turns, fmt.Errorf("pi session closed before agent_end (%v)", ev["endReason"])
			}
		}
	}
}

func extractLastOutcomeFromAgentEnd(ev pirpc.Event) (string, string) {
	msgs, ok := ev["messages"].([]any)
	if !ok {
		return "", ""
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		text, errMsg := extractAssistantOutcome(msgs[i])
		if text != "" || errMsg != "" {
			return text, errMsg
		}
	}
	return "", ""
}

func extractAssistantOutcome(msgAny any) (string, string) {
	msg, ok := msgAny.(map[string]any)
	if !ok {
		return "", ""
	}
	if role, _ := msg["role"].(string); role != "" && role != "assistant" {
		return "", ""
	}
	var errMsg string
	if stopReason, _ := msg["stopReason"].(string); stopReason == "error" {
		errMsg, _ = msg["errorMessage"].(string)
		if errMsg == "" {
			errMsg = "assistant stopped with error"
		}
	} else if em, _ := msg["errorMessage"].(string); em != "" {
		errMsg = em
	}

	switch c := msg["content"].(type) {
	case string:
		return c, errMsg
	case []any:
		var parts []string
		for _, item := range c {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if block["type"] == "text" {
				if t, ok := block["text"].(string); ok {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n"), errMsg
	default:
		return "", errMsg
	}
}

func enrichFromEvent(req Request) Request {
	if req.EventPath == "" {
		return req
	}
	raw, err := os.ReadFile(req.EventPath)
	if err != nil {
		return req
	}
	var payload struct {
		PullRequest struct {
			Number int `json:"number"`
			Base   struct {
				Ref string `json:"ref"`
			} `json:"base"`
			Head struct {
				SHA string `json:"sha"`
			} `json:"head"`
		} `json:"pull_request"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return req
	}
	if req.PRNumber == 0 {
		req.PRNumber = payload.PullRequest.Number
	}
	if req.BaseRef == "" {
		req.BaseRef = payload.PullRequest.Base.Ref
	}
	if req.HeadSHA == "" {
		req.HeadSHA = payload.PullRequest.Head.SHA
	}
	if req.Repo == "" {
		req.Repo = payload.Repository.FullName
	}
	return req
}

func readDiffStat(ctx context.Context, workspace, baseRef, headSHA string) string {
	if workspace == "" || baseRef == "" {
		return ""
	}
	target := "HEAD"
	if headSHA != "" {
		target = headSHA
	}
	cmd := exec.CommandContext(ctx, "git", "-C", workspace, "diff", "--stat", baseRef+"..."+target)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return ""
	}
	return out.String()
}

func buildChildEnv(cfg Config, req Request, piDir string) []string {
	env := os.Environ()
	add := func(k, v string) {
		if v != "" {
			env = append(env, k+"="+v)
		}
	}
	add("PI_CODING_AGENT_DIR", piDir)
	add("PI_SKIP_VERSION_CHECK", "1")
	add("PI_MODEL", cfg.Model)
	add("PI_PROVIDER", cfg.Provider)
	add("AGENT_GATEWAY_ENDPOINT", cfg.GatewayEndpoint)
	add("AGENT_GITHUB_REPO", req.Repo)
	add("AGENT_GITHUB_RUN_ID", req.RunID)
	if req.PRNumber > 0 {
		add("AGENT_GITHUB_PR", fmt.Sprintf("%d", req.PRNumber))
	}
	return env
}
