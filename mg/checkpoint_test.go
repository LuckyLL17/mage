package mg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

var (
	cpCallsA            int64
	cpCallsB            int64
	cpCallsFlaky        int64
	cpCallsFlakyParent  int64
	cpCallsParent       int64
	cpCallsSerialParent int64
	cpCallsMixedParent  int64
	cpCallsUndeclared   int64
	cpCallsArg          int64
	cpCallsCancel       int64
	cpCallsFPError      int64

	cpFpA        = "a1"
	cpFpB        = "b1"
	cpFpFlaky    = "f1"
	cpFpParent   = "p1"
	cpFpSerial   = "s1"
	cpFpMixed    = "m1"
	cpFpArg      = "x1"
	cpFpCancel   = "c1"
	cpFlakyFails bool
	cpFPError    error
	cpArgSeen    []string
)

func cpStringFP(s *string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return *s, nil }
}

func init() {
	AddCheckpoint(cpStageA, Checkpoint{ID: "stage-a", Inputs: cpStringFP(&cpFpA)})
	AddCheckpoint(cpStageB, Checkpoint{ID: "stage-b", Inputs: cpStringFP(&cpFpB)})
	AddCheckpoint(cpFlaky, Checkpoint{ID: "flaky", Inputs: cpStringFP(&cpFpFlaky)})
	AddCheckpoint(cpParent, Checkpoint{ID: "parent", Inputs: cpStringFP(&cpFpParent)})
	AddCheckpoint(cpFlakyParent, Checkpoint{ID: "flaky-parent", Inputs: cpStringFP(&cpFpParent)})
	AddCheckpoint(cpSerialParent, Checkpoint{ID: "serial-parent", Inputs: cpStringFP(&cpFpSerial)})
	AddCheckpoint(cpMixedParent, Checkpoint{ID: "mixed-parent", Inputs: cpStringFP(&cpFpMixed)})
	AddCheckpoint(cpArgTarget, Checkpoint{ID: "arg", Inputs: cpStringFP(&cpFpArg)})
	AddCheckpoint(cpCancelTarget, Checkpoint{ID: "cancel", Inputs: cpStringFP(&cpFpCancel)})
	AddCheckpoint(cpFPErrorTarget, Checkpoint{
		ID: "fp-error",
		Inputs: func(context.Context) (string, error) {
			return "", cpFPError
		},
	})
}

func cpStageA(ctx context.Context) error {
	return RunCheckpoint(ctx, cpStageA, func(ctx context.Context) error {
		atomic.AddInt64(&cpCallsA, 1)
		return nil
	})
}

func cpStageB(ctx context.Context) error {
	return RunCheckpoint(ctx, cpStageB, func(ctx context.Context) error {
		atomic.AddInt64(&cpCallsB, 1)
		return nil
	})
}

func cpFlaky(ctx context.Context) error {
	return RunCheckpoint(ctx, cpFlaky, func(ctx context.Context) error {
		atomic.AddInt64(&cpCallsFlaky, 1)
		if cpFlakyFails {
			return errors.New("boom")
		}
		return nil
	})
}

func cpParent(ctx context.Context) error {
	return RunCheckpoint(ctx, cpParent, func(ctx context.Context) error {
		CtxDeps(ctx, cpStageA, cpStageB)
		atomic.AddInt64(&cpCallsParent, 1)
		return nil
	})
}

func cpFlakyParent(ctx context.Context) error {
	return RunCheckpoint(ctx, cpFlakyParent, func(ctx context.Context) error {
		CtxDeps(ctx, cpStageA, cpFlaky)
		atomic.AddInt64(&cpCallsFlakyParent, 1)
		return nil
	})
}

func cpSerialParent(ctx context.Context) error {
	return RunCheckpoint(ctx, cpSerialParent, func(ctx context.Context) error {
		SerialCtxDeps(ctx, cpStageA, cpStageB)
		atomic.AddInt64(&cpCallsSerialParent, 1)
		return nil
	})
}

func cpUndeclared(context.Context) {
	atomic.AddInt64(&cpCallsUndeclared, 1)
}

func cpMixedParent(ctx context.Context) error {
	return RunCheckpoint(ctx, cpMixedParent, func(ctx context.Context) error {
		CtxDeps(ctx, cpUndeclared)
		atomic.AddInt64(&cpCallsMixedParent, 1)
		return nil
	})
}

func cpArgTarget(ctx context.Context, s string) error {
	return RunCheckpoint(ctx, cpArgTarget, func(ctx context.Context) error {
		atomic.AddInt64(&cpCallsArg, 1)
		cpArgSeen = append(cpArgSeen, s)
		return nil
	}, s)
}

// cpCancelTarget returns ctx.Err() when the context is already cancelled,
// mimicking a target that notices cancellation.
func cpCancelTarget(ctx context.Context) error {
	return RunCheckpoint(ctx, cpCancelTarget, func(ctx context.Context) error {
		atomic.AddInt64(&cpCallsCancel, 1)
		return ctx.Err()
	})
}

func cpFPErrorTarget(ctx context.Context) error {
	return RunCheckpoint(ctx, cpFPErrorTarget, func(ctx context.Context) error {
		atomic.AddInt64(&cpCallsFPError, 1)
		return nil
	})
}

func cpResetState() {
	atomic.StoreInt64(&cpCallsA, 0)
	atomic.StoreInt64(&cpCallsB, 0)
	atomic.StoreInt64(&cpCallsFlaky, 0)
	atomic.StoreInt64(&cpCallsFlakyParent, 0)
	atomic.StoreInt64(&cpCallsParent, 0)
	atomic.StoreInt64(&cpCallsSerialParent, 0)
	atomic.StoreInt64(&cpCallsMixedParent, 0)
	atomic.StoreInt64(&cpCallsUndeclared, 0)
	atomic.StoreInt64(&cpCallsArg, 0)
	atomic.StoreInt64(&cpCallsCancel, 0)
	atomic.StoreInt64(&cpCallsFPError, 0)
	cpFpA, cpFpB, cpFpFlaky, cpFpParent, cpFpSerial, cpFpMixed, cpFpArg, cpFpCancel =
		"a1", "b1", "f1", "p1", "s1", "m1", "x1", "c1"
	cpFlakyFails = false
	cpFPError = nil
	cpArgSeen = nil
}

// cpSetup starts a checkpoint-enabled run with a fresh on-disk state dir.
func cpSetup(t *testing.T, project string) {
	t.Helper()
	t.Setenv(CheckpointEnv, "1")
	t.Setenv(CheckpointDirEnv, t.TempDir())
	t.Setenv(ProjectIDEnv, project)
	cpResetState()
	ResetCheckpoints()
	ResetDeps()
	t.Cleanup(func() {
		ResetCheckpoints()
		ResetDeps()
	})
}

// cpNewProcess simulates starting a new mage process in the same project:
// in-memory caches clear, the on-disk checkpoint state and registrations
// survive.
func cpNewProcess() {
	ResetCheckpoints()
	ResetDeps()
}

func cpLastDecision(t *testing.T) Decision {
	t.Helper()
	ds := CheckpointDecisions()
	if len(ds) == 0 {
		t.Fatal("expected at least one checkpoint decision")
	}
	return ds[len(ds)-1]
}

func cpDecisionFor(t *testing.T, target interface{}, args ...interface{}) Decision {
	t.Helper()
	fn := F(target, args...)
	ds := CheckpointDecisions()
	for i := len(ds) - 1; i >= 0; i-- {
		if ds[i].Target == fn.Name() && ds[i].ArgsID == fn.ID() {
			return ds[i]
		}
	}
	t.Fatalf("no decision recorded for %s", fn.Name())
	return Decision{}
}

func cpLoadFile(t *testing.T) *checkpointFile {
	t.Helper()
	path, err := checkpointPath()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f := &checkpointFile{}
	if err := json.Unmarshal(data, f); err != nil {
		t.Fatal(err)
	}
	return f
}

func cpRecoverDeps(t *testing.T, fn func(context.Context) error) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatal("expected the dependency run to panic")
		}
	}()
	_ = fn(context.Background())
}

func TestCheckpointReuse(t *testing.T) {
	cpSetup(t, "proj-1")

	if err := cpStageA(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt64(&cpCallsA); got != 1 {
		t.Fatalf("expected body to run once, got %d", got)
	}
	if d := cpLastDecision(t); d.Reused || d.Reason != ReasonNoPreviousResult {
		t.Fatalf("first run decision = %+v, want miss/%s", d, ReasonNoPreviousResult)
	}

	cpNewProcess()
	if err := cpStageA(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt64(&cpCallsA); got != 1 {
		t.Fatalf("expected body to stay at 1 run after reuse, got %d", got)
	}
	d := cpLastDecision(t)
	if !d.Reused || d.Reason != ReasonReused || d.ID != "stage-a" {
		t.Fatalf("second run decision = %+v, want reused", d)
	}

	f := cpLoadFile(t)
	if f.Version != checkpointSchema || f.Checksum != f.checksum() {
		t.Fatal("persisted checkpoint file failed its own validation")
	}
	rec, ok := f.Nodes[nodeKey(F(cpStageA).Name(), "null")]
	if !ok {
		t.Fatal("expected a persisted record for stage-a")
	}
	if rec.Status != statusSucceeded || rec.InputHash != "a1" || rec.Project != "proj-1" {
		t.Fatalf("unexpected record: %+v", rec)
	}
}

func TestCheckpointInputChange(t *testing.T) {
	cpSetup(t, "proj-1")

	_ = cpStageA(context.Background())
	cpNewProcess()

	cpFpA = "a2"
	if err := cpStageA(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt64(&cpCallsA); got != 2 {
		t.Fatalf("expected body to rerun after input change, got %d", got)
	}
	if d := cpLastDecision(t); d.Reason != ReasonInputChanged {
		t.Fatalf("reason = %q, want %q", d.Reason, ReasonInputChanged)
	}

	// same fingerprint: reuse again
	cpNewProcess()
	_ = cpStageA(context.Background())
	if got := atomic.LoadInt64(&cpCallsA); got != 2 {
		t.Fatalf("expected body to stay at 2, got %d", got)
	}

	// reverting the input changes the fingerprint relative to the last
	// successful result, so the node runs again rather than trusting it.
	cpNewProcess()
	cpFpA = "a1"
	_ = cpStageA(context.Background())
	if got := atomic.LoadInt64(&cpCallsA); got != 3 {
		t.Fatalf("expected body to run again after reverting inputs, got %d", got)
	}
	if d := cpLastDecision(t); d.Reason != ReasonInputChanged {
		t.Fatalf("reason = %q, want %q", d.Reason, ReasonInputChanged)
	}
}

func TestCheckpointArgsAreIndependentNodes(t *testing.T) {
	cpSetup(t, "proj-1")

	_ = cpArgTarget(context.Background(), "one")
	_ = cpArgTarget(context.Background(), "two")
	if got := atomic.LoadInt64(&cpCallsArg); got != 2 {
		t.Fatalf("expected both argument sets to run, got %d", got)
	}
	if d := cpDecisionFor(t, cpArgTarget, "two"); d.Reason != ReasonArgsChanged {
		t.Fatalf("second argument set reason = %q, want %q", d.Reason, ReasonArgsChanged)
	}

	cpNewProcess()
	_ = cpArgTarget(context.Background(), "one")
	_ = cpArgTarget(context.Background(), "two")
	if got := atomic.LoadInt64(&cpCallsArg); got != 2 {
		t.Fatalf("expected neither argument set to rerun, got %d", got)
	}
	d := cpDecisionFor(t, cpArgTarget, "two")
	if !d.Reused {
		t.Fatalf("expected both argument sets to reuse, last decision %+v", d)
	}
	if len(cpArgSeen) != 2 {
		t.Fatalf("expected 2 executed argument values total, got %v", cpArgSeen)
	}
}

func TestCheckpointProjectChange(t *testing.T) {
	cpSetup(t, "proj-1")
	_ = cpStageA(context.Background())

	cpNewProcess()
	t.Setenv(ProjectIDEnv, "proj-2")
	_ = cpStageA(context.Background())
	if d := cpLastDecision(t); d.Reason != ReasonProjectChanged {
		t.Fatalf("reason = %q, want %q", d.Reason, ReasonProjectChanged)
	}
	if got := atomic.LoadInt64(&cpCallsA); got != 2 {
		t.Fatalf("expected rerun after project change, got %d", got)
	}

	// old project's results were discarded, so switching back also runs.
	cpNewProcess()
	t.Setenv(ProjectIDEnv, "proj-1")
	_ = cpStageA(context.Background())
	if got := atomic.LoadInt64(&cpCallsA); got != 3 {
		t.Fatalf("expected run under old project id, got %d", got)
	}
}

func TestCheckpointWorkDirMismatch(t *testing.T) {
	cpSetup(t, "proj-1")

	// plant a valid-looking record from a different working directory.
	name := F(cpStageA).Name()
	key := nodeKey(name, "null")
	checkpoints.mu.Lock()
	checkpoints.loadLocked()
	checkpoints.store.Nodes[key] = &nodeRecord{
		Project:   "proj-1",
		Target:    name,
		ID:        "stage-a",
		ArgsID:    "null",
		WorkDir:   filepath.Clean("/some/other/directory"),
		InputHash: "a1",
		Status:    statusSucceeded,
	}
	checkpoints.mu.Unlock()

	_ = cpStageA(context.Background())
	if d := cpLastDecision(t); d.Reason != ReasonWorkDirChanged {
		t.Fatalf("reason = %q, want %q", d.Reason, ReasonWorkDirChanged)
	}
	if got := atomic.LoadInt64(&cpCallsA); got != 1 {
		t.Fatalf("expected rerun after working dir mismatch, got %d", got)
	}
}

func TestCheckpointCorruptStateDiscarded(t *testing.T) {
	cpSetup(t, "proj-1")
	_ = cpStageA(context.Background())

	path, err := checkpointPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	cpNewProcess()
	_ = cpStageA(context.Background())
	if d := cpLastDecision(t); d.Reason != ReasonCorrupt {
		t.Fatalf("reason = %q, want %q", d.Reason, ReasonCorrupt)
	}
	if got := atomic.LoadInt64(&cpCallsA); got != 2 {
		t.Fatalf("expected rerun after corruption, got %d", got)
	}

	// state was rewritten and is usable again.
	cpNewProcess()
	_ = cpStageA(context.Background())
	if got := atomic.LoadInt64(&cpCallsA); got != 2 {
		t.Fatalf("expected reuse after corruption recovery, got %d", got)
	}
}

func TestCheckpointFailureNotPersisted(t *testing.T) {
	cpSetup(t, "proj-1")
	cpFlakyFails = true
	if err := cpFlaky(context.Background()); err == nil {
		t.Fatal("expected flaky target to fail")
	}
	if got := atomic.LoadInt64(&cpCallsFlaky); got != 1 {
		t.Fatalf("expected one failed run, got %d", got)
	}

	cpNewProcess()
	if err := cpFlaky(context.Background()); err == nil {
		t.Fatal("expected flaky target to fail again while still broken")
	}
	if got := atomic.LoadInt64(&cpCallsFlaky); got != 2 {
		t.Fatalf("failed run must not be persisted, got %d runs", got)
	}

	cpNewProcess()
	cpFlakyFails = false
	if err := cpFlaky(context.Background()); err != nil {
		t.Fatal(err)
	}
	cpNewProcess()
	_ = cpFlaky(context.Background())
	if got := atomic.LoadInt64(&cpCallsFlaky); got != 3 {
		t.Fatalf("expected successful result to be reused, got %d runs", got)
	}
}

func TestCheckpointDependencyFailurePartialSuccess(t *testing.T) {
	cpSetup(t, "proj-1")
	cpFlakyFails = true

	cpRecoverDeps(t, cpFlakyParent)
	if got := atomic.LoadInt64(&cpCallsA); got != 1 {
		t.Fatalf("expected stage-a to succeed once, got %d", got)
	}
	if got := atomic.LoadInt64(&cpCallsFlaky); got != 1 {
		t.Fatalf("expected flaky to run once and fail, got %d", got)
	}
	if got := atomic.LoadInt64(&cpCallsFlakyParent); got != 0 {
		t.Fatalf("parent must not finish when a dependency failed, got %d", got)
	}
	f := cpLoadFile(t)
	if _, ok := f.Nodes[nodeKey(F(cpFlaky).Name(), "null")]; ok {
		t.Fatal("failed dependency must not have a persisted record")
	}
	if _, ok := f.Nodes[nodeKey(F(cpFlakyParent).Name(), "null")]; ok {
		t.Fatal("parent of a failed dependency must not have a persisted record")
	}

	// second run: successful sibling reused, failed dependency and parent run.
	cpNewProcess()
	cpFlakyFails = false
	if err := cpFlakyParent(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt64(&cpCallsA); got != 1 {
		t.Fatalf("successful sibling should be reused, got %d runs", got)
	}
	if got := atomic.LoadInt64(&cpCallsFlaky); got != 2 {
		t.Fatalf("failed dependency should rerun, got %d runs", got)
	}
	if got := atomic.LoadInt64(&cpCallsFlakyParent); got != 1 {
		t.Fatalf("parent should run once, got %d", got)
	}
	if d := cpDecisionFor(t, cpStageA); !d.Reused {
		t.Fatalf("stage-a decision = %+v, want reused", d)
	}

	// third run: everything reused.
	cpNewProcess()
	if err := cpFlakyParent(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt64(&cpCallsA); got != 1 {
		t.Fatalf("stage-a should be reused on the third run, got %d", got)
	}
	if got := atomic.LoadInt64(&cpCallsFlaky); got != 2 {
		t.Fatalf("flaky should be reused on the third run, got %d", got)
	}
	if got := atomic.LoadInt64(&cpCallsFlakyParent); got != 1 {
		t.Fatalf("parent should be reused on the third run, got %d", got)
	}
	if d := cpDecisionFor(t, cpFlakyParent); !d.Reused {
		t.Fatalf("parent decision = %+v, want reused", d)
	}
}

func TestCheckpointDependencyInputChangeInvalidatesParent(t *testing.T) {
	cpSetup(t, "proj-1")
	if err := cpParent(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt64(&cpCallsParent); got != 1 {
		t.Fatalf("expected parent to run once, got %d", got)
	}

	cpNewProcess()
	cpFpB = "b2"
	if err := cpParent(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d := cpDecisionFor(t, cpParent); d.Reason != ReasonDependencyChanged {
		t.Fatalf("parent reason = %q, want %q", d.Reason, ReasonDependencyChanged)
	}
	if got := atomic.LoadInt64(&cpCallsA); got != 1 {
		t.Fatalf("unchanged dependency should be reused, got %d", got)
	}
	if got := atomic.LoadInt64(&cpCallsB); got != 2 {
		t.Fatalf("changed dependency should rerun, got %d", got)
	}
	if got := atomic.LoadInt64(&cpCallsParent); got != 2 {
		t.Fatalf("parent of a changed dependency should rerun, got %d", got)
	}

	cpNewProcess()
	_ = cpParent(context.Background())
	if got := atomic.LoadInt64(&cpCallsParent); got != 2 {
		t.Fatalf("parent should be reused once inputs settle, got %d", got)
	}
}

func TestCheckpointSerialDepsShareState(t *testing.T) {
	cpSetup(t, "proj-1")
	if err := cpSerialParent(context.Background()); err != nil {
		t.Fatal(err)
	}
	cpNewProcess()
	cpFpB = "b2"
	if err := cpSerialParent(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d := cpDecisionFor(t, cpSerialParent); d.Reason != ReasonDependencyChanged {
		t.Fatalf("serial parent reason = %q, want %q", d.Reason, ReasonDependencyChanged)
	}
	if got := atomic.LoadInt64(&cpCallsB); got != 2 {
		t.Fatalf("serial dependency should rerun, got %d", got)
	}
	if got := atomic.LoadInt64(&cpCallsSerialParent); got != 2 {
		t.Fatalf("serial parent should rerun, got %d", got)
	}
}

func TestCheckpointUndeclaredDependencyPreventsReuse(t *testing.T) {
	cpSetup(t, "proj-1")
	if err := cpMixedParent(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt64(&cpCallsUndeclared); got != 1 {
		t.Fatalf("undeclared dep should run once, got %d", got)
	}
	rec := cpLoadFile(t).Nodes[nodeKey(F(cpMixedParent).Name(), "null")]
	if rec == nil || len(rec.Deps) != 1 || rec.Deps[0].ID != "" {
		t.Fatalf("expected parent record to keep an uncheckpointed dep edge, got %+v", rec)
	}

	cpNewProcess()
	if err := cpMixedParent(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d := cpLastDecision(t); d.Reason != ReasonDependencyChanged {
		t.Fatalf("parent reason = %q, want %q", d.Reason, ReasonDependencyChanged)
	}
	if got := atomic.LoadInt64(&cpCallsUndeclared); got != 2 {
		t.Fatalf("undeclared dep must execute again, got %d", got)
	}
	if got := atomic.LoadInt64(&cpCallsMixedParent); got != 2 {
		t.Fatalf("parent should rerun, got %d", got)
	}
}

func TestCheckpointCancellationNotPersisted(t *testing.T) {
	cpSetup(t, "proj-1")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := cpCancelTarget(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if got := atomic.LoadInt64(&cpCallsCancel); got != 1 {
		t.Fatalf("expected one cancelled run, got %d", got)
	}

	cpNewProcess()
	if err := cpCancelTarget(context.Background()); err != nil {
		t.Fatalf("target succeeds with a live context: %v", err)
	}
	if got := atomic.LoadInt64(&cpCallsCancel); got != 2 {
		t.Fatalf("cancelled run must not be reused, got %d runs", got)
	}

	// a target that returns nil even though its context was cancelled is
	// not trusted either.
	cpNewProcess()
	called := int64(0)
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	err := RunCheckpoint(ctx2, cpStageB, func(context.Context) error {
		atomic.AddInt64(&called, 1)
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation error, got %v", err)
	}
	cpNewProcess()
	_ = RunCheckpoint(context.Background(), cpStageB, func(context.Context) error {
		atomic.AddInt64(&called, 1)
		return nil
	})
	if got := atomic.LoadInt64(&called); got != 2 {
		t.Fatalf("nil return after cancellation must not be persisted, got %d runs", got)
	}
}

func TestCheckpointInputsErrorRunsWithoutPersisting(t *testing.T) {
	cpSetup(t, "proj-1")
	cpFPError = errors.New("cannot hash")
	if err := cpFPErrorTarget(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d := cpLastDecision(t); d.Reason != ReasonInputsUnavailable {
		t.Fatalf("reason = %q, want %q", d.Reason, ReasonInputsUnavailable)
	}
	cpNewProcess()
	_ = cpFPErrorTarget(context.Background())
	if got := atomic.LoadInt64(&cpCallsFPError); got != 2 {
		t.Fatalf("target with unavailable fingerprint must run every time, got %d", got)
	}
}

func TestCheckpointDisabledRunsNormally(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(CheckpointEnv, "0")
	t.Setenv(CheckpointDirEnv, dir)
	t.Setenv(ProjectIDEnv, "proj-1")
	cpResetState()
	ResetCheckpoints()
	ResetDeps()
	t.Cleanup(func() {
		ResetCheckpoints()
		ResetDeps()
	})

	_ = cpStageA(context.Background())
	cpNewProcess()
	_ = cpStageA(context.Background())
	if got := atomic.LoadInt64(&cpCallsA); got != 2 {
		t.Fatalf("disabled checkpoints must not skip the body, got %d", got)
	}
	for _, d := range CheckpointDecisions() {
		if d.Reason != ReasonDisabled {
			t.Fatalf("unexpected decision when disabled: %+v", d)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("disabled checkpoints must not write state, got %v", entries)
	}
}

func TestCheckpointUndeclaredTargetUnaffected(t *testing.T) {
	cpSetup(t, "proj-1")

	Deps(cpUndeclared)
	cpNewProcess()
	Deps(cpUndeclared)
	if got := atomic.LoadInt64(&cpCallsUndeclared); got != 2 {
		t.Fatalf("undeclared target must run every process, got %d", got)
	}
	for _, d := range CheckpointDecisions() {
		if d.Target == F(cpUndeclared).Name() {
			t.Fatalf("undeclared target must not produce checkpoint decisions: %+v", d)
		}
	}
}

func TestCheckpointMultiRootAndDepShareNode(t *testing.T) {
	cpSetup(t, "proj-1")

	// root 1 runs stage-a directly; root 2 is a parent depending on it: the
	// shared dependency executes once in the process.
	if err := cpStageA(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := cpParent(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt64(&cpCallsA); got != 1 {
		t.Fatalf("shared dependency should run once across roots, got %d", got)
	}
	if d := cpDecisionFor(t, cpStageA); !d.Reused {
		t.Fatalf("shared dependency decision = %+v, want reused by the second root", d)
	}
}

func TestRunCheckpointUnregisteredPanics(t *testing.T) {
	t.Setenv(CheckpointEnv, "1")
	t.Setenv(CheckpointDirEnv, t.TempDir())
	ResetCheckpoints()
	ResetDeps()
	t.Cleanup(func() {
		ResetCheckpoints()
		ResetDeps()
	})

	defer func() {
		v := recover()
		if v == nil {
			t.Fatal("expected panic")
		}
		if want := "no checkpoint registered"; !strings.Contains(fmt.Sprint(v), want) {
			t.Fatalf("panic = %v, want substring %q", v, want)
		}
	}()
	_ = RunCheckpoint(context.Background(), cpUndeclared, func(context.Context) error { return nil })
}

func TestAddCheckpointValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target interface{}
		cp     Checkpoint
	}{
		{"nil target", nil, Checkpoint{ID: "x", Inputs: func(context.Context) (string, error) { return "", nil }}},
		{"empty id", cpStageB, Checkpoint{ID: "", Inputs: func(context.Context) (string, error) { return "", nil }}},
		{"nil inputs", cpStageB, Checkpoint{ID: "x", Inputs: nil}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected panic")
				}
			}()
			AddCheckpoint(tc.target, tc.cp)
		})
	}
}
