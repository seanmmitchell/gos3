package cluster

import (
	"errors"
	"testing"
)

func TestDistinctMemoryAndDurableAcks(t *testing.T) {
	a, err := NewAcks(AckPolicy{Memory: 2, Durable: 2}, 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, ack := range []struct {
		id      string
		durable bool
	}{
		{"a", false}, {"a", false}, {"b", true}, {"b", true},
	} {
		if err := a.Record(ack.id, ack.durable); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Require(); !errors.Is(err, ErrInsufficientAcks) {
		t.Fatalf("Require() = %v; want insufficient acks", err)
	}
	if memory, durable := a.Counts(); memory != 2 || durable != 1 {
		t.Fatalf("counts = %d, %d; want 2, 1", memory, durable)
	}
	if err := a.Record("a", true); err != nil {
		t.Fatal(err)
	}
	if err := a.Require(); err != nil {
		t.Fatal(err)
	}
}

func TestAckPolicyBounds(t *testing.T) {
	for _, p := range []AckPolicy{{Memory: 0, Durable: 1}, {Memory: 3, Durable: 1}, {Memory: 2, Durable: 0}, {Memory: 1, Durable: 3}} {
		if err := p.Validate(2); err == nil {
			t.Fatalf("accepted invalid policy %+v", p)
		}
	}
	if _, err := NewAcks(AckPolicy{Memory: 2, Durable: 1}, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := NewAcks(AckPolicy{Memory: 1, Durable: 2}, 2); err != nil {
		t.Fatal(err)
	}
}
