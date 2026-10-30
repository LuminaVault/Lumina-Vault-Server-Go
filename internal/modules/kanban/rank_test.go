package kanban_test

import (
	"lumina/internal/modules/kanban"
	"testing"

	"github.com/stretchr/testify/require"
)

func p(s string) *string { return &s }

// Ports Tests/AppTests/Kanban/RankStringTests.swift (5 tests). Swift compares
// Strings by unicode scalars; ranks are ASCII, so Go's < is the same order.

func TestBetweenNilNilIsMidpoint(t *testing.T) {
	require.NotEmpty(t, kanban.Between(nil, nil))
}

func TestBetweenOrdersCorrectly(t *testing.T) {
	a := kanban.Between(nil, nil)
	b := kanban.Between(&a, nil)
	mid := kanban.Between(&a, &b)
	require.Less(t, a, mid)
	require.Less(t, mid, b)
}

func TestRepeatedInsertAtFrontStaysOrdered(t *testing.T) {
	first := kanban.Between(nil, nil)
	for range 50 {
		next := kanban.Between(nil, &first)
		require.Less(t, next, first)
		first = next
	}
}

func TestRepeatedInsertAtEndStaysOrdered(t *testing.T) {
	last := kanban.Between(nil, nil)
	for range 50 {
		next := kanban.Between(&last, nil)
		require.Less(t, last, next)
		last = next
	}
}

func TestAdjacentKeysGetMidpoint(t *testing.T) {
	a := kanban.Between(nil, nil)
	b := kanban.Between(&a, nil)
	lo, hi := a, b
	for range 30 {
		mid := kanban.Between(&lo, &hi)
		require.True(t, lo < mid && mid < hi, "lo=%s mid=%s hi=%s", lo, mid, hi)
		hi = mid
	}
}

// Byte-identical outputs, worked by hand from RankString.swift. Swift and Go
// must produce exactly these keys, because ranks are stored and compared.
func TestBetweenExactKeys(t *testing.T) {
	cases := []struct {
		lo, hi *string
		want   string
	}{
		{nil, nil, "i"},
		{p("i"), nil, "r"},
		{p("r"), nil, "v"},
		{nil, p("i"), "9"},
		{p("9"), p("i"), "d"},
		{p("z"), nil, "zi"},
		{p("zzz"), nil, "zzzi"},
		{p("a"), p("b"), "ai"},
		{p("i"), p("i"), "ii"},
		{p("i5"), p("j"), "i5i"},
		{nil, p("1"), "0i"},
		{nil, p("0"), "0i"}, // not below "0": a Swift quirk kept as is
		{p("A"), nil, "i"},  // unknown digits read as 0
		{p(""), p(""), "0i"},
		{p("ab"), p("az"), "an"},
	}
	for _, c := range cases {
		require.Equal(t, c.want, kanban.Between(c.lo, c.hi), "between(%v, %v)", deref(c.lo), deref(c.hi))
	}
}

func deref(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}
