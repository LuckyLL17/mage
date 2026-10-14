package mage

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/magefile/mage/mg"
)

type integrationSink struct {
	mu     sync.Mutex
	events []mg.Event
}

func (s *integrationSink) Observe(event mg.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
}

func (s *integrationSink) snapshot() []mg.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]mg.Event, len(s.events))
	copy(out, s.events)
	return out
}

type eventRun struct {
	code   int
	stdout string
	stderr string
	events []mg.Event
}

func invokeWithEvents(t *testing.T, inv Invocation) eventRun {
	t.Helper()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	sink := &integrationSink{}
	inv.Stdout = stdout
	inv.Stderr = stderr
	inv.Stdin = nil
	inv.Events = sink
	code := Invoke(inv)
	return eventRun{code: code, stdout: stdout.String(), stderr: stderr.String(), events: sink.snapshot()}
}

func eventsByKind(events []mg.Event, kind mg.EventKind) []mg.Event {
	var out []mg.Event
	for _, event := range events {
		if event.Kind == kind {
			out = append(out, event)
		}
	}
	return out
}

func assertSingleExited(t *testing.T, events []mg.Event, wantCode int) {
	t.Helper()
	exited := eventsByKind(events, mg.EventExited)
	if len(exited) != 1 {
		t.Fatalf("expected exactly one exited event, got %d: %+v", len(exited), exited)
	}
	if exited[0].ExitCode != wantCode {
		t.Fatalf("exited code = %d, want %d", exited[0].ExitCode, wantCode)
	}
}

func assertCoherentStream(t *testing.T, events []mg.Event) string {
	t.Helper()
	if len(events) == 0 {
		t.Fatal("expected events")
	}
	runID := ""
	var previous uint64
	terminals := map[string]string{}
	for _, event := range events {
		if runID == "" {
			runID = event.RunID
		}
		if event.RunID != runID {
			t.Fatalf("event %s run id %q differs from %q", event.Kind, event.RunID, runID)
		}
		if event.Sequence <= previous {
			t.Fatalf("sequence not strictly increasing at %s: %d after %d", event.Kind, event.Sequence, previous)
		}
		previous = event.Sequence

		if event.ExecutionID != "" {
			switch event.Kind {
			case mg.EventTargetCompleted, mg.EventTargetFailed:
				if previous, ok := terminals[event.ExecutionID]; ok {
					t.Fatalf("execution %s terminates twice: %s and %s", event.ExecutionID, previous, event.Kind)
				}
				terminals[event.ExecutionID] = string(event.Kind)
			}
		}
	}
	if runID == "" {
		t.Fatal("no run id in events")
	}
	return runID
}

func TestInvocationEventsDiamond(t *testing.T) {
	run := invokeWithEvents(t, Invocation{Dir: "testdata/events", Args: []string{"All"}})
	if run.code != 0 {
		t.Fatalf("exit code %d, stderr: %s", run.code, run.stderr)
	}

	events := run.events
	assertCoherentStream(t, events)

	selected := eventsByKind(events, mg.EventTargetSelected)
	if len(selected) != 1 || selected[0].Target.Display != "all" {
		t.Fatalf("expected one selected root target all: %+v", selected)
	}

	expanded := eventsByKind(events, mg.EventDepsExpanded)
	if len(expanded) != 3 {
		t.Fatalf("expected 3 dependency expansions, got %d: %+v", len(expanded), expanded)
	}
	if !expanded[0].Parallel || len(expanded[0].Dependencies) != 2 {
		t.Fatalf("root must expand left and right in parallel: %+v", expanded[0])
	}

	executions := map[string]bool{}
	started := map[string]int{}
	completed := map[string]int{}
	failed := map[string]int{}
	var skipped []mg.Event
	for _, event := range events {
		if event.Target.Name == "" {
			continue
		}
		switch event.Kind {
		case mg.EventTargetStarted:
			started[event.Target.Name]++
			executions[event.ExecutionID] = true
		case mg.EventTargetCompleted:
			completed[event.Target.Name]++
		case mg.EventTargetFailed:
			failed[event.Target.Name]++
		case mg.EventTargetSkipped:
			skipped = append(skipped, event)
		}
	}
	for _, target := range []string{"main.All", "main.Left", "main.Right", "main.Common"} {
		if started[target] != 1 {
			t.Fatalf("%s started %d times", target, started[target])
		}
		if completed[target] != 1 {
			t.Fatalf("%s completed %d times", target, completed[target])
		}
		if failed[target] != 0 {
			t.Fatalf("%s failed unexpectedly", target)
		}
	}
	if len(skipped) != 1 {
		t.Fatalf("common must be skipped exactly once for the second parent, got %+v", skipped)
	}
	if skipped[0].Target.Name != "main.Common" ||
		skipped[0].ReusesExecution != skipped[0].ExecutionID {
		t.Fatalf("skip must reference the shared common execution: %+v", skipped[0])
	}
	commonExec := skipped[0].ExecutionID
	commonStarted := 0
	for _, event := range eventsByKind(events, mg.EventTargetStarted) {
		if event.ExecutionID == commonExec {
			commonStarted++
		}
	}
	if commonStarted != 1 {
		t.Fatalf("common body must start exactly once, got %d", commonStarted)
	}

	parentExecutions := map[string]bool{}
	for _, target := range []string{"main.Left", "main.Right"} {
		for _, event := range eventsByKind(events, mg.EventTargetStarted) {
			if event.Target.Name == target {
				parentExecutions[event.ExecutionID] = true
			}
		}
	}
	commonRunner := ""
	for _, event := range eventsByKind(events, mg.EventTargetStarted) {
		if event.ExecutionID == commonExec {
			commonRunner = event.ParentExecution
		}
	}
	if !parentExecutions[commonRunner] {
		t.Fatalf("common must run for one of its parents, got %q", commonRunner)
	}
	if !parentExecutions[skipped[0].ParentExecution] || skipped[0].ParentExecution == commonRunner {
		t.Fatalf("common must be skipped for the other parent: runner %q skip %+v", commonRunner, skipped[0])
	}

	if !strings.Contains(run.stdout, "all") || !strings.Contains(run.stdout, "left") ||
		!strings.Contains(run.stdout, "right") || !strings.Contains(run.stdout, "common") {
		t.Fatalf("default target output changed: %q", run.stdout)
	}
	assertSingleExited(t, events, 0)
}

func TestInvocationEventsFailureExitCode(t *testing.T) {
	run := invokeWithEvents(t, Invocation{Dir: "testdata/events", Args: []string{"Fails"}})
	if run.code != 3 {
		t.Fatalf("exit code = %d, want 3, stderr: %s", run.code, run.stderr)
	}
	failed := eventsByKind(run.events, mg.EventTargetFailed)
	if len(failed) != 1 || failed[0].Target.Display != "fails" || failed[0].ExitCode != 3 {
		t.Fatalf("expected one failed event with code 3, got %+v", failed)
	}
	assertCoherentStream(t, run.events)
	assertSingleExited(t, run.events, 3)
}

func TestInvocationEventsPanicExitCode(t *testing.T) {
	run := invokeWithEvents(t, Invocation{Dir: "testdata/events", Args: []string{"Panics"}})
	if run.code != 1 {
		t.Fatalf("exit code = %d, want 1", run.code)
	}
	failed := eventsByKind(run.events, mg.EventTargetFailed)
	if len(failed) != 1 || failed[0].ExitCode != 1 || !strings.Contains(failed[0].Error, "kaboom") {
		t.Fatalf("expected failed event for the panic, got %+v", failed)
	}
	assertSingleExited(t, run.events, 1)
}

func TestInvocationEventsUnknownTarget(t *testing.T) {
	run := invokeWithEvents(t, Invocation{Dir: "testdata/events", Args: []string{"does-not-exist"}})
	if run.code != 2 {
		t.Fatalf("exit code = %d, want 2", run.code)
	}
	selected := eventsByKind(run.events, mg.EventTargetSelected)
	if len(selected) != 1 || selected[0].Reason != "unknown_target" {
		t.Fatalf("expected unknown target selection event, got %+v", selected)
	}
	assertSingleExited(t, run.events, 2)
}

func TestInvocationEventsTimeout(t *testing.T) {
	run := invokeWithEvents(t, Invocation{
		Dir:     "testdata/context",
		Args:    []string{"Timeout"},
		Timeout: 50 * time.Millisecond,
	})
	if run.code != 1 {
		t.Fatalf("exit code = %d, want 1, stderr: %s", run.code, run.stderr)
	}
	timedOut := eventsByKind(run.events, mg.EventTimedOut)
	if len(timedOut) != 1 {
		t.Fatalf("expected one timed_out event, got %+v", timedOut)
	}
	assertSingleExited(t, run.events, 1)
}

func TestInvocationEventsCachedBinarySkipsCompileEvents(t *testing.T) {
	first := invokeWithEvents(t, Invocation{
		Dir:      "testdata/events",
		Args:     []string{"All"},
		HashFast: true,
	})
	if first.code != 0 {
		t.Fatalf("first run failed: %s", first.stderr)
	}

	second := invokeWithEvents(t, Invocation{
		Dir:      "testdata/events",
		Args:     []string{"All"},
		HashFast: true,
	})
	if second.code != 0 {
		t.Fatalf("cached run failed: %s", second.stderr)
	}
	if len(eventsByKind(second.events, mg.EventCompileStarted)) != 0 ||
		len(eventsByKind(second.events, mg.EventCompileFinished)) != 0 {
		t.Fatalf("cached binary run must not produce compile events: %+v", second.events)
	}
	if len(eventsByKind(second.events, mg.EventTargetSelected)) != 1 {
		t.Fatalf("cached run must still report target selection: %+v", second.events)
	}
	if len(eventsByKind(second.events, mg.EventTargetCompleted)) == 0 {
		t.Fatal("cached run must still report target completion")
	}
	assertSingleExited(t, second.events, 0)
}

type panickingSink struct{}

func (panickingSink) Observe(mg.Event) { panic("broken sink") }

func TestInvocationEventsPanickingSinkIsIgnored(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	inv := Invocation{
		Dir:      "testdata/events",
		Args:     []string{"All"},
		HashFast: true,
		Stdout:   stdout,
		Stderr:   stderr,
		Events:   panickingSink{},
	}
	if code := Invoke(inv); code != 0 {
		t.Fatalf("a panicking sink must not change the exit code, got %d: %s", code, stderr)
	}
	if !strings.Contains(stdout.String(), "common") {
		t.Fatalf("a panicking sink must not change target output: %q", stdout)
	}
}

func TestInvocationEventsSerialTarget(t *testing.T) {
	run := invokeWithEvents(t, Invocation{
		Dir:      "testdata/events",
		Args:     []string{"Serial"},
		HashFast: true,
	})
	if run.code != 0 {
		t.Fatalf("serial run failed: %s", run.stderr)
	}
	serial := 0
	parallel := 0
	for _, event := range eventsByKind(run.events, mg.EventDepsExpanded) {
		// Left and Right each declare Common through Deps, so the nested
		// Common expansions are parallel; the two root expansions produced by
		// SerialDeps are serial.
		if event.Parallel {
			parallel++
		} else {
			serial++
			if len(event.Dependencies) != 1 {
				t.Fatalf("serial expansion must list one dependency: %+v", event)
			}
		}
	}
	if serial != 2 || parallel != 2 {
		t.Fatalf("expected 2 serial and 2 nested parallel expansions, got %d serial %d parallel", serial, parallel)
	}
	assertSingleExited(t, run.events, 0)
}
