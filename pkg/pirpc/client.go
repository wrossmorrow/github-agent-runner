// Package pirpc implements a JSONL client for a `pi --mode rpc` subprocess.
package pirpc

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os/exec"
	"sync"
	"time"
)

const (
	subscriberBuffer = 1024
	maxLineBytes     = 64 * 1024 * 1024
)

// CommandResult is the correlated response to a single RPC command.
type CommandResult struct {
	Success bool            `json:"success"`
	Error   string          `json:"error,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Event is one session event emitted by pi on stdout.
type Event = map[string]any

// Client manages a `pi --mode rpc` child process and multiplexes command
// responses and streaming events.
type Client struct {
	bin  string
	args []string
	dir  string
	env  []string

	writeMu sync.Mutex
	stdin   io.WriteCloser
	cmd     *exec.Cmd

	mu   sync.Mutex
	dead bool

	pendingMu sync.Mutex
	pending   map[string]chan CommandResult
	seq       int64

	subsMu sync.Mutex
	subs   map[chan Event]struct{}

	exitCh chan error
}

// NewClient constructs a Client for the given binary, arguments, working directory,
// and environment. If dir is empty, the current working directory is inherited.
func NewClient(bin string, args []string, dir string, env []string) *Client {
	return &Client{
		bin:     bin,
		args:    args,
		dir:     dir,
		env:     env,
		pending: map[string]chan CommandResult{},
		subs:    map[chan Event]struct{}{},
		exitCh:  make(chan error, 1),
	}
}

// Start launches the RPC subprocess and begins consuming stdout/stderr.
func (c *Client) Start() error {
	cmd := exec.Command(c.bin, c.args...)
	if c.dir != "" {
		cmd.Dir = c.dir
	}
	if len(c.env) > 0 {
		cmd.Env = c.env
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}

	c.cmd = cmd
	c.stdin = stdin

	go func() {
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			log.Printf("pi stderr: %s", sc.Text())
		}
	}()

	go func() {
		c.readLoop(stdout)
		err := cmd.Wait()
		c.handleExit(err)
		c.exitCh <- err
	}()

	return nil
}

// Wait blocks until the child process exits and returns its Wait error.
func (c *Client) Wait() error {
	return <-c.exitCh
}

// Stop terminates the child process immediately.
func (c *Client) Stop() error {
	if c.cmd == nil || c.cmd.Process == nil {
		return nil
	}
	return c.cmd.Process.Kill()
}

// CloseStdin closes the child's stdin pipe to signal EOF.
func (c *Client) CloseStdin() error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.stdin == nil {
		return nil
	}
	err := c.stdin.Close()
	c.stdin = nil
	return err
}

func (c *Client) isDead() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dead
}

func (c *Client) handleExit(err error) {
	c.mu.Lock()
	c.dead = true
	c.mu.Unlock()

	msg := "pi rpc process exited"
	if err != nil {
		msg += ": " + err.Error()
	}

	c.pendingMu.Lock()
	pending := c.pending
	c.pending = map[string]chan CommandResult{}
	c.pendingMu.Unlock()
	for _, ch := range pending {
		ch <- CommandResult{Success: false, Error: msg}
	}

	endReason := "closed"
	if err != nil {
		endReason = "crashed"
	}
	c.broadcast(Event{"type": "session_closed", "endReason": endReason})

	c.subsMu.Lock()
	for ch := range c.subs {
		delete(c.subs, ch)
		close(ch)
	}
	c.subsMu.Unlock()
}

func (c *Client) readLoop(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), maxLineBytes)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var raw struct {
			Type    string          `json:"type"`
			ID      string          `json:"id"`
			Success bool            `json:"success"`
			Error   string          `json:"error"`
			Data    json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(line, &raw); err != nil {
			continue
		}
		if raw.Type == "response" {
			c.resolve(raw.ID, CommandResult{
				Success: raw.Success,
				Error:   raw.Error,
				Data:    raw.Data,
			})
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		c.broadcast(slimEvent(ev))
	}
	if err := sc.Err(); err != nil {
		log.Printf("pi rpc read loop error: %v", err)
	}
}

func (c *Client) resolve(id string, res CommandResult) {
	c.pendingMu.Lock()
	ch, ok := c.pending[id]
	delete(c.pending, id)
	c.pendingMu.Unlock()
	if ok {
		ch <- res
	}
}

// slimEvent removes cumulative partial snapshots from message_update events to
// avoid O(n^2) memory growth on long streaming outputs.
func slimEvent(ev Event) Event {
	if ev["type"] != "message_update" {
		return ev
	}
	delete(ev, "message")
	ame, ok := ev["assistantMessageEvent"].(map[string]any)
	if !ok {
		return ev
	}
	if partial, ok := ame["partial"].(map[string]any); ok {
		if idx, ok := ame["contentIndex"].(float64); ok {
			if content, ok := partial["content"].([]any); ok && int(idx) >= 0 && int(idx) < len(content) {
				if block, ok := content[int(idx)].(map[string]any); ok && block["type"] == "toolCall" {
					ame["block"] = block
				}
			}
		}
		delete(ame, "partial")
	}
	return ev
}

func (c *Client) broadcast(ev Event) {
	c.subsMu.Lock()
	defer c.subsMu.Unlock()
	for ch := range c.subs {
		select {
		case ch <- ev:
		default:
			delete(c.subs, ch)
			close(ch)
		}
	}
}

// Subscribe registers a subscriber channel for future RPC events.
func (c *Client) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, subscriberBuffer)
	c.subsMu.Lock()
	if c.isDead() {
		close(ch)
	} else {
		c.subs[ch] = struct{}{}
	}
	c.subsMu.Unlock()
	return ch, func() {
		c.subsMu.Lock()
		if _, ok := c.subs[ch]; ok {
			delete(c.subs, ch)
			close(ch)
		}
		c.subsMu.Unlock()
	}
}

// Send writes a JSONL command with a generated correlation id and waits for its response.
func (c *Client) Send(ctx context.Context, command map[string]any) (*CommandResult, error) {
	if c.isDead() {
		return nil, fmt.Errorf("pi rpc process is not running")
	}

	c.pendingMu.Lock()
	c.seq++
	id := fmt.Sprintf("ar_%d", c.seq)
	ch := make(chan CommandResult, 1)
	c.pending[id] = ch
	c.pendingMu.Unlock()

	full := make(map[string]any, len(command)+1)
	for k, v := range command {
		full[k] = v
	}
	full["id"] = id

	if err := c.writeJSON(full); err != nil {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, err
	}

	select {
	case res := <-ch:
		return &res, nil
	case <-ctx.Done():
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, ctx.Err()
	}
}

// State queries pi's current session state via `get_state`.
func (c *Client) State(ctx context.Context) (map[string]any, error) {
	return c.sendMapCommand(ctx, "get_state")
}

// Stats queries pi's token/message statistics via `get_session_stats`.
func (c *Client) Stats(ctx context.Context) (map[string]any, error) {
	return c.sendMapCommand(ctx, "get_session_stats")
}

func (c *Client) sendMapCommand(ctx context.Context, cmdType string) (map[string]any, error) {
	ctx, cancel := withDefaultTimeout(ctx, 30*time.Second)
	defer cancel()

	res, err := c.Send(ctx, map[string]any{"type": cmdType})
	if err != nil {
		return nil, err
	}
	if !res.Success {
		return nil, fmt.Errorf("%s failed: %s", cmdType, res.Error)
	}
	var data map[string]any
	if len(res.Data) > 0 {
		if err := json.Unmarshal(res.Data, &data); err != nil {
			return nil, fmt.Errorf("%s: bad data: %w", cmdType, err)
		}
	}
	return data, nil
}

func withDefaultTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, d)
}

func (c *Client) writeJSON(v any) error {
	line, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.stdin == nil {
		return fmt.Errorf("pi rpc stdin is not open")
	}
	if _, err := c.stdin.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("writing to pi rpc stdin: %w", err)
	}
	return nil
}
