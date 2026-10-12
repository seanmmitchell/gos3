package iam

import (
	"crypto/rand"
	"fmt"
	"strings"
)

const (
	maxKeysPerUser    = 10
	maxPolicyNameLen  = 128
	maxUserNameLen    = 64
	maxDescriptionLen = 1024

	generatedAccessKeyLen = 20
	generatedSecretKeyLen = 40
)

const (
	accessKeyAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	secretKeyAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
)

// GenerateAccessKey returns a random 20-character access key ID of
// [A-Z0-9] (crypto/rand, unbiased).
func GenerateAccessKey() (string, error) {
	return randomString(accessKeyAlphabet, generatedAccessKeyLen)
}

// GenerateSecretKey returns a random 40-character secret key from the
// URL-safe base64 alphabet (240 bits of entropy).
func GenerateSecretKey() (string, error) {
	return randomString(secretKeyAlphabet, generatedSecretKeyLen)
}

// randomString draws n characters uniformly from alphabet (len <= 256)
// using rejection sampling.
func randomString(alphabet string, n int) (string, error) {
	limit := 256 - 256%len(alphabet) // bytes >= limit would bias the result
	out := make([]byte, 0, n)
	buf := make([]byte, n+n/2)
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("iam: random source: %w", err)
		}
		for _, b := range buf {
			if int(b) >= limit {
				continue
			}
			out = append(out, alphabet[int(b)%len(alphabet)])
			if len(out) == n {
				break
			}
		}
	}
	return string(out), nil
}

// validAccessKey reports whether a caller supplied access key ID is
// acceptable: 3..128 characters of [A-Za-z0-9._-].
func validAccessKey(s string) bool {
	if len(s) < 3 || len(s) > 128 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !isAlnum(c) && c != '.' && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

// validSecretKey reports whether a caller supplied secret is acceptable:
// 8..128 printable ASCII characters (no spaces).
func validSecretKey(s string) bool {
	if len(s) < 8 || len(s) > 128 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// validUserName reports whether s is a valid, non-reserved user name.
func validUserName(s string) bool {
	return validName(s, maxUserNameLen) && !isReservedUserName(s)
}

// isReservedUserName reports whether s is "root" or "anonymous" (any case).
func isReservedUserName(s string) bool {
	return strings.EqualFold(s, "root") || strings.EqualFold(s, "anonymous")
}

// normalizeKeyStatus canonicalizes a key status ("active" -> "Active").
func normalizeKeyStatus(s string) (string, bool) {
	switch {
	case strings.EqualFold(s, KeyActive):
		return KeyActive, true
	case strings.EqualFold(s, KeyInactive):
		return KeyInactive, true
	}
	return "", false
}
