package redact

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

const ticketURL = "https://esxi01.example.test/nfc/52a1b2c3-d4e5-f607-1829-3a4b5c6d7e8f/disk-0.vmdk"

func TestURL(t *testing.T) {
	u, _ := url.Parse(ticketURL)
	got := URL(u)
	if strings.Contains(got, "52a1b2c3") || !strings.HasSuffix(got, "/disk-0.vmdk") || !strings.Contains(got, "esxi01.example.test") {
		t.Fatalf("URL redaction: %s", got)
	}
	u2, _ := url.Parse("https://user:pw@vc.example.test/sdk?ticket=abc")
	if got := URL(u2); strings.Contains(got, "pw") || strings.Contains(got, "ticket") {
		t.Fatalf("userinfo/query leaked: %s", got)
	}
}

func TestSecretsInErrors(t *testing.T) {
	s := NewSecrets()
	u, _ := url.Parse(ticketURL)
	s.AddURL(u)
	s.Add("hunter2hunter2")
	orig := fmt.Errorf("Get %q: dial tcp: connection refused (password was hunter2hunter2)", ticketURL)
	err := s.Error(orig)
	msg := err.Error()
	for _, leak := range []string{"52a1b2c3", "hunter2hunter2"} {
		if strings.Contains(msg, leak) {
			t.Fatalf("leaked %q in %q", leak, msg)
		}
	}
	if !errors.Is(err, orig) {
		t.Fatal("redacted error must unwrap to the original")
	}
}

func TestTextPatterns(t *testing.T) {
	in := `failed: GET https://10.0.0.5/nfc/5200aaaa-bbbb/disk-1.vmdk password=s3cr3t Cookie: vmware_soap_session="abcdef123456" https://admin:pw@vc/sdk`
	out := Text(in)
	for _, leak := range []string{"5200aaaa", "s3cr3t", "abcdef123456", "admin:pw"} {
		if strings.Contains(out, leak) {
			t.Fatalf("leaked %q in %q", leak, out)
		}
	}
	if !strings.Contains(out, "disk-1.vmdk") {
		t.Fatalf("lost useful context: %q", out)
	}
}

func TestHeaders(t *testing.T) {
	s := NewSecrets()
	h := http.Header{}
	h.Set("Content-Type", "application/x-vnd.vmware-streamVmdk")
	h.Set("Set-Cookie", "vmware_soap_session=abc")
	h.Set("X-Unknown", "something")
	h.Set("Content-Length", "123")
	got := s.Headers(h)
	if got["Set-Cookie"] != mask || got["X-Unknown"] != mask {
		t.Fatalf("sensitive headers not masked: %v", got)
	}
	if got["Content-Type"] != "application/x-vnd.vmware-streamVmdk" || got["Content-Length"] != "123" {
		t.Fatalf("safe headers altered: %v", got)
	}
}
