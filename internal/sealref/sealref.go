// Package sealref mints and opens sealed capability references for serving session
// files over HTTP. A ref is base64url(nonce || AES-256-GCM(payload)); the key is
// ephemeral (crypto/rand at process start), so refs do not survive a restart — the
// UI re-fetches and the server re-mints. The ref IS the capability: it carries the
// session identity, the file path, a content-type hint and a short expiry, all
// sealed so a client cannot forge or alter them. Possession of a fresh ref is the
// auth; an <img src> can present it without the UI's bearer header.
package sealref

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"time"
)

// Payload is the sealed capability content (compact JSON keys keep refs short).
type Payload struct {
	SessionKey  string `json:"sk"`
	Created     int64  `json:"c"`
	Path        string `json:"p"`
	ContentType string `json:"ct,omitempty"`
	Exp         int64  `json:"e"` // unix seconds (0 = no expiry; callers should set one)
}

// Sealer seals/opens payloads under one ephemeral AES-256-GCM key.
type Sealer struct{ aead cipher.AEAD }

// New creates a Sealer with a fresh random 256-bit key (ephemeral per process).
func New() (*Sealer, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: aead}, nil
}

// Seal returns base64url(nonce || ciphertext) for the payload.
func (s *Sealer) Seal(p Payload) (string, error) {
	plain, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := s.aead.Seal(nonce, nonce, plain, nil)
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

// ErrInvalid is returned for any malformed, tampered, foreign-key, or expired ref.
var ErrInvalid = errors.New("sealref: invalid or expired reference")

// Open decrypts and validates a ref against now. It never distinguishes failure
// causes (tamper vs expiry vs malformed) to a caller, by design.
func (s *Sealer) Open(ref string, now time.Time) (Payload, error) {
	raw, err := base64.RawURLEncoding.DecodeString(ref)
	if err != nil {
		return Payload{}, ErrInvalid
	}
	ns := s.aead.NonceSize()
	if len(raw) < ns {
		return Payload{}, ErrInvalid
	}
	plain, err := s.aead.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return Payload{}, ErrInvalid
	}
	var p Payload
	if err := json.Unmarshal(plain, &p); err != nil {
		return Payload{}, ErrInvalid
	}
	if p.Exp != 0 && now.Unix() > p.Exp {
		return Payload{}, ErrInvalid
	}
	return p, nil
}
