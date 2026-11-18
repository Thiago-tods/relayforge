package delivery

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Thiago-tods/relayforge/internal/observability"
)

func TestWorkerCheckReportsAStalledLoop(t *testing.T) {
	w := &Worker{
		d:            WorkerDeps{Config: DefaultWorkerConfig()},
		loopBeat:     observability.NewHeartbeat(),
		maintainBeat: observability.NewHeartbeat(),
	}
	ctx := context.Background()
	if err := w.Check(ctx); err != nil {
		t.Fatalf("a fresh worker must be ready: %v", err)
	}
	w.loopBeat.BeatAt(time.Now().Add(-5 * time.Minute))
	if err := w.Check(ctx); !errors.Is(err, observability.ErrStalled) {
		t.Fatalf("a claim loop that stopped turning must be reported: %v", err)
	}
	w.loopBeat.Beat()
	w.maintainBeat.BeatAt(time.Now().Add(-10 * time.Minute))
	if err := w.Check(ctx); !errors.Is(err, observability.ErrStalled) {
		t.Fatalf("a maintenance loop that stopped must be reported: %v", err)
	}
}
