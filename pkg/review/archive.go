package review

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"strings"
	"time"
)

// SessionArchiveMetadata captures generic run metadata sent alongside a compressed
// transcript to an optional HTTP sessionSink endpoint.
type SessionArchiveMetadata struct {
	SessionID   string         `json:"session_id"`
	SessionKind string         `json:"session_kind"`
	SessionName string         `json:"session_name"`
	Repo        string         `json:"repo,omitempty"`
	PRNumber    int            `json:"pr_number,omitempty"`
	IssueNumber int            `json:"issue_number,omitempty"`
	RunID       string         `json:"run_id,omitempty"`
	HeadSHA     string         `json:"head_sha,omitempty"`
	BaseRef     string         `json:"base_ref,omitempty"`
	Tag         string         `json:"tag,omitempty"`
	Provider    string         `json:"provider,omitempty"`
	Model       string         `json:"model,omitempty"`
	Mode        string         `json:"mode,omitempty"`
	Output      string         `json:"output,omitempty"`
	Verdict     string         `json:"verdict,omitempty"`
	Findings    int            `json:"findings"`
	Turns       int            `json:"turns"`
	EndReason   string         `json:"end_reason"`
	StartedAt   time.Time      `json:"started_at"`
	EndedAt     time.Time      `json:"ended_at"`
	Stats       map[string]any `json:"stats,omitempty"`
}

func generateUUIDv4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		now := time.Now().UnixNano()
		return fmt.Sprintf("ar-%x", now)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func deriveSessionKind(mode, output string) string {
	switch {
	case output == OutputReleaseDraft:
		return "release-draft"
	case IsWriteOutput(output):
		return "write-branch"
	case mode == "suggest" || output == OutputSuggest:
		return "suggest"
	default:
		return "review"
	}
}

func deriveSessionName(req Request, sessionID string) string {
	repo := strings.TrimSpace(req.Repo)
	switch {
	case repo != "" && req.PRNumber > 0:
		return fmt.Sprintf("%s#%d", repo, req.PRNumber)
	case repo != "" && req.IssueNumber > 0:
		return fmt.Sprintf("%s#%d", repo, req.IssueNumber)
	case repo != "" && strings.TrimSpace(req.Tag) != "":
		return fmt.Sprintf("%s@%s", repo, strings.TrimSpace(req.Tag))
	case repo != "" && strings.TrimSpace(req.RunID) != "":
		return fmt.Sprintf("%s@%s", repo, strings.TrimSpace(req.RunID))
	case repo != "":
		return repo
	default:
		return sessionID
	}
}

// uploadSessionArchive sends a fail-open multipart/form-data POST containing `metadata`
// (metadata.json) and, if present, `transcript` (transcript.jsonl.gz).
func uploadSessionArchive(ctx context.Context, client *http.Client, endpoint, token string, meta SessionArchiveMetadata, sessionFile string) error {
	metaBytes, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal session metadata: %w", err)
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)

	metaHeader := make(textproto.MIMEHeader)
	metaHeader.Set("Content-Disposition", `form-data; name="metadata"; filename="metadata.json"`)
	metaHeader.Set("Content-Type", "application/json")
	metaPart, err := mw.CreatePart(metaHeader)
	if err != nil {
		return err
	}
	if _, err := metaPart.Write(metaBytes); err != nil {
		return err
	}

	if sessionFile != "" {
		if raw, err := os.ReadFile(sessionFile); err == nil && len(raw) > 0 {
			var gzBuf bytes.Buffer
			gz := gzip.NewWriter(&gzBuf)
			if _, err := gz.Write(raw); err == nil {
				if err := gz.Close(); err == nil {
					trHeader := make(textproto.MIMEHeader)
					trHeader.Set("Content-Disposition", `form-data; name="transcript"; filename="transcript.jsonl.gz"`)
					trHeader.Set("Content-Type", "application/gzip")
					trPart, err := mw.CreatePart(trHeader)
					if err != nil {
						return err
					}
					if _, err := trPart.Write(gzBuf.Bytes()); err != nil {
						return err
					}
				}
			}
		}
	}

	if err := mw.Close(); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if meta.SessionID != "" {
		req.Header.Set("X-Session-Id", meta.SessionID)
	}
	if tok := strings.TrimSpace(token); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("sessionSink returned HTTP %d", resp.StatusCode)
	}
	return nil
}
