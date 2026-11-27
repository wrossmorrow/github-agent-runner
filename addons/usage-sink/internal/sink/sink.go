package sink

import (
	"context"
	"errors"
	"io"

	"github.com/wrossmorrow/github-agent-runner/addons/usage-sink/internal/als"
)

// Sink persists or forwards parsed UsageRecord events.
type Sink interface {
	Write(ctx context.Context, rec als.UsageRecord) error
	Flush(ctx context.Context) error
	Close(ctx context.Context) error
}

// MultiSink fans writes, flushes, and closes out across multiple sinks.
type MultiSink struct {
	sinks []Sink
}

// NewMultiSink wraps zero or more sinks into a fan-out Sink.
func NewMultiSink(sinks ...Sink) *MultiSink {
	return &MultiSink{sinks: sinks}
}

func (m *MultiSink) Write(ctx context.Context, rec als.UsageRecord) error {
	var errs []error
	for _, s := range m.sinks {
		if err := s.Write(ctx, rec); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (m *MultiSink) Flush(ctx context.Context) error {
	var errs []error
	for _, s := range m.sinks {
		if err := s.Flush(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (m *MultiSink) Close(ctx context.Context) error {
	var errs []error
	for _, s := range m.sinks {
		if err := s.Close(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

var _ io.Closer = (*nopCloser)(nil)

type nopCloser struct{}

func (nopCloser) Close() error { return nil }
