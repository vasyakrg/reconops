package runner

import "time"

// after returns a channel that fires after ms milliseconds. Wrapped so the
// poll loop in watchRunCompletion does not allocate a Timer per iteration
// in tests that override behaviour.
func after(ms int) <-chan time.Time { return time.After(time.Duration(ms) * time.Millisecond) }
