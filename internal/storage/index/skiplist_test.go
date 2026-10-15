package index

import (
	"math/rand/v2"
	"slices"
	"strconv"
	"testing"
)

func TestSkipListAgainstReference(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	s := newSkipList[int]()
	ref := map[string]int{}
	for i := 0; i < 20000; i++ {
		k := strconv.Itoa(rng.IntN(3000))
		switch rng.IntN(3) {
		case 0, 1:
			s.Set(k, i)
			ref[k] = i
		case 2:
			_, want := ref[k]
			if got := s.Delete(k); got != want {
				t.Fatalf("Delete(%q) = %v, want %v", k, got, want)
			}
			delete(ref, k)
		}
		if s.Len() != len(ref) {
			t.Fatalf("Len = %d, want %d", s.Len(), len(ref))
		}
	}
	keys := make([]string, 0, len(ref))
	for k := range ref {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var got []string
	for n := s.First(); n != nil; n = n.Next() {
		got = append(got, n.key)
		if n.val != ref[n.key] {
			t.Fatalf("value for %q = %d, want %d", n.key, n.val, ref[n.key])
		}
	}
	if !slices.Equal(got, keys) {
		t.Fatalf("iteration order mismatch")
	}
	for _, probe := range []string{"", "0", "1", "15", "2999", "3", "9", "99999", "\xff"} {
		i, _ := slices.BinarySearch(keys, probe)
		var want string
		if i < len(keys) {
			want = keys[i]
		}
		if n := s.SeekGE(probe); (n == nil) != (want == "") || (n != nil && n.key != want) {
			t.Fatalf("SeekGE(%q) mismatch, want %q", probe, want)
		}
		j := i
		if j < len(keys) && keys[j] == probe {
			j++
		}
		want = ""
		if j < len(keys) {
			want = keys[j]
		}
		if n := s.SeekGT(probe); (n == nil) != (want == "") || (n != nil && n.key != want) {
			t.Fatalf("SeekGT(%q) mismatch, want %q", probe, want)
		}
	}
	for _, k := range keys {
		if v, ok := s.Get(k); !ok || v != ref[k] {
			t.Fatalf("Get(%q) = %d, %v", k, v, ok)
		}
	}
	if _, ok := s.Get("nope"); ok {
		t.Fatal("Get of missing key succeeded")
	}
	for _, k := range keys {
		s.Delete(k)
	}
	if s.Len() != 0 || s.First() != nil || s.level != 1 {
		t.Fatalf("not empty after deleting everything: len=%d level=%d", s.Len(), s.level)
	}
}
