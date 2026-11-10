package testenv

import "time"

// MigrationBudget returns ten times work, clamped to one through ten minutes.
// work is the fixture's own measured core-migration time, which takes the same
// database-wide session lock as the migrations this budget protects.
func MigrationBudget(work time.Duration) time.Duration {
	// Clamp before multiplying so even extreme durations cannot overflow.
	if work <= time.Minute/10 {
		return time.Minute
	}
	if work >= time.Minute {
		return 10 * time.Minute
	}
	return 10 * work
}
