package notify

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"
)

// NewUUID returns a random RFC 4122 version 4 UUID.
func NewUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // the system random source is gone; nothing sensible left to do
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Service wires reader, tracker and client together.
type Service struct {
	cfg     Config
	reader  *Reader
	client  *Client
	tracker *Tracker
	log     *slog.Logger

	lastPollOK   atomic.Int64 // unix seconds
	pollErrors   atomic.Int64
	submitErrors atomic.Int64
	eventsSent   atomic.Int64
	openProblems atomic.Int64
}

func NewService(cfg Config, log *slog.Logger) (*Service, error) {
	reader, err := OpenReader(cfg.Database, cfg.Watch)
	if err != nil {
		return nil, err
	}
	client, err := NewClient(cfg.Notifications)
	if err != nil {
		return nil, err
	}
	policy := Policy{
		Severity:         cfg.Watch.Severity,
		For:              cfg.Watch.For,
		RecoverAfter:     cfg.Watch.RecoverAfter,
		GoneAfter:        cfg.Watch.GoneAfter,
		DaemonStaleAfter: cfg.Watch.DaemonStaleAfter,
	}
	return &Service{cfg: cfg, reader: reader, client: client, tracker: NewTracker(policy, NewUUID), log: log}, nil
}

// Run blocks until ctx is cancelled.
func (s *Service) Run(ctx context.Context) error {
	defer s.reader.Close()

	// Resume from the incidents Icinga Notifications already has. Without this a restart
	// could not send recoveries for problems reported before it.
	for {
		open, err := s.client.OpenIncidents(ctx)
		if err == nil {
			for _, o := range open {
				s.tracker.Restore(o.Key, o.Severity)
			}
			s.log.Info("resumed open incidents from Icinga Notifications", "count", len(open))
			break
		}
		s.log.Error("cannot read open incidents, retrying", "error", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(5 * time.Second):
		}
	}

	if s.cfg.Listen != "" {
		go s.serve(ctx)
	}

	ticker := time.NewTicker(s.cfg.Watch.Interval)
	defer ticker.Stop()
	for {
		s.poll(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (s *Service) poll(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Watch.Interval+30*time.Second)
	defer cancel()

	snap, err := s.reader.Snapshot(ctx)
	if err != nil {
		// Without data nothing is evaluated: problems stay open, nothing recovers.
		s.pollErrors.Add(1)
		s.log.Error("cannot read Icinga for Kubernetes database", "error", err)
		return
	}
	for _, e := range s.tracker.Evaluate(snap) {
		if err := s.client.Submit(ctx, e); err != nil {
			// Not committed, so the same event (same ID) is retried on the next poll.
			s.submitErrors.Add(1)
			s.log.Error("cannot submit event, will retry", "object", e.Key.String(), "error", err)
			continue
		}
		s.tracker.Commit(e)
		s.eventsSent.Add(1)
		kind := "problem"
		if e.Severity == SevNone {
			kind = "recovery"
		}
		s.log.Info("event submitted", "type", kind, "severity", e.Severity, "object", e.Key.String(), "summary", e.Summary)
	}
	s.openProblems.Store(int64(s.tracker.OpenProblems()))
	s.lastPollOK.Store(time.Now().Unix())
}

func (s *Service) serve(ctx context.Context) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		age := time.Since(time.Unix(s.lastPollOK.Load(), 0))
		if s.lastPollOK.Load() == 0 || age > 3*s.cfg.Watch.Interval+30*time.Second {
			http.Error(w, "no successful poll recently", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprintf(w, "ikn_last_successful_poll_timestamp_seconds %d\n", s.lastPollOK.Load())
		fmt.Fprintf(w, "ikn_poll_errors_total %d\n", s.pollErrors.Load())
		fmt.Fprintf(w, "ikn_submit_errors_total %d\n", s.submitErrors.Load())
		fmt.Fprintf(w, "ikn_events_submitted_total %d\n", s.eventsSent.Load())
		fmt.Fprintf(w, "ikn_open_problems %d\n", s.openProblems.Load())
	})
	srv := &http.Server{Addr: s.cfg.Listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		s.log.Error("health endpoint failed", "error", err)
	}
}
