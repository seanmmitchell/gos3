package discovery

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

func testInfo(id, name string) Info {
	return Info{
		Service: ServiceName, ID: id, Name: name, Version: "1", Region: "r",
		Endpoints: Endpoints{S3: "http://" + name + ":9000", All: []string{"http://" + name + ":9000"}},
		Features:  []string{"f"},
	}
}

func TestValidID(t *testing.T) {
	for _, id := range []string{"b3f1c2d4e5f60718", "a", "A-b_c.d", strings.Repeat("x", 64)} {
		if !validID(id) {
			t.Errorf("validID(%q) = false", id)
		}
	}
	for _, id := range []string{"", " ", "a b", "a/b", "ä", strings.Repeat("x", 65), "a\n"} {
		if validID(id) {
			t.Errorf("validID(%q) = true", id)
		}
	}
}

func TestPeerTableMergeAndTTL(t *testing.T) {
	clk := newFakeClock()
	tb := newPeerTable(time.Minute)
	t0 := clk.Now()

	// Unverified beacon sighting.
	needProbe := tb.hint(candidate{id: "p1", name: "beacon-name", source: SourceBeacon, urls: []string{"http://10.0.0.1:9000"}}, t0, "self")
	if !needProbe {
		t.Fatal("unverified hint should request a probe")
	}
	p, ok := tb.get("p1")
	if !ok || p.Verified || !p.Healthy || p.Name != "beacon-name" || !slices.Equal(p.Sources, []Source{SourceBeacon}) {
		t.Fatalf("after hint: %+v", p)
	}

	// Verified via probe from mDNS at another address.
	clk.Advance(10 * time.Second)
	t1 := clk.Now()
	p, ok = tb.observeInfo(testInfo("p1", "real-name"), "http://10.0.0.2:9000", SourceMDNS, t1, "self")
	if !ok || !p.Verified || !p.Healthy || p.Name != "real-name" {
		t.Fatalf("after observe: %+v", p)
	}
	if !slices.Equal(p.Sources, []Source{SourceBeacon, SourceMDNS}) {
		t.Errorf("sources = %v", p.Sources)
	}
	if !slices.Equal(p.Addresses, []string{"http://10.0.0.1:9000", "http://10.0.0.2:9000"}) {
		t.Errorf("addresses = %v", p.Addresses)
	}
	if !p.FirstSeen.Equal(t0) || !p.LastSeen.Equal(t1) {
		t.Errorf("first/last seen = %v / %v", p.FirstSeen, p.LastSeen)
	}

	// A beacon hint of a verified healthy peer at a known address needs no probe
	// and does not override verified fields.
	clk.Advance(10 * time.Second)
	if tb.hint(candidate{id: "p1", name: "spoof", source: SourceBeacon, urls: []string{"http://10.0.0.2:9000"}}, clk.Now(), "self") {
		t.Error("known verified address should not need a probe")
	}
	p, _ = tb.get("p1")
	if p.Name != "real-name" || !p.LastSeen.Equal(clk.Now()) {
		t.Errorf("hint on verified peer: %+v", p)
	}
	// ... but a new address does.
	if !tb.hint(candidate{id: "p1", source: SourceBeacon, urls: []string{"http://10.0.0.3:9000"}}, clk.Now(), "self") {
		t.Error("new address should need a probe")
	}
	p, _ = tb.get("p1")
	if slices.Contains(p.Addresses, "http://10.0.0.3:9000") {
		t.Error("unverified address added to verified peer")
	}

	// Probe failure marks unhealthy; bye too.
	tb.probeFailed("p1", "connection refused", clk.Now())
	p, _ = tb.get("p1")
	if p.Healthy || p.Error != "connection refused" || !p.Verified {
		t.Errorf("after failure: %+v", p)
	}
	if !tb.hint(candidate{id: "p1", source: SourceBeacon, urls: []string{"http://10.0.0.2:9000"}}, clk.Now(), "self") {
		t.Error("unhealthy peer should be re-probed on sighting")
	}
	tb.observeInfo(testInfo("p1", "real-name"), "http://10.0.0.2:9000", "", clk.Now(), "self")
	tb.bye("p1", func(a string) bool { return a == "http://10.0.0.2:9000" })
	p, _ = tb.get("p1")
	if p.Healthy || p.Error == "" {
		t.Errorf("after bye: %+v", p)
	}
	tb.bye("unknown", func(string) bool { return true }) // no-op

	// TTL expiry: last seen at clk.Now(); not expired at exactly TTL, expired after.
	last := p.LastSeen
	tb.expire(last.Add(time.Minute))
	if _, ok := tb.get("p1"); !ok {
		t.Fatal("expired too early")
	}
	tb.expire(last.Add(time.Minute + time.Nanosecond))
	if _, ok := tb.get("p1"); ok {
		t.Fatal("not expired after TTL")
	}
}

func TestPeerTableIgnoresSelfAndInvalid(t *testing.T) {
	tb := newPeerTable(time.Minute)
	now := time.Now()
	if tb.hint(candidate{id: "self", source: SourceBeacon}, now, "self") {
		t.Error("self hint accepted")
	}
	if _, ok := tb.observeInfo(testInfo("self", "me"), "http://x:1", SourceScan, now, "self"); ok {
		t.Error("self info accepted")
	}
	if tb.hint(candidate{id: "bad id", source: SourceBeacon}, now, "self") {
		t.Error("invalid id accepted")
	}
	if len(tb.snapshot("")) != 0 {
		t.Error("table not empty")
	}
	// snapshot filters self even if present (e.g. ID changed later).
	tb.observeInfo(testInfo("a", "a"), "http://a:1", SourceScan, now, "")
	if got := tb.snapshot("a"); len(got) != 0 {
		t.Errorf("snapshot did not exclude self: %v", got)
	}
}

func TestPeerTableSeedsNeverExpire(t *testing.T) {
	clk := newFakeClock()
	tb := newPeerTable(time.Minute)
	tb.configure(time.Minute, []string{"http://seed:9000"})
	tb.observeInfo(testInfo("s", "seed"), "http://seed:9000", SourceStatic, clk.Now(), "")
	tb.observeInfo(testInfo("o", "other"), "http://other:9000", SourceBeacon, clk.Now(), "")
	// Seen at the seed address but via another mechanism only: not pinned.
	tb.observeInfo(testInfo("x", "x"), "http://seed2:9000", SourceScan, clk.Now(), "")
	clk.Advance(time.Hour)
	tb.expire(clk.Now())
	ids := ids(tb.snapshot(""))
	if !slices.Equal(ids, []string{"s"}) {
		t.Fatalf("after expiry: %v", ids)
	}
	// Removing the seed from the config makes it expirable.
	tb.configure(time.Minute, nil)
	tb.expire(clk.Now())
	if len(tb.snapshot("")) != 0 {
		t.Fatal("former seed not expired")
	}
}

func ids(ps []Peer) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.ID
	}
	return out
}

func TestPeerTableSortAndCopies(t *testing.T) {
	tb := newPeerTable(time.Minute)
	now := time.Now()
	tb.observeInfo(testInfo("3", "b"), "http://b:1", SourceScan, now, "")
	tb.observeInfo(testInfo("2", "a"), "http://a:1", SourceScan, now, "")
	tb.observeInfo(testInfo("1", "b"), "http://b2:1", SourceScan, now, "")
	got := tb.snapshot("")
	if !slices.Equal(ids(got), []string{"2", "1", "3"}) {
		t.Fatalf("order = %v", ids(got))
	}
	// Mutating the snapshot does not affect the table.
	got[0].Addresses[0] = "mutated"
	got[0].Endpoints.All[0] = "mutated"
	p, _ := tb.get("2")
	if p.Addresses[0] == "mutated" || p.Endpoints.All[0] == "mutated" {
		t.Fatal("snapshot aliases table data")
	}
	// Empty slices encode as JSON arrays.
	tb2 := newPeerTable(time.Minute)
	tb2.hint(candidate{id: "z", source: SourceBeacon}, now, "")
	p, _ = tb2.get("z")
	b, _ := json.Marshal(p)
	if !strings.Contains(string(b), `"addresses":[]`) {
		t.Errorf("json = %s", b)
	}
}

func TestPeerTableBounds(t *testing.T) {
	tb := newPeerTable(time.Hour)
	now := time.Now()
	// Address list is bounded.
	for i := range maxAddressesPerPeer + 10 {
		tb.observeInfo(testInfo("p", "p"), fmt.Sprintf("http://h%d:1", i), SourceScan, now, "")
	}
	p, _ := tb.get("p")
	if len(p.Addresses) != maxAddressesPerPeer || p.Addresses[len(p.Addresses)-1] != fmt.Sprintf("http://h%d:1", maxAddressesPerPeer+9) {
		t.Fatalf("addresses: %d %v", len(p.Addresses), p.Addresses[len(p.Addresses)-1])
	}
	// Unverified sightings (anyone on the link can announce any ID) are
	// bounded separately and much lower; the stalest one makes room.
	for i := range maxUnverifiedPeers + 10 {
		tb.hint(candidate{id: fmt.Sprintf("u%d", i), source: SourceBeacon}, now.Add(time.Duration(i)), "")
	}
	if n := len(tb.snapshot("")); n != maxUnverifiedPeers+1 {
		t.Fatalf("size %d, want %d", n, maxUnverifiedPeers+1)
	}
	if _, ok := tb.get("u9"); ok {
		t.Error("stalest unverified entry not evicted")
	}
	if _, ok := tb.get(fmt.Sprintf("u%d", maxUnverifiedPeers+9)); !ok {
		t.Error("newest unverified entry missing")
	}
	// Verified peers fill the table ("p" is one), evicting unverified
	// entries first.
	for i := range maxPeers - 1 {
		if _, ok := tb.observeInfo(testInfo(fmt.Sprintf("v%d", i), "v"), "http://v:1", SourceScan, now, ""); !ok {
			t.Fatalf("verified peer %d rejected", i)
		}
	}
	if n := len(tb.snapshot("")); n != maxPeers {
		t.Fatalf("size %d", n)
	}
	if tb.unverified != 0 {
		t.Errorf("unverified count %d", tb.unverified)
	}
	// Then unverified newcomers are rejected.
	if tb.hint(candidate{id: "late", source: SourceBeacon}, now, "") {
		t.Error("unverified accepted into full verified table")
	}
	if _, ok := tb.get("late"); ok {
		t.Error("unverified displaced verified peer")
	}
	// The unverified count follows verification and expiry.
	tb2 := newPeerTable(time.Minute)
	tb2.hint(candidate{id: "a", source: SourceBeacon}, now, "")
	tb2.hint(candidate{id: "b", source: SourceBeacon}, now, "")
	tb2.observeInfo(testInfo("a", "a"), "http://a:1", SourceBeacon, now, "")
	if tb2.unverified != 1 {
		t.Errorf("after verification: %d", tb2.unverified)
	}
	tb2.expire(now.Add(time.Hour))
	if tb2.unverified != 0 || len(tb2.peers) != 0 {
		t.Errorf("after expiry: %d %d", tb2.unverified, len(tb2.peers))
	}
}

func TestPeerTableDue(t *testing.T) {
	clk := newFakeClock()
	tb := newPeerTable(time.Hour)
	tb.observeInfo(testInfo("v", "v"), "http://v1:1", SourceScan, clk.Now(), "")
	tb.observeInfo(testInfo("v", "v"), "http://v2:1", SourceScan, clk.Now(), "")
	tb.hint(candidate{id: "u", source: SourceBeacon, urls: []string{"http://u:1"}}, clk.Now(), "")
	if d := tb.due(clk.Now(), reprobeEvery); len(d) != 0 {
		t.Fatalf("due too early: %v", d)
	}
	clk.Advance(reprobeEvery)
	d := tb.due(clk.Now(), reprobeEvery)
	if len(d) != 1 || d[0].id != "v" || !slices.Equal(d[0].urls, []string{"http://v2:1", "http://v1:1"}) {
		t.Fatalf("due = %+v", d)
	}
}

func TestSanitizeInfo(t *testing.T) {
	in := Info{
		Service: ServiceName, ID: "x",
		Name:     "  evil\x00name\n" + strings.Repeat("n", 1000),
		Version:  "\xff\xfe",
		Features: make([]string, 1000),
	}
	for i := range in.Features {
		in.Features[i] = "f"
	}
	for range 1000 {
		in.Endpoints.All = append(in.Endpoints.All, "http://a")
	}
	out := sanitizeInfo(in)
	if strings.ContainsAny(out.Name, "\x00\n") || len(out.Name) > maxFieldLen {
		t.Errorf("name = %q", out.Name)
	}
	if out.Version != "��" && out.Version != "�" {
		t.Errorf("version = %q", out.Version)
	}
	if len(out.Features) != maxFeatures || len(out.Endpoints.All) != maxEndpointsAll {
		t.Errorf("lists %d %d", len(out.Features), len(out.Endpoints.All))
	}
}
