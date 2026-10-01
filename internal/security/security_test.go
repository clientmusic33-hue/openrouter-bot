package security

import (
	"net"
	"strings"
	"testing"
)

func TestValidateExternalURLBlocksSSRF(t *testing.T) {
	blocked := []string{
		"",
		"ftp://example.com/file",
		"file:///etc/passwd",
		"http://localhost:8080/admin",
		"http://127.0.0.1/secret",
		"http://[::1]:8080/",
		"http://10.0.0.1/internal",
		"http://172.16.5.4/internal",
		"http://192.168.1.1/router",
		"http://169.254.169.254/latest/meta-data/",
		"http://100.64.0.1/cgnat",
		"http://0.0.0.0/",
		"http://metadata.google.internal/computeMetadata/v1/",
		"http://service.internal/api",
		"https://user:pass@example.com/",
	}

	for _, u := range blocked {
		if _, err := ValidateExternalURL(u); err == nil {
			t.Errorf("ValidateExternalURL(%q) should have failed", u)
		}
	}

	allowed := []string{
		"https://example.com/article?id=1",
		"http://93.184.216.34/index.html",
		"https://api.github.com/repos/golang/go",
	}

	for _, u := range allowed {
		if _, err := ValidateExternalURL(u); err != nil {
			t.Errorf("ValidateExternalURL(%q) failed unexpectedly: %v", u, err)
		}
	}
}

func TestIsPrivateIP(t *testing.T) {
	if !IsPrivateIP(net.ParseIP("127.0.0.1")) {
		t.Error("127.0.0.1 must be private")
	}
	if !IsPrivateIP(net.ParseIP("169.254.169.254")) {
		t.Error("169.254.169.254 must be private")
	}
	if IsPrivateIP(net.ParseIP("8.8.8.8")) {
		t.Error("8.8.8.8 must be public")
	}
}

func TestSafePathPreventsTraversal(t *testing.T) {
	base := t.TempDir()

	if _, err := SafePath(base, "../etc/passwd"); err == nil {
		t.Error("expected traversal error for ../etc/passwd")
	}
	if _, err := SafePath(base, "sub/../../outside"); err == nil {
		t.Error("expected traversal error for sub/../../outside")
	}

	good, err := SafePath(base, "users/123.json")
	if err != nil {
		t.Fatalf("SafePath valid path failed: %v", err)
	}
	if !strings.HasPrefix(good, base) {
		t.Errorf("SafePath returned %q outside %q", good, base)
	}
}

func TestRedactSecrets(t *testing.T) {
	raw := "token=123456789:AAHfiqksKZ8WmR2zP1xY9aBcDeFgHiJkLmN key=sk-or-v1-1234567890abcdef custom=mySuperSecret123"
	got := RedactSecrets(raw, "mySuperSecret123")

	for _, leak := range []string{"AAHfiqksKZ8WmR2zP1xY9aBcDeFgHiJkLmN", "sk-or-v1-1234567890abcdef", "mySuperSecret123"} {
		if strings.Contains(got, leak) {
			t.Errorf("RedactSecrets leaked %q in %q", leak, got)
		}
	}
}
