package mage

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/magefile/mage/mg"
)

// eventPollInterval is how often the forwarder checks the event log file for
// new lines while the compiled magefile is running.
const eventPollInterval = 10 * time.Millisecond

// eventDrainTimeout bounds the final read of the event log after the
// compiled magefile exits.
const eventDrainTimeout = time.Second

// eventForwarder consumes the structured event log written by the compiled
// magefile subprocess, merges those events with events produced by the mage
// driver (compilation and the guaranteed terminal exited event) and delivers
// everything to one mg.EventSink.
type eventForwarder struct {
	sink  mg.EventSink
	runID string
	path  string
	file  *os.File

	mu      sync.Mutex
	seq     uint64
	sawExit bool

	stop chan struct{}
	done chan struct{}
}

// newEventForwarder creates the append-only event log file and starts tailing
// it. Event forwarding failures never fail an invocation: a nil forwarder and
// nil error simply means structured events are not available.
func newEventForwarder(sink mg.EventSink, cacheDir string) *eventForwarder {
	dir := cacheDir
	if dir == "" {
		dir = os.TempDir()
	}
	file, err := os.CreateTemp(dir, "mage-events-*.jsonl")
	if err != nil {
		file, err = os.CreateTemp(os.TempDir(), "mage-events-*.jsonl")
		if err != nil {
			return nil
		}
	}
	forwarder := &eventForwarder{
		sink:  sink,
		runID: mg.NewRunID(),
		path:  file.Name(),
		file:  file,
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	go forwarder.tail()
	return forwarder
}

// tail reads JSON lines as they are appended by the compiled magefile.
func (f *eventForwarder) tail() {
	defer close(f.done)
	reader := bufio.NewReader(f.file)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			f.consume(line)
		}
		if err == nil {
			continue
		}
		select {
		case <-f.stop:
			f.drain(reader)
			return
		default:
			time.Sleep(eventPollInterval)
		}
	}
}

// drain reads any events still in the log after the magefile exited.
func (f *eventForwarder) drain(reader *bufio.Reader) {
	deadline := time.Now().Add(eventDrainTimeout)
	for time.Now().Before(deadline) {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			f.consume(line)
		}
		if err != nil {
			return
		}
	}
}

// consume decodes a single JSON line and delivers it to the sink.
func (f *eventForwarder) consume(line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return
	}
	var event mg.Event
	if err := json.Unmarshal(line, &event); err != nil {
		return
	}
	// Internal control lines (for example _root) coordinate mg and the
	// generated main, but never reach EventSinks.
	if strings.HasPrefix(string(event.Kind), "_") {
		return
	}
	f.deliver(event)
}

// deliver stamps the merged stream sequence and run id and safely dispatches
// the event. A panicking sink can never affect the running build.
func (f *eventForwarder) deliver(event mg.Event) {
	f.mu.Lock()
	f.seq++
	event.Sequence = f.seq
	if event.RunID == "" {
		event.RunID = f.runID
	}
	if event.Kind == mg.EventExited {
		f.sawExit = true
	}
	f.mu.Unlock()

	func() {
		defer func() {
			_ = recover()
		}()
		f.sink.Observe(event)
	}()
}

func (f *eventForwarder) compileStarted() {
	f.deliver(mg.Event{Kind: mg.EventCompileStarted})
}

func (f *eventForwarder) compileFinished(err error) {
	event := mg.Event{Kind: mg.EventCompileFinished}
	if err != nil {
		event.Error = err.Error()
	}
	f.deliver(event)
}

// finish stops the tailer, drains remaining events and guarantees exactly one
// terminal exited event, synthesizing one when the magefile did not emit its
// own (for example when only listing targets or when the process was killed).
func (f *eventForwarder) finish(code int) {
	close(f.stop)
	<-f.done

	f.mu.Lock()
	missingExit := !f.sawExit
	f.mu.Unlock()
	if missingExit {
		f.deliver(mg.Event{Kind: mg.EventExited, ExitCode: code})
	}

	_ = f.file.Close()
	_ = os.Remove(f.path)
}
