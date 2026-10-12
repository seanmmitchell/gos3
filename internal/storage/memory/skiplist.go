package memory

import "math/rand/v2"

// skipList is an ordered map from string keys (compared byte-wise) to values
// of type V. Lookups, inserts, deletes and seeks are O(log n) expected;
// stepping to the next key is O(1). It is not safe for concurrent use; the
// owning bucket's lock guards it.
type skipList[V any] struct {
	head  slNode[V] // sentinel; head.next[i] is the first node at level i
	level int       // number of levels currently in use (>= 1)
	n     int
}

type slNode[V any] struct {
	key  string
	val  V
	next []*slNode[V]
}

const (
	slMaxLevel = 32
	// Each additional level is kept with probability 1/4, which gives an
	// expected ~1.33 pointers per node.
	slLevelMask = 3
)

func newSkipList[V any]() *skipList[V] {
	s := &skipList[V]{level: 1}
	s.head.next = make([]*slNode[V], slMaxLevel)
	return s
}

func randomLevel() int {
	l := 1
	for l < slMaxLevel && rand.Uint32()&slLevelMask == 0 {
		l++
	}
	return l
}

// Len returns the number of keys.
func (s *skipList[V]) Len() int { return s.n }

// findPredecessors fills update[i] with the last node at level i whose key is
// < key and returns the candidate node at level 0 (first node with key >= key).
func (s *skipList[V]) findPredecessors(key string, update []*slNode[V]) *slNode[V] {
	x := &s.head
	for i := s.level - 1; i >= 0; i-- {
		for x.next[i] != nil && x.next[i].key < key {
			x = x.next[i]
		}
		if update != nil {
			update[i] = x
		}
	}
	return x.next[0]
}

// Get returns the value stored for key.
func (s *skipList[V]) Get(key string) (V, bool) {
	if x := s.findPredecessors(key, nil); x != nil && x.key == key {
		return x.val, true
	}
	var zero V
	return zero, false
}

// Set inserts or replaces the value for key.
func (s *skipList[V]) Set(key string, val V) {
	var update [slMaxLevel]*slNode[V]
	x := s.findPredecessors(key, update[:])
	if x != nil && x.key == key {
		x.val = val
		return
	}
	lvl := randomLevel()
	if lvl > s.level {
		for i := s.level; i < lvl; i++ {
			update[i] = &s.head
		}
		s.level = lvl
	}
	node := &slNode[V]{key: key, val: val, next: make([]*slNode[V], lvl)}
	for i := 0; i < lvl; i++ {
		node.next[i] = update[i].next[i]
		update[i].next[i] = node
	}
	s.n++
}

// Delete removes key and reports whether it was present.
func (s *skipList[V]) Delete(key string) bool {
	var update [slMaxLevel]*slNode[V]
	x := s.findPredecessors(key, update[:])
	if x == nil || x.key != key {
		return false
	}
	for i := 0; i < len(x.next); i++ {
		if update[i].next[i] == x {
			update[i].next[i] = x.next[i]
		}
	}
	for s.level > 1 && s.head.next[s.level-1] == nil {
		s.level--
	}
	s.n--
	return true
}

// SeekGE returns the first node whose key is >= key, or nil.
func (s *skipList[V]) SeekGE(key string) *slNode[V] {
	return s.findPredecessors(key, nil)
}

// SeekGT returns the first node whose key is > key, or nil.
func (s *skipList[V]) SeekGT(key string) *slNode[V] {
	x := s.findPredecessors(key, nil)
	if x != nil && x.key == key {
		x = x.next[0]
	}
	return x
}

// First returns the node with the smallest key, or nil.
func (s *skipList[V]) First() *slNode[V] { return s.head.next[0] }

// Next returns the node following n in key order, or nil.
func (n *slNode[V]) Next() *slNode[V] { return n.next[0] }
