package discovery

import "container/list"

// lru is a bounded map that forgets its least recently used key when a new
// key would exceed its capacity. Unlike clearing a full map, this never
// resets the state of active keys (e.g. a flooding sender's rate-limit
// bucket) because many other keys were seen. It is not safe for concurrent
// use.
type lru[K comparable, V any] struct {
	max   int
	order *list.List // of *lruItem[K, V]; front = most recently used
	items map[K]*list.Element
}

type lruItem[K comparable, V any] struct {
	key K
	val V
}

func newLRU[K comparable, V any](capacity int) *lru[K, V] {
	return &lru[K, V]{max: capacity, order: list.New(), items: make(map[K]*list.Element)}
}

// get returns the value of key and marks it most recently used.
func (c *lru[K, V]) get(key K) (V, bool) {
	el, ok := c.items[key]
	if !ok {
		var zero V
		return zero, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*lruItem[K, V]).val, true
}

// put stores val under key as the most recently used entry, evicting the
// least recently used entry when the cache is full.
func (c *lru[K, V]) put(key K, val V) {
	if el, ok := c.items[key]; ok {
		el.Value.(*lruItem[K, V]).val = val
		c.order.MoveToFront(el)
		return
	}
	if c.order.Len() >= c.max {
		if back := c.order.Back(); back != nil {
			delete(c.items, back.Value.(*lruItem[K, V]).key)
			c.order.Remove(back)
		}
	}
	c.items[key] = c.order.PushFront(&lruItem[K, V]{key: key, val: val})
}

// len returns the number of entries.
func (c *lru[K, V]) len() int { return c.order.Len() }
