// Package diagnose implements a read-only inspection of a VM export: it
// reports the lease metadata, how device URLs map to disks, the response
// headers and format of the export stream, and whether HTTP Range requests
// are honored. The lease is always aborted; diagnostics never complete one.
// All URLs, cookies, tickets and credentials are redacted from the report.
package diagnose

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/vmware/govmomi/vim25/types"

	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/redact"
	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/transfer"
	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/vmdk"
	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/vsphere"
)

// Config controls a diagnostic run.
type Config struct {
	Connection     vsphere.ConnectionConfig
	CredentialsDir string
	VM             vsphere.VMSelector
	// Disk optionally limits probing to one disk; otherwise every disk URL is probed.
	Disk              vsphere.DiskSelector
	LeaseReadyTimeout time.Duration
	IdleTimeout       time.Duration
	// ProbeBytes is how much of each stream is read by probe requests (default 64 KiB).
	ProbeBytes int64
	// FullRead downloads and decodes each probed stream completely (discarding
	// the data) to measure single-stream throughput and validate the format.
	FullRead bool
	// SkipRange disables the Range probe.
	SkipRange bool
	// AllowPoweredOn permits inspecting a VM that is not powered off. The
	// export request itself will then normally be refused by vSphere.
	AllowPoweredOn bool
}

// HTTPResult describes one probe request.
type HTTPResult struct {
	Request       string            `json:"request"`
	Status        int               `json:"status,omitempty"`
	Headers       map[string]string `json:"headers,omitempty"`
	ContentLength int64             `json:"contentLength"`
	BytesRead     int64             `json:"bytesRead"`
	Error         string            `json:"error,omitempty"`
}

// Format describes the first bytes of an export stream.
type Format struct {
	Magic           string           `json:"magic"`
	SparseExtent    bool             `json:"sparseExtent"`
	StreamOptimized bool             `json:"streamOptimized"`
	Header          *vmdk.Header     `json:"header,omitempty"`
	Descriptor      *vmdk.Descriptor `json:"descriptor,omitempty"`
	Error           string           `json:"error,omitempty"`
}

// FullRead summarizes a complete read of a stream.
type FullRead struct {
	Bytes          int64   `json:"bytes"`
	Seconds        float64 `json:"seconds"`
	BytesPerSecond float64 `json:"bytesPerSecond"`
	SHA1           string  `json:"sha1,omitempty"`
	SHA256         string  `json:"sha256,omitempty"`
	Grains         int64   `json:"grains"`
	AllocatedBytes int64   `json:"allocatedBytes"`
	GTMarkers      int     `json:"gtMarkers"`
	GDMarkers      int     `json:"gdMarkers"`
	FooterPresent  bool    `json:"footerPresent"`
	Ordered        bool    `json:"ordered"`
	TrailingBytes  int64   `json:"trailingBytes"`
	GuestDigest    string  `json:"guestDigest,omitempty"`
	Error          string  `json:"error,omitempty"`
}

// Probe is the result of probing one device URL.
type Probe struct {
	DeviceKey string      `json:"deviceKey"`
	TargetID  string      `json:"targetId"`
	Head      *HTTPResult `json:"head,omitempty"`
	First     *HTTPResult `json:"first,omitempty"`
	Format    *Format     `json:"format,omitempty"`
	Range     *HTTPResult `json:"range,omitempty"`
	// RangeHonored is true when a Range request returned 206 with the bytes
	// found at the same offset of a plain request.
	RangeHonored *bool      `json:"rangeHonored,omitempty"`
	Repeat       *HTTPResult `json:"repeat,omitempty"`
	// StableAcrossRequests compares the first ProbeBytes of two plain requests.
	StableAcrossRequests *bool     `json:"stableAcrossRequests,omitempty"`
	Full                 *FullRead `json:"full,omitempty"`
}

// DeviceURL is a redacted lease device URL.
type DeviceURL struct {
	Key              string `json:"key"`
	ImportKey        string `json:"importKey,omitempty"`
	Disk             bool   `json:"disk"`
	TargetID         string `json:"targetId,omitempty"`
	FileSize         int64  `json:"fileSize"`
	DatastoreKey     string `json:"datastoreKey,omitempty"`
	URL              string `json:"url"`
	HostIsWildcard   bool   `json:"hostIsWildcard"`
	HasSSLThumbprint bool   `json:"hasSslThumbprint"`
	KeyParses        bool   `json:"keyParses"`
	MatchedDisk      int32  `json:"matchedDiskKey,omitempty"`
}

// Report is the diagnostic output.
type Report struct {
	GeneratedAt time.Time `json:"generatedAt"`
	Server      struct {
		APIType  string `json:"apiType"`
		Version  string `json:"version"`
		Build    string `json:"build"`
		FullName string `json:"fullName"`
	} `json:"server"`
	VM              *vsphere.VMInfo `json:"vm,omitempty"`
	ColdSourceCheck string          `json:"coldSourceCheck"`
	Lease           struct {
		Ref                   string      `json:"ref,omitempty"`
		TimeoutSeconds        int32       `json:"timeoutSeconds"`
		TotalDiskCapacityInKB int64       `json:"totalDiskCapacityInKB"`
		DeviceURLs            []DeviceURL `json:"deviceURLs"`
		Error                 string      `json:"error,omitempty"`
		Disposition           string      `json:"disposition,omitempty"`
	} `json:"lease"`
	Probes   []Probe                 `json:"probes"`
	Manifest []vsphere.ManifestEntry `json:"manifest,omitempty"`
	// ManifestStatus is "ok", "unavailable" or an error.
	ManifestStatus string   `json:"manifestStatus,omitempty"`
	Notes          []string `json:"notes,omitempty"`
}

// Run performs the diagnosis. A report is returned even on error, holding
// whatever was collected.
func Run(ctx context.Context, cfg Config) (*Report, error) {
	rep := &Report{GeneratedAt: time.Now().UTC()}
	if cfg.LeaseReadyTimeout == 0 {
		cfg.LeaseReadyTimeout = 5 * time.Minute
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = 2 * time.Minute
	}
	if cfg.ProbeBytes <= 0 {
		cfg.ProbeBytes = 64 << 10
	}
	if err := cfg.Connection.LoadCredentials(cfg.CredentialsDir); err != nil {
		return rep, err
	}
	c, err := vsphere.Connect(ctx, cfg.Connection)
	if err != nil {
		return rep, err
	}
	defer c.Logout(context.WithoutCancel(ctx))
	about := c.About()
	rep.Server.APIType, rep.Server.Version, rep.Server.Build, rep.Server.FullName = about.ApiType, about.Version, about.Build, about.FullName

	ref, err := c.ResolveVM(ctx, cfg.VM)
	if err != nil {
		return rep, err
	}
	vm, err := c.InspectVM(ctx, ref)
	if err != nil {
		return rep, err
	}
	rep.VM = vm
	rep.ColdSourceCheck = "ok"
	if err := vm.CheckColdSource(); err != nil {
		rep.ColdSourceCheck = err.Error()
		if !cfg.AllowPoweredOn {
			return rep, err
		}
	}
	var only *vsphere.DiskInfo
	if cfg.Disk != (vsphere.DiskSelector{}) {
		d, err := vm.SelectDisk(cfg.Disk)
		if err != nil {
			return rep, err
		}
		only = d
	}

	lease, err := c.ExportVM(ctx, vm.Ref, cfg.LeaseReadyTimeout)
	if err != nil {
		rep.Lease.Error = err.Error()
		return rep, err
	}
	defer func() {
		if aerr := lease.Abort(context.WithoutCancel(ctx), errors.New("diagnostic run finished; diagnostics never complete a lease")); aerr != nil {
			rep.Lease.Disposition = "abort failed: " + aerr.Error()
		} else {
			rep.Lease.Disposition = "aborted"
		}
	}()
	info := lease.Info()
	rep.Lease.Ref, rep.Lease.TimeoutSeconds, rep.Lease.TotalDiskCapacityInKB = info.Lease.Value, info.LeaseTimeout, info.TotalDiskCapacityInKB

	// Keep the lease alive while probing.
	pctx, pcancel := context.WithCancelCause(ctx)
	defer pcancel(nil)
	ka := lease.StartKeepalive(vsphere.KeepaliveInterval(lease.Timeout()), 3, func() int32 { return 0 }, func(e error) { pcancel(e) })
	defer ka.Stop()

	for _, d := range info.DeviceUrl {
		du := DeviceURL{Key: d.Key, ImportKey: d.ImportKey, Disk: d.Disk != nil && *d.Disk, TargetID: d.TargetId,
			FileSize: d.FileSize, DatastoreKey: d.DatastoreKey, HostIsWildcard: strings.Contains(d.Url, "://*"), HasSSLThumbprint: d.SslThumbprint != ""}
		if u, err := c.ParseURL(d.Url); err == nil {
			du.URL = redact.URL(u)
		} else {
			du.URL = "REDACTED"
		}
		if _, err := vsphere.ParseDeviceKey(d.Key); err == nil {
			du.KeyParses = true
		}
		for i := range vm.Disks {
			if vm.Disks[i].ExpectedLeaseKey != "" && vm.Disks[i].ExpectedLeaseKey == d.Key {
				du.MatchedDisk = vm.Disks[i].Key
			}
		}
		rep.Lease.DeviceURLs = append(rep.Lease.DeviceURLs, du)
	}

	for _, d := range info.DeviceUrl {
		if d.Disk == nil || !*d.Disk {
			continue
		}
		if only != nil && d.Key != only.ExpectedLeaseKey {
			continue
		}
		u, err := c.PrepareNFCURL(d)
		if err != nil {
			rep.Probes = append(rep.Probes, Probe{DeviceKey: d.Key, TargetID: d.TargetId, First: &HTTPResult{Error: err.Error()}})
			continue
		}
		rep.Probes = append(rep.Probes, probe(pctx, c, u, d, cfg))
	}
	if only != nil && len(rep.Probes) == 0 {
		rep.Notes = append(rep.Notes, fmt.Sprintf("no lease device URL has the expected key %q for the selected disk", only.ExpectedLeaseKey))
	}

	if m, err := lease.Manifest(pctx); err != nil {
		if errors.Is(err, vsphere.ErrManifestUnavailable) {
			rep.ManifestStatus = "unavailable"
		} else {
			rep.ManifestStatus = err.Error()
		}
	} else {
		rep.Manifest, rep.ManifestStatus = m, "ok"
	}
	if cause := context.Cause(pctx); cause != nil && ctx.Err() == nil {
		rep.Notes = append(rep.Notes, "lease keepalive failed during probing: "+c.Secrets.String(cause.Error()))
	}
	rep.Notes = append(rep.Notes, notes(rep)...)
	return rep, nil
}

func probe(ctx context.Context, c *vsphere.Client, u *url.URL, d types.HttpNfcLeaseDeviceUrl, cfg Config) Probe {
	p := Probe{DeviceKey: d.Key, TargetID: d.TargetId}
	hc := &c.Client.Client.Client
	s := c.Secrets

	// HEAD (may not be supported).
	p.Head = request(ctx, hc, http.MethodHead, u, nil, 0, cfg.IdleTimeout, s)

	// Plain GET: first ProbeBytes.
	var first []byte
	p.First, first = get(ctx, hc, u, nil, cfg.ProbeBytes, cfg.IdleTimeout, s, nil)
	if len(first) > 0 {
		p.Format = sniff(first)
	}

	if !cfg.SkipRange && len(first) >= 1024 {
		// Request bytes 512-1023 and compare them with the same offset of the plain read.
		h := http.Header{"Range": {"bytes=512-1023"}}
		var got []byte
		p.Range, got = get(ctx, hc, u, h, 512, cfg.IdleTimeout, s, []int{http.StatusOK, http.StatusPartialContent})
		honored := p.Range.Status == http.StatusPartialContent && bytes.Equal(got, first[512:1024])
		p.RangeHonored = &honored
	}

	var second []byte
	p.Repeat, second = get(ctx, hc, u, nil, cfg.ProbeBytes, cfg.IdleTimeout, s, nil)
	if len(first) > 0 && p.Repeat.Error == "" {
		stable := bytes.Equal(first, second)
		p.StableAcrossRequests = &stable
	}

	if cfg.FullRead {
		p.Full = fullRead(ctx, hc, u, cfg.IdleTimeout, s)
	}
	return p
}

func request(ctx context.Context, hc *http.Client, method string, u *url.URL, h http.Header, _ int64, idle time.Duration, s *redact.Secrets) *HTTPResult {
	r := &HTTPResult{Request: method, ContentLength: -1}
	rctx, cancel := context.WithTimeout(ctx, idle)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, method, u.String(), nil)
	if err != nil {
		r.Error = s.String(err.Error())
		return r
	}
	for k, vs := range h {
		req.Header[k] = vs
	}
	resp, err := hc.Do(req)
	if err != nil {
		r.Error = s.String(err.Error())
		return r
	}
	defer resp.Body.Close()
	r.Status, r.Headers, r.ContentLength = resp.StatusCode, s.Headers(resp.Header), resp.ContentLength
	return r
}

// get reads up to limit bytes and then closes the connection.
func get(ctx context.Context, hc *http.Client, u *url.URL, h http.Header, limit int64, idle time.Duration, s *redact.Secrets, accept []int) (*HTTPResult, []byte) {
	name := "GET"
	if rng := h.Get("Range"); rng != "" {
		name = "GET Range: " + rng
	}
	r := &HTTPResult{Request: name, ContentLength: -1}
	resp, err := transfer.Get(ctx, hc, u, transfer.Options{IdleTimeout: idle, Header: h, AcceptStatus: accept})
	if err != nil {
		var se *transfer.StatusError
		if errors.As(err, &se) {
			r.Status = se.Code
		}
		r.Error = s.String(err.Error())
		return r, nil
	}
	defer resp.Body.Close()
	r.Status, r.Headers, r.ContentLength = resp.StatusCode, s.Headers(resp.Header), resp.ContentLength
	buf, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	r.BytesRead = int64(len(buf))
	if err != nil {
		r.Error = s.String(err.Error())
	}
	return r, buf
}

func sniff(b []byte) *Format {
	f := &Format{}
	if len(b) >= 4 {
		f.Magic = hex.EncodeToString(b[:4])
	}
	if len(b) < vmdk.SectorSize {
		f.Error = "fewer than 512 bytes received"
		return f
	}
	h, err := vmdk.ParseHeader(b[:vmdk.SectorSize])
	if err != nil {
		f.Error = err.Error()
		return f
	}
	f.SparseExtent = true
	f.Header = &h
	if err := h.ValidateStreamOptimized(); err != nil {
		f.Error = err.Error()
	}
	start := int64(h.DescriptorOffset) * vmdk.SectorSize
	end := start + int64(h.DescriptorSize)*vmdk.SectorSize
	if h.DescriptorSize > 0 && end <= int64(len(b)) {
		if d, err := vmdk.ParseDescriptor(b[start:end]); err == nil {
			f.Descriptor = &d
			f.StreamOptimized = f.Error == "" && d.ValidateStreamOptimized(h) == nil && h.GDOffset == vmdk.GDAtEnd
		} else if f.Error == "" {
			f.Error = err.Error()
		}
	}
	return f
}

func fullRead(ctx context.Context, hc *http.Client, u *url.URL, idle time.Duration, s *redact.Secrets) *FullRead {
	fr := &FullRead{}
	start := time.Now()
	resp, err := transfer.Get(ctx, hc, u, transfer.Options{IdleTimeout: idle})
	if err != nil {
		fr.Error = s.String(err.Error())
		return fr
	}
	defer resp.Body.Close()
	h1, h256 := sha1.New(), sha256.New()
	cr := &counter{}
	res, err := vmdk.Decode(io.TeeReader(resp.Body, io.MultiWriter(h1, h256, cr)), vmdk.DecodeOptions{})
	fr.Bytes = cr.n
	fr.Seconds = time.Since(start).Seconds()
	if fr.Seconds > 0 {
		fr.BytesPerSecond = float64(fr.Bytes) / fr.Seconds
	}
	if res != nil {
		fr.Grains, fr.AllocatedBytes, fr.GTMarkers, fr.GDMarkers = res.Grains, res.AllocatedBytes, res.GTMarkers, res.GDMarkers
		fr.FooterPresent, fr.Ordered, fr.TrailingBytes = res.Footer != nil, res.Ordered, res.TrailingBytes
		if res.Digest != nil {
			fr.GuestDigest = "kvsp-chunked-sha256-v1:" + hex.EncodeToString(res.Digest)
		}
	}
	if err != nil {
		fr.Error = s.String(err.Error())
		return fr
	}
	fr.SHA1, fr.SHA256 = hex.EncodeToString(h1.Sum(nil)), hex.EncodeToString(h256.Sum(nil))
	return fr
}

type counter struct{ n int64 }

func (c *counter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }

func notes(rep *Report) []string {
	var out []string
	for _, p := range rep.Probes {
		if p.Format != nil && !p.Format.StreamOptimized {
			out = append(out, fmt.Sprintf("%s: stream is not a streamOptimized VMDK with the grain directory at the end; populate would reject it", p.DeviceKey))
		}
		if p.RangeHonored != nil && !*p.RangeHonored {
			out = append(out, fmt.Sprintf("%s: Range requests are not honored (status %d); the stream must be read sequentially", p.DeviceKey, p.Range.Status))
		}
		if p.StableAcrossRequests != nil && !*p.StableAcrossRequests {
			out = append(out, fmt.Sprintf("%s: two requests returned different leading bytes; the stream is not stable across requests", p.DeviceKey))
		}
	}
	for _, d := range rep.Lease.DeviceURLs {
		if d.Disk && !d.KeyParses {
			out = append(out, fmt.Sprintf("device key %q does not follow the documented /<vm>/<Controller><n>:<unit> format; populate would reject it", d.Key))
		} else if d.Disk && d.MatchedDisk == 0 {
			out = append(out, fmt.Sprintf("device key %q does not match the expected key of any disk", d.Key))
		}
	}
	return out
}
