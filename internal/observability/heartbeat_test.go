package observability

import (
	"errors"
	"testing"
	"time"
)

func TestHeartbeatReportsStalledLoops(t *testing.T) {
	var nilBeat *Heartbeat
	nilBeat.Beat()
	if err := nilBeat.Check(time.Nanosecond); err != nil {
		t.Errorf("a nil heartbeat is always healthy: %v", err)
	}
	if err := (&Heartbeat{}).Check(time.Nanosecond); err != nil {
		t.Errorf("a heartbeat that never started is healthy until its first beat: %v", err)
	}

	h := NewHeartbeat()
	if err := h.Check(time.Minute); err != nil {
		t.Errorf("a fresh heartbeat must be healthy: %v", err)
	}
	time.Sleep(15 * time.Millisecond)
	if err := h.Check(5 * time.Millisecond); !errors.Is(err, ErrStalled) {
		t.Errorf("a heartbeat older than maxAge must be stalled, got %v", err)
	}
	h.Beat()
	if err := h.Check(time.Minute); err != nil {
		t.Errorf("a beat must clear the stall: %v", err)
	}
	h.BeatAt(time.Now().Add(-time.Hour))
	if err := h.Check(time.Minute); !errors.Is(err, ErrStalled) {
		t.Errorf("an old beat must be stalled, got %v", err)
	}
}
