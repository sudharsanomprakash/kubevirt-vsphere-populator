// Package vsphere discovers the source VM and disk and manages the
// HttpNfcLease used to export it, using govmomi.
package vsphere

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/vmware/govmomi/session"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"

	"github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/redact"
)

// Environment variables and Secret keys used for credentials.
const (
	EnvUsername = "VSPHERE_USERNAME"
	EnvPassword = "VSPHERE_PASSWORD"

	KeyUsername   = "username"
	KeyPassword   = "password"
	KeyCABundle   = "ca.crt"
	KeyThumbprint = "thumbprint"
)

// ConnectionConfig describes how to reach and authenticate to vCenter or ESXi.
type ConnectionConfig struct {
	// Server is a host name, host:port or https URL. The /sdk path is implied.
	Server   string
	Username string
	Password string
	// CAFile is a PEM bundle used to verify the server (and NFC hosts).
	CAFile string
	// Thumbprint pins the server certificate by its SHA-1 or SHA-256
	// fingerprint (colon-separated hex, as shown by vSphere).
	Thumbprint string
	// InsecureSkipVerifyForTesting disables TLS verification. It must never
	// be used outside of test environments.
	InsecureSkipVerifyForTesting bool
	// TrustLeaseThumbprints accepts the per-device SSL thumbprints returned
	// by the lease (which arrive over the already-verified API connection)
	// for NFC hosts that are not trusted by CAFile.
	TrustLeaseThumbprints bool
	// UserAgent is sent with API requests.
	UserAgent string
}

// LoadCredentials fills Username/Password (and optional CA/thumbprint) from
// files in dir (a mounted Secret), falling back to environment variables.
// Values from files take precedence.
func (c *ConnectionConfig) LoadCredentials(dir string) error {
	read := func(key string) (string, bool, error) {
		if dir == "" {
			return "", false, nil
		}
		b, err := os.ReadFile(filepath.Join(dir, key))
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		if err != nil {
			return "", false, err
		}
		return strings.TrimRight(string(b), "\r\n"), true, nil
	}
	if v, ok, err := read(KeyUsername); err != nil {
		return err
	} else if ok {
		c.Username = v
	} else if c.Username == "" {
		c.Username = os.Getenv(EnvUsername)
	}
	if v, ok, err := read(KeyPassword); err != nil {
		return err
	} else if ok {
		c.Password = v
	} else if c.Password == "" {
		c.Password = os.Getenv(EnvPassword)
	}
	if c.CAFile == "" && dir != "" {
		if _, err := os.Stat(filepath.Join(dir, KeyCABundle)); err == nil {
			c.CAFile = filepath.Join(dir, KeyCABundle)
		}
	}
	if c.Thumbprint == "" {
		if v, ok, err := read(KeyThumbprint); err != nil {
			return err
		} else if ok {
			c.Thumbprint = strings.TrimSpace(v)
		}
	}
	if c.Username == "" || c.Password == "" {
		return errors.New("vSphere credentials are missing: provide username and password files or the VSPHERE_USERNAME/VSPHERE_PASSWORD environment variables")
	}
	return nil
}

// ServerURL returns the SDK URL for the configured server.
func (c *ConnectionConfig) ServerURL() (*url.URL, error) {
	s := strings.TrimSpace(c.Server)
	if s == "" {
		return nil, errors.New("server is empty")
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("invalid server %q: %w", c.Server, err)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("server must use https, got %q", u.Scheme)
	}
	if u.User != nil {
		return nil, errors.New("server URL must not contain credentials; use the credentials Secret")
	}
	if u.Host == "" {
		return nil, fmt.Errorf("invalid server %q", c.Server)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/sdk"
	}
	u.RawQuery, u.Fragment = "", ""
	return u, nil
}

// Client is an authenticated vSphere API session.
type Client struct {
	*vim25.Client
	cfg     ConnectionConfig
	Secrets *redact.Secrets
	manager *session.Manager
}

// Connect verifies TLS according to cfg, logs in and returns a client. The
// returned client's Secrets set contains the password and must be used to
// redact any text derived from requests.
func Connect(ctx context.Context, cfg ConnectionConfig) (*Client, error) {
	secrets := redact.NewSecrets()
	secrets.Add(cfg.Password)
	u, err := cfg.ServerURL()
	if err != nil {
		return nil, err
	}
	sc := soap.NewClient(u, cfg.InsecureSkipVerifyForTesting)
	if cfg.CAFile != "" {
		if err := sc.SetRootCAs(cfg.CAFile); err != nil {
			return nil, fmt.Errorf("load CA bundle: %w", err)
		}
	}
	if cfg.Thumbprint != "" {
		tp := normalizeThumbprint(cfg.Thumbprint)
		if tp == "" {
			return nil, fmt.Errorf("thumbprint %q is not a SHA-1 or SHA-256 fingerprint", cfg.Thumbprint)
		}
		sc.SetThumbprint(u.Host, tp)
	}
	if cfg.UserAgent != "" {
		sc.UserAgent = cfg.UserAgent
	}
	vc, err := vim25.NewClient(ctx, sc)
	if err != nil {
		return nil, secrets.Error(fmt.Errorf("connect to %s: %w", u.Host, describeTLSError(err)))
	}
	m := session.NewManager(vc)
	if err := m.Login(ctx, url.UserPassword(cfg.Username, cfg.Password)); err != nil {
		return nil, secrets.Error(fmt.Errorf("log in to %s as %s: %w", u.Host, cfg.Username, err))
	}
	return &Client{Client: vc, cfg: cfg, Secrets: secrets, manager: m}, nil
}

// Logout ends the session. Errors are ignored because the session is
// short-lived and expires on the server.
func (c *Client) Logout(ctx context.Context) { _ = c.manager.Logout(ctx) }

// APIType returns "VirtualCenter" or "HostAgent".
func (c *Client) APIType() string { return c.ServiceContent.About.ApiType }

// About returns the server's About information.
func (c *Client) About() types.AboutInfo { return c.ServiceContent.About }

func describeTLSError(err error) error {
	if soap.IsCertificateUntrusted(err) {
		return fmt.Errorf("%w (the server certificate is not trusted: supply a CA bundle or a verified thumbprint)", err)
	}
	return err
}

// normalizeThumbprint accepts colon-separated or plain hex SHA-1/SHA-256
// fingerprints and returns the upper-case colon-separated form used by govmomi.
func normalizeThumbprint(tp string) string {
	h := strings.ToUpper(strings.NewReplacer(":", "", " ", "").Replace(strings.TrimSpace(tp)))
	if len(h) != 40 && len(h) != 64 {
		return ""
	}
	for _, r := range h {
		if !strings.ContainsRune("0123456789ABCDEF", r) {
			return ""
		}
	}
	parts := make([]string, 0, len(h)/2)
	for i := 0; i < len(h); i += 2 {
		parts = append(parts, h[i:i+2])
	}
	return strings.Join(parts, ":")
}

// NormalizeThumbprint is exported for validation by callers.
func NormalizeThumbprint(tp string) string { return normalizeThumbprint(tp) }

// PrepareNFCURL resolves a lease device URL (substituting "*" with the host
// used for the API connection) and registers the lease-provided thumbprint
// for its host when allowed. The URL is added to the redaction set.
func (c *Client) PrepareNFCURL(dev types.HttpNfcLeaseDeviceUrl) (*url.URL, error) {
	u, err := c.ParseURL(dev.Url)
	if err != nil {
		return nil, errors.New("lease returned an unparsable device URL")
	}
	c.Secrets.AddURL(u)
	if u.Scheme != "https" {
		return nil, fmt.Errorf("lease device URL uses %q, only https is accepted", u.Scheme)
	}
	if dev.SslThumbprint != "" && c.cfg.TrustLeaseThumbprints {
		host := u.Host
		if _, _, err := net.SplitHostPort(host); err != nil {
			host = net.JoinHostPort(host, "443")
		}
		c.SetThumbprint(host, dev.SslThumbprint)
	}
	return u, nil
}
