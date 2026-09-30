package billing

import "time"

// Fixed instants, so a failure message is reproducible.
func testTime() time.Time { return time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC) }

func zeroTime() time.Time { return time.Time{} }
