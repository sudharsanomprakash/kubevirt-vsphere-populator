// Package convert runs the pinned qemu-img binary as a subprocess to convert a
// staged streamOptimized VMDK into raw guest disk bytes on the destination.
package convert

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// QemuImg is a located qemu-img binary.
type QemuImg struct {
	Path    string
	Version string
}

var versionRE = regexp.MustCompile(`qemu-img version ([0-9][^\s]*)`)

// Find locates qemu-img (at path, or on $PATH when path is empty) and records its version.
func Find(ctx context.Context, path string) (*QemuImg, error) {
	if path == "" {
		p, err := exec.LookPath("qemu-img")
		if err != nil {
			return nil, fmt.Errorf("qemu-img not found: %w", err)
		}
		path = p
	}
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return nil, fmt.Errorf("run %s --version: %w", path, err)
	}
	m := versionRE.FindSubmatch(out)
	if m == nil {
		return nil, fmt.Errorf("cannot parse qemu-img version from %q", firstLine(out))
	}
	return &QemuImg{Path: path, Version: string(m[1])}, nil
}

func firstLine(b []byte) string {
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}

// Info is the subset of `qemu-img info --output=json` used for cross-checks.
type Info struct {
	Format         string `json:"format"`
	VirtualSize    int64  `json:"virtual-size"`
	FormatSpecific struct {
		Type string `json:"type"`
		Data struct {
			CreateType string `json:"create-type"`
			ParentCID  uint64 `json:"parent-cid"`
			Extents    []struct {
				Compressed  bool  `json:"compressed"`
				VirtualSize int64 `json:"virtual-size"`
				ClusterSize int64 `json:"cluster-size"`
			} `json:"extents"`
		} `json:"data"`
	} `json:"format-specific"`
}

// Info inspects a staged VMDK with qemu-img, forcing the vmdk format so that
// no format probing takes place.
func (q *QemuImg) Info(ctx context.Context, file string) (*Info, error) {
	cmd := exec.CommandContext(ctx, q.Path, "info", "--output=json", "-f", "vmdk", file)
	var stderr bytes.Buffer
	cmd.Stderr = &limitedBuffer{buf: &stderr, max: 16 << 10}
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("qemu-img info: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var info Info
	if err := json.Unmarshal(out, &info); err != nil {
		return nil, fmt.Errorf("parse qemu-img info output: %w", err)
	}
	return &info, nil
}

// CheckStaged verifies that qemu-img sees the staged file as a compressed
// streamOptimized VMDK with the expected virtual size.
func (q *QemuImg) CheckStaged(ctx context.Context, file string, virtualSize int64) (*Info, error) {
	info, err := q.Info(ctx, file)
	if err != nil {
		return nil, err
	}
	if info.Format != "vmdk" {
		return info, fmt.Errorf("qemu-img reports format %q, want vmdk", info.Format)
	}
	if info.VirtualSize != virtualSize {
		return info, fmt.Errorf("qemu-img reports virtual size %d, want %d", info.VirtualSize, virtualSize)
	}
	if ct := info.FormatSpecific.Data.CreateType; ct != "" && !strings.EqualFold(ct, "streamOptimized") {
		return info, fmt.Errorf("qemu-img reports create-type %q, want streamOptimized", ct)
	}
	return info, nil
}

// Target selects how qemu-img writes the destination.
type Target int

const (
	// NewFile makes qemu-img create a new raw file (the path must not exist).
	NewFile Target = iota
	// ExistingDevice writes into an existing block device (qemu-img -n).
	// qemu-img does not assume that an existing target reads as zeros, so
	// zero regions of the source are explicitly written as zeros.
	ExistingDevice
)

// ConvertOptions controls a conversion.
type ConvertOptions struct {
	Target Target
	// Progress receives qemu-img's progress in percent (0-100).
	Progress func(percent float64)
	// Coroutines is qemu-img's -m parallelism (1-16, default 8).
	Coroutines int
}

// Args returns the qemu-img argument list for a conversion. It is exported
// for documentation and tests.
func Args(src, dst string, opts ConvertOptions) []string {
	args := []string{"convert", "-p", "-f", "vmdk", "-O", "raw", "-t", "writeback"}
	if opts.Coroutines > 0 {
		args = append(args, "-m", strconv.Itoa(min(opts.Coroutines, 16)))
	}
	if opts.Target == ExistingDevice {
		args = append(args, "-n")
	} else {
		args = append(args, "-o", "preallocation=off")
	}
	return append(args, src, dst)
}

var progressRE = regexp.MustCompile(`\(([0-9]+(?:\.[0-9]+)?)/100%\)`)

// Convert runs qemu-img convert. Cancelling the context kills qemu-img and
// any process it started.
func (q *QemuImg) Convert(ctx context.Context, src, dst string, opts ConvertOptions) error {
	cmd := exec.CommandContext(ctx, q.Path, Args(src, dst, opts)...)
	var stderr bytes.Buffer
	cmd.Stderr = &limitedBuffer{buf: &stderr, max: 64 << 10}
	cmd.Stdout = &progressWriter{fn: opts.Progress}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start qemu-img: %w", err)
	}
	err := cmd.Wait()
	if ctx.Err() != nil {
		return fmt.Errorf("qemu-img convert cancelled: %w", ctx.Err())
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return fmt.Errorf("qemu-img convert exited with status %d: %s", ee.ExitCode(), strings.TrimSpace(stderr.String()))
		}
		return fmt.Errorf("qemu-img convert: %w", err)
	}
	return nil
}

// progressWriter parses qemu-img -p output, which redraws a single line with
// carriage returns.
type progressWriter struct {
	fn   func(float64)
	line []byte
}

func (w *progressWriter) Write(p []byte) (int, error) {
	for _, b := range p {
		if b == '\r' || b == '\n' {
			w.flush()
			continue
		}
		if len(w.line) < 256 {
			w.line = append(w.line, b)
		}
	}
	return len(p), nil
}

func (w *progressWriter) flush() {
	if m := progressRE.FindSubmatch(w.line); m != nil && w.fn != nil {
		if v, err := strconv.ParseFloat(string(m[1]), 64); err == nil {
			w.fn(v)
		}
	}
	w.line = w.line[:0]
}

// limitedBuffer keeps at most max bytes (the most recent ones are dropped
// once the limit is reached) so that a misbehaving subprocess cannot exhaust memory.
type limitedBuffer struct {
	buf *bytes.Buffer
	max int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.max - l.buf.Len(); room > 0 {
		l.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}
