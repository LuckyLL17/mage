package mg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Checkpoint declares the resume policy of a target. Register one with
// AddCheckpoint and run the target body through RunCheckpoint to opt into
// cross-run checkpoints; checkpointing itself is explicitly enabled per run
// with the MAGEFILE_CHECKPOINT environment variable.
type Checkpoint struct {
	// ID is the stable identity of the target. It must not change when the
	// target's implementation is edited, as long as a successful run still
	// produces the same logical result. Renaming an ID forces a rerun.
	ID string

	// Inputs returns the input fingerprint of the target. It runs before
	// the target body and, on reuse attempts, for each of the target's
	// checkpointed dependencies. A changed fingerprint forces that node and
	// every node depending on it to rerun. It must be deterministic for the
	// same project, working directory and inputs. Invocation arguments are
	// already part of the node identity, so the fingerprint does not receive
	// them.
	Inputs func(context.Context) (string, error)
}

// AddCheckpoint declares a checkpoint for target. target is the target
// function value (or a method on a mg.Namespace type), exactly as passed to
// mg.Deps. A target may only reuse a previous result while all of its direct
// dependencies have registered checkpoints too; otherwise the target always
// runs so undeclared dependencies are executed.
//
// AddCheckpoint is normally called from init so declarations exist before any
// target runs. Registering the same target again replaces its declaration.
func AddCheckpoint(target interface{}, cp Checkpoint) {
	if target == nil {
		panic("mg.AddCheckpoint: target must not be nil")
	}
	if cp.ID == "" {
		panic("mg.AddCheckpoint: checkpoint ID must not be empty")
	}
	if cp.Inputs == nil {
		panic("mg.AddCheckpoint: checkpoint Inputs function must not be nil")
	}
	checkpoints.declare(funcName(target), cp)
}

// Decision records whether one RunCheckpoint node reused a previously
// persisted successful result, or why it had to run again.
type Decision struct {
	// Target is the fully qualified runtime name of the target.
	Target string
	// ID is the stable checkpoint ID declared by the target.
	ID string
	// ArgsID is the JSON encoding of the target arguments, as produced by
	// mg.F.
	ArgsID string
	// Reused reports whether a previous successful result was reused.
	Reused bool
	// Reason explains the verdict. It is one of the Reason* constants.
	Reason string
}

// Reasons reported by CheckpointDecisions.
const (
	ReasonReused            = "reused previous successful result"
	ReasonDisabled          = "checkpoints disabled"
	ReasonCorrupt           = "checkpoint state is corrupt and was discarded"
	ReasonProjectChanged    = "project or import closure changed"
	ReasonWorkDirChanged    = "working directory changed"
	ReasonArgsChanged       = "invocation arguments changed"
	ReasonInputChanged      = "input fingerprint changed"
	ReasonDependencyChanged = "a checkpointed dependency changed, failed, or is not checkpointed"
	ReasonNoPreviousResult  = "no previous successful result"
	ReasonInputsUnavailable = "input fingerprint could not be computed"
)

const (
	checkpointSchema     = 1
	statusSucceeded      = "succeeded"
	checkpointDirName    = "checkpoints"
	checkpointFileSuffix = ".json"
	nodeKeySep           = "\x00"
)

// RunCheckpoint executes body as the resumable implementation of target.
// target identifies the node exactly as mg.Deps identifies it (via mg.F);
// target must have a Checkpoint registered with AddCheckpoint.
//
// When checkpointing is enabled, a successful body is persisted together
// with its checkpoint ID, arguments, working directory, input fingerprint
// and all of its direct dependencies. A later run skips the body only when
// the project identity (MAGEFILE_PROJECT_ID), target ID, arguments, working
// directory and input fingerprint all match and every direct dependency is
// checkpointed and still valid (its own fingerprint is recomputed and
// compared). Input changes, project or import closure changes, corrupt
// state, cancellation, dependency failures and partial successes of
// parallel dependencies cause the affected node and every node depending on
// it to run again; unrelated successful nodes remain reusable.
//
// When checkpointing is disabled (the default), body runs exactly as if
// RunCheckpoint were not used.
func RunCheckpoint(ctx context.Context, target interface{}, body func(context.Context) error, args ...interface{}) error {
	if body == nil {
		panic("mg.RunCheckpoint: checkpoint body must not be nil")
	}
	fn := F(target, args...)
	name, argsID, key := fn.Name(), fn.ID(), nodeKey(fn.Name(), fn.ID())

	cp, registered := checkpoints.lookupDecl(name)

	if !CheckpointEnabled() {
		if registered {
			checkpoints.decide(name, cp.ID, argsID, false, ReasonDisabled)
		}
		return body(withNodeKey(ctx, key))
	}

	if !registered {
		// Explicit opt-in surface: running through RunCheckpoint without a
		// declaration is a programming error rather than a silent miss.
		panic("mg.RunCheckpoint: no checkpoint registered for " + displayName(name) +
			", call mg.AddCheckpoint for this target")
	}

	wdir, err := workingDir()
	if err != nil {
		checkpoints.decide(name, cp.ID, argsID, false, ReasonInputsUnavailable)
		return body(withNodeKey(ctx, key))
	}
	inputHash, err := cp.Inputs(ctx)
	if err != nil {
		if Verbose() {
			logger.Println("Checkpoint input fingerprint failed for", displayName(name), ":", err)
		}
		checkpoints.decide(name, cp.ID, argsID, false, ReasonInputsUnavailable)
		return body(withNodeKey(ctx, key))
	}

	reason := checkpoints.evaluate(ctx, key, name, cp.ID, argsID, wdir, inputHash)
	switch reason {
	case ReasonProjectChanged:
		// the whole scope is stale: drop every previous result.
		checkpoints.resetNodes()
	case ReasonWorkDirChanged, ReasonInputChanged, ReasonDependencyChanged:
		checkpoints.invalidate(key)
	case ReasonReused, ReasonCorrupt, ReasonArgsChanged, ReasonNoPreviousResult:
		// corrupt state was already discarded; different argument sets and
		// first runs never had a record under this key.
	}

	if reason == ReasonReused {
		checkpoints.decide(name, cp.ID, argsID, true, ReasonReused)
		if Verbose() {
			logger.Printf("Checkpoint hit: skipping %s (%s)", displayName(name), cp.ID)
		}
		return nil
	}

	checkpoints.decide(name, cp.ID, argsID, false, reason)
	if Verbose() {
		logger.Printf("Checkpoint miss: running %s (%s): %s", displayName(name), cp.ID, reason)
	}

	runErr := body(withNodeKey(ctx, key))
	if runErr != nil {
		// the node failed: its result is invalid, and so is every result
		// that depended on it.
		checkpoints.invalidate(key)
		return runErr
	}
	if ctx.Err() != nil {
		// a nil return after cancellation does not prove the work finished:
		// never checkpoint it.
		checkpoints.invalidate(key)
		return ctx.Err()
	}

	rec := &nodeRecord{
		Project:     ProjectID(),
		Target:      name,
		ID:          cp.ID,
		ArgsID:      argsID,
		WorkDir:     wdir,
		InputHash:   inputHash,
		Deps:        checkpoints.edgesFor(key),
		CompletedAt: time.Now().UTC(),
		Status:      statusSucceeded,
	}
	if err := checkpoints.commit(rec); err != nil && Verbose() {
		logger.Println("warning: could not persist checkpoint for", displayName(name), ":", err)
	}
	return nil
}

// CheckpointDecisions returns the reuse/rerun decisions collected in this
// process, in the order the corresponding RunCheckpoint nodes were entered.
// It is intended for inspection and tests.
func CheckpointDecisions() []Decision {
	return checkpoints.snapshotDecisions()
}

// ResetCheckpoints clears the in-memory checkpoint state, decisions and
// cached store. Registered checkpoint declarations and persisted results on
// disk are kept, so the call simulates starting a new mage process in the
// same project. Do not call it while targets are running.
func ResetCheckpoints() {
	checkpoints.reset()
}

type nodeRecord struct {
	Project     string    `json:"project"`
	Target      string    `json:"target"`
	ID          string    `json:"id"`
	ArgsID      string    `json:"args_id"`
	WorkDir     string    `json:"workdir"`
	InputHash   string    `json:"input_hash"`
	Deps        []nodeDep `json:"deps"`
	CompletedAt time.Time `json:"completed_at"`
	Status      string    `json:"status"`
}

// nodeDep identifies one direct dependency of a persisted node. All direct
// dependencies are recorded, including ones without checkpoints: such
// dependencies prevent reuse until they become checkpointed, because a
// reused body never executes them.
type nodeDep struct {
	Key       string `json:"key"`
	Target    string `json:"target"`
	ID        string `json:"id"`
	InputHash string `json:"input_hash"`
}

type checkpointFile struct {
	Version  int                    `json:"version"`
	Project  string                 `json:"project"`
	Nodes    map[string]*nodeRecord `json:"nodes"`
	Checksum string                 `json:"checksum"`
}

type checkpointManager struct {
	mu        sync.Mutex
	decls     map[string]Checkpoint
	loaded    bool
	path      string
	corrupt   bool
	store     *checkpointFile
	decisions []Decision
	edges     map[string]map[string]struct{}
}

var checkpoints = &checkpointManager{
	decls: map[string]Checkpoint{},
	store: freshCheckpointFile(""),
	edges: map[string]map[string]struct{}{},
}

func freshCheckpointFile(project string) *checkpointFile {
	return &checkpointFile{
		Version: checkpointSchema,
		Project: project,
		Nodes:   map[string]*nodeRecord{},
	}
}

func (m *checkpointManager) declare(name string, cp Checkpoint) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.decls[name] = cp
}

func (m *checkpointManager) lookupDecl(name string) (Checkpoint, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp, ok := m.decls[name]
	return cp, ok
}

func (m *checkpointManager) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loaded = false
	m.path = ""
	m.corrupt = false
	m.store = freshCheckpointFile("")
	m.decisions = nil
	m.edges = map[string]map[string]struct{}{}
}

func (m *checkpointManager) decide(target, id, argsID string, reused bool, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.decisions = append(m.decisions, Decision{
		Target: target,
		ID:     id,
		ArgsID: argsID,
		Reused: reused,
		Reason: reason,
	})
}

func (m *checkpointManager) snapshotDecisions() []Decision {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Decision, len(m.decisions))
	copy(out, m.decisions)
	return out
}

// addEdge records that parent directly depended on child during the current
// run. It is called from the dependency scheduler.
func (m *checkpointManager) addEdge(parent, child string) {
	if parent == "" || child == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	children := m.edges[parent]
	if children == nil {
		children = map[string]struct{}{}
		m.edges[parent] = children
	}
	children[child] = struct{}{}
}

// edgesFor returns the persisted identities of every direct dependency a
// node just ran with. Checkpointed dependencies carry their ID and last
// input hash; undeclared dependencies carry only their identity and prevent
// reuse until they are themselves checkpointed.
func (m *checkpointManager) edgesFor(parent string) []nodeDep {
	m.mu.Lock()
	defer m.mu.Unlock()
	f := m.loadLocked()
	children := m.edges[parent]
	deps := make([]nodeDep, 0, len(children))
	for key := range children {
		target, _ := splitNodeKey(key)
		dep := nodeDep{Key: key, Target: target}
		if r, ok := f.Nodes[key]; ok && r.Status == statusSucceeded {
			dep.ID = r.ID
			dep.InputHash = r.InputHash
		}
		deps = append(deps, dep)
	}
	sort.Slice(deps, func(i, j int) bool { return deps[i].Key < deps[j].Key })
	return deps
}

// evaluate returns ReasonReused when a valid result exists for key,
// otherwise the reason why the node has to run. Dependency fingerprints are
// recomputed, so a parent is never reused while a child's inputs changed.
func (m *checkpointManager) evaluate(ctx context.Context, key, target, id, argsID, wdir, inputHash string) string {
	m.mu.Lock()
	m.loadLocked()
	if m.corrupt {
		m.mu.Unlock()
		return ReasonCorrupt
	}
	project := ProjectID()
	r, ok := m.store.Nodes[key]
	if ok {
		if !validRecord(key, r) || r.Project != project {
			m.mu.Unlock()
			return ReasonProjectChanged
		}
		if r.WorkDir != wdir {
			m.mu.Unlock()
			return ReasonWorkDirChanged
		}
		if r.InputHash != inputHash {
			m.mu.Unlock()
			return ReasonInputChanged
		}
	} else {
		for _, existing := range m.store.Nodes {
			if existing.Target == target && existing.ID == id && existing.ArgsID != argsID && existing.Project == project {
				m.mu.Unlock()
				return ReasonArgsChanged
			}
		}
		m.mu.Unlock()
		return ReasonNoPreviousResult
	}

	// Snapshot the dependency records under the lock: parallel siblings may
	// concurrently commit their own results. The Inputs functions run after
	// releasing the lock.
	deps := make([]nodeDep, len(r.Deps))
	copy(deps, r.Deps)
	snapshot := make(map[string]*nodeRecord, len(deps))
	for _, dep := range deps {
		if cur, ok := m.store.Nodes[dep.Key]; ok {
			copy := *cur
			snapshot[dep.Key] = &copy
		}
	}
	m.mu.Unlock()

	for _, dep := range deps {
		cur, ok := snapshot[dep.Key]
		if !ok || !validRecord(dep.Key, cur) || cur.Project != project || cur.ID != dep.ID {
			return ReasonDependencyChanged
		}
		cp, registered := m.lookupDecl(dep.Target)
		if !registered {
			return ReasonDependencyChanged
		}
		hash, err := cp.Inputs(ctx)
		if err != nil || hash != cur.InputHash {
			return ReasonDependencyChanged
		}
	}
	return ReasonReused
}

func validRecord(key string, r *nodeRecord) bool {
	return r != nil && r.Status == statusSucceeded && r.ID != "" && r.Target != "" &&
		nodeKey(r.Target, r.ArgsID) == key
}

// invalidate deletes the node and every persisted node that transitively
// depends on it.
func (m *checkpointManager) invalidate(key string) {
	m.mu.Lock()
	changed := m.invalidateLocked(key)
	if changed {
		_ = m.writeLocked()
	}
	m.mu.Unlock()
}

// resetNodes drops all persisted nodes (used when the project identity
// changed).
func (m *checkpointManager) resetNodes() {
	m.mu.Lock()
	f := m.loadLocked()
	f.Nodes = map[string]*nodeRecord{}
	f.Project = ProjectID()
	_ = m.writeLocked()
	m.mu.Unlock()
}

// invalidateLocked removes key and its dependents, reporting whether
// anything was removed. The caller must hold m.mu.
func (m *checkpointManager) invalidateLocked(key string) bool {
	f := m.loadLocked()
	removed := map[string]bool{}
	queue := []string{key}
	for len(queue) > 0 {
		k := queue[0]
		queue = queue[1:]
		if removed[k] {
			continue
		}
		if _, ok := f.Nodes[k]; !ok {
			continue
		}
		removed[k] = true
		for other, r := range f.Nodes {
			if removed[other] {
				continue
			}
			for _, dep := range r.Deps {
				if dep.Key == k {
					queue = append(queue, other)
					break
				}
			}
		}
	}
	for k := range removed {
		delete(f.Nodes, k)
	}
	return len(removed) > 0
}

func (m *checkpointManager) commit(rec *nodeRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	f := m.loadLocked()
	f.Project = rec.Project
	f.Nodes[nodeKey(rec.Target, rec.ArgsID)] = rec
	return m.writeLocked()
}

func (m *checkpointManager) loadLocked() *checkpointFile {
	if m.loaded {
		return m.store
	}
	m.loaded = true
	m.store = freshCheckpointFile(ProjectID())

	path, err := checkpointPath()
	if err != nil {
		return m.store
	}
	m.path = path

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return m.store
	}
	if err != nil {
		m.markCorrupt(path, err)
		return m.store
	}
	loaded := &checkpointFile{}
	if err := json.Unmarshal(data, loaded); err != nil {
		m.markCorrupt(path, err)
		return m.store
	}
	if loaded.Version != checkpointSchema || !loaded.valid() || loaded.Checksum != loaded.checksum() {
		m.markCorrupt(path, fmt.Errorf("checkpoint file failed validation"))
		return m.store
	}
	m.store = loaded
	return m.store
}

func (m *checkpointManager) markCorrupt(path string, err error) {
	m.corrupt = true
	m.store = freshCheckpointFile(ProjectID())
	if Verbose() {
		logger.Printf("warning: checkpoint state at %s is unusable (%v); all checkpointed targets will run again", path, err)
	}
}

func (m *checkpointManager) writeLocked() error {
	if m.path == "" {
		path, err := checkpointPath()
		if err != nil {
			return err
		}
		m.path = path
	}
	f := m.loadLocked()
	f.Version = checkpointSchema
	f.Checksum = f.checksum()
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(m.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".checkpoint-*"+checkpointFileSuffix)
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, m.path)
}

func (f *checkpointFile) valid() bool {
	if f.Nodes == nil {
		return false
	}
	for key, r := range f.Nodes {
		if !validRecord(key, r) {
			return false
		}
	}
	return true
}

func (f *checkpointFile) checksum() string {
	copy := *f
	copy.Checksum = ""
	b, err := json.Marshal(&copy)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func nodeKey(name, argsID string) string {
	return name + nodeKeySep + argsID
}

func splitNodeKey(key string) (name, argsID string) {
	name, argsID, _ = strings.Cut(key, nodeKeySep)
	return name, argsID
}

type checkpointNodeKey struct{}

func withNodeKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, checkpointNodeKey{}, key)
}

func nodeKeyFromContext(ctx context.Context) string {
	key, _ := ctx.Value(checkpointNodeKey{}).(string)
	return key
}

func workingDir() (string, error) {
	wdir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return filepath.Clean(wdir), nil
}

func checkpointPath() (string, error) {
	wdir, err := workingDir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(wdir))
	dir := os.Getenv(CheckpointDirEnv)
	if dir == "" {
		dir = filepath.Join(CacheDir(), checkpointDirName)
	}
	return filepath.Join(dir, hex.EncodeToString(sum[:])+checkpointFileSuffix), nil
}
