// Package security provides SSRF validation, safe HTTP clients, path traversal
// prevention, and secret redaction.
package security

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var (
	// ErrInvalidURL indicates a malformed or disallowed URL.
	ErrInvalidURL = errors.New("invalid or disallowed URL")
	// ErrPrivateAddress indicates an SSRF attempt against a private or internal address.
	ErrPrivateAddress = errors.New("access to private or internal network address is forbidden")
	// ErrPathTraversal indicates an attempt to escape a base directory.
	ErrPathTraversal = errors.New("path traversal detected")
)

var (
	cgnatNet     = mustCIDR("100.64.0.0/10")
	benchmarkNet = mustCIDR("198.18.0.0/15")

	botTokenPattern = regexp.MustCompile(`\b\d{8,12}:[A-Za-z0-9_-]{30,50}\b`)
	apiKeyPattern   = regexp.MustCompile(`\b(?:sk-or-v1-|sk-|gsk_|nvapi-)[A-Za-z0-9_-]{12,}\b`)
)

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

// IsPrivateIP reports whether ip belongs to loopback, private RFC1918,
// link-local, metadata, CGNAT, multicast, or unspecified ranges.
func IsPrivateIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() ||
		ip.IsUnspecified() {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		if cgnatNet.Contains(v4) || benchmarkNet.Contains(v4) {
			return true
		}
		// 0.0.0.0/8
		if v4[0] == 0 {
			return true
		}
	}
	return false
}

// ValidateExternalURL parses rawURL and verifies that it targets a public
// HTTP/HTTPS endpoint and not a local/internal/metadata host.
func ValidateExternalURL(rawURL string) (*url.URL, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, fmt.Errorf("%w: empty URL", ErrInvalidURL)
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidURL, err)
	}

	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, fmt.Errorf("%w: scheme %q not allowed", ErrInvalidURL, parsed.Scheme)
	}

	if parsed.User != nil {
		return nil, fmt.Errorf("%w: userinfo not allowed in URL", ErrInvalidURL)
	}

	host := strings.ToLower(strings.TrimSpace(parsed.Hostname()))
	if host == "" {
		return nil, fmt.Errorf("%w: missing host", ErrInvalidURL)
	}

	if host == "localhost" ||
		strings.HasSuffix(host, ".localhost") ||
		strings.HasSuffix(host, ".local") ||
		strings.HasSuffix(host, ".internal") ||
		host == "metadata.google.internal" ||
		host == "169.254.169.254" {
		return nil, fmt.Errorf("%w: host %q is internal", ErrPrivateAddress, host)
	}

	if ip := net.ParseIP(host); ip != nil {
		if IsPrivateIP(ip) {
			return nil, fmt.Errorf("%w: IP %s is internal", ErrPrivateAddress, ip.String())
		}
	}

	return parsed, nil
}

// SafeHTTPClient returns an HTTP client hardened against SSRF (including DNS
// rebinding and redirect-based SSRF).
func SafeHTTPClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}

	dialer := &net.Dialer{
		Timeout:   8 * time.Second,
		KeepAlive: 15 * time.Second,
	}

	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}

			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			if len(ips) == 0 {
				return nil, fmt.Errorf("no IP addresses resolved for %s", host)
			}

			for _, ipAddr := range ips {
				if IsPrivateIP(ipAddr.IP) {
					return nil, fmt.Errorf("%w: resolved %s to %s", ErrPrivateAddress, host, ipAddr.IP.String())
				}
			}

			return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
		},
		TLSHandshakeTimeout:   8 * time.Second,
		ResponseHeaderTimeout: timeout,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       30 * time.Second,
	}

	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("stopped after 5 redirects")
			}
			_, err := ValidateExternalURL(req.URL.String())
			return err
		},
	}
}

// SafePath resolves relPath inside baseDir and rejects any path that escapes
// baseDir via ".." or absolute path components.
func SafePath(baseDir, relPath string) (string, error) {
	if strings.TrimSpace(baseDir) == "" {
		return "", fmt.Errorf("%w: empty base directory", ErrPathTraversal)
	}
	if strings.ContainsRune(relPath, '\x00') {
		return "", fmt.Errorf("%w: null byte in path", ErrPathTraversal)
	}

	absBase, err := filepath.Abs(filepath.Clean(baseDir))
	if err != nil {
		return "", err
	}

	cleanedRel := filepath.Clean("/" + strings.TrimLeft(relPath, `/\`))
	target := filepath.Join(absBase, cleanedRel)
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}

	if absTarget != absBase && !strings.HasPrefix(absTarget, absBase+string(filepath.Separator)) {
		return "", ErrPathTraversal
	}

	// Also reject explicit ".." segments in the raw input.
	for _, part := range strings.FieldsFunc(relPath, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." {
			return "", ErrPathTraversal
		}
	}

	return absTarget, nil
}

// SanitizeFilename strips directory separators, control characters, and leading
// dots from an untrusted filename.
func SanitizeFilename(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	var b strings.Builder
	for _, r := range name {
		switch {
		case r < 32 || r == 127:
			continue
		case r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' || r == '"' || r == '<' || r == '>' || r == '|':
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimLeft(strings.TrimSpace(b.String()), ".")
	if out == "" {
		return "file"
	}
	if len(out) > 128 {
		out = out[:128]
	}
	return out
}

// RedactSecrets replaces any occurrence of known secret values or common API
// token patterns with "[REDACTED]".
func RedactSecrets(text string, secrets ...string) string {
	if text == "" {
		return ""
	}
	out := text
	for _, s := range secrets {
		s = strings.TrimSpace(s)
		if len(s) >= 6 {
			out = strings.ReplaceAll(out, s, "[REDACTED]")
		}
	}
	out = botTokenPattern.ReplaceAllString(out, "[REDACTED_BOT_TOKEN]")
	out = apiKeyPattern.ReplaceAllString(out, "[REDACTED_API_KEY]")
	return out
}
