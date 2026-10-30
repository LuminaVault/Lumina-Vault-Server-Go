package kanban

import "strings"

// rankDigits are RankString's base-36 digits (Kanban/RankString.swift).
const rankDigits = "0123456789abcdefghijklmnopqrstuvwxyz"

const rankBase = len(rankDigits) // 36

func rankVal(r rune) int {
	if i := strings.IndexRune(rankDigits, r); i >= 0 {
		return i
	}
	return 0 // Swift: firstIndex(of:) ?? 0
}

func rankChr(i int) byte { return rankDigits[max(0, min(rankBase-1, i))] }

// Between is RankString.between: a key strictly between lo and hi in byte
// order (nil means the unbounded start or end). It is a line-by-line port,
// quirks included: a key past the end of hi reads as digit 0, and equal keys
// get the midpoint digit appended.
func Between(lo, hi *string) string {
	var a []rune
	if lo != nil {
		a = []rune(*lo)
	}
	var b []rune
	if hi != nil {
		b = []rune(*hi)
	}
	var out []byte
	for i := 0; ; {
		da := 0
		if i < len(a) {
			da = rankVal(a[i])
		}
		db := rankBase
		if hi != nil {
			db = 0
			if i < len(b) {
				db = rankVal(b[i])
			}
		}
		if db-da > 1 {
			return string(append(out, rankChr((da+db)/2)))
		}
		out = append(out, rankChr(da))
		i++
		if i >= len(a) && (hi == nil || i >= len(b)) {
			return string(append(out, rankChr(rankBase/2)))
		}
	}
}
