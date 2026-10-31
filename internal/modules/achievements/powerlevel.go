package achievements

import "math"

// XPInput holds the PowerLevel.xp arguments. Fields left zero match Swift's
// default arguments (graphConnections, activeSpaces, streakDays = 0).
type XPInput struct {
	MemoriesTotal    int
	SessionsCount    int
	JobsCount        int
	BadgesEarned     int
	GraphConnections int
	ActiveSpaces     int
	StreakDays       int
}

// PowerLevelXP is Swift's PowerLevel.xp (Sources/App/Achievements/PowerLevel.swift),
// shared with /v1/dashboard/profile (Dashboard, not yet ported):
//
//	memories×1 + sessions×3 + jobs×2 + badges×10 + connections×1 + spaces×5 + streak×3
func PowerLevelXP(in XPInput) int {
	return in.MemoriesTotal*1 + in.SessionsCount*3 + in.JobsCount*2 + in.BadgesEarned*10 +
		in.GraphConnections*1 + in.ActiveSpaces*5 + in.StreakDays*3
}

// PowerLevel is Swift's PowerLevel.level(forXP:): floor(sqrt(xp)) + 1, and 1
// for xp ≤ 0. Swift computes it through Double, so does this.
func PowerLevel(xp int) int {
	if xp <= 0 {
		return 1
	}
	return int(math.Floor(math.Sqrt(float64(xp)))) + 1
}
