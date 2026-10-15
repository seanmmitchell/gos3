package crypt

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

// ObjectDataKey derives the stream key of the data blob (or part blob) with
// the given ID from a bucket master key:
//
//	HKDF-SHA256(secret = master, salt = none, info = "gos3-object-data-v1/" + dataID)
//
// truncated to KeySize bytes. Binding every stream to its immutable data
// identity makes ciphertext substituted between two objects of the same
// bucket fail authentication, and gives every stream its own key (see the
// package documentation). The derivation is part of the on-disk format of
// encrypted fs and s3 backings and must never change.
func ObjectDataKey(master []byte, dataID string) []byte {
	key, err := hkdf.Key(sha256.New, master, nil, "gos3-object-data-v1/"+dataID, KeySize)
	if err != nil {
		// Only possible for lengths above 255*sha256.Size.
		panic(err)
	}
	return key
}

// Key wrapping.
//
// Wrap seals a key (typically a bucket master key) under a 32-byte
// key-encryption key (KEK) with AES-256-GCM. The wrapped form is
//
//	version (1 byte, wrapVersion = 1) || nonce (12 random bytes) || ciphertext || GCM tag (16 bytes)
//
// so it is len(key) + WrapOverhead bytes. The GCM additional data is
//
//	"gos3-wrap-v1" || 0x00 || aad
//
// where aad is chosen by the caller to bind the wrapped key to its use
// (for example the ID of the bucket it belongs to): unwrapping with a
// different aad fails like a wrong KEK does. Random nonces are safe here
// because a KEK wraps few keys (far below the 2^32 messages GCM allows per
// key with random 96-bit nonces).
const (
	// WrapOverhead is the number of bytes Wrap adds to a key.
	WrapOverhead = 1 + nonceSize + Overhead

	wrapVersion = 1
)

// wrapLabel prefixes the additional data of wrapped keys, separating it
// from every other use of a KEK.
const wrapLabel = "gos3-wrap-v1\x00"

// ErrUnwrap is wrapped by Unwrap errors caused by a wrong KEK, wrong
// additional data or a damaged or unsupported wrapped key.
var ErrUnwrap = errors.New("crypt: cannot unwrap key (wrong key-encryption key or damaged data)")

// kekAEAD returns the AES-256-GCM AEAD of a KEK.
func kekAEAD(kek []byte) (cipher.AEAD, error) {
	if len(kek) != KeySize {
		return nil, fmt.Errorf("crypt: key-encryption key has %d bytes, want %d", len(kek), KeySize)
	}
	block, err := aes.NewCipher(kek)
	if err != nil {
		return nil, fmt.Errorf("crypt: %w", err)
	}
	return cipher.NewGCM(block)
}

// wrapAAD returns the GCM additional data for the caller's aad.
func wrapAAD(aad []byte) []byte {
	return append([]byte(wrapLabel), aad...)
}

// Wrap seals key under kek (KeySize bytes) bound to aad, in the format
// described above.
func Wrap(kek, key, aad []byte) ([]byte, error) {
	aead, err := kekAEAD(kek)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 1+nonceSize, len(key)+WrapOverhead)
	out[0] = wrapVersion
	if _, err := rand.Read(out[1 : 1+nonceSize]); err != nil {
		return nil, fmt.Errorf("crypt: nonce: %w", err)
	}
	return aead.Seal(out, out[1:1+nonceSize], key, wrapAAD(aad)), nil
}

// Unwrap opens a key sealed by Wrap with the same kek and aad. A wrong kek
// or aad, or damaged or truncated data, returns an error wrapping
// ErrUnwrap; a kek of the wrong length returns a plain error.
func Unwrap(kek, wrapped, aad []byte) ([]byte, error) {
	aead, err := kekAEAD(kek)
	if err != nil {
		return nil, err
	}
	if len(wrapped) < WrapOverhead {
		return nil, fmt.Errorf("%w: wrapped key is too short (%d bytes)", ErrUnwrap, len(wrapped))
	}
	if wrapped[0] != wrapVersion {
		return nil, fmt.Errorf("%w: unsupported wrapped key version %d", ErrUnwrap, wrapped[0])
	}
	key, err := aead.Open(nil, wrapped[1:1+nonceSize], wrapped[1+nonceSize:], wrapAAD(aad))
	if err != nil {
		return nil, ErrUnwrap
	}
	return key, nil
}

// KeyCheck returns a short public fingerprint of a KEK,
//
//	hex(HMAC-SHA256(kek, "gos3-kek-check-v1")[:16])
//
// which can be stored next to wrapped keys to tell a wrong KEK apart from
// damaged data without revealing anything useful about the KEK.
func KeyCheck(kek []byte) string {
	m := hmac.New(sha256.New, kek)
	m.Write([]byte("gos3-kek-check-v1"))
	return hex.EncodeToString(m.Sum(nil)[:16])
}
