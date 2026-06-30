// Package portable defines the on-disk format for exporting and importing
// em-wall rules and custom groups, plus the passphrase-based encryption that
// wraps every bundle. Exports are ALWAYS encrypted: Seal refuses an empty
// passphrase and Open requires the same one to recover the bundle.
//
// Format: the plaintext Bundle is JSON-marshalled, then sealed with
// AES-256-GCM. The key is derived from the passphrase with PBKDF2-HMAC-SHA256
// over a random per-export salt. The ciphertext, salt, and nonce travel in a
// JSON Envelope — itself the bytes written to the .embackup file.
package portable

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

const (
	// BundleSchema tags the plaintext payload so future format changes can
	// be detected on import.
	BundleSchema = "em-wall.bundle.v1"
	// EnvelopeSchema tags the encrypted wrapper.
	EnvelopeSchema = "em-wall.enc.v1"
	// KDFName identifies the key-derivation function in the envelope.
	KDFName = "pbkdf2-sha256"
	// PBKDF2Iterations is the work factor. High enough to make offline
	// guessing of a weak passphrase expensive; cheap enough for a one-shot
	// import on a laptop.
	PBKDF2Iterations = 600_000

	saltLen = 16
	keyLen  = 32 // AES-256
)

var (
	// ErrEmptyPassphrase is returned by Seal/Open when no passphrase is
	// given. Exports are always encrypted, so a passphrase is mandatory.
	ErrEmptyPassphrase = errors.New("portable: passphrase required")
	// ErrWrongPassphrase is returned by Open when decryption fails — almost
	// always a bad passphrase, but also a truncated/corrupted file.
	ErrWrongPassphrase = errors.New("portable: wrong passphrase or corrupt file")
	// ErrBadEnvelope is returned when the outer JSON isn't a recognised
	// encrypted em-wall export.
	ErrBadEnvelope = errors.New("portable: not an em-wall export file")
	// ErrBadSchema is returned when the decrypted bundle has an unknown
	// schema tag.
	ErrBadSchema = errors.New("portable: unsupported bundle schema")
)

// Bundle is the plaintext export payload.
type Bundle struct {
	Schema       string        `json:"schema"`
	ExportedAt   string        `json:"exportedAt"` // RFC3339, stamped by the caller
	AppVersion   string        `json:"appVersion"`
	Rules        []BundleRule  `json:"rules"`
	CustomGroups []BundleGroup `json:"customGroups"`
}

// BundleRule is a rule stripped of DB identity (ID/timestamps). Interface
// strings (proxy:NAME / xray:NAME / utunN) are carried verbatim; the import
// target keeps them as-is even if the referenced outbound is absent.
type BundleRule struct {
	Pattern   string `json:"pattern"`
	Action    string `json:"action"`
	Interface string `json:"interface"`
	Enabled   bool   `json:"enabled"`
}

// BundleGroup is a custom group definition, portable across machines.
type BundleGroup struct {
	Key         string   `json:"key"`
	DisplayName string   `json:"displayName"`
	Description string   `json:"description"`
	Patterns    []string `json:"patterns"`
	Color       string   `json:"color"`
}

// envelope is the encrypted wrapper actually written to disk.
type envelope struct {
	Schema     string `json:"schema"`
	KDF        string `json:"kdf"`
	Iterations int    `json:"iterations"`
	Salt       string `json:"salt"`       // base64
	Nonce      string `json:"nonce"`      // base64
	Ciphertext string `json:"ciphertext"` // base64 (includes GCM tag)
}

// Seal marshals b, encrypts it under passphrase, and returns the envelope
// JSON bytes ready to write to a file.
func Seal(b Bundle, passphrase string) ([]byte, error) {
	if passphrase == "" {
		return nil, ErrEmptyPassphrase
	}
	if b.Schema == "" {
		b.Schema = BundleSchema
	}
	plaintext, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("marshal bundle: %w", err)
	}

	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("salt: %w", err)
	}
	key, err := pbkdf2.Key(sha256.New, passphrase, salt, PBKDF2Iterations, keyLen)
	if err != nil {
		return nil, fmt.Errorf("derive key: %w", err)
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("nonce: %w", err)
	}
	ciphertext := gcm.Seal(nil, nonce, plaintext, nil)

	env := envelope{
		Schema:     EnvelopeSchema,
		KDF:        KDFName,
		Iterations: PBKDF2Iterations,
		Salt:       base64.StdEncoding.EncodeToString(salt),
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
		Ciphertext: base64.StdEncoding.EncodeToString(ciphertext),
	}
	out, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal envelope: %w", err)
	}
	return out, nil
}

// Open decrypts envelope bytes with passphrase and returns the Bundle.
func Open(data []byte, passphrase string) (Bundle, error) {
	if passphrase == "" {
		return Bundle{}, ErrEmptyPassphrase
	}
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil || env.Schema != EnvelopeSchema {
		return Bundle{}, ErrBadEnvelope
	}
	salt, err := base64.StdEncoding.DecodeString(env.Salt)
	if err != nil {
		return Bundle{}, ErrBadEnvelope
	}
	nonce, err := base64.StdEncoding.DecodeString(env.Nonce)
	if err != nil {
		return Bundle{}, ErrBadEnvelope
	}
	ciphertext, err := base64.StdEncoding.DecodeString(env.Ciphertext)
	if err != nil {
		return Bundle{}, ErrBadEnvelope
	}
	iter := env.Iterations
	if iter <= 0 {
		iter = PBKDF2Iterations
	}
	key, err := pbkdf2.Key(sha256.New, passphrase, salt, iter, keyLen)
	if err != nil {
		return Bundle{}, fmt.Errorf("derive key: %w", err)
	}
	gcm, err := newGCM(key)
	if err != nil {
		return Bundle{}, err
	}
	if len(nonce) != gcm.NonceSize() {
		return Bundle{}, ErrBadEnvelope
	}
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return Bundle{}, ErrWrongPassphrase
	}
	var b Bundle
	if err := json.Unmarshal(plaintext, &b); err != nil {
		return Bundle{}, ErrWrongPassphrase
	}
	if b.Schema != BundleSchema {
		return Bundle{}, ErrBadSchema
	}
	return b, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm: %w", err)
	}
	return gcm, nil
}
