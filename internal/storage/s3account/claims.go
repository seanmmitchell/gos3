package s3account

import (
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"
	"sync"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// ErrClaimed is returned (wrapped) when a remote location cannot be used
// because it is in use: claimed by a backing of this server (as a home, a
// container or a native bucket's remote bucket, see Claims), or holding a
// gos3 store a new container or native bucket would nest in or adopt. It
// wraps storage.ErrInvalidTarget; the admin API answers 409 Conflict.
var ErrClaimed = fmt.Errorf("%w: the remote location is in use", storage.ErrInvalidTarget)

// NewClaims returns the server-wide claims registry: an in-memory Claims
// shared by every s3 backing of the server.
//
// Rules (per service: claims on different endpoints never conflict;
// remote buckets compared case-insensitively, prefixes exactly):
//   - a native claim conflicts with every other claim on the same remote
//     bucket (a native bucket's remote bucket is used by nothing else);
//   - home and container claims conflict with native claims on the same
//     remote bucket and with each other when their prefixes are equal or
//     nested ("" nests every prefix).
//
// Claiming exactly what is already claimed (same backing, role, location
// and gos3 bucket) is not a conflict for homes and native buckets: the
// claim is counted and stays until every holder released it. A container
// is claimed once (a second AddContainer of the same location conflicts).
func NewClaims() Claims {
	return &claims{byBucket: map[claimKey][]*claimEntry{}}
}

type claims struct {
	mu       sync.Mutex
	byBucket map[claimKey][]*claimEntry
}

// claimKey is a remote bucket of a service: the normalised endpoint
// (serviceKey) and the lower-cased bucket name.
type claimKey struct{ service, bucket string }

func keyOf(endpoint, bucket string) claimKey {
	return claimKey{service: serviceKey(endpoint), bucket: strings.ToLower(bucket)}
}

// serviceKey normalises an endpoint URL to the lower-case host:port of
// the service, with the scheme's default port when none is given (as
// config's s3Server, used by the configuration's overlap check).
func serviceKey(endpoint string) string {
	endpoint = strings.TrimSpace(endpoint)
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return strings.ToLower(endpoint)
	}
	host, port := strings.ToLower(u.Hostname()), u.Port()
	if port == "" {
		port = "80"
		if strings.EqualFold(u.Scheme, "https") {
			port = "443"
		}
	}
	return net.JoinHostPort(host, port)
}

type claimEntry struct {
	c    Claim
	refs int
}

// Claim implements Claims.
func (r *claims) Claim(c Claim) (func(), error) {
	if c.Bucket == "" {
		return nil, fmt.Errorf("%w: a claim needs a remote bucket", storage.ErrInvalidTarget)
	}
	switch c.Role {
	case RoleHome, RoleContainer:
	case RoleNative:
		c.Prefix = ""
	default:
		return nil, fmt.Errorf("%w: unknown claim role %q", storage.ErrInvalidTarget, c.Role)
	}
	key := keyOf(c.Endpoint, c.Bucket)
	r.mu.Lock()
	defer r.mu.Unlock()
	var same *claimEntry
	for _, e := range r.byBucket[key] {
		if sameClaim(e.c, c) && c.Role != RoleContainer {
			same = e
			continue
		}
		if conflicts(e.c, c) {
			return nil, fmt.Errorf("%w: cannot use %s %s: backing %q uses %s %s", ErrClaimed,
				c.Location, useOf(c), e.c.Backing, e.c.Location, useOf(e.c))
		}
	}
	if same == nil {
		same = &claimEntry{c: c}
		r.byBucket[key] = append(r.byBucket[key], same)
	}
	same.refs++
	var once sync.Once
	return func() { once.Do(func() { r.release(key, same) }) }, nil
}

func (r *claims) release(key claimKey, e *claimEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e.refs--; e.refs > 0 {
		return
	}
	list := slices.DeleteFunc(r.byBucket[key], func(x *claimEntry) bool { return x == e })
	if len(list) == 0 {
		delete(r.byBucket, key)
		return
	}
	r.byBucket[key] = list
}

// Holders implements Claims.
func (r *claims) Holders(endpoint, bucket string) []Claim {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Claim
	for _, e := range r.byBucket[keyOf(endpoint, bucket)] {
		out = append(out, e.c)
	}
	return out
}

// sameClaim reports whether a and b are the same use.
func sameClaim(a, b Claim) bool {
	return a.Backing == b.Backing && a.Role == b.Role && a.Prefix == b.Prefix && a.GosBucket == b.GosBucket
}

// conflicts reports whether claims a and b (on the same remote bucket)
// cannot coexist.
func conflicts(a, b Claim) bool {
	if a.Role == RoleNative || b.Role == RoleNative {
		return true
	}
	return nested(a.Prefix, b.Prefix)
}

// nested reports whether prefix a contains b or b contains a ("" contains
// every prefix).
func nested(a, b string) bool {
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

// useOf describes a claim's use for error messages.
func useOf(c Claim) string {
	switch c.Role {
	case RoleNative:
		return fmt.Sprintf("as the remote bucket of native bucket %q", c.GosBucket)
	case RoleHome:
		return "as a home"
	}
	return "as a container"
}

// claim records a use of loc by the backing (on its service).
func (b *Backend) claim(loc Location, role ClaimRole, gosBucket string) (func(), error) {
	return b.opts.Claims.Claim(Claim{Backing: b.name, Endpoint: b.endpoint, Location: loc, Role: role, GosBucket: gosBucket})
}

// holders returns the claims on a remote bucket of the backing's service.
func (b *Backend) holders(bucket string) []Claim {
	return b.opts.Claims.Holders(b.endpoint, bucket)
}
