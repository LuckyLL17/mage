package mg

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// EventLogEnv is the environment variable that points at the file to which a
// compiled magefile writes one structured Event per line as JSON. The mage
// driver sets it when an EventSink is configured; external orchestrators may
// also set it when running a compiled magefile directly.
const EventLogEnv = "MAGEFILE_EVENT_LOG"

// EventRunIDEnv is the environment variable that carries the identifier shared
// by every Event produced for a single mage invocation, including events
// emitted by the mage driver and by the compiled magefile subprocess.
const EventRunIDEnv = "MAGEFILE_RUN_ID"

// rootControlKind marks the internal control line that the stdlib-only
// generated main appends before running a root target body. It carries the
// goroutine running the root and its execution id, so mg can associate
// context-free Deps calls with their root target without importing mg in the
// generated main. Control lines are filtered out by the mage driver and never
// reach EventSinks.
const rootControlKind = "_root"

// EventKind identifies the kind of a structured execution Event.
type EventKind string

// Structured execution event kinds. They cover the whole lifecycle of a mage
// invocation: compilation, target selection, dependency expansion, target
// start/completion/skip/failure, interruption and the final process exit.
const (
	// EventCompileStarted marks the start of the magefile compilation. It is
	// only emitted by the mage driver, and only when the magefile is actually
	// compiled (a cached binary produces no compilation events).
	EventCompileStarted EventKind = "compile_started"

	// EventCompileFinished marks the end of the magefile compilation. The
	// event's Error field is empty on success and describes the failure
	// otherwise.
	EventCompileFinished EventKind = "compile_finished"

	// EventTargetSelected marks that a command line target has been resolved
	// and is about to run. For a target name that cannot be resolved the
	// event's Error field is set.
	EventTargetSelected EventKind = "target_selected"

	// EventDepsExpanded marks that a target requested the listed dependencies
	// via Deps, CtxDeps, SerialDeps or SerialCtxDeps. The event's Parallel
	// field reports whether the dependencies are run in parallel.
	EventDepsExpanded EventKind = "deps_expanded"

	// EventTargetStarted marks that a target body started executing.
	EventTargetStarted EventKind = "target_started"

	// EventTargetCompleted marks that a target body finished successfully.
	EventTargetCompleted EventKind = "target_completed"

	// EventTargetSkipped marks that a request to run a target did not execute
	// its body, because the shared dependency had already been (or was being)
	// executed exactly once. ReusesExecution points at the execution that
	// performed the work and whose result was reused.
	EventTargetSkipped EventKind = "target_skipped"

	// EventTargetFailed marks that a target body returned an error or
	// panicked. The Error and ExitCode fields describe the failure.
	EventTargetFailed EventKind = "target_failed"

	// EventCanceled marks that the run was interrupted by a canceled context
	// or an interrupt signal.
	EventCanceled EventKind = "canceled"

	// EventTimedOut marks that the run exceeded its configured timeout.
	EventTimedOut EventKind = "timed_out"

	// EventExited is the terminal event for an invocation. Its ExitCode field
	// is the process exit code. Exactly one exited event is produced for every
	// invocation with an EventSink.
	EventExited EventKind = "exited"
)

// TargetID identifies a target function. The zero value represents the run
// itself rather than a specific target.
type TargetID struct {
	// Name is the fully qualified name of the target function, for example
	// "main.Build" or "example.com/module.Build".
	Name string `json:"name"`

	// ID is an additional qualifier for targets that share a name, such as
	// the JSON encoding of the arguments passed to mg.F.
	ID string `json:"id,omitempty"`

	// Display is the human readable name as it is shown in mage output.
	Display string `json:"display,omitempty"`
}

// Event is a single structured execution event. Every event carries the
// invariant RunID of its invocation, a process-local monotonic Sequence, an
// ExecutionID for the target execution it belongs to and the
// ParentExecution of the target that requested this execution. Root targets
// selected from the command line have an empty ParentExecution.
//
// Dependencies requested through Deps may run in parallel, so events of
// different executions may interleave; the per-target ordering of events
// (selected, started and exactly one of completed, failed or skipped) and the
// final result of every target are deterministic.
type Event struct {
	// Time is when the event occurred.
	Time time.Time `json:"time"`

	// Sequence is a per-process monotonic event sequence number.
	Sequence uint64 `json:"seq"`

	// Kind is the kind of the event.
	Kind EventKind `json:"kind"`

	// RunID identifies the mage invocation; it is identical for every event
	// produced by that invocation.
	RunID string `json:"run_id"`

	// ExecutionID uniquely identifies a target execution within the run.
	ExecutionID string `json:"execution_id,omitempty"`

	// ParentExecution is the ExecutionID of the target that requested this
	// execution as a dependency. It is empty for command line root targets and
	// for events that do not belong to a target.
	ParentExecution string `json:"parent_execution_id,omitempty"`

	// Target identifies the target the event belongs to.
	Target TargetID `json:"target"`

	// ReusesExecution is set on skipped events to the ExecutionID that
	// executed the shared dependency's body exactly once.
	ReusesExecution string `json:"reuses_execution_id,omitempty"`

	// Dependencies lists the requested dependency targets for a
	// deps_expanded event.
	Dependencies []TargetID `json:"dependencies,omitempty"`

	// Parallel reports whether an expanded dependency set runs in parallel.
	Parallel bool `json:"parallel,omitempty"`

	// Error describes a failure for compile_finished, target_selected,
	// target_failed, canceled or timed_out events.
	Error string `json:"error,omitempty"`

	// ExitCode is the exit code for target_failed and exited events.
	ExitCode int `json:"exit_code,omitempty"`

	// Reason optionally provides additional machine readable context.
	Reason string `json:"reason,omitempty"`
}

// EventSink consumes structured execution events. Implementations must be safe
// for concurrent use, must not block for long and must not panic: a panicking
// sink is recovered and detached from producing further failures, so event
// consumption can never change mage's output, exit codes or dependency
// semantics.
type EventSink interface {
	Observe(event Event)
}

// EventSinkFunc adapts a function to the EventSink interface.
type EventSinkFunc func(Event)

// Observe implements EventSink.
func (f EventSinkFunc) Observe(event Event) {
	f(event)
}

// eventState is the single per-run event coordinator for a process.
type eventState struct {
	runID string
	sink  EventSink

	mu     sync.Mutex
	seq    uint64
	roots  uint64
	closed bool
}

func newEventState(runID string, sink EventSink) *eventState {
	return &eventState{runID: runID, sink: sink}
}

// emit assigns sequence, time and run id and delivers the event to the sink in
// sequence order. Delivery failures and panics never propagate to target
// execution.
func (s *eventState) emit(event Event) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.seq++
	if event.Time.IsZero() {
		event.Time = time.Now()
	}
	event.Sequence = s.seq
	event.RunID = s.runID
	safeObserve(s.sink, event)
}

func (s *eventState) nextRootExecution() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.roots++
	return "root-" + strconv.FormatUint(s.roots, 10)
}

func (s *eventState) close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

// safeObserve keeps a panicking or otherwise failing sink from affecting the
// build.
func safeObserve(sink EventSink, event Event) {
	if sink == nil {
		return
	}
	defer func() {
		_ = recover()
	}()
	sink.Observe(event)
}

// eventFrame associates an event state with the execution that is currently
// running on a goroutine.
type eventFrame struct {
	state     *eventState
	execution string
	target    TargetID
}

type eventFrameCtxKey struct{}

func withFrame(ctx context.Context, frame *eventFrame) context.Context {
	return context.WithValue(ctx, eventFrameCtxKey{}, frame)
}

func frameFromContext(ctx context.Context) *eventFrame {
	if ctx == nil {
		return nil
	}
	frame, _ := ctx.Value(eventFrameCtxKey{}).(*eventFrame)
	return frame
}

// defaultEventState is the state configured by StartEventLog in a compiled
// magefile process.
var defaultEventState atomic.Value // *eventState

// ambientFrames keeps the active execution frame per goroutine, so that Deps
// and SerialDeps calls that do not accept a context can still be associated
// with their parent target.
var (
	ambientFrames sync.Map // map[int64]*ambientFrame
	ambientDepth  int64
)

type ambientFrame struct {
	goid     int64
	frame    eventFrame
	previous *ambientFrame
}

func pushAmbient(frame eventFrame) func() {
	gid := goroutineID()
	entry := &ambientFrame{goid: gid, frame: frame}
	if previous, ok := ambientFrames.Load(gid); ok {
		entry.previous = previous.(*ambientFrame)
	}
	ambientFrames.Store(gid, entry)
	atomic.AddInt64(&ambientDepth, 1)
	return func() {
		atomic.AddInt64(&ambientDepth, -1)
		if entry.previous != nil {
			ambientFrames.Store(gid, entry.previous)
		} else {
			ambientFrames.Delete(gid)
		}
	}
}

// rootRegistration mirrors the _root control line written by the stdlib-only
// generated magefile main.
type rootRegistration struct {
	Kind        string   `json:"kind"`
	ExecutionID string   `json:"execution_id"`
	Goroutine   int64    `json:"goroutine"`
	Target      TargetID `json:"target"`
}

var (
	bootstrapOnce    sync.Once
	rootControlRead  *os.File
	rootControlWrite *os.File
	rootFrames       sync.Map // map[int64]eventFrame
	rootFramesReadMu sync.Mutex
)

// ensureEventRuntime lazily configures process-wide event reporting from the
// environment when mg runs inside a compiled magefile. It is a no-op when
// events are disabled or already configured, for example through
// WithEventSink or StartEventLog.
func ensureEventRuntime() *eventState {
	if state, _ := defaultEventState.Load().(*eventState); state != nil {
		return state
	}
	bootstrapOnce.Do(func() {
		path := os.Getenv(EventLogEnv)
		if path == "" {
			return
		}
		runID := os.Getenv(EventRunIDEnv)
		if runID == "" {
			runID = NewRunID()
		}
		writeFile, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return
		}
		readFile, err := os.Open(path)
		if err != nil {
			_ = writeFile.Close()
			return
		}
		defaultEventState.Store(newEventState(runID, &fileEventSink{file: writeFile}))
		rootControlRead = readFile
		rootControlWrite = writeFile
	})
	state, _ := defaultEventState.Load().(*eventState)
	return state
}

// resetEventBootstrap clears lazy bootstrap state. It exists for tests that
// exercise the compiled magefile protocol in-process.
func resetEventBootstrap() {
	bootstrapOnce = sync.Once{}
	if rootControlRead != nil {
		_ = rootControlRead.Close()
		rootControlRead = nil
	}
	if rootControlWrite != nil {
		_ = rootControlWrite.Close()
		rootControlWrite = nil
	}
	rootFrames = sync.Map{}
	defaultEventState = atomic.Value{}
}

// refreshRootFrames reads the _root control lines appended so far and
// remembers the root execution frame for each reported goroutine.
func refreshRootFrames() {
	if rootControlRead == nil {
		return
	}
	state, _ := defaultEventState.Load().(*eventState)
	if state == nil {
		return
	}
	rootFramesReadMu.Lock()
	defer rootFramesReadMu.Unlock()
	if _, err := rootControlRead.Seek(0, io.SeekStart); err != nil {
		return
	}
	data, err := io.ReadAll(rootControlRead)
	if err != nil {
		return
	}
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var registration rootRegistration
		if err := json.Unmarshal(line, &registration); err != nil || registration.Kind != rootControlKind {
			continue
		}
		rootFrames.Store(registration.Goroutine, eventFrame{
			state:     state,
			execution: registration.ExecutionID,
			target:    registration.Target,
		})
	}
}

// resolveFrame finds the active event frame through the context, then through
// the goroutine's ambient dependency frame, then through the root control
// registration of the current goroutine, and finally through the process
// default state. It returns nil when structured events are not enabled.
func resolveFrame(ctx context.Context) *eventFrame {
	if frame := frameFromContext(ctx); frame != nil && frame.state != nil {
		return frame
	}
	if atomic.LoadInt64(&ambientDepth) > 0 {
		if entry, ok := ambientFrames.Load(goroutineID()); ok {
			frame := entry.(*ambientFrame).frame
			return &frame
		}
	}
	state := ensureEventRuntime()
	if state == nil {
		return nil
	}
	if rootControlRead != nil {
		if entry, ok := rootFrames.Load(goroutineID()); ok {
			frame := entry.(eventFrame)
			return &frame
		}
		refreshRootFrames()
		if entry, ok := rootFrames.Load(goroutineID()); ok {
			frame := entry.(eventFrame)
			return &frame
		}
	}
	return &eventFrame{state: state}
}

// ambientContext returns a context carrying the calling goroutine's ambient
// execution frame, so Deps and SerialDeps preserve parent association even
// though they do not accept a context.
func ambientContext() context.Context {
	if atomic.LoadInt64(&ambientDepth) == 0 {
		if _, ok := defaultEventState.Load().(*eventState); !ok && ensureEventRuntime() == nil {
			return context.Background()
		}
	}
	if frame := resolveFrame(context.Background()); frame != nil {
		return withFrame(context.Background(), frame)
	}
	return context.Background()
}

// goroutineID returns the calling goroutine's id. It is only used to associate
// context-free Deps calls with the target that runs on the same goroutine.
func goroutineID() int64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	line := string(buf[:n])
	const prefix = "goroutine "
	if len(line) <= len(prefix) {
		return 0
	}
	line = line[len(prefix):]
	end := 0
	for end < len(line) && line[end] >= '0' && line[end] <= '9' {
		end++
	}
	id, _ := strconv.ParseInt(line[:end], 10, 64)
	return id
}

// fileEventSink writes events as JSON lines to an append-only file. A write
// failure permanently disables the sink instead of failing the build.
type fileEventSink struct {
	file     *os.File
	disabled int32
}

func newFileEventSink(path string) (*fileEventSink, func(), error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, func() {}, err
	}
	sink := &fileEventSink{file: file}
	close := func() {
		_ = sink.file.Close()
	}
	return sink, close, nil
}

// Observe implements EventSink.
func (s *fileEventSink) Observe(event Event) {
	if atomic.LoadInt32(&s.disabled) != 0 {
		return
	}
	line, err := json.Marshal(event)
	if err != nil {
		return
	}
	line = append(line, '\n')
	if _, err := s.file.Write(line); err != nil {
		atomic.StoreInt32(&s.disabled, 1)
	}
}

// WithEventSink returns a context whose dependency execution reports
// structured events to sink. It is the public entry point for embedded
// callers that drive mg.Deps, mg.CtxDeps, mg.SerialDeps or ObserveTarget in
// their own process; the mage CLI and generated magefile main configure the
// same event model automatically.
func WithEventSink(ctx context.Context, sink EventSink) context.Context {
	if sink == nil {
		return ctx
	}
	state := newEventState(NewRunID(), sink)
	return withFrame(ctx, &eventFrame{state: state})
}

// StartEventLog configures structured event reporting for a compiled magefile
// process based on the MAGEFILE_EVENT_LOG and MAGEFILE_RUN_ID environment
// variables. When event logging is not enabled it returns ctx unchanged and a
// no-op closer. Failures while opening the event log never fail the run:
// event reporting is simply disabled.
func StartEventLog(ctx context.Context) (context.Context, func()) {
	path := os.Getenv(EventLogEnv)
	if path == "" {
		return ctx, func() {}
	}
	runID := os.Getenv(EventRunIDEnv)
	if runID == "" {
		runID = NewRunID()
	}
	sink, closeFile, err := newFileEventSink(path)
	if err != nil {
		return ctx, func() {}
	}
	state := newEventState(runID, sink)
	defaultEventState.Store(state)
	return withFrame(ctx, &eventFrame{state: state}), func() {
		state.close()
		closeFile()
	}
}

// NewRunID returns a new opaque run identifier.
func NewRunID() string {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err == nil {
		return hex.EncodeToString(bytes[:])
	}
	return strconv.FormatInt(time.Now().UnixNano(), 16) + "-" + strconv.Itoa(os.Getpid())
}

// ObserveTarget runs fn as a command line selected root target and emits the
// target's lifecycle events. It returns fn's error unchanged so existing
// error handling and exit codes are preserved. The context passed to fn
// carries the target's execution identity, which nested dependency calls
// associate themselves with. A panicking target emits a target_failed event
// and re-panics so that existing panic handling and exit codes are preserved.
func ObserveTarget(ctx context.Context, target TargetID, fn func(context.Context) error) error {
	frame := resolveFrame(ctx)
	if frame == nil {
		return fn(ctx)
	}
	state := frame.state
	execution := state.nextRootExecution()
	execCtx := withFrame(ctx, &eventFrame{state: state, execution: execution, target: target})
	popAmbient := pushAmbient(eventFrame{state: state, execution: execution, target: target})
	defer popAmbient()

	state.emit(Event{Kind: EventTargetSelected, ExecutionID: execution, Target: target})
	state.emit(Event{Kind: EventTargetStarted, ExecutionID: execution, Target: target})

	var err error
	func() {
		defer func() {
			if value := recover(); value != nil {
				state.emit(failureEvent(execution, "", target, value))
				panic(value)
			}
		}()
		err = fn(execCtx)
	}()
	if err != nil {
		state.emit(failureEvent(execution, "", target, err))
		return err
	}
	state.emit(Event{Kind: EventTargetCompleted, ExecutionID: execution, Target: target})
	return nil
}

// EmitSelectionError reports that a requested command line target name could
// not be resolved to a target.
func EmitSelectionError(ctx context.Context, name string, cause error) {
	frame := resolveFrame(ctx)
	if frame == nil {
		return
	}
	event := Event{Kind: EventTargetSelected, Target: TargetID{Name: name, Display: name}, Reason: "unknown_target"}
	if cause != nil {
		event.Error = cause.Error()
	}
	frame.state.emit(event)
}

// EmitInterrupt reports that the run was interrupted. A deadline exceeded
// cause produces a timed_out event; every other cause produces a canceled
// event.
func EmitInterrupt(ctx context.Context, cause error) {
	frame := resolveFrame(ctx)
	if frame == nil {
		return
	}
	kind := EventCanceled
	if errors.Is(cause, context.DeadlineExceeded) {
		kind = EventTimedOut
	}
	event := Event{Kind: kind, ExecutionID: frame.execution, Target: frame.target}
	if cause != nil {
		event.Error = cause.Error()
	}
	frame.state.emit(event)
}

// EmitExit emits the terminal exited event for the process. It is used by the
// generated magefile main file and is a no-op when event reporting is off.
func EmitExit(code int, cause error) {
	state, _ := defaultEventState.Load().(*eventState)
	if state == nil {
		return
	}
	event := Event{Kind: EventExited, ExitCode: code}
	if cause != nil {
		event.Error = fmt.Sprint(cause)
	}
	state.emit(event)
}

// TargetRef returns the TargetID that identifies f.
func TargetRef(f Fn) TargetID {
	return TargetID{Name: f.Name(), ID: f.ID(), Display: displayName(f.Name())}
}

// dependencyExecutionID returns a stable execution identifier for a unique
// dependency target, so a shared dependency keeps one execution identity no
// matter which parent requests it.
func dependencyExecutionID(f Fn) string {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(f.Name()))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(f.ID()))
	return "exec-" + strconv.FormatUint(hash.Sum64(), 16)
}

// failureEvent builds a target_failed event from a returned error or a
// recovered panic value.
func failureEvent(execution, parent string, target TargetID, value interface{}) Event {
	event := Event{
		Kind:            EventTargetFailed,
		ExecutionID:     execution,
		ParentExecution: parent,
		Target:          target,
		Error:           fmt.Sprint(value),
		ExitCode:        1,
	}
	if err, ok := value.(error); ok {
		event.ExitCode = ExitStatus(err)
	}
	return event
}
