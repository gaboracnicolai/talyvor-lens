package poolroyalty

// micro converts a whole-LENS test value to integer µLENS (SEC-2: 1 LENS = 1e6 µLENS).
// Shared across the poolroyalty test files.
func micro(lens float64) int64 { return int64(lens * 1e6) }
