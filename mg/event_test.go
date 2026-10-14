package mg

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

type recordedSink struct {
	mu     sync.Mutex
	events []Event
}

func (r *recordedSink) Observe(event Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *recordedSink) snapshot() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Event, len(r.events))
	copy(out, r.events)
	return out
}

func eventsOfKind(events []Event, kind EventKind) []Event {
	var out []Event
	for _, event := range events {
		if event.Kind == kind {
			out = append(out, event)
		}
	}
	return out
}

func executionForName(events []Event, name string) string {
	for _, event := range events {
		if event.Target.Name == name && event.ExecutionID != "" {
			return event.ExecutionID
		}
	}
	return ""
}

// assertLifecycle verifies that each execution has at most one started event
// and exactly one terminal event, in deterministic order.
func assertLifecycle(t *testing.T, events []Event) {
	t.Helper()
	type lifecyclePhase struct {
		started  int
		terminal string
	}
	executions := map[string]*lifecyclePhase{}
	for _, event := range events {
		if event.ExecutionID == "" {
			continue
		}
		current := executions[event.ExecutionID]
		if current == nil {
			current = &lifecyclePhase{}
			executions[event.ExecutionID] = current
		}
		switch event.Kind {
		case EventTargetStarted:
			current.started++
		case EventTargetCompleted, EventTargetFailed:
			if current.started == 0 {
				t.Fatalf("%s terminal event without a started event: %+v", event.ExecutionID, event)
			}
			if current.terminal != "" {
				t.Fatalf("%s has two terminal events", event.ExecutionID)
			}
			current.terminal = string(event.Kind)
		}
	}
	for id, current := range executions {
		if current.started > 1 {
			t.Fatalf("%s started %d times", id, current.started)
		}
	}
}

func TestEventDepsDiamondSharedDepRunsOnce(t *testing.T) {
	ResetDeps()
	sink := &recordedSink{}
	ctx := WithEventSink(context.Background(), sink)

	common := func() {}
	left := func() { Deps(common) }
	right := func() { Deps(common) }

	recovered := func() (value interface{}) {
		defer func() { value = recover() }()
		ObserveTarget(ctx, TargetID{Name: "main.Root", Display: "root"}, func(context.Context) error {
			Deps(left, right)
			return nil
		})
		return nil
	}()
	if recovered != nil {
		t.Fatalf("unexpected panic: %v", recovered)
	}

	events := sink.snapshot()
	if len(events) == 0 {
		t.Fatal("expected events")
	}
	runID := events[0].RunID
	if runID == "" {
		t.Fatal("expected run id")
	}
	var previous uint64
	for _, event := range events {
		if event.RunID != runID {
			t.Fatalf("event %s has unexpected run id %q", event.Kind, event.RunID)
		}
		if event.Sequence <= previous {
			t.Fatalf("event sequence not strictly increasing: %d after %d", event.Sequence, previous)
		}
		previous = event.Sequence
	}

	root := executionForName(events, "main.Root")
	leftExec := executionForName(events, funcName(left))
	rightExec := executionForName(events, funcName(right))
	commonExec := executionForName(events, funcName(common))
	if root == "" || leftExec == "" || rightExec == "" || commonExec == "" {
		t.Fatalf("missing execution identities: root=%q left=%q right=%q common=%q",
			root, leftExec, rightExec, commonExec)
	}
	if root != "root-1" {
		t.Fatalf("root execution id = %q, want root-1", root)
	}

	for _, kind := range []EventKind{EventTargetSelected, EventTargetStarted, EventTargetCompleted} {
		var matches []Event
		for _, event := range eventsOfKind(events, kind) {
			if event.ExecutionID == root {
				matches = append(matches, event)
			}
		}
		if len(matches) != 1 {
			t.Fatalf("expected exactly one %s for root, got %+v", kind, matches)
		}
	}

	expanded := eventsOfKind(events, EventDepsExpanded)
	if len(expanded) != 3 {
		t.Fatalf("expected 3 deps_expanded events (root, left, right), got %d", len(expanded))
	}
	if !expanded[0].Parallel || expanded[0].ExecutionID != root {
		t.Fatalf("root dependency expansion must be parallel: %+v", expanded[0])
	}
	for _, event := range expanded[1:] {
		// Deps runs in parallel even when only one dependency is listed;
		// SerialDeps is what produces serial expansions.
		if !event.Parallel {
			t.Fatalf("Deps expansion must be reported as parallel: %+v", event)
		}
		if len(event.Dependencies) != 1 || event.Dependencies[0].Name != funcName(common) {
			t.Fatalf("expected common dependency expansion, got %+v", event)
		}
	}

	started := eventsOfKind(events, EventTargetStarted)
	completed := eventsOfKind(events, EventTargetCompleted)
	skipped := eventsOfKind(events, EventTargetSkipped)
	if len(started) != 4 {
		t.Fatalf("expected 4 started targets (root + 3 deps), got %d: %+v", len(started), started)
	}
	if len(completed) != 4 {
		t.Fatalf("expected 4 completed targets (root + 3 deps), got %d", len(completed))
	}
	if len(skipped) != 1 {
		t.Fatalf("expected exactly 1 skipped target for the shared dep, got %d: %+v", len(skipped), skipped)
	}
	skip := skipped[0]
	if skip.Target.Name != funcName(common) || skip.ExecutionID != commonExec ||
		skip.ReusesExecution != commonExec {
		t.Fatalf("skip event must reference the shared common execution: %+v", skip)
	}
	parents := map[string]bool{}
	for _, event := range started {
		if event.Target.Name == funcName(common) {
			parents[event.ParentExecution] = true
		}
	}
	parents[skip.ParentExecution] = true
	if !parents[leftExec] || !parents[rightExec] {
		t.Fatalf("shared dep must be associated with both parents, got %v", parents)
	}
	for _, event := range started {
		switch event.Target.Name {
		case funcName(left), funcName(right):
			if event.ParentExecution != root {
				t.Fatalf("%s parent = %q, want %q", event.Target.Name, event.ParentExecution, root)
			}
		}
	}

	if failed := eventsOfKind(events, EventTargetFailed); len(failed) != 0 {
		t.Fatalf("unexpected failed events: %+v", failed)
	}
	assertLifecycle(t, events)
}

func TestEventSerialDepsExpandedSerially(t *testing.T) {
	ResetDeps()
	sink := &recordedSink{}
	ctx := WithEventSink(context.Background(), sink)

	first := func() {}
	second := func() {}

	ObserveTarget(ctx, TargetID{Name: "main.SerialRoot", Display: "serial"}, func(context.Context) error {
		SerialDeps(first, second)
		return nil
	})

	var expanded []Event
	for _, event := range sink.snapshot() {
		if event.Kind == EventDepsExpanded {
			expanded = append(expanded, event)
		}
	}
	if len(expanded) != 2 {
		t.Fatalf("expected 2 serial deps_expanded events, got %d", len(expanded))
	}
	for _, event := range expanded {
		if event.Parallel {
			t.Fatalf("serial dependency expansion must not be parallel: %+v", event)
		}
		if len(event.Dependencies) != 1 {
			t.Fatalf("expected one dependency per serial expansion, got %d", len(event.Dependencies))
		}
	}
}

func TestEventFailureKeepsExitCode(t *testing.T) {
	ResetDeps()
	sink := &recordedSink{}
	ctx := WithEventSink(context.Background(), sink)

	var recovered interface{}
	func() {
		defer func() { recovered = recover() }()
		ObserveTarget(ctx, TargetID{Name: "main.FailRoot", Display: "fail"}, func(context.Context) error {
			Deps(func() error { return Fatal(7, "ouch") })
			return nil
		})
	}()
	if recovered == nil {
		t.Fatal("expected the aggregated fatal error to panic")
	}
	if code := ExitStatus(asError(t, recovered)); code != 7 {
		t.Fatalf("exit code = %d, want 7", code)
	}

	failed := eventsOfKind(sink.snapshot(), EventTargetFailed)
	if len(failed) != 2 {
		t.Fatalf("expected failed events for dependency and root, got %+v", failed)
	}
	depFailure, rootFailure := failed[0], failed[1]
	if depFailure.ExitCode != 7 || depFailure.Error != "ouch" {
		t.Fatalf("unexpected dependency failure: %+v", depFailure)
	}
	if depFailure.ParentExecution != rootFailure.ExecutionID {
		t.Fatalf("dependency failure must reference its parent: %+v / %+v", depFailure, rootFailure)
	}
	if rootFailure.ExitCode != 7 {
		t.Fatalf("root failure exit code = %d, want 7", rootFailure.ExitCode)
	}
}

func TestEventPanicNonError(t *testing.T) {
	ResetDeps()
	sink := &recordedSink{}
	ctx := WithEventSink(context.Background(), sink)

	var recovered interface{}
	func() {
		defer func() { recovered = recover() }()
		ObserveTarget(ctx, TargetID{Name: "main.PanicRoot", Display: "panic"}, func(context.Context) error {
			Deps(func() { panic("boom") })
			return nil
		})
	}()
	if recovered == nil {
		t.Fatal("expected panic")
	}
	failed := eventsOfKind(sink.snapshot(), EventTargetFailed)
	if len(failed) != 2 {
		t.Fatalf("expected failed events for dependency and root, got %+v", failed)
	}
	for _, event := range failed {
		if event.ExitCode != 1 {
			t.Fatalf("non-error panic exit code = %d, want 1: %+v", event.ExitCode, event)
		}
	}
}

func TestEventFailedSharedDepSkippedOnce(t *testing.T) {
	ResetDeps()
	sink := &recordedSink{}
	ctx := WithEventSink(context.Background(), sink)

	common := func() { panic("shared failure") }

	var recovered interface{}
	func() {
		defer func() { recovered = recover() }()
		ObserveTarget(ctx, TargetID{Name: "main.SharedFailRoot", Display: "sharedfail"}, func(context.Context) error {
			Deps(common, common)
			return nil
		})
	}()
	if recovered == nil {
		t.Fatal("expected panic")
	}

	events := sink.snapshot()
	commonExec := ""
	for _, event := range events {
		if event.Target.Name == funcName(common) {
			commonExec = event.ExecutionID
			break
		}
	}
	failed := eventsOfKind(events, EventTargetFailed)
	skipped := eventsOfKind(events, EventTargetSkipped)
	failedCommon := 0
	for _, event := range failed {
		if event.ExecutionID == commonExec {
			failedCommon++
		}
	}
	if failedCommon != 1 {
		t.Fatalf("shared failing dep must fail exactly once, got %d", failedCommon)
	}
	if len(skipped) != 1 || skipped[0].ReusesExecution != commonExec {
		t.Fatalf("second requester must skip and reuse the failed execution: %+v", skipped)
	}
}

func TestEventSinkPanicNeverBreaksRun(t *testing.T) {
	ResetDeps()
	sink := mgPanicSink{}
	ctx := WithEventSink(context.Background(), sink)

	ran := false
	ObserveTarget(ctx, TargetID{Name: "main.RobustRoot", Display: "robust"}, func(context.Context) error {
		Deps(func() { ran = true })
		return nil
	})
	if !ran {
		t.Fatal("dependency body must run even if the sink panics")
	}
}

type mgPanicSink struct{}

func (mgPanicSink) Observe(Event) { panic("sink broken") }

func TestEventCtxDepsCanceled(t *testing.T) {
	ResetDeps()
	sink := &recordedSink{}
	base, cancel := context.WithCancel(context.Background())
	ctx := WithEventSink(base, sink)
	cancel()

	var recovered interface{}
	func() {
		defer func() { recovered = recover() }()
		CtxDeps(ctx, func(ctx context.Context) error { return ctx.Err() })
	}()
	if recovered == nil {
		t.Fatal("expected canceled dependency to fail the run")
	}
	failed := eventsOfKind(sink.snapshot(), EventTargetFailed)
	if len(failed) != 1 {
		t.Fatalf("expected one failed event, got %+v", failed)
	}
	if failed[0].Error != "context canceled" {
		t.Fatalf("unexpected failure: %+v", failed[0])
	}
}

func TestEventInterruptKinds(t *testing.T) {
	sink := &recordedSink{}
	ctx := WithEventSink(context.Background(), sink)

	EmitInterrupt(ctx, context.DeadlineExceeded)
	EmitInterrupt(ctx, context.Canceled)

	events := sink.snapshot()
	if len(events) != 2 || events[0].Kind != EventTimedOut || events[1].Kind != EventCanceled {
		t.Fatalf("unexpected interrupt events: %+v", events)
	}
}

func TestEventsDisabledWithoutSink(t *testing.T) {
	ResetDeps()
	ran := 0
	ObserveTarget(context.Background(), TargetID{Name: "main.NoEventsRoot"}, func(context.Context) error {
		Deps(func() { ran++ })
		return nil
	})
	if ran != 1 {
		t.Fatalf("dependency must run once without events, ran %d", ran)
	}
}

func asError(t *testing.T, value interface{}) error {
	t.Helper()
	err, ok := value.(error)
	if !ok {
		t.Fatalf("expected error, got %T: %v", value, value)
	}
	return err
}

// TestEventBootstrapFromEnv exercises the protocol between the stdlib-only
// generated main and mg: a _root control line in the event log associates
// context-free Deps calls on this goroutine with the root execution.
func TestEventBootstrapFromEnv(t *testing.T) {
	ResetDeps()
	resetEventBootstrap()
	t.Cleanup(func() { resetEventBootstrap() })

	path := filepath.Join(t.TempDir(), "events.jsonl")
	t.Setenv(EventLogEnv, path)
	t.Setenv(EventRunIDEnv, "run-from-env")

	rootTarget := TargetID{Name: "main.Root", Display: "root"}
	registration := rootRegistration{
		Kind:        rootControlKind,
		ExecutionID: "root-7",
		Goroutine:   goroutineID(),
		Target:      rootTarget,
	}
	line, err := json.Marshal(registration)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	Deps(func() {})

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	var sawExpanded bool
	for _, raw := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
		var event Event
		if err := json.Unmarshal(raw, &event); err != nil {
			t.Fatalf("bad event line %q: %v", raw, err)
		}
		if event.Kind == rootControlKind {
			continue
		}
		if event.RunID != "run-from-env" {
			t.Fatalf("run id = %q", event.RunID)
		}
		kinds = append(kinds, string(event.Kind))
		if event.Kind == EventDepsExpanded {
			sawExpanded = true
			if event.ExecutionID != "root-7" || event.Target.Name != "main.Root" {
				t.Fatalf("deps expansion not associated with control-line root: %+v", event)
			}
		}
	}
	if !sawExpanded {
		t.Fatalf("expected deps_expanded event in %v", kinds)
	}
}

func TestDependencyExecutionIDStable(t *testing.T) {
	f := F(func() {})
	id1 := dependencyExecutionID(f)
	id2 := dependencyExecutionID(f)
	if id1 == "" || id1 != id2 {
		t.Fatalf("dependency execution id must be stable: %q / %q", id1, id2)
	}
	other := F(func() {})
	if id1 == dependencyExecutionID(other) {
		t.Fatal("different targets must have different execution ids")
	}
	withArgs := F(func(string) {}, "value")
	if id1 == dependencyExecutionID(withArgs) {
		t.Fatal("targets with different argument ids must differ")
	}
}
