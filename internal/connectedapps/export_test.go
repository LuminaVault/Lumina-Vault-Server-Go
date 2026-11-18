package connectedapps

import "time"

// SetNow swaps the clock for tests in the external test package.
func (s *Service) SetNow(f func() time.Time) { s.now = f }
