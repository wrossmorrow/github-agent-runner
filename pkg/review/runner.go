package review

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/wrossmorrow/github-agent-runner/api/v1alpha1"
	"github.com/wrossmorrow/github-agent-runner/pkg/pirpc"
)

const (
	defaultMaxTransportRetries = 2
	defaultRetryBackoff        = 2 * time.Second

	transientRetryPrompt = "The previous response was interrupted by a transient network/socket error. Continue where you left off and finish the review."

	formatNudgePrompt = `Your previous response did not contain a valid JSON review object.
Do not run any more tools. Respond with ONLY a single valid JSON object matching:
{"verdict":"APPROVE"|"COMMENT"|"REQUEST_CHANGES","summary":"concise summary","comments":[{"path":"relative/path","line":1,"severity":"info"|"warning"|"error","body":"comment"}]}`

	formatSuggestNudgePrompt = `Your previous response did not contain a valid JSON review object.
Do not run any more tools. Respond with ONLY a single valid JSON object matching:
{"verdict":"APPROVE"|"COMMENT"|"REQUEST_CHANGES","summary":"concise summary","comments":[{"path":"relative/path","startLine":10,"line":12,"severity":"info"|"warning"|"error","body":"comment","suggestion":"optional replacement code"}]}`
)

// Config holds runtime settings loaded by agentrunnerd from /etc/agentrunner/config.json.
type Config struct {
	PiBin               string              `json:"piBin,omitempty"`
	PiArgs              []string            `json:"piArgs,omitempty"`
	Harness             string              `json:"harness"`
	Mode                string              `json:"mode,omitempty"`
	Provider            string              `json:"provider,omitempty"`
	Model               string              `json:"model"`
	GatewayEndpoint     string              `json:"gatewayEndpoint"`
	SessionSinkEndpoint string              `json:"sessionSinkEndpoint,omitempty"`
	Limits              v1alpha1.LimitsSpec `json:"limits,omitempty"`
	AgentsDirs          []string            `json:"agentsDirs,omitempty"`
	ProtectedBranches   *[]string           `json:"protectedBranches,omitempty"`
	Attribution         map[string]string   `json:"attributionHeaders,omitempty"`
}

// Runner executes a single headless review against `pi --mode rpc`.
type Runner struct {
	Config            Config
	Poster            Poster
	RetryBackoff      time.Duration
	SessionSinkClient *http.Client
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

	req = HydrateFromEvent(req)
	if IsWriteOutput(req.Output) {
		dirty, err := listChangedPaths(ctx, req.Workspace)
		if err != nil {
			return nil, fmt.Errorf("checking workspace git status before write-branch run: %w", err)
		}
		if len(dirty) > 0 {
			return nil, fmt.Errorf("workspace has pre-existing uncommitted changes before write-branch run (%d path(s), e.g. %q); ensure the workspace is clean before invoking write outputs", len(dirty), dirty[0])
		}
	}
	if req.Output == OutputReleaseDraft && req.BaseRef == "" {
		if prev := resolvePreviousTag(ctx, req.Workspace, req.Tag); prev != "" {
			req.BaseRef = prev
		}
	}

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
		agentsMD = selectDefaultSystemPrompt(r.Config.Mode, req.Output)
	}

	diffStat := readDiffStat(ctx, req.Workspace, req.BaseRef, req.HeadSHA)
	_, err = os.Stat(filepath.Join(req.Workspace, "AGENTS.md"))
	hasRepoAgentsMD := err == nil

	userPrompt := BuildUserPrompt(req, diffStat, hasRepoAgentsMD)

	sessionID, err := resolveSessionID(req.SessionID)
	if err != nil {
		return nil, err
	}
	req.SessionID = sessionID
	sessionKind := deriveSessionKind(r.Config.Mode, req.Output)
	sessionName := deriveSessionName(req, sessionID)
	startedAt := time.Now().UTC()

	gatewayToken := strings.TrimSpace(os.Getenv("AGENT_GATEWAY_TOKEN"))
	piDir, err := writePiConfigDir(r.Config, req, agentsMD, gatewayToken, sessionID, sessionKind, sessionName)
	if err != nil {
		return &Outcome{SessionID: sessionID}, err
	}
	defer func() { _ = os.RemoveAll(piDir) }()
	sessionFile := filepath.Join(piDir, sessionID+".jsonl")

	_, _ = fmt.Fprintf(logOut, "agentrunner: session=%s provider=%s model=%s gateway=%s hasGatewayToken=%v (len=%d)\n",
		sessionID, providerName(r.Config), r.Config.Model, r.Config.GatewayEndpoint, gatewayToken != "", len(gatewayToken))

	bin := r.Config.PiBin
	if bin == "" {
		bin = "pi"
	}
	args := r.Config.PiArgs
	if len(args) == 0 {
		args = []string{"--mode", "rpc", "--session", sessionFile}
		if r.Config.GatewayEndpoint != "" && r.Config.Model != "" {
			args = append(args, "--provider", providerName(r.Config), "--model", r.Config.Model)
		}
	}

	env := buildChildEnv(r.Config, req, piDir)
	client := pirpc.NewClient(bin, args, req.Workspace, env)
	client.Stderr = logOut
	if err := client.Start(); err != nil {
		return &Outcome{SessionID: sessionID}, fmt.Errorf("starting pi rpc: %w", err)
	}

	var outcome *Outcome
	turns := 0
	defer func() {
		var stats map[string]any
		sinkEndpoint := strings.TrimSpace(r.Config.SessionSinkEndpoint)
		if sinkEndpoint != "" {
			statsCtx, statsCancel := context.WithTimeout(context.Background(), 2*time.Second)
			stats, _ = client.Stats(statsCtx)
			statsCancel()
		}
		_ = client.CloseStdin()
		_ = client.Stop()

		if sinkEndpoint != "" {
			endReason := "error"
			switch {
			case outcome != nil:
				endReason = "closed"
			case ctx.Err() == context.DeadlineExceeded:
				endReason = "timeout"
			}
			meta := SessionArchiveMetadata{
				SessionID:   sessionID,
				SessionKind: sessionKind,
				SessionName: sessionName,
				Repo:        req.Repo,
				PRNumber:    req.PRNumber,
				IssueNumber: req.IssueNumber,
				RunID:       req.RunID,
				HeadSHA:     req.HeadSHA,
				BaseRef:     req.BaseRef,
				Tag:         req.Tag,
				Provider:    providerName(r.Config),
				Model:       r.Config.Model,
				Mode:        r.Config.Mode,
				Output:      req.Output,
				Turns:       turns,
				EndReason:   endReason,
				StartedAt:   startedAt,
				EndedAt:     time.Now().UTC(),
				Stats:       stats,
			}
			if outcome != nil && outcome.Result != nil {
				meta.Verdict = outcome.Result.Verdict
				meta.Findings = len(outcome.Result.Comments)
			}
			uploadCtx, uploadCancel := context.WithTimeout(context.Background(), 15*time.Second)
			if uerr := uploadSessionArchive(uploadCtx, r.SessionSinkClient, sinkEndpoint, os.Getenv("SESSION_SINK_TOKEN"), meta, sessionFile); uerr != nil {
				_, _ = fmt.Fprintf(logOut, "agentrunner: warning: session archive upload failed: %v\n", uerr)
			}
			uploadCancel()
		}
	}()

	events, unsub := client.Subscribe()
	defer unsub()

	maxRetries := defaultMaxTransportRetries
	if r.Config.Limits.MaxTransportRetries != nil {
		maxRetries = *r.Config.Limits.MaxTransportRetries
		if maxRetries < 0 {
			maxRetries = 0
		}
	}
	backoff := r.RetryBackoff
	if backoff <= 0 {
		backoff = defaultRetryBackoff
	}

	finalText, err := sendPromptWithTransportRetries(
		ctx, client, events, userPrompt, transientRetryPrompt, agentsMD, maxRetries, backoff, r.Config.Limits.MaxTurns, &turns, logOut,
	)
	if err != nil {
		return &Outcome{SessionID: sessionID, Turns: turns}, err
	}

	var reviewRes *Result
	if req.Output == OutputReleaseDraft {
		reviewRes, err = ExtractReleaseResult(finalText, req.Tag)
	} else {
		reviewRes, err = ExtractReviewResult(finalText)
		if err != nil && ctx.Err() == nil && (r.Config.Limits.MaxTurns <= 0 || turns < r.Config.Limits.MaxTurns) {
			nudgePrompt := formatNudgePrompt
			if r.Config.Mode == v1alpha1.AgentModeSuggest || req.Output == OutputSuggest {
				nudgePrompt = formatSuggestNudgePrompt
			}
			_, _ = fmt.Fprintln(logOut, "agentrunner: assistant output missing valid review JSON; nudging once in-session...")
			nudgeText, nudgeErr := sendPromptWithTransportRetries(
				ctx, client, events, nudgePrompt, nudgePrompt, agentsMD, maxRetries, backoff, r.Config.Limits.MaxTurns, &turns, logOut,
			)
			if nudgeErr != nil {
				return &Outcome{SessionID: sessionID, Turns: turns}, nudgeErr
			}
			finalText = nudgeText
			reviewRes, err = ExtractReviewResult(finalText)
		}
	}
	if err != nil {
		preview := strings.TrimSpace(finalText)
		if preview == "" {
			preview = "<empty>"
		} else if len(preview) > 400 {
			preview = preview[:400] + "..."
		}
		_, _ = fmt.Fprintf(logOut, "pi final assistant text: %s\n", preview)
		return &Outcome{SessionID: sessionID, Turns: turns}, err
	}

	if r.Poster != nil {
		if err := r.Poster.PostReview(ctx, req, reviewRes); err != nil {
			return &Outcome{SessionID: sessionID, Turns: turns}, fmt.Errorf("posting review: %w", err)
		}
	}

	exitCode := 0
	if reviewRes.ShouldFail(req.FailOn) {
		exitCode = 1
	}

	outcome = &Outcome{
		SessionID: sessionID,
		Result:    reviewRes,
		ExitCode:  exitCode,
		Turns:     turns,
	}
	return outcome, nil
}

func selectDefaultSystemPrompt(mode, output string) string {
	switch {
	case output == OutputReleaseDraft:
		return DefaultReleaseNotesSystemPrompt
	case IsWriteOutput(output):
		return DefaultWriteBranchSystemPrompt
	case mode == v1alpha1.AgentModeSuggest || output == OutputSuggest:
		return DefaultSuggestSystemPrompt
	default:
		return DefaultReviewerSystemPrompt
	}
}

func sendPromptWithTransportRetries(
	ctx context.Context,
	client *pirpc.Client,
	events <-chan pirpc.Event,
	initialMsg string,
	retryMsg string,
	systemPrompt string,
	maxRetries int,
	backoff time.Duration,
	maxTurns int,
	turns *int,
	logOut io.Writer,
) (string, error) {
	skipSend := false
	for attempt := 0; ; attempt++ {
		if !skipSend {
			msg := initialMsg
			if attempt > 0 {
				msg = retryMsg
			}
			promptCmd := map[string]any{
				"type":         "prompt",
				"message":      msg,
				"systemPrompt": systemPrompt,
			}

			res, err := client.Send(ctx, promptCmd)
			if err != nil {
				return "", fmt.Errorf("sending prompt: %w", err)
			}
			if !res.Success && strings.Contains(strings.ToLower(res.Error), "already processing") {
				_, _ = fmt.Fprintln(logOut, "agentrunner: pi still processing; queuing prompt with streamingBehavior=followUp...")
				promptCmd["streamingBehavior"] = "followUp"
				res, err = client.Send(ctx, promptCmd)
				if err != nil {
					return "", fmt.Errorf("sending followUp prompt: %w", err)
				}
			}
			if !res.Success {
				return "", fmt.Errorf("prompt rejected: %s", res.Error)
			}
		}
		skipSend = false

		remainingTurns := 0
		if maxTurns > 0 {
			remainingTurns = maxTurns - *turns
			if remainingTurns <= 0 {
				remainingTurns = 1
			}
		}

		attemptText, attemptTurns, err := streamUntilEnd(ctx, events, remainingTurns, client, logOut)
		*turns += attemptTurns
		if err == nil {
			return attemptText, nil
		}

		canRetry := attempt < maxRetries &&
			ctx.Err() == nil &&
			isTransientTransportError(err) &&
			(maxTurns <= 0 || *turns < maxTurns)
		if !canRetry {
			return "", err
		}

		delay := backoff * time.Duration(attempt+1)
		_, _ = fmt.Fprintf(logOut, "transient model transport error (%v); retrying in-session (%d/%d) after %s...\n", err, attempt+1, maxRetries, delay)
		piActive, waitErr := waitForBackoffOrActiveTurn(ctx, events, delay, turns, logOut)
		if waitErr != nil {
			return "", waitErr
		}
		skipSend = piActive
	}
}

func waitForBackoffOrActiveTurn(
	ctx context.Context,
	events <-chan pirpc.Event,
	delay time.Duration,
	turns *int,
	logOut io.Writer,
) (bool, error) {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-timer.C:
			return false, nil
		case ev, ok := <-events:
			if !ok {
				return false, fmt.Errorf("pi rpc event stream closed unexpectedly")
			}
			evType, _ := ev["type"].(string)
			switch evType {
			case "auto_retry_start", "agent_start":
				_, _ = fmt.Fprintf(logOut, "agentrunner: pi became active during backoff (%s); awaiting active turn...\n", evType)
				return true, nil
			case "turn_start":
				*turns++
				_, _ = fmt.Fprintln(logOut, "agentrunner: pi started next turn during backoff; awaiting active turn...")
				return true, nil
			case "session_closed":
				return false, fmt.Errorf("pi session closed during retry backoff (%v)", ev["endReason"])
			}
		}
	}
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

func isTransientTransportError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if !strings.HasPrefix(msg, "pi model error:") {
		return false
	}
	for _, marker := range []string{
		"und_err_socket",
		"econnreset",
		"etimedout",
		"socket hang up",
		"other side closed",
		"fetch failed",
		"terminated",
		"connection reset",
		"unexpected eof",
		"broken pipe",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
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

func resolvePreviousTag(ctx context.Context, workspace, currentTag string) string {
	if strings.TrimSpace(workspace) == "" {
		return ""
	}
	candidates := []string{"HEAD^"}
	trimmedTag := strings.TrimSpace(currentTag)
	if trimmedTag != "" && !strings.HasPrefix(trimmedTag, "-") && !strings.ContainsAny(trimmedTag, " \t\n\r") {
		candidates = []string{trimmedTag + "^", "HEAD^"}
	}
	for _, ref := range candidates {
		cmd := exec.CommandContext(ctx, "git", "-C", workspace, "describe", "--tags", "--abbrev=0", ref)
		if out, err := cmd.Output(); err == nil {
			if prev := strings.TrimSpace(string(out)); prev != "" {
				return prev
			}
		}
	}
	return ""
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
	var env []string
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "SESSION_SINK_TOKEN=") || strings.HasPrefix(entry, "AGENT_GATEWAY_TOKEN=") {
			continue
		}
		env = append(env, entry)
	}
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
