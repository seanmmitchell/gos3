package discovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// MaxInfoSize bounds the /_gos3/info document accepted by probes.
const MaxInfoSize = 64 << 10

// Probe timeouts.
const (
	// defaultProbeTimeout applies to Probe calls whose context has no deadline
	// and to periodic re-probes.
	defaultProbeTimeout = 5 * time.Second
	// candidateProbeTimeout applies to verification of beacon/mDNS/DNS candidates.
	candidateProbeTimeout = 3 * time.Second
)

// ErrNotGos3 is returned (wrapped) when a probed URL does not serve a valid
// gos3 identity document.
var ErrNotGos3 = errors.New("discovery: not a gos3 server")

// prober fetches identity documents. It never uses an HTTP proxy (discovery
// targets are LAN addresses), never follows redirects and never reuses
// connections, so no goroutines outlive a probe.
type prober struct {
	client *http.Client
}

func newProber() *prober {
	tr := &http.Transport{
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: -1,
		}).DialContext,
		TLSHandshakeTimeout:    5 * time.Second,
		ResponseHeaderTimeout:  10 * time.Second,
		DisableKeepAlives:      true,
		DisableCompression:     true,
		MaxResponseHeaderBytes: 16 << 10,
	}
	return &prober{client: &http.Client{
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

// close releases idle resources.
func (p *prober) close() { p.client.CloseIdleConnections() }

// fetch performs GET <base>/_gos3/info (base must be normalised) and returns
// the validated, sanitised document. The caller bounds the time with ctx.
func (p *prober) fetch(ctx context.Context, base string) (Info, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+InfoPath, nil)
	if err != nil {
		return Info{}, fmt.Errorf("%w: %v", ErrInvalidURL, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "gos3-discovery/1")
	resp, err := p.client.Do(req)
	if err != nil {
		return Info{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return Info{}, fmt.Errorf("%w: GET %s returned %s", ErrNotGos3, InfoPath, resp.Status)
	}
	if resp.ContentLength > MaxInfoSize {
		return Info{}, fmt.Errorf("%w: info document too large (%d bytes)", ErrNotGos3, resp.ContentLength)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxInfoSize+1))
	if err != nil {
		return Info{}, err
	}
	if len(body) > MaxInfoSize {
		return Info{}, fmt.Errorf("%w: info document exceeds %d bytes", ErrNotGos3, MaxInfoSize)
	}
	return ParseInfo(body)
}

// ParseInfo decodes and validates an identity document: it must be a single
// JSON object with "service": "gos3" and a valid "id" (1-64 characters of
// [A-Za-z0-9._-]). Strings and lists are cleaned and bounded.
func ParseInfo(b []byte) (Info, error) {
	if len(b) > MaxInfoSize {
		return Info{}, fmt.Errorf("%w: info document exceeds %d bytes", ErrNotGos3, MaxInfoSize)
	}
	var info Info
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(&info); err != nil {
		return Info{}, fmt.Errorf("%w: invalid JSON: %v", ErrNotGos3, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return Info{}, fmt.Errorf("%w: trailing data after JSON document", ErrNotGos3)
	}
	if info.Service != ServiceName {
		return Info{}, fmt.Errorf("%w: service is %q", ErrNotGos3, clip(info.Service, 32))
	}
	if !validID(info.ID) {
		return Info{}, fmt.Errorf("%w: missing or invalid id", ErrNotGos3)
	}
	return sanitizeInfo(info), nil
}

// withDefaultTimeout bounds ctx by d unless it already has a deadline.
func withDefaultTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, d)
}
