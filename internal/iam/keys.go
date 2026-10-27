package iam

import (
	"crypto/rand"
	"fmt"
	"strings"
)

const (
	maxKeysPerUser = 10
	// maxUsers and maxCustomPolicies bound the IAM state, which is held in
	// memory, cloned by every mutation and rewritten to iam.json in full.
	// At the caps the file stays well below maxStateWriteSize for typical
	// documents; persist enforces that bound for the rest.
	maxUsers          = 5000
	maxCustomPolicies = 1000
	maxPolicyNameLen  = 128
	maxUserNameLen    = 64
	maxDescriptionLen = 1024

	generatedAccessKeyLen = 20
	generatedSecretKeyLen = 40

	// minNewSecretKeyLen is the minimum length of a caller supplied secret
	// for a new key. Any SigV4 signature (a shared presigned URL, a logged
	// request) lets an attacker test guesses of the secret offline, so short
	// human-chosen secrets are refused. Keys already stored with shorter
	// secrets (validSecretKey's 8 characters) keep loading and working.
	minNewSecretKeyLen = 16
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

// validSecretKey reports whether a stored secret is acceptable: 8..128
// printable ASCII characters (no spaces). New caller supplied secrets must
// also be at least minNewSecretKeyLen long (validNewSecretKey).
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

// validNewSecretKey reports whether a caller supplied secret for a new key
// is acceptable: minNewSecretKeyLen..128 printable ASCII characters.
func validNewSecretKey(s string) bool {
	return len(s) >= minNewSecretKeyLen && validSecretKey(s)
}

// validUserName reports whether s is a valid, non-reserved user name.
func validUserName(s string) bool {
	return validName(s, maxUserNameLen) && !isReservedUserName(s)
}

// isReservedUserName reports whether s is "root" or "anonymous" (any case).
func isReservedUserName(s string) bool {
	return strings.EqualFold(s, "root") || strings.EqualFold(s, "anonymous")
}

// User names and access key IDs share one namespace in bucket-policy
// principals: a bare entry such as "alice" names the user alice or the
// access key alice (see principalEntryMatches), and a bare 12-digit entry
// names the account. To keep every bare entry unambiguous, an access key ID
// may not equal a user name, a reserved name or an account ID, and a user
// name may not equal an access key ID (or the root access key) or an
// account ID. CreateUser and CreateAccessKey enforce this; keys loaded from
// an older iam.json that break the rule are refused at authentication
// (keyIDConflict) and logged, rather than failing startup.

// keyIDConflict returns why access key ID ak cannot be used next to the
// users of st ("" when it can).
func keyIDConflict(st *state, ak string) string {
	switch {
	case isReservedUserName(ak):
		return "it equals a reserved user name"
	case isAccountID(ak):
		return "it is a 12-digit account ID"
	case st.users[ak] != nil:
		return "it equals the name of user " + ak
	}
	return ""
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
