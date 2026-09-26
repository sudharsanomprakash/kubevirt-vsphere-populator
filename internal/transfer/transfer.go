// Package transfer downloads an export stream with strict response checks,
// an idle timeout and a bounded local staging file.
package transfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Errors returned by this package.
var (
	ErrIdle        = errors.New("no data received within the idle timeout")
	ErrStatus      = errors.New("unexpected HTTP status")
	ErrStagingFull = errors.New("staging capacity exceeded")
	ErrShortBody   = errors.New("response body shorter than Content-Length")
)

// StatusError reports a non-200 response.
type StatusError struct {
	Code   int
	Status string
}

func (e *StatusError) Error() string { return fmt.Sprintf("%v: %s", ErrStatus, e.Status) }
func (e *StatusError) Unwrap() error { return ErrStatus }

// Response is an open download.
type Response struct {
	StatusCode    int
	Header        http.Header
	ContentLength int64
	Body          io.ReadCloser
}

// Options controls Get.
type Options struct {
	// IdleTimeout aborts the transfer when no bytes arrive for this long.
	IdleTimeout time.Duration
	// Header holds extra request headers.
	Header http.Header
	// AcceptStatus lists acceptable status codes (default: 200 only).
	AcceptStatus []int
}

// Get issues a GET request. The returned body enforces the idle timeout and
// reports a truncated body (fewer bytes than Content-Length) as an error.
func Get(ctx context.Context, client *http.Client, u *url.URL, opts Options) (*Response, error) {
	rctx, cancel := context.WithCancelCause(ctx)
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, u.String(), nil)
	if err != nil {
		cancel(nil)
		return nil, err
	}
	for k, vs := range opts.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	// Ask for the representation as-is; a transparently decompressed body
	// would not match the manifest checksum.
	req.Header.Set("Accept-Encoding", "identity")

	idle := newIdleTimer(opts.IdleTimeout, func() { cancel(ErrIdle) })
	resp, err := client.Do(req)
	if err != nil {
		idle.stop()
		cause := context.Cause(rctx)
		cancel(nil)
		if errors.Is(cause, ErrIdle) {
			return nil, ErrIdle
		}
		return nil, err
	}
	accept := opts.AcceptStatus
	if len(accept) == 0 {
		accept = []int{http.StatusOK}
	}
	ok := false
	for _, c := range accept {
		ok = ok || resp.StatusCode == c
	}
	if !ok {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		idle.stop()
		cancel(nil)
		return nil, &StatusError{Code: resp.StatusCode, Status: resp.Status}
	}
	if enc := resp.Header.Get("Content-Encoding"); enc != "" && enc != "identity" {
		resp.Body.Close()
		idle.stop()
		cancel(nil)
		return nil, fmt.Errorf("unexpected Content-Encoding %q", enc)
	}
	body := &body{rc: resp.Body, idle: idle, ctx: rctx, cancel: cancel, want: resp.ContentLength}
	return &Response{StatusCode: resp.StatusCode, Header: resp.Header, ContentLength: resp.ContentLength, Body: body}, nil
}

type idleTimer struct {
	d time.Duration
	t *time.Timer
}

func newIdleTimer(d time.Duration, fire func()) *idleTimer {
	it := &idleTimer{d: d}
	if d > 0 {
		it.t = time.AfterFunc(d, fire)
	}
	return it
}

func (i *idleTimer) reset() {
	if i.t != nil {
		i.t.Reset(i.d)
	}
}

func (i *idleTimer) stop() {
	if i.t != nil {
		i.t.Stop()
	}
}

type body struct {
	rc     io.ReadCloser
	idle   *idleTimer
	ctx    context.Context
	cancel context.CancelCauseFunc
	n      int64
	want   int64
	once   sync.Once
}

func (b *body) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	b.n += int64(n)
	if n > 0 {
		b.idle.reset()
	}
	if err != nil && err != io.EOF {
		if cause := context.Cause(b.ctx); errors.Is(cause, ErrIdle) {
			return n, fmt.Errorf("%w after %d bytes", ErrIdle, b.n)
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			if b.want < 0 {
				return n, fmt.Errorf("connection closed before the end of the chunked response after %d bytes: %w", b.n, io.ErrUnexpectedEOF)
			}
			return n, fmt.Errorf("%w: got %d of %d bytes", ErrShortBody, b.n, b.want)
		}
		return n, err
	}
	if err == io.EOF && b.want >= 0 && b.n != b.want {
		return n, fmt.Errorf("%w: got %d of %d bytes", ErrShortBody, b.n, b.want)
	}
	return n, err
}

func (b *body) Close() error {
	var err error
	b.once.Do(func() {
		b.idle.stop()
		err = b.rc.Close()
		b.cancel(nil)
	})
	return err
}

// Staging is a bounded temporary file for the exported stream.
type Staging struct {
	f     *os.File
	path  string
	limit int64
	n     int64
}

// StagingReserve is left free on the staging filesystem.
const StagingReserve = 64 << 20

// NewStaging creates a staging file in dir. limit caps the file size; when
// limit is zero the currently available space minus StagingReserve is used.
// It fails if less than minBytes are available.
func NewStaging(dir string, limit, minBytes int64) (*Staging, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return nil, fmt.Errorf("statfs staging directory: %w", err)
	}
	avail := int64(st.Bavail)*int64(st.Bsize) - StagingReserve
	if limit <= 0 || limit > avail {
		limit = avail
	}
	if limit < minBytes {
		return nil, fmt.Errorf("%w: staging directory %s has %d usable bytes, at least %d are required", ErrStagingFull, dir, limit, minBytes)
	}
	f, err := os.CreateTemp(dir, "export-*.vmdk")
	if err != nil {
		return nil, fmt.Errorf("create staging file: %w", err)
	}
	return &Staging{f: f, path: f.Name(), limit: limit}, nil
}

// Path returns the staging file path.
func (s *Staging) Path() string { return s.path }

// Limit returns the maximum number of bytes accepted.
func (s *Staging) Limit() int64 { return s.limit }

// Size returns the number of bytes written.
func (s *Staging) Size() int64 { return s.n }

// Write appends p, failing with ErrStagingFull once the limit would be exceeded.
func (s *Staging) Write(p []byte) (int, error) {
	if s.n+int64(len(p)) > s.limit {
		return 0, fmt.Errorf("%w: limit is %d bytes", ErrStagingFull, s.limit)
	}
	n, err := s.f.Write(p)
	s.n += int64(n)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	return n, err
}

// Finish flushes and closes the file for writing.
func (s *Staging) Finish() error {
	if err := s.f.Sync(); err != nil {
		return err
	}
	return s.f.Close()
}

// Remove deletes the staging file. It is safe to call more than once.
func (s *Staging) Remove() error {
	_ = s.f.Close()
	err := os.Remove(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// CleanStaleStaging removes staging files left by an interrupted earlier run.
func CleanStaleStaging(dir string) (int, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "export-*.vmdk"))
	if err != nil {
		return 0, err
	}
	for _, m := range matches {
		if err := os.Remove(m); err != nil && !errors.Is(err, os.ErrNotExist) {
			return 0, err
		}
	}
	return len(matches), nil
}
