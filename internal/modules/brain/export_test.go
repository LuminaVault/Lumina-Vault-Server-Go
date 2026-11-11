package brain

import "time"

// SetSimilarTimeout lets the HTTP tests force the similarity query past its deadline.
func SetSimilarTimeout(m *Module, d time.Duration) { m.similarTimeout = d }
