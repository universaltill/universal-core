package secretcrypt

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
)

func testKey(t *testing.T) string {
	t.Helper()
	key := make([]byte, keySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("generate test key: %v", err)
	}
	return base64.StdEncoding.EncodeToString(key)
}

func TestNewCryptor_EmptyKeyIsDisabledNotAnError(t *testing.T) {
	c, err := NewCryptor("")
	if err != nil {
		t.Fatalf("expected no error for an unconfigured (empty) key, got %v", err)
	}
	if c.Enabled() {
		t.Fatal("expected a nil Cryptor (empty key) to report Enabled() == false")
	}
}

func TestNewCryptor_InvalidBase64IsAnError(t *testing.T) {
	if _, err := NewCryptor("not valid base64!!!"); err == nil {
		t.Fatal("expected an error for a malformed base64 key")
	}
}

func TestNewCryptor_WrongLengthKeyIsAnError(t *testing.T) {
	shortKey := base64.StdEncoding.EncodeToString([]byte("too short"))
	if _, err := NewCryptor(shortKey); err == nil {
		t.Fatal("expected an error for a key that doesn't decode to exactly 32 bytes")
	}
}

func TestNewCryptor_ValidKeyIsEnabled(t *testing.T) {
	c, err := NewCryptor(testKey(t))
	if err != nil {
		t.Fatalf("NewCryptor: %v", err)
	}
	if !c.Enabled() {
		t.Fatal("expected a Cryptor built from a valid 32-byte key to report Enabled() == true")
	}
}

// TestEncryptDecrypt_RoundTrips is the core correctness proof: a real
// secret (an API-key-shaped string) survives Encrypt then Decrypt
// unchanged.
func TestEncryptDecrypt_RoundTrips(t *testing.T) {
	c, err := NewCryptor(testKey(t))
	if err != nil {
		t.Fatalf("NewCryptor: %v", err)
	}
	plaintext := "sk-ant-api03-fake-secret-key-value-1234567890"
	ciphertext, err := c.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if ciphertext == plaintext {
		t.Fatal("expected Encrypt to actually transform the plaintext, got it back unchanged")
	}
	if strings.Contains(ciphertext, plaintext) {
		t.Fatal("expected the plaintext to not appear verbatim inside the ciphertext")
	}
	got, err := c.Decrypt(ciphertext)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if got != plaintext {
		t.Fatalf("expected round-trip to recover %q, got %q", plaintext, got)
	}
}

// TestEncrypt_DifferentEachCall confirms a fresh nonce is actually used
// every call — encrypting the same plaintext twice must not produce the
// same ciphertext (a static/reused nonce would break GCM's
// authenticated-encryption guarantee, a real cryptographic bug, not a
// cosmetic one).
func TestEncrypt_DifferentEachCall(t *testing.T) {
	c, err := NewCryptor(testKey(t))
	if err != nil {
		t.Fatalf("NewCryptor: %v", err)
	}
	a, err := c.Encrypt("same plaintext")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	b, err := c.Encrypt("same plaintext")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if a == b {
		t.Fatal("expected two Encrypt calls on identical plaintext to produce different ciphertext (fresh nonce each time)")
	}
}

// TestDecrypt_WrongKeyFails confirms a ciphertext encrypted under one
// key cannot be decrypted with a different key — the whole point of
// this package existing rather than, say, reversible obfuscation.
func TestDecrypt_WrongKeyFails(t *testing.T) {
	c1, err := NewCryptor(testKey(t))
	if err != nil {
		t.Fatalf("NewCryptor: %v", err)
	}
	c2, err := NewCryptor(testKey(t))
	if err != nil {
		t.Fatalf("NewCryptor: %v", err)
	}
	ciphertext, err := c1.Encrypt("a real secret")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := c2.Decrypt(ciphertext); err == nil {
		t.Fatal("expected decrypting with the wrong key to fail")
	}
}

func TestDecrypt_CorruptedCiphertextFails(t *testing.T) {
	c, err := NewCryptor(testKey(t))
	if err != nil {
		t.Fatalf("NewCryptor: %v", err)
	}
	ciphertext, err := c.Encrypt("a real secret")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	corrupted := ciphertext[:len(ciphertext)-4] + "abcd"
	if _, err := c.Decrypt(corrupted); err == nil {
		t.Fatal("expected decrypting corrupted ciphertext to fail, not return garbage")
	}
}

func TestEncrypt_DisabledCryptorErrors(t *testing.T) {
	var c *Cryptor
	if _, err := c.Encrypt("secret"); err == nil {
		t.Fatal("expected an error calling Encrypt on a disabled (nil) Cryptor")
	}
}

func TestDecrypt_DisabledCryptorErrors(t *testing.T) {
	var c *Cryptor
	if _, err := c.Decrypt("anything"); err == nil {
		t.Fatal("expected an error calling Decrypt on a disabled (nil) Cryptor")
	}
}
