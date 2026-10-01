package security

import (
	"strings"
	"testing"
)

func TestEncryptSecretRoundTrip(t *testing.T) {
	const master = "unit-test-master-key"

	sealed, err := EncryptSecret("hf_supersecret", master)
	if err != nil {
		t.Fatalf("EncryptSecret: %v", err)
	}

	if strings.Contains(sealed, "hf_supersecret") {
		t.Fatalf("the sealed value leaks the plaintext: %q", sealed)
	}

	opened, err := DecryptSecret(sealed, master)
	if err != nil {
		t.Fatalf("DecryptSecret: %v", err)
	}
	if opened != "hf_supersecret" {
		t.Fatalf("DecryptSecret = %q, want hf_supersecret", opened)
	}
}

func TestHexMasterKeyIsUsedAsIs(t *testing.T) {
	const master = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	sealed, err := EncryptSecret("value", master)
	if err != nil {
		t.Fatalf("EncryptSecret: %v", err)
	}

	opened, err := DecryptSecret(sealed, master)
	if err != nil {
		t.Fatalf("DecryptSecret: %v", err)
	}
	if opened != "value" {
		t.Fatalf("DecryptSecret = %q, want value", opened)
	}
}

func TestDecryptSecretRejectsWrongKeyAndTampering(t *testing.T) {
	sealed, err := EncryptSecret("sk-secret-value", "right-key")
	if err != nil {
		t.Fatalf("EncryptSecret: %v", err)
	}

	if _, err := DecryptSecret(sealed, "wrong-key"); err == nil {
		t.Fatal("a wrong master key must not open the secret")
	}

	tampered := "v1:" + sealed[len("v1:")+1:]
	if _, err := DecryptSecret(tampered, "right-key"); err == nil {
		t.Fatal("a modified blob must not open")
	}

	if _, err := DecryptSecret("garbage", "right-key"); err == nil {
		t.Fatal("a malformed blob must not open")
	}
}

func TestEncryptSecretRequiresMasterKey(t *testing.T) {
	if _, err := EncryptSecret("value", "   "); err == nil {
		t.Fatal("an empty master key must be refused")
	}
}
