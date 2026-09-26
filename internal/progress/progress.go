// Package progress tracks the worker's phases and byte counters and serves
// them as JSON for the controller.
package progress

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"
)

// Phases of a population.
const (
	PhaseStarting     = "Starting"
	PhaseDiscovering  = "Discovering"
	PhaseLeasing      = "WaitingForLease"
	PhaseDownloading  = "Downloading"
	PhaseDecoding     = "VerifyingStream"
	PhaseConverting   = "Converting"
	PhaseFlushing     = "Flushing"
	PhaseVerifying    = "VerifyingDestination"
	PhaseCompleting   = "CompletingLease"
	PhaseSucceeded    = "Succeeded"
	PhaseFailed       = "Failed"
	PhaseCancelled    = "Cancelled"
	PhaseInterrupting = "Interrupting"
)

// Snapshot is a point-in-time view of progress.
type Snapshot struct {
	Phase string `json:"phase"`
	// StreamBytes counts exported stream bytes received.
	StreamBytes int64 `json:"streamBytes"`
	// GuestBytes is the guest-disk offset reached by the stream decoder.
	GuestBytes int64 `json:"guestBytes"`
	// VirtualSize is the guest disk size.
	VirtualSize int64 `json:"virtualSize"`
	// ConvertPercent is qemu-img's reported progress.
	ConvertPercent float64 `json:"convertPercent"`
	// VerifiedBytes counts destination bytes read back.
	VerifiedBytes int64 `json:"verifiedBytes"`
	// TransferPercent is GuestBytes/VirtualSize during the download.
	TransferPercent float64 `json:"transferPercent"`
	// Elapsed since the worker started.
	ElapsedSeconds float64 `json:"elapsedSeconds"`
	// ThroughputBytesPerSecond is the average download rate so far.
	ThroughputBytesPerSecond float64 `json:"throughputBytesPerSecond"`
	Message                  string  `json:"message,omitempty"`
}

// Tracker is safe for concurrent use.
type Tracker struct {
	mu            sync.Mutex
	s             Snapshot
	start         time.Time
	downloadStart time.Time
	downloadEnd   time.Time
}

// New returns a tracker in the Starting phase.
func New() *Tracker {
	return &Tracker{s: Snapshot{Phase: PhaseStarting}, start: time.Now()}
}

// SetPhase changes the phase.
func (t *Tracker) SetPhase(p string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if p == PhaseDownloading && t.downloadStart.IsZero() {
		t.downloadStart = now
	}
	if t.s.Phase == PhaseDownloading && p != PhaseDownloading {
		t.downloadEnd = now
	}
	t.s.Phase = p
}

// SetMessage records a short human-readable message (must not contain secrets).
func (t *Tracker) SetMessage(m string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.s.Message = m
}

// Download records stream and guest progress.
func (t *Tracker) Download(streamBytes, guestBytes, virtualSize int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.s.StreamBytes, t.s.GuestBytes, t.s.VirtualSize = streamBytes, guestBytes, virtualSize
}

// Convert records qemu-img progress.
func (t *Tracker) Convert(p float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.s.ConvertPercent = p
}

// Verify records read-back progress.
func (t *Tracker) Verify(done int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.s.VerifiedBytes = done
}

// LeasePercent returns the value reported to HttpNfcLeaseProgress: the
// share of the guest disk covered by the stream, capped at 99 until the
// lease is completed.
func (t *Tracker) LeasePercent() int32 {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.s.VirtualSize <= 0 {
		return 0
	}
	return int32(min(99, t.s.GuestBytes*100/t.s.VirtualSize))
}

// Snapshot returns the current state.
func (t *Tracker) Snapshot() Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.s
	now := time.Now()
	s.ElapsedSeconds = now.Sub(t.start).Seconds()
	if s.VirtualSize > 0 {
		s.TransferPercent = float64(s.GuestBytes) * 100 / float64(s.VirtualSize)
	}
	if !t.downloadStart.IsZero() {
		end := now
		if !t.downloadEnd.IsZero() {
			end = t.downloadEnd
		}
		if d := end.Sub(t.downloadStart).Seconds(); d > 0 {
			s.ThroughputBytesPerSecond = float64(s.StreamBytes) / d
		}
	}
	return s
}

// Serve exposes GET /progress and /healthz until ctx is cancelled.
func (t *Tracker) Serve(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/progress", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(t.Snapshot())
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
