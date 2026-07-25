// Package secretcrypt encrypts a tenant-supplied secret (an Anthropic/
// OpenAI API key, so far — see internal/kernel/aiprovider's doc comment
// on the BYOK plugin model this exists for) before it's stored via the
// generic entity/crud machinery every other field in this kernel goes
// through unencrypted. A real customer's own third-party API key is
// meaningfully different from ordinary tenant business data (a vendor
// name, an order total): it's a credential that could run up real
// charges or expose customer data on their own account if it leaked
// from a DB backup, a replica, or an admin's raw SQL query — this
// package is the one place that gap gets closed for it.
//
// Deliberately narrow: this is not a generic new entity.FieldType (a
// "FieldSecret" the whole entity/crud/form pipeline would need to know
// about) — there's exactly one caller today (internal/api's AI-provider
// settings handler), and a generic kernel-wide secret-field mechanism
// for a single caller would be exactly the premature abstraction
// CLAUDE.md warns against. If a second, unrelated feature ever needs
// the same kind of encrypted-at-rest field, generalize then, against
// two real examples instead of one imagined one.
package secretcrypt

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
)

// keySize is AES-256's required key length in bytes.
const keySize = 32

// Cryptor encrypts/decrypts with one AES-256-GCM key. The zero value is
// unusable — construct via NewCryptor, which is also where "no
// encryption key configured" becomes a legitimate, always-safe disabled
// state (see Enabled) rather than a Cryptor that silently produces
// garbage or, worse, stores plaintext.
type Cryptor struct {
	key []byte
}

// NewCryptor builds a Cryptor from base64Key, a base64-encoded 32-byte
// AES-256 key (e.g. from the SECRET_ENCRYPTION_KEY environment
// variable — internal/api's own caller reads that, this package never
// reads an environment variable itself, same "explicit config in,
// nothing implicit" discipline every other client constructor in this
// kernel holds to).
//
// base64Key == "" returns (nil, nil) — a legitimately disabled Cryptor,
// not an error: a deployment that never configures this feature at all
// shouldn't be forced to set an encryption key it has no use for yet.
// Any other malformed value (bad base64, wrong decoded length) is a
// real configuration error and returns one loudly — silently accepting
// a wrong-sized key would either panic deep inside crypto/aes or
// silently truncate/pad it into a different, undocumented key, neither
// of which "fail loud enough that a deployer notices before storing a
// real customer secret with it" allows for.
func NewCryptor(base64Key string) (*Cryptor, error) {
	if base64Key == "" {
		return nil, nil
	}
	key, err := base64.StdEncoding.DecodeString(base64Key)
	if err != nil {
		return nil, fmt.Errorf("secretcrypt: SECRET_ENCRYPTION_KEY is not valid base64: %w", err)
	}
	if len(key) != keySize {
		return nil, fmt.Errorf("secretcrypt: SECRET_ENCRYPTION_KEY must decode to exactly %d bytes (AES-256), got %d", keySize, len(key))
	}
	return &Cryptor{key: key}, nil
}

// Enabled reports whether c is usable — false for a nil *Cryptor, same
// nil-receiver ergonomic every client in this kernel's AI-plugin family
// (aiprovider.Provider's implementations) establishes, so a caller can
// write `if cryptor.Enabled() { ... }` without a separate nil check.
func (c *Cryptor) Enabled() bool {
	return c != nil && len(c.key) == keySize
}

// Encrypt returns plaintext encrypted with AES-256-GCM, as a single
// base64-encoded string (a random 12-byte nonce prepended to the
// ciphertext, GCM's standard "seal" convention) — the shape Decrypt
// expects back. A fresh random nonce every call, never reused for the
// same key, is what makes GCM's authenticated-encryption guarantee
// actually hold.
func (c *Cryptor) Encrypt(plaintext string) (string, error) {
	if !c.Enabled() {
		return "", fmt.Errorf("secretcrypt: no encryption key configured")
	}
	block, err := aes.NewCipher(c.key)
	if err != nil {
		return "", fmt.Errorf("secretcrypt: build cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("secretcrypt: build GCM: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("secretcrypt: generate nonce: %w", err)
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt reverses Encrypt. A ciphertext that doesn't decrypt (wrong
// key, corrupted/truncated data, or genuinely not something this
// Cryptor encrypted) is reported as a plain error — never a panic, and
// never a partial/garbage plaintext returned alongside the error.
func (c *Cryptor) Decrypt(ciphertext string) (string, error) {
	if !c.Enabled() {
		return "", fmt.Errorf("secretcrypt: no encryption key configured")
	}
	sealed, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", fmt.Errorf("secretcrypt: ciphertext is not valid base64: %w", err)
	}
	block, err := aes.NewCipher(c.key)
	if err != nil {
		return "", fmt.Errorf("secretcrypt: build cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("secretcrypt: build GCM: %w", err)
	}
	nonceSize := gcm.NonceSize()
	if len(sealed) < nonceSize {
		return "", fmt.Errorf("secretcrypt: ciphertext too short")
	}
	nonce, encrypted := sealed[:nonceSize], sealed[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, encrypted, nil)
	if err != nil {
		return "", fmt.Errorf("secretcrypt: decrypt failed: %w", err)
	}
	return string(plaintext), nil
}
