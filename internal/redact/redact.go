// Package redact removes credentials, cookies, tickets and ticket-bearing URLs
// from text before it is logged, reported or written to Kubernetes objects.
package redact

import (
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
)

const mask = "REDACTED"

// URL returns a representation of u that keeps the scheme, host and final
// path element (for example "disk-0.vmdk") but removes user information,
// query parameters, fragments and all other path elements, which may carry
// lease tickets.
func URL(u *url.URL) string {
	if u == nil {
		return ""
	}
	base := path.Base(u.Path)
	if base == "." || base == "/" {
		base = ""
	}
	s := u.Scheme + "://" + u.Host + "/"
	if strings.Count(strings.Trim(u.Path, "/"), "/") > 0 {
		s += mask + "/"
	}
	return s + base
}

// URLString redacts a URL given as a string; unparsable input is fully masked.
func URLString(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return mask
	}
	return URL(u)
}

// Secrets is a set of sensitive strings (passwords, full URLs, tickets,
// cookie values) that must never appear in output.
type Secrets struct {
	mu     sync.RWMutex
	values map[string]struct{}
}

// NewSecrets returns an empty set.
func NewSecrets() *Secrets { return &Secrets{values: map[string]struct{}{}} }

// Add registers sensitive values. Values shorter than four characters are
// ignored to avoid masking common substrings.
func (s *Secrets) Add(values ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range values {
		if len(v) >= 4 {
			s.values[v] = struct{}{}
		}
	}
}

// AddURL registers a URL and its individual path elements (which may be
// lease tickets), plus its query values.
func (s *Secrets) AddURL(u *url.URL) {
	if u == nil {
		return
	}
	s.Add(u.String())
	cp := *u
	cp.Host = "*"
	s.Add(cp.String())
	for _, p := range strings.Split(u.Path, "/") {
		// Keep short, generic names such as "nfc" or "disk-0.vmdk" readable.
		if len(p) >= 16 {
			s.Add(p)
		}
	}
	for _, vs := range u.Query() {
		s.Add(vs...)
	}
	if u.User != nil {
		if pw, ok := u.User.Password(); ok {
			s.Add(pw)
		}
	}
}

// String replaces every registered value in text, then masks URLs whose path
// looks ticket-bearing and common credential patterns.
func (s *Secrets) String(text string) string {
	if s != nil {
		s.mu.RLock()
		vals := make([]string, 0, len(s.values))
		for v := range s.values {
			vals = append(vals, v)
		}
		s.mu.RUnlock()
		// Replace longer values first so that a URL is masked as a whole.
		sort.Slice(vals, func(i, j int) bool { return len(vals[i]) > len(vals[j]) })
		for _, v := range vals {
			text = strings.ReplaceAll(text, v, mask)
		}
	}
	return Text(text)
}

// Error returns an error whose message has been redacted. The original error
// remains available to errors.Is/As through Unwrap, but its text is never
// rendered.
func (s *Secrets) Error(err error) error {
	if err == nil {
		return nil
	}
	return &redactedError{msg: s.String(err.Error()), err: err}
}

type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

var (
	nfcURL      = regexp.MustCompile(`(https?://[^\s/"']+)/nfc/[^\s"']*`)
	userinfoURL = regexp.MustCompile(`(https?://)[^\s/@"']+@`)
	sessionKV   = regexp.MustCompile(`(?i)\b(vmware_soap_session|vmware_cgi_ticket|vmware_api_session_id)\s*=\s*"?[^\s"',;&]*"?`)
	kvSecret    = regexp.MustCompile(`(?i)\b(password|passwd|pwd|secret|token|ticket|cookie|authorization)(["']?\s*[:=]\s*["']?)[^\s"',;&]+`)
)

// Text masks NFC URLs, URL user information and key=value credentials in text.
func Text(text string) string {
	text = nfcURL.ReplaceAllStringFunc(text, func(m string) string {
		u, err := url.Parse(m)
		if err != nil {
			return mask
		}
		return URL(u)
	})
	text = userinfoURL.ReplaceAllString(text, "${1}"+mask+"@")
	text = sessionKV.ReplaceAllString(text, "${1}="+mask)
	text = kvSecret.ReplaceAllString(text, "${1}${2}"+mask)
	return text
}

var safeHeaders = map[string]bool{
	"Accept-Ranges": true, "Cache-Control": true, "Connection": true, "Content-Disposition": true,
	"Content-Encoding": true, "Content-Length": true, "Content-Range": true, "Content-Type": true,
	"Date": true, "Etag": true, "Last-Modified": true, "Server": true, "Transfer-Encoding": true,
	"Vary": true, "X-Frame-Options": true,
}

// Headers returns a copy of h with every header that is not on a small
// allow-list masked, and with registered secrets removed from values.
func (s *Secrets) Headers(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, vs := range h {
		ck := http.CanonicalHeaderKey(k)
		if !safeHeaders[ck] {
			out[ck] = mask
			continue
		}
		out[ck] = s.String(strings.Join(vs, ", "))
	}
	return out
}
