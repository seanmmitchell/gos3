package crypt

import (
	"bytes"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
)

func seqKey(n int) []byte {
	k := make([]byte, n)
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

// TestObjectDataKey pins the derivation: it must stay byte-for-byte equal
// to the one the fs backend has always used for its data files
// (hkdf.Key(sha256.New, master, nil, "gos3-object-data-v1/"+id, KeySize)),
// recomputed here from RFC 5869 by hand and checked against a fixed vector.
func TestObjectDataKey(t *testing.T) {
	master := seqKey(32)
	const id = "0123456789abcdef"
	got := ObjectDataKey(master, id)

	// The fs formula.
	fsKey, err := hkdf.Key(sha256.New, master, nil, "gos3-object-data-v1/"+id, KeySize)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, fsKey) {
		t.Fatalf("ObjectDataKey = %x, fs formula gives %x", got, fsKey)
	}

	// RFC 5869 by hand: PRK = HMAC(zero salt, master); OKM = HMAC(PRK, info || 0x01).
	ext := hmac.New(sha256.New, make([]byte, sha256.Size))
	ext.Write(master)
	exp := hmac.New(sha256.New, ext.Sum(nil))
	exp.Write([]byte("gos3-object-data-v1/" + id))
	exp.Write([]byte{1})
	if manual := exp.Sum(nil)[:KeySize]; !bytes.Equal(got, manual) {
		t.Fatalf("ObjectDataKey = %x, RFC 5869 gives %x", got, manual)
	}

	const want = "b4a7d612f3b9ec80be0a0d9993c5d467c2d757d99963971119ae100123a978fc"
	if hex.EncodeToString(got) != want {
		t.Fatalf("ObjectDataKey = %x, want %s", got, want)
	}
	if bytes.Equal(got, ObjectDataKey(master, id+"x")) {
		t.Fatal("different IDs give the same key")
	}
}

func TestWrapRoundTrip(t *testing.T) {
	kek := newKey(t)
	for _, n := range []int{0, 1, 16, KeySize, 100} {
		key := randBytes(t, n)
		w, err := Wrap(kek, key, []byte("bucket-1"))
		if err != nil {
			t.Fatal(err)
		}
		if len(w) != n+WrapOverhead || w[0] != wrapVersion {
			t.Fatalf("wrapped %d bytes into %d (version %d)", n, len(w), w[0])
		}
		got, err := Unwrap(kek, w, []byte("bucket-1"))
		if err != nil {
			t.Fatalf("Unwrap: %v", err)
		}
		if !bytes.Equal(got, key) {
			t.Fatalf("Unwrap = %x, want %x", got, key)
		}
	}
	// Random nonces: wrapping twice differs.
	key := randBytes(t, KeySize)
	a, _ := Wrap(kek, key, nil)
	b, _ := Wrap(kek, key, nil)
	if bytes.Equal(a, b) {
		t.Fatal("two wraps of one key are identical")
	}
}

func TestUnwrapFailures(t *testing.T) {
	kek := newKey(t)
	key := randBytes(t, KeySize)
	aad := []byte("bucket-1")
	w, err := Wrap(kek, key, aad)
	if err != nil {
		t.Fatal(err)
	}
	flip := func(i int) []byte {
		c := bytes.Clone(w)
		c[i] ^= 1
		return c
	}
	cases := map[string]struct {
		kek, wrapped, aad []byte
	}{
		"wrong kek":     {newKey(t), w, aad},
		"wrong aad":     {kek, w, []byte("bucket-2")},
		"nil aad":       {kek, w, nil},
		"version":       {kek, flip(0), aad},
		"nonce":         {kek, flip(1), aad},
		"ciphertext":    {kek, flip(1 + nonceSize), aad},
		"tag":           {kek, flip(len(w) - 1), aad},
		"truncated":     {kek, w[:len(w)-1], aad},
		"too short":     {kek, w[:WrapOverhead-1], aad},
		"empty":         {kek, nil, aad},
		"appended byte": {kek, append(bytes.Clone(w), 0), aad},
	}
	for name, c := range cases {
		got, err := Unwrap(c.kek, c.wrapped, c.aad)
		if !errors.Is(err, ErrUnwrap) || got != nil {
			t.Errorf("%s: Unwrap = %x, %v; want ErrUnwrap", name, got, err)
		}
	}
}

func TestWrapKEKLength(t *testing.T) {
	for _, n := range []int{0, 16, 24, 31, 33} {
		if _, err := Wrap(make([]byte, n), []byte("k"), nil); err == nil {
			t.Errorf("Wrap with a %d-byte KEK succeeded", n)
		}
		_, err := Unwrap(make([]byte, n), make([]byte, 64), nil)
		if err == nil || errors.Is(err, ErrUnwrap) {
			t.Errorf("Unwrap with a %d-byte KEK: %v, want a length error", n, err)
		}
	}
}

func TestKeyCheck(t *testing.T) {
	kek := seqKey(32)
	// hex(HMAC-SHA256(kek, "gos3-kek-check-v1")[:16])
	const want = "060382606baefb4f3ee4da7df2029a10"
	if got := KeyCheck(kek); got != want {
		t.Fatalf("KeyCheck = %s, want %s", got, want)
	}
	other := seqKey(32)
	other[0] = 1
	if KeyCheck(other) == want {
		t.Fatal("different KEKs have the same check")
	}
}
