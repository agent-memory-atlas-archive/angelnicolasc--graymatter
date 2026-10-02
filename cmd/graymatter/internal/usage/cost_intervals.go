package usage

import (
	"strings"
	"time"
)

func costScopeLevel(scope string) int {
	switch {
	case scope == "organization":
		return 4
	case strings.HasPrefix(scope, "project:"):
		return 3
	case strings.HasPrefix(scope, "session:"):
		return 2
	case strings.HasPrefix(scope, "request:"):
		return 1
	default:
		return 0
	}
}

type costAccountKey struct{ provider, account, currency string }

// Explicit scopes at one level are disjoint unless their IDs match. Indexing
// each level and exact scope avoids comparing every unrelated request/session.
// Across levels we intentionally preserve conservative overlap suppression.
type costOverlapIndex struct {
	levels [5]*costInterval
	scopes map[string]*costInterval
}

func (x *costOverlapIndex) first(c CostObservation, best int) int {
	level := costScopeLevel(c.Scope)
	for candidate, root := range x.levels {
		if candidate != level || level == 0 || level == 4 {
			best = firstCostOverlap(root, c.StartAt, c.EndAt, best)
		}
	}
	if level > 0 && level < 4 {
		best = firstCostOverlap(x.scopes[c.Scope], c.StartAt, c.EndAt, best)
	}
	return best
}

func (x *costOverlapIndex) add(c CostObservation, order int) {
	level := costScopeLevel(c.Scope)
	x.levels[level] = insertCostInterval(x.levels[level], newCostInterval(c, order))
	if level > 0 && level < 4 {
		if x.scopes == nil {
			x.scopes = make(map[string]*costInterval)
		}
		x.scopes[c.Scope] = insertCostInterval(x.scopes[c.Scope], newCostInterval(c, order))
	}
}

// A deterministic treap indexes interval starts. maxEnd prunes disjoint time
// windows; firstOrder prunes branches that cannot beat the earliest accepted
// overlap. Disjoint large ledgers take expected O(N log N), with O(N) storage.
// Query results are independent of the tree shape and timestamp ties.
type costInterval struct {
	start, end, maxEnd time.Time
	order, firstOrder  int
	priority           uint64
	left, right        *costInterval
}

func newCostInterval(c CostObservation, order int) *costInterval {
	// SplitMix64 disperses sequential acceptance positions without global RNG
	// state or nondeterministic tests. Equal priorities use acceptance order.
	p := uint64(order) + 0x9e3779b97f4a7c15
	p = (p ^ (p >> 30)) * 0xbf58476d1ce4e5b9
	p = (p ^ (p >> 27)) * 0x94d049bb133111eb
	p ^= p >> 31
	return &costInterval{start: c.StartAt, end: c.EndAt, maxEnd: c.EndAt, order: order, firstOrder: order, priority: p}
}
func (n *costInterval) update() {
	n.maxEnd, n.firstOrder = n.end, n.order
	for _, child := range []*costInterval{n.left, n.right} {
		if child == nil {
			continue
		}
		if child.maxEnd.After(n.maxEnd) {
			n.maxEnd = child.maxEnd
		}
		if child.firstOrder < n.firstOrder {
			n.firstOrder = child.firstOrder
		}
	}
}
func earlierCostPriority(a, b *costInterval) bool {
	return a.priority < b.priority || a.priority == b.priority && a.order < b.order
}
func insertCostInterval(root, node *costInterval) *costInterval {
	if root == nil {
		return node
	}
	if node.start.Before(root.start) || node.start.Equal(root.start) && node.order < root.order {
		root.left = insertCostInterval(root.left, node)
		if earlierCostPriority(root.left, root) {
			child := root.left
			root.left, child.right = child.right, root
			root.update()
			child.update()
			return child
		}
	} else {
		root.right = insertCostInterval(root.right, node)
		if earlierCostPriority(root.right, root) {
			child := root.right
			root.right, child.left = child.left, root
			root.update()
			child.update()
			return child
		}
	}
	root.update()
	return root
}
func firstCostOverlap(root *costInterval, start, end time.Time, best int) int {
	if root == nil || root.firstOrder >= best || !root.maxEnd.After(start) {
		return best
	}
	if !root.start.Before(end) {
		return firstCostOverlap(root.left, start, end, best)
	}
	if start.Before(root.end) && root.order < best {
		best = root.order
	}
	first, second := root.left, root.right
	if first == nil || second != nil && second.firstOrder < first.firstOrder {
		first, second = second, first
	}
	best = firstCostOverlap(first, start, end, best)
	return firstCostOverlap(second, start, end, best)
}
