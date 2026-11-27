package als

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"

	alsv3 "github.com/envoyproxy/go-control-plane/envoy/service/accesslog/v3"
	"github.com/prometheus/client_golang/prometheus"
)

const defaultBufferSize = 4096

// Writer consumes parsed UsageRecord entries asynchronously from the ALS stream.
type Writer interface {
	Write(ctx context.Context, rec UsageRecord) error
}

// Metrics tracks Envoy ALS ingestion and sink write outcomes.
type Metrics struct {
	Received prometheus.Counter
	Written  prometheus.Counter
	Dropped  prometheus.Counter
	Errors   prometheus.Counter
}

// NewMetrics registers ALS ingestion Prometheus counters on reg (or prometheus.DefaultRegisterer if nil).
func NewMetrics(reg prometheus.Registerer) *Metrics {
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}
	m := &Metrics{
		Received: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "usage_sink_records_received_total",
			Help: "Total parsed usage records received from Envoy ALS.",
		}),
		Written: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "usage_sink_records_written_total",
			Help: "Total usage records successfully written to configured sinks.",
		}),
		Dropped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "usage_sink_dropped_records_total",
			Help: "Total usage records dropped because the async queue was full.",
		}),
		Errors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "usage_sink_sink_errors_total",
			Help: "Total errors encountered while writing records to sinks.",
		}),
	}
	reg.MustRegister(m.Received, m.Written, m.Dropped, m.Errors)
	return m
}

// Server implements envoy.service.accesslog.v3.AccessLogService.
type Server struct {
	alsv3.UnimplementedAccessLogServiceServer

	writer        Writer
	onlyAIGateway bool
	logger        *slog.Logger
	metrics       *Metrics

	queue        chan UsageRecord
	done         chan struct{}
	workerCtx    context.Context
	workerCancel context.CancelFunc
	closeOnce    sync.Once
	mu           sync.RWMutex
	closed       bool
}

// NewServer constructs an Envoy AccessLogService gRPC handler backed by an async queue
// so slow sink operations never backpressure Envoy's gRPC stream.
func NewServer(writer Writer, onlyAIGateway bool, bufferSize int, logger *slog.Logger, metrics *Metrics) *Server {
	if bufferSize <= 0 {
		bufferSize = defaultBufferSize
	}
	if logger == nil {
		logger = slog.Default()
	}
	workerCtx, workerCancel := context.WithCancel(context.Background())
	s := &Server{
		writer:        writer,
		onlyAIGateway: onlyAIGateway,
		logger:        logger,
		metrics:       metrics,
		queue:         make(chan UsageRecord, bufferSize),
		done:          make(chan struct{}),
		workerCtx:     workerCtx,
		workerCancel:  workerCancel,
	}
	go s.runWorker()
	return s
}

func (s *Server) runWorker() {
	defer close(s.done)
	for rec := range s.queue {
		if s.writer == nil {
			continue
		}
		if err := s.writer.Write(s.workerCtx, rec); err != nil {
			s.logger.Warn("failed to write usage record", slog.String("response_id", rec.ResponseID), slog.Any("error", err))
			if s.metrics != nil {
				s.metrics.Errors.Inc()
			}
			continue
		}
		if s.metrics != nil {
			s.metrics.Written.Inc()
		}
	}
}

// StreamAccessLogs receives Envoy HTTP access log batches and enqueues parsed usage records.
func (s *Server) StreamAccessLogs(stream alsv3.AccessLogService_StreamAccessLogsServer) error {
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return stream.SendAndClose(&alsv3.StreamAccessLogsResponse{})
		}
		if err != nil {
			return err
		}
		httpLogs := msg.GetHttpLogs()
		if httpLogs == nil {
			continue
		}
		for _, entry := range httpLogs.GetLogEntry() {
			rec, ok := ParseHTTPEntry(entry, s.onlyAIGateway)
			if !ok {
				continue
			}
			if s.metrics != nil {
				s.metrics.Received.Inc()
			}
			s.enqueue(rec)
		}
	}
}

func (s *Server) enqueue(rec UsageRecord) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		if s.metrics != nil {
			s.metrics.Dropped.Inc()
		}
		return
	}
	select {
	case s.queue <- rec:
	default:
		if s.metrics != nil {
			s.metrics.Dropped.Inc()
		}
		s.logger.Warn("usage sink queue full; dropping record", slog.String("response_id", rec.ResponseID))
	}
}

// Close closes the async queue and waits for queued records to drain or ctx to expire.
func (s *Server) Close(ctx context.Context) error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		close(s.queue)
		s.mu.Unlock()
	})
	select {
	case <-s.done:
		s.workerCancel()
		return nil
	case <-ctx.Done():
		s.workerCancel()
		return ctx.Err()
	}
}
