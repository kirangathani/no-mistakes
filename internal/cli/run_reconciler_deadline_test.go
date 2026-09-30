package cli

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// passedDeadlineCtx is a context whose deadline has passed but whose timer has
// not fired yet, the window a --wait expiry opens on every run-state read.
type passedDeadlineCtx struct{ context.Context }

func (passedDeadlineCtx) Deadline() (time.Time, bool) { return time.Now().Add(-time.Second), true }

// A run-state read in that window used to return ctx.Err() - still nil - as
// success with an empty result, so Reconcile handed the drive loop a nil run
// and an elapsed --wait was reported as "run not found".
func TestRunStateReadAfterItsDeadlineIsAnError(t *testing.T) {
	socketPath := filepath.Join(makeSocketSafeTempDir(t), "reconcile.sock")
	srv := ipc.NewServer()
	srv.Handle(ipc.MethodGetRun, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.GetRunResult{Run: &ipc.RunInfo{ID: "run-1", Status: types.RunRunning}}, nil
	})
	startIPCServer(t, srv, socketPath)

	run, err := (&ipcRunStateSource{socketPath: socketPath}).Reconcile(passedDeadlineCtx{context.Background()}, "run-1")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Reconcile past its deadline = (%v, %v), want context.DeadlineExceeded", run, err)
	}
}

// The same window reaches the caller's classification: the read reports the
// passed deadline before the drive context's own timer fires, and that must
// still read as an elapsed --wait rather than a failure.
func TestAxiWaitElapsedBeforeTheDriveTimerFires(t *testing.T) {
	if !isAxiWaitElapsed(context.Background(), passedDeadlineCtx{context.Background()}, context.DeadlineExceeded) {
		t.Fatal("a passed drive deadline whose timer has not fired was not classified as an elapsed wait")
	}
	if isAxiWaitElapsed(context.Background(), context.Background(), context.DeadlineExceeded) {
		t.Fatal("a deadline error with no drive deadline was classified as an elapsed wait")
	}
}
