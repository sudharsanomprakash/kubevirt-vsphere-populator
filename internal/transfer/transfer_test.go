package transfer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"
)

func serve(t *testing.T, h http.HandlerFunc) (*http.Client, *url.URL) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL + "/nfc/ticket/disk-0.vmdk")
	return srv.Client(), u
}

func TestOK(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 100000)
	c, u := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept-Encoding") != "identity" {
			t.Errorf("Accept-Encoding %q", r.Header.Get("Accept-Encoding"))
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		_, _ = w.Write(data)
	})
	resp, err := Get(context.Background(), c, u, Options{IdleTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read: %v", err)
	}
}

func TestStatusCodes(t *testing.T) {
	for _, code := range []int{http.StatusPartialContent, http.StatusNotFound, http.StatusServiceUnavailable, http.StatusInternalServerError, http.StatusFound} {
		c, u := serve(t, func(w http.ResponseWriter, r *http.Request) {
			if code == http.StatusFound {
				// A redirect to an error page must not be followed into success.
				http.Redirect(w, r, "/error", code)
				return
			}
			w.WriteHeader(code)
		})
		c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		_, err := Get(context.Background(), c, u, Options{})
		var se *StatusError
		if !errors.As(err, &se) || se.Code != code || !errors.Is(err, ErrStatus) {
			t.Fatalf("code %d: got %v", code, err)
		}
	}
}

func TestTruncatedWithContentLength(t *testing.T) {
	c, u := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100000")
		_, _ = w.Write(make([]byte, 5000))
		// Hijack and close to cut the body short.
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		conn.Close()
	})
	resp, err := Get(context.Background(), c, u, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, err = io.ReadAll(resp.Body)
	if !errors.Is(err, ErrShortBody) {
		t.Fatalf("got %v, want ErrShortBody", err)
	}
}

func TestTruncatedChunked(t *testing.T) {
	c, u := serve(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(make([]byte, 5000))
		w.(http.Flusher).Flush()
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		conn.Close()
	})
	resp, err := Get(context.Background(), c, u, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Fatal("truncated chunked body read without error")
	}
}

func TestIdleTimeout(t *testing.T) {
	release := make(chan struct{})
	c, u := serve(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(make([]byte, 1000))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	defer close(release)
	resp, err := Get(context.Background(), c, u, Options{IdleTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	start := time.Now()
	_, err = io.ReadAll(resp.Body)
	if !errors.Is(err, ErrIdle) {
		t.Fatalf("got %v, want ErrIdle", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("idle timeout too slow")
	}
}

func TestIdleTimeoutBeforeHeaders(t *testing.T) {
	release := make(chan struct{})
	c, u := serve(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	defer close(release)
	_, err := Get(context.Background(), c, u, Options{IdleTimeout: 200 * time.Millisecond})
	if !errors.Is(err, ErrIdle) {
		t.Fatalf("got %v, want ErrIdle", err)
	}
}

func TestCancellation(t *testing.T) {
	release := make(chan struct{})
	c, u := serve(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(make([]byte, 1000))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	resp, err := Get(ctx, c, u, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	time.AfterFunc(100*time.Millisecond, cancel)
	if _, err := io.ReadAll(resp.Body); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

func TestContentEncodingRejected(t *testing.T) {
	c, u := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "br")
		_, _ = w.Write([]byte("x"))
	})
	if _, err := Get(context.Background(), c, u, Options{}); err == nil {
		t.Fatal("Content-Encoding accepted")
	}
}

func TestStaging(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStaging(dir, 1000, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(make([]byte, 600)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(make([]byte, 600)); !errors.Is(err, ErrStagingFull) {
		t.Fatalf("got %v, want ErrStagingFull", err)
	}
	if err := s.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.Path()); !os.IsNotExist(err) {
		t.Fatal("staging file not removed")
	}
	if err := s.Remove(); err != nil {
		t.Fatal("second Remove failed")
	}
	if _, err := NewStaging(dir, 0, 1<<62); !errors.Is(err, ErrStagingFull) {
		t.Fatalf("insufficient space not detected: %v", err)
	}
	// Stale files from an interrupted run are cleaned up.
	_ = os.WriteFile(dir+"/export-123.vmdk", []byte("old"), 0o600)
	if n, err := CleanStaleStaging(dir); err != nil || n != 1 {
		t.Fatalf("CleanStaleStaging: %d %v", n, err)
	}
}
