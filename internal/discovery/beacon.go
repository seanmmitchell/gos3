package discovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/net/ipv4"
)

// Beacon protocol constants (docs/discovery.md §3).
const (
	BeaconProto = "gos3-beacon/1"

	beaconAnnounce = "announce"
	beaconQuery    = "query"
	beaconBye      = "bye"

	// MaxBeaconSize is the largest beacon datagram sent or accepted.
	MaxBeaconSize = 1400
	// maxBeaconURLs bounds the URLs carried in (and taken from) an announce.
	maxBeaconURLs = 16
	// maxNonceLen bounds query nonces.
	maxNonceLen = 128
	// maxReplyDelay is the upper bound of the random delay before answering
	// a query.
	maxReplyDelay = 500 * time.Millisecond
	// maxPendingReplies bounds concurrently scheduled query replies; further
	// queries are dropped until replies drain.
	maxPendingReplies = 32
	// ownNonceRing is how many of our own query nonces are remembered.
	ownNonceRing = 8
)

var (
	// errBeaconIgnored marks well-formed datagrams of another protocol
	// version or an unknown type; they are silently dropped.
	errBeaconIgnored = errors.New("beacon: ignored message")
	// errBeaconInvalid marks malformed datagrams.
	errBeaconInvalid = errors.New("beacon: invalid message")
)

// beaconMsg is one beacon datagram. Unknown fields are ignored so that
// later revisions of gos3-beacon/1 can add fields.
type beaconMsg struct {
	Proto   string   `json:"proto"`
	Type    string   `json:"type"`
	ID      string   `json:"id,omitempty"`
	Name    string   `json:"name,omitempty"`
	Version string   `json:"version,omitempty"`
	Region  string   `json:"region,omitempty"`
	URLs    []string `json:"urls,omitempty"`
	Info    string   `json:"info,omitempty"`
	TS      int64    `json:"ts,omitempty"`
	Nonce   string   `json:"nonce,omitempty"`
}

// encodeBeacon marshals m, dropping trailing URLs until the datagram fits in
// MaxBeaconSize.
func encodeBeacon(m beaconMsg) ([]byte, error) {
	m.Proto = BeaconProto
	for {
		b, err := json.Marshal(m)
		if err != nil {
			return nil, err
		}
		if len(b) <= MaxBeaconSize {
			return b, nil
		}
		if len(m.URLs) == 0 {
			return nil, fmt.Errorf("beacon message exceeds %d bytes", MaxBeaconSize)
		}
		m.URLs = m.URLs[:len(m.URLs)-1]
	}
}

// decodeBeacon parses and validates one datagram. It returns errBeaconIgnored
// for other protocols / unknown types and errBeaconInvalid (wrapped) for
// malformed input.
func decodeBeacon(b []byte) (beaconMsg, error) {
	var m beaconMsg
	if len(b) == 0 || len(b) > MaxBeaconSize {
		return m, fmt.Errorf("%w: size %d", errBeaconInvalid, len(b))
	}
	if !utf8.Valid(b) {
		return m, fmt.Errorf("%w: not UTF-8", errBeaconInvalid)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(&m); err != nil {
		return beaconMsg{}, fmt.Errorf("%w: %v", errBeaconInvalid, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return beaconMsg{}, fmt.Errorf("%w: trailing data", errBeaconInvalid)
	}
	if m.Proto != BeaconProto {
		return beaconMsg{}, errBeaconIgnored
	}
	switch m.Type {
	case beaconAnnounce:
		if !validID(m.ID) {
			return beaconMsg{}, fmt.Errorf("%w: announce without valid id", errBeaconInvalid)
		}
		if len(m.URLs) > maxBeaconURLs {
			m.URLs = m.URLs[:maxBeaconURLs]
		}
		m.Name = cleanField(m.Name, maxFieldLen)
		m.Version = cleanField(m.Version, maxFieldLen)
		m.Region = cleanField(m.Region, maxFieldLen)
	case beaconQuery:
		if len(m.Nonce) > maxNonceLen {
			return beaconMsg{}, fmt.Errorf("%w: nonce too long", errBeaconInvalid)
		}
	case beaconBye:
		if !validID(m.ID) {
			return beaconMsg{}, fmt.Errorf("%w: bye without valid id", errBeaconInvalid)
		}
	default:
		return beaconMsg{}, errBeaconIgnored
	}
	return m, nil
}

// announceFor builds the announce message describing info.
func announceFor(info Info, now time.Time) beaconMsg {
	return beaconMsg{
		Proto:   BeaconProto,
		Type:    beaconAnnounce,
		ID:      info.ID,
		Name:    cleanField(info.Name, maxFieldLen),
		Version: cleanField(info.Version, maxFieldLen),
		Region:  cleanField(info.Region, maxFieldLen),
		URLs:    advertisedURLs(info),
		Info:    InfoPath,
		TS:      now.Unix(),
	}
}

// advertisedURLs returns the normalised, de-duplicated base URLs of info
// (Endpoints.S3 first, then Endpoints.All), at most maxBeaconURLs.
func advertisedURLs(info Info) []string {
	var out []string
	for _, raw := range append([]string{info.Endpoints.S3}, info.Endpoints.All...) {
		if raw == "" {
			continue
		}
		if u, err := NormalizeBaseURL(raw); err == nil && !slices.Contains(out, u) {
			out = append(out, u)
			if len(out) == maxBeaconURLs {
				break
			}
		}
	}
	return out
}

// beaconCandidateURLs turns the URLs of an announce received from src into
// probe candidates. Nothing in a datagram is authenticated, so the server
// only ever probes the sender: every URL's host is replaced by the source IP
// (keeping scheme and port), except that a sender on this host (isLocal)
// may also announce loopback and local addresses, which then stay as they
// are (a server listening on 127.0.0.1 is reachable there). Host names are
// never resolved. Without any usable URL the source IP with DefaultPort is
// used; without a source address there are no candidates.
func beaconCandidateURLs(urls []string, src net.Addr, isLocal func(netip.Addr) bool) []string {
	srcIP, ok := ipFromAddr(src)
	if !ok || !srcIP.IsValid() || srcIP.IsUnspecified() || srcIP.IsMulticast() {
		return nil
	}
	srcIP = srcIP.WithZone("")
	srcLocal := isLocal != nil && isLocal(srcIP)
	var out []string
	add := func(u string) {
		if len(out) < maxBeaconURLs && !slices.Contains(out, u) {
			out = append(out, u)
		}
	}
	for _, raw := range urls {
		if len(raw) > maxURLLen {
			continue
		}
		scheme, host, port, ok := "", "", "", false
		if n, err := NormalizeBaseURL(raw); err == nil {
			scheme, host, port, ok = urlHostPort(n)
			if ip, err := netip.ParseAddr(strings.Trim(host, "[]")); ok && err == nil {
				ip = ip.Unmap()
				if ip == srcIP || (srcLocal && (ip.IsLoopback() || isLocal(ip))) {
					add(n)
					continue
				}
			}
		} else if u, err := url.Parse(raw); err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() == "" {
			// "http://:9000" – host omitted.
			scheme, port, ok = u.Scheme, u.Port(), true
			if port != "" {
				if _, err := parsePort(port); err != nil {
					ok = false
				}
			}
		}
		if ok {
			add(baseURL(scheme, srcIP.String(), effectivePort(scheme, port)))
		}
	}
	if len(out) == 0 {
		add(baseURL("http", srcIP.String(), DefaultPort))
	}
	return out
}

// sink receives what discovery mechanisms observe.
type sink interface {
	// candidate reports an unverified sighting; it must not block.
	candidate(c candidate)
	// bye reports that peer id announced its shutdown in a datagram from
	// src.
	bye(id string, src netip.Addr)
}

// beaconOptions configures a beacon runner.
type beaconOptions struct {
	interval  time.Duration
	advertise bool // send announces, answer queries, send bye
	query     bool // send a query when starting
	selfInfo  func() Info
	sink      sink
	logger    *slog.Logger
	now       func() time.Time
}

// beacon runs the UDP beacon protocol on two sockets: mconn listens on the
// group port (joined to the multicast group) and uconn, an ephemeral unicast
// socket, sends every datagram and receives unicast replies to our queries.
// Using a separate unicast socket keeps replies from being load-balanced to
// another process sharing the group port.
type beacon struct {
	beaconOptions
	uconn  net.PacketConn
	upc    *ipv4.PacketConn // multicast options of uconn; nil when unavailable
	mconn  net.PacketConn   // nil when multicast could not be joined
	dests  func() []net.Addr
	ifaces []net.Interface // multicast interfaces to send on
	local  *localNets      // which senders are on this host or link
	limit  *sourceLimiter  // announces taken per sender

	sendMu   sync.Mutex
	nonceMu  sync.Mutex
	nonces   []string
	replySem chan struct{}
	readers  sync.WaitGroup
	replies  sync.WaitGroup
}

// newBeacon wires a beacon from already opened sockets (used by tests).
func newBeacon(o beaconOptions, uconn, mconn net.PacketConn, dests func() []net.Addr) *beacon {
	if o.logger == nil {
		o.logger = slog.New(slog.DiscardHandler)
	}
	if o.now == nil {
		o.now = time.Now
	}
	if o.selfInfo == nil {
		o.selfInfo = func() Info { return Info{} }
	}
	if o.interval <= 0 {
		o.interval = DefaultBeaconInterval
	}
	return &beacon{
		beaconOptions: o,
		uconn:         uconn,
		mconn:         mconn,
		dests:         dests,
		local:         hostNets,
		limit:         newSourceLimiter(),
		replySem:      make(chan struct{}, maxPendingReplies),
	}
}

// openBeacon opens the sockets for group. A failure to join the multicast
// group is reported through multicastErr and the beacon keeps running with
// its unicast socket only.
func openBeacon(group *net.UDPAddr, o beaconOptions, multicastErr func(error)) (*beacon, error) {
	uc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		return nil, fmt.Errorf("beacon: open UDP socket: %w", err)
	}
	b := newBeacon(o, uc, nil, func() []net.Addr { return []net.Addr{group} })
	b.upc = ipv4.NewPacketConn(uc)
	if err := b.upc.SetMulticastTTL(1); err != nil {
		b.logger.Debug("beacon: set multicast TTL", "err", err)
	}
	if err := b.upc.SetMulticastLoopback(true); err != nil {
		b.logger.Debug("beacon: enable multicast loopback", "err", err)
	}
	// Unicast replies to queries stay on the link too.
	if err := b.upc.SetTTL(1); err != nil {
		b.logger.Debug("beacon: set unicast TTL", "err", err)
	}
	b.ifaces = multicastInterfaces(false)
	mc, joined, err := listenGroup(group, b.ifaces)
	if err != nil {
		if multicastErr != nil {
			multicastErr(err)
		}
	} else {
		b.mconn = mc
		b.logger.Debug("beacon: joined multicast group", "group", group.String(), "interfaces", joined)
	}
	return b, nil
}

// listenGroup binds the group port and joins the group on every multicast
// interface. It returns the number of successful joins.
func listenGroup(group *net.UDPAddr, ifaces []net.Interface) (net.PacketConn, int, error) {
	c, err := net.ListenMulticastUDP("udp4", nil, group)
	if err != nil {
		firstErr := err
		for i := range ifaces {
			if c, err = net.ListenMulticastUDP("udp4", &ifaces[i], group); err == nil {
				break
			}
		}
		if c == nil {
			return nil, 0, firstErr
		}
	}
	pc := ipv4.NewPacketConn(c)
	joined := 1
	for i := range ifaces {
		if pc.JoinGroup(&ifaces[i], group) == nil {
			joined++
		}
	}
	_ = pc.SetMulticastLoopback(true)
	return c, joined, nil
}

// multicastInterfaces returns the up, multicast-capable interfaces with an
// address of the family (IPv6 when v6, else IPv4).
func multicastInterfaces(v6 bool) []net.Interface {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []net.Interface
	for _, ifi := range ifs {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagMulticast == 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && (ipn.IP.To4() == nil) == v6 {
				out = append(out, ifi)
				break
			}
		}
	}
	return out
}

// run serves the beacon until ctx is done, then sends a bye (when
// advertising), closes the sockets and waits for its goroutines.
func (b *beacon) run(ctx context.Context) {
	b.startReader(ctx, b.uconn)
	b.startReader(ctx, b.mconn)
	if b.query {
		b.sendQuery()
	}
	if b.advertise {
		b.sendAnnounce(nil)
		t := time.NewTimer(jitter(b.interval))
	loop:
		for {
			select {
			case <-ctx.Done():
				t.Stop()
				break loop
			case <-t.C:
				b.sendAnnounce(nil)
				t.Reset(jitter(b.interval))
			}
		}
		b.sendBye()
	} else {
		<-ctx.Done()
	}
	b.close()
}

// close closes the sockets and waits for readers and pending replies.
func (b *beacon) close() {
	if b.uconn != nil {
		_ = b.uconn.Close()
	}
	if b.mconn != nil {
		_ = b.mconn.Close()
	}
	b.readers.Wait()
	b.replies.Wait() // no new replies after readers exited
}

func (b *beacon) startReader(ctx context.Context, c net.PacketConn) {
	if c == nil {
		return
	}
	b.readers.Add(1)
	go func() {
		defer b.readers.Done()
		b.readLoop(ctx, c)
	}()
}

func (b *beacon) readLoop(ctx context.Context, c net.PacketConn) {
	buf := make([]byte, 2048)
	failures := 0
	for {
		n, src, err := c.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				return
			}
			// Transient errors (e.g. ICMP port unreachable surfacing as
			// ECONNREFUSED); back off if they persist.
			failures++
			if failures > 10 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(100 * time.Millisecond):
				}
			}
			continue
		}
		failures = 0
		if n > MaxBeaconSize {
			continue
		}
		b.handle(ctx, buf[:n], src)
	}
}

// handle processes one received datagram. Datagrams from senders that are
// neither on this host nor on a directly connected subnet are dropped: the
// group port also receives unicast datagrams, and beacons never cross a
// router legitimately (TTL 1).
func (b *beacon) handle(ctx context.Context, data []byte, src net.Addr) {
	srcIP, ok := ipFromAddr(src)
	if !ok || !b.local.onLink(srcIP) {
		b.logger.Debug("beacon: dropping datagram from a sender that is not on the local link", "from", addrString(src))
		return
	}
	srcIP = srcIP.WithZone("")
	m, err := decodeBeacon(data)
	if err != nil {
		if !errors.Is(err, errBeaconIgnored) {
			b.logger.Debug("beacon: dropping datagram", "from", addrString(src), "err", err)
		}
		return
	}
	selfID := b.selfInfo().ID
	switch m.Type {
	case beaconAnnounce:
		if m.ID == selfID {
			return
		}
		if !b.limit.allow(srcIP, b.now()) {
			b.logger.Debug("beacon: too many announces from sender, dropping", "from", addrString(src))
			return
		}
		b.sink.candidate(candidate{
			id: m.ID, name: m.Name, version: m.Version, region: m.Region,
			source: SourceBeacon,
			urls:   beaconCandidateURLs(m.URLs, src, b.local.isLocal),
		})
	case beaconQuery:
		if !b.advertise || b.ownNonce(m.Nonce) {
			return
		}
		b.scheduleReply(ctx, src)
	case beaconBye:
		if m.ID != selfID {
			b.sink.bye(m.ID, srcIP)
		}
	}
}

// Per-sender announce budget: a burst of sourceBurst announces, refilled
// at one per sourceRefill. Servers announce every beacon interval (≥ 1 s)
// and on queries, so this only throttles floods.
const (
	sourceBurst      = 32
	sourceRefill     = 250 * time.Millisecond
	maxSourceEntries = 1024
)

// sourceLimiter is a token bucket per sender address.
type sourceLimiter struct {
	mu      sync.Mutex
	buckets map[netip.Addr]*tokenBucket
}

type tokenBucket struct {
	tokens float64
	at     time.Time
}

func newSourceLimiter() *sourceLimiter {
	return &sourceLimiter{buckets: make(map[netip.Addr]*tokenBucket)}
}

// allow takes one token of src's bucket.
func (l *sourceLimiter) allow(src netip.Addr, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[src]
	if !ok {
		if len(l.buckets) >= maxSourceEntries {
			for k, v := range l.buckets {
				if now.Sub(v.at) > sourceBurst*sourceRefill {
					delete(l.buckets, k)
				}
			}
			if len(l.buckets) >= maxSourceEntries {
				clear(l.buckets)
			}
		}
		b = &tokenBucket{tokens: sourceBurst, at: now}
		l.buckets[src] = b
	}
	if d := now.Sub(b.at); d > 0 {
		b.tokens = min(sourceBurst, b.tokens+float64(d)/float64(sourceRefill))
		b.at = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// scheduleReply answers a query with a unicast announce after a random
// 0-500 ms delay.
func (b *beacon) scheduleReply(ctx context.Context, to net.Addr) {
	select {
	case b.replySem <- struct{}{}:
	default:
		return // too many pending replies; the querier will hear periodic announces
	}
	b.replies.Add(1)
	go func() {
		defer b.replies.Done()
		defer func() { <-b.replySem }()
		t := time.NewTimer(rand.N(maxReplyDelay + 1))
		defer t.Stop()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		b.sendAnnounce(to)
	}()
}

// sendAnnounce sends our announce to the group (to == nil) or unicast.
func (b *beacon) sendAnnounce(to net.Addr) {
	info := b.selfInfo()
	if !validID(info.ID) {
		b.logger.Debug("beacon: not announcing, server ID unknown")
		return
	}
	data, err := encodeBeacon(announceFor(info, b.now()))
	if err != nil {
		b.logger.Debug("beacon: encode announce", "err", err)
		return
	}
	if to != nil {
		b.sendTo(data, to)
		return
	}
	b.sendGroup(data)
}

// sendQuery multicasts a query with a fresh nonce.
func (b *beacon) sendQuery() {
	nonce := randomHex(8)
	b.nonceMu.Lock()
	b.nonces = append(b.nonces, nonce)
	if len(b.nonces) > ownNonceRing {
		b.nonces = b.nonces[len(b.nonces)-ownNonceRing:]
	}
	b.nonceMu.Unlock()
	data, err := encodeBeacon(beaconMsg{Type: beaconQuery, Nonce: nonce})
	if err == nil {
		b.sendGroup(data)
	}
}

// sendBye multicasts a best-effort bye.
func (b *beacon) sendBye() {
	id := b.selfInfo().ID
	if !validID(id) {
		return
	}
	if data, err := encodeBeacon(beaconMsg{Type: beaconBye, ID: id}); err == nil {
		b.sendGroup(data)
	}
}

func (b *beacon) ownNonce(n string) bool {
	if n == "" {
		return false
	}
	b.nonceMu.Lock()
	defer b.nonceMu.Unlock()
	return slices.Contains(b.nonces, n)
}

func (b *beacon) sendTo(data []byte, to net.Addr) {
	b.sendMu.Lock()
	defer b.sendMu.Unlock()
	if _, err := b.uconn.WriteTo(data, to); err != nil {
		b.logger.Debug("beacon: send", "to", addrString(to), "err", err)
	}
}

// sendGroup sends data to every destination; multicast destinations are
// sent out of every multicast interface.
func (b *beacon) sendGroup(data []byte) {
	b.sendMu.Lock()
	defer b.sendMu.Unlock()
	for _, d := range b.dests() {
		ua, _ := d.(*net.UDPAddr)
		if b.upc != nil && ua != nil && ua.IP.IsMulticast() && len(b.ifaces) > 0 {
			sent := false
			for i := range b.ifaces {
				if b.upc.SetMulticastInterface(&b.ifaces[i]) != nil {
					continue
				}
				if _, err := b.upc.WriteTo(data, nil, d); err == nil {
					sent = true
				}
			}
			if sent {
				continue
			}
		}
		if _, err := b.uconn.WriteTo(data, d); err != nil {
			b.logger.Debug("beacon: send", "to", addrString(d), "err", err)
		}
	}
}

// jitter returns d ± 10 %.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	return d - d/10 + rand.N(d/5+1)
}

func addrString(a net.Addr) string {
	if a == nil {
		return ""
	}
	return a.String()
}
