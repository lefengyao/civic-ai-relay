package secret

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
)

func testEncodedKey(t *testing.T) string {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(key)
}

func TestBoxUsesRandomNonceAndRoundTrips(t *testing.T) {
	box, err := New(testEncodedKey(t))
	if err != nil {
		t.Fatal(err)
	}
	a, err := box.Seal("provider-secret")
	if err != nil {
		t.Fatal(err)
	}
	b, err := box.Seal("provider-secret")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("nonce was reused")
	}
	if strings.Contains(a, "provider-secret") {
		t.Fatal("plaintext appears in ciphertext")
	}
	got, err := box.Open(a)
	if err != nil || got != "provider-secret" {
		t.Fatalf("Open() = %q, %v", got, err)
	}
}

func TestBoxRejectsTampering(t *testing.T) {
	box, err := New(testEncodedKey(t))
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := box.Seal("secret")
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	decoded[len(decoded)-1] ^= 1
	if _, err := box.Open(base64.StdEncoding.EncodeToString(decoded)); err == nil {
		t.Fatal("tampered ciphertext was accepted")
	}
}

func TestNewRejectsInvalidKeys(t *testing.T) {
	for _, encoded := range []string{"not-base64", base64.StdEncoding.EncodeToString(make([]byte, 16)), base64.RawStdEncoding.EncodeToString(make([]byte, 32))} {
		if _, err := New(encoded); err == nil {
			t.Fatalf("New(%q) accepted invalid key", encoded)
		}
	}
}

func TestDigestIsKeyedAndStable(t *testing.T) {
	box, err := New(testEncodedKey(t))
	if err != nil {
		t.Fatal(err)
	}
	first := box.Digest("crk_example")
	if first == "" || first == "crk_example" {
		t.Fatal("digest is empty or plaintext")
	}
	if first != box.Digest("crk_example") {
		t.Fatal("digest is not stable")
	}
}
