package sink

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"sync"

	"github.com/wrossmorrow/github-agent-runner/addons/usage-sink/internal/als"
)

// JSONLSink writes one JSON line per UsageRecord to w.
type JSONLSink struct {
	mu  sync.Mutex
	enc *json.Encoder
}

// NewJSONLSink constructs a JSONLSink writing to w (defaults to os.Stdout when nil).
func NewJSONLSink(w io.Writer) *JSONLSink {
	if w == nil {
		w = os.Stdout
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return &JSONLSink{enc: enc}
}

func (s *JSONLSink) Write(_ context.Context, rec als.UsageRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enc.Encode(rec)
}

func (s *JSONLSink) Flush(context.Context) error { return nil }

func (s *JSONLSink) Close(context.Context) error { return nil }
