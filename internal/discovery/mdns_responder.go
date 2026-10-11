package discovery

import (
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
)

// Responder limits (docs/discovery.md §2).
const (
	// mdnsMaxQuestions is how many distinct questions of one query are
	// looked at; duplicates are collapsed first.
	mdnsMaxQuestions = 8
	// mdnsMulticastGap: the same answer is multicast on an interface at
	// most once per second (RFC 6762 §6).
	mdnsMulticastGap = time.Second
	// mdnsMaxResponsesPerSecond bounds all responses together, whatever
	// the number of (possibly spoofed) sources.
	mdnsMaxResponsesPerSecond = 50
	maxMulticastEntries       = 256
)

// mdnsPacket is one datagram received by the responder.
type mdnsPacket struct {
	data    []byte
	src     netip.AddrPort // source; IPv6 link-local addresses keep their zone
	ifIndex int            // arrival interface, 0 when unknown
	ttl     int            // IP TTL / IPv6 hop limit, 0 when unknown
}

// mdnsWriter sends the responder's answers on the socket the query arrived
// on (a fake in tests).
type mdnsWriter interface {
	// multicast sends b to the mDNS group out of interface ifIndex.
	multicast(b []byte, ifIndex int) error
	// unicast sends b to one address.
	unicast(b []byte, to netip.AddrPort, ifIndex int) error
}

// mdnsResponder answers mDNS queries for this server's own records (its
// zone) only, and only from senders on this host or a directly connected
// subnet. It is safe for concurrent use.
type mdnsResponder struct {
	logger *slog.Logger
	local  *localNets
	limit  *sourceLimiter
	now    func() time.Time
	zone   atomic.Pointer[mdnsZone]

	mu            sync.Mutex
	lastMulticast map[string]time.Time // answer set + interface → last multicast
	second        time.Time            // start of the current global rate window
	sent          int                  // responses sent in that window
}

func newMDNSResponder(logger *slog.Logger) *mdnsResponder {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &mdnsResponder{
		logger:        logger,
		local:         hostNets,
		limit:         newSourceLimiter(),
		now:           time.Now,
		lastMulticast: make(map[string]time.Time),
	}
}

// setZone replaces the advertised records; nil stops answering.
func (r *mdnsResponder) setZone(z *mdnsZone) { r.zone.Store(z) }

// serve answers queries arriving on s until it is closed.
func (r *mdnsResponder) serve(s *mdnsSocket) {
	buf := make([]byte, mdnsMaxPacket+1)
	failures := 0
	for {
		n, src, meta, err := s.readFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			failures++
			if failures > 10 {
				time.Sleep(100 * time.Millisecond)
			}
			continue
		}
		failures = 0
		if n > mdnsMaxPacket {
			continue
		}
		r.handle(mdnsPacket{data: buf[:n], src: src, ifIndex: meta.ifIndex, ttl: meta.ttl}, s)
	}
}

// handle processes one datagram:
//
//   - senders that are not on this host or a directly connected subnet,
//     broadcast or unspecified source addresses, and datagrams from other
//     hosts that arrived with a TTL / hop limit below 255 (forwarded by a
//     router, so the on-link source is spoofed) are ignored;
//   - only standard queries are looked at; duplicate questions are
//     collapsed and at most mdnsMaxQuestions distinct ones are considered;
//   - questions about anything but this server's own service, instance and
//     host records get no answer, and a query without such questions costs
//     the sender nothing;
//   - each sender is rate-limited with the beacon's token bucket, and all
//     responses together to mdnsMaxResponsesPerSecond;
//   - answers go to the multicast group (TTL 255, at most once per second
//     per answer set and interface), except for questions with the
//     unicast-response bit (RFC 6762 §5.4) and legacy queries from a port
//     other than 5353 (§6.7), which are answered by unicast to the sender;
//   - every response is a single packet of at most mdnsMaxResponse bytes.
func (r *mdnsResponder) handle(p mdnsPacket, w mdnsWriter) {
	src := p.src.Addr()
	if len(p.data) > mdnsMaxPacket || p.src.Port() == 0 || !r.local.onLink(src) || r.local.isBroadcast(src) || r.local.routed(src, p.ttl) {
		r.logger.Debug("mDNS: dropping datagram from a sender that is not on the local link", "from", p.src.String())
		return
	}
	z := r.zone.Load()
	if z == nil {
		return
	}
	var q dns.Msg
	if err := q.Unpack(p.data); err != nil {
		return
	}
	if q.Response || q.Opcode != dns.OpcodeQuery || q.Rcode != dns.RcodeSuccess {
		return
	}
	legacy := p.src.Port() != mdnsPort
	var (
		seen                     = make(map[string]bool)
		uq, mq                   []dns.Question // answered questions, per transport
		uAns, uExtra, mAns, mExt []dns.RR
	)
	for _, qu := range q.Question {
		key := strings.ToLower(qu.Name) + "|" + strconv.Itoa(int(qu.Qtype)) + "|" + strconv.Itoa(int(qu.Qclass&^(1<<15)))
		if seen[key] {
			continue
		}
		if len(seen) >= mdnsMaxQuestions {
			break
		}
		seen[key] = true
		ans, extra := z.records(qu)
		if len(ans) == 0 {
			continue
		}
		if legacy || qu.Qclass&(1<<15) != 0 {
			uq = append(uq, qu)
			uAns, uExtra = append(uAns, ans...), append(uExtra, extra...)
		} else {
			mq = append(mq, qu)
			mAns, mExt = append(mAns, ans...), append(mExt, extra...)
		}
	}
	if len(uq) == 0 && len(mq) == 0 {
		return
	}
	now := r.now()
	mkey := ""
	sendMulticast := false
	if len(mq) > 0 {
		mkey = multicastKey(mq, p.ifIndex)
		sendMulticast = r.multicastDue(mkey, now)
	}
	// A query whose multicast answer is suppressed (just sent) and that
	// needs no unicast reply sends nothing, so it must not use up the
	// sender's or the global budget: otherwise a flood of such queries
	// would starve the queries that do get answers.
	if !sendMulticast && len(uq) == 0 {
		return
	}
	if !r.limit.allow(src.WithZone(""), now) || !r.globalAllow(now) {
		r.logger.Debug("mDNS: too many queries, dropping", "from", p.src.String())
		return
	}
	if sendMulticast {
		r.markMulticast(mkey, now)
		if b, ok := packResponse(0, nil, mAns, mExt, false); ok {
			if err := w.multicast(b, p.ifIndex); err != nil {
				r.logger.Debug("mDNS: multicast response", "err", err)
			}
		}
	}
	if len(uq) > 0 {
		id := uint16(0)
		var questions []dns.Question
		if legacy {
			// A legacy resolver expects its ID and question back.
			id = q.Id
			for _, qu := range uq {
				qu.Qclass &^= 1 << 15
				questions = append(questions, qu)
			}
		}
		if b, ok := packResponse(id, questions, uAns, uExtra, legacy); ok {
			if err := w.unicast(b, p.src, p.ifIndex); err != nil {
				r.logger.Debug("mDNS: unicast response", "to", p.src.String(), "err", err)
			}
		}
	}
}

// globalAllow counts one response against the global per-second budget.
func (r *mdnsResponder) globalAllow(now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if now.Sub(r.second) >= time.Second || now.Before(r.second) {
		r.second, r.sent = now, 0
	}
	if r.sent >= mdnsMaxResponsesPerSecond {
		return false
	}
	r.sent++
	return true
}

// multicastKey identifies the answer set to questions qs on interface
// ifIndex for multicast suppression.
func multicastKey(qs []dns.Question, ifIndex int) string {
	keys := make([]string, len(qs))
	for i, q := range qs {
		keys[i] = strings.ToLower(q.Name) + "/" + strconv.Itoa(int(q.Qtype))
	}
	slices.Sort(keys)
	return strconv.Itoa(ifIndex) + "|" + strings.Join(keys, ",")
}

// multicastDue reports whether the answer set key may be multicast now
// (it was not multicast in the last mdnsMulticastGap). It records nothing:
// markMulticast does once the response is actually sent.
func (r *mdnsResponder) multicastDue(key string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.lastMulticast[key]
	return !ok || now.Sub(t) >= mdnsMulticastGap || now.Before(t)
}

// markMulticast records that the answer set key was multicast at now.
func (r *mdnsResponder) markMulticast(key string, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.lastMulticast) >= maxMulticastEntries {
		for k, t := range r.lastMulticast {
			if now.Sub(t) >= mdnsMulticastGap {
				delete(r.lastMulticast, k)
			}
		}
		if len(r.lastMulticast) >= maxMulticastEntries {
			clear(r.lastMulticast)
		}
	}
	r.lastMulticast[key] = now
}

// packResponse builds an authoritative mDNS response from the answer and
// additional records (duplicates removed, additional records that are
// already answers dropped). legacy replies carry TTLs of at most 10 s.
// Additional records, then trailing answers, are dropped until the packet
// fits in mdnsMaxResponse bytes.
func packResponse(id uint16, questions []dns.Question, answers, extra []dns.RR, legacy bool) ([]byte, bool) {
	uniq := func(rrs []dns.RR, skip []dns.RR) []dns.RR {
		var out []dns.RR
		for _, rr := range rrs {
			if !slices.Contains(out, rr) && !slices.Contains(skip, rr) {
				out = append(out, rr)
			}
		}
		return out
	}
	answers = uniq(answers, nil)
	extra = uniq(extra, answers)
	if legacy {
		cp := func(rrs []dns.RR) []dns.RR {
			out := make([]dns.RR, len(rrs))
			for i, rr := range rrs {
				out[i] = dns.Copy(rr)
				out[i].Header().Ttl = min(out[i].Header().Ttl, mdnsLegacyTTL)
			}
			return out
		}
		answers, extra = cp(answers), cp(extra)
	}
	m := &dns.Msg{
		MsgHdr:   dns.MsgHdr{Id: id, Response: true, Authoritative: true, Opcode: dns.OpcodeQuery},
		Compress: true,
		Question: questions,
		Answer:   answers,
		Extra:    extra,
	}
	for {
		b, err := m.Pack()
		if err == nil && len(b) <= mdnsMaxResponse {
			return b, true
		}
		switch {
		case len(m.Extra) > 0:
			m.Extra = m.Extra[:len(m.Extra)-1]
		case len(m.Answer) > 1:
			m.Answer = m.Answer[:len(m.Answer)-1]
		default:
			return nil, false
		}
	}
}
