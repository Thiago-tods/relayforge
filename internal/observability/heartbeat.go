package observability

import (
	"errors"
	"sync/atomic"
	"time"
)

// Heartbeat records that a background loop is still making progress. A loop calls
// Beat on every iteration; a readiness check calls Check to learn whether the loop
// has stalled. A process whose database ping succeeds but whose claim loop is wedged
// is therefore reported as not ready instead of silently doing nothing.
//
// A nil *Heartbeat ignores Beat and always reports healthy, so instrumented code
// works without one. The zero value is not started: Check reports healthy until the
// first Beat, which lets a process become ready before its loop has run once.
type Heartbeat struct{ last atomic.Int64 }

// NewHeartbeat returns a heartbeat that counts as just beaten, so a loop that never
// starts is detected after maxAge as well.
func NewHeartbeat() *Heartbeat {
	h := &Heartbeat{}
	h.Beat()
	return h
}

// Beat records progress now.
func (h *Heartbeat) Beat() {
	if h != nil {
		h.last.Store(time.Now().UnixNano())
	}
}

// BeatAt records progress at a given time. It exists for tests that need an old beat.
func (h *Heartbeat) BeatAt(t time.Time) {
	if h != nil {
		h.last.Store(t.UnixNano())
	}
}

// ErrStalled is returned by Check when a loop has not beaten within the allowed age.
var ErrStalled = errors.New("background loop has stalled")

// Check returns ErrStalled when the last beat is older than maxAge.
func (h *Heartbeat) Check(maxAge time.Duration) error {
	if h == nil {
		return nil
	}
	last := h.last.Load()
	if last == 0 {
		return nil
	}
	if time.Since(time.Unix(0, last)) > maxAge {
		return ErrStalled
	}
	return nil
}
