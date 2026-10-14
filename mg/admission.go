package mg

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// AdmissionPolicyEnv is the environment variable that points to the optional
// target admission policy file. When it is unset, target admission is
// disabled and mage behaves exactly as it does without a policy.
const AdmissionPolicyEnv = "MAGEFILE_ADMISSION_POLICY"

// AdmissionRootEnv carries the resolved identity of the root target that is
// running. The generated main file publishes the admitted root before its
// body runs, which lets the dependency scheduler explain a rejected
// transitive node from the requested target even though the generated file
// does not import this package.
const AdmissionRootEnv = "MAGEFILE_ADMISSION_ROOT"

// AdmissionTargetsEnv carries the parsed target registry (a JSON object keyed
// by fully qualified runtime function name). The mage CLI populates it when
// it compiles a magefile; when it is absent (for example a cached binary),
// runtime function names are used as a fallback identity.
const AdmissionTargetsEnv = "MAGEFILE_ADMISSION_TARGETS"

// AdmissionDeniedExitCode is the exit status used when a target invocation is
// rejected by the admission policy. It mirrors EX_NOPERM from sysexits(3) so
// that CI can distinguish authorization failures from ordinary target errors
// (exit 1) and usage errors such as unknown targets (exit 2).
const AdmissionDeniedExitCode = 77

// TargetRequest describes a single target invocation that is subject to the
// admission policy. Every entry point into mage (the generated main file, the
// mage CLI, embedded mage.Invoke calls and the mg dependency scheduler)
// converts its invocation into a TargetRequest and then uses the exact same
// Policy decision.
type TargetRequest struct {
	// Target is the fully resolved, lower-cased mage target name as used on
	// the command line, e.g. "build" or "deploy:ships". It is empty for
	// transitive dependencies that have no registered CLI target name.
	Target string

	// Func is the fully qualified runtime function name, e.g. "main.Build"
	// or "github.com/acme/deploy.Ships".
	Func string

	// Namespace is the namespace prefix of Target, e.g. "deploy" or
	// "tools:sub". It is empty for top-level targets.
	Namespace string

	// Alias is the alias token used on the command line when the target was
	// requested through an alias. It is empty for direct and transitive
	// invocations.
	Alias string

	// Args holds the invocation arguments without the target token itself.
	// Positional values and -flag=value tokens are included verbatim;
	// arguments given to mg.F are their JSON-decoded values.
	Args []string

	// Dir is the absolute working directory the target runs in.
	Dir string
}

// Rule selectively allows or denies target invocations. A rule matches a
// request only when every selector that is set on the rule also matches the
// request; a rule with no selectors matches every request.
type Rule struct {
	// Action is "allow" or "deny".
	Action string `json:"action"`

	// Target matches the resolved target name exactly and
	// case-insensitively, e.g. "build" or "deploy:ships".
	Target string `json:"target,omitempty"`

	// Func matches the fully qualified runtime function name. It supports
	// shell-style wildcards using path.Match semantics, e.g. "main.Deploy.*"
	// or "github.com/acme/unsafe/*".
	Func string `json:"func,omitempty"`

	// Namespace matches the namespace prefix of the target exactly and
	// case-insensitively. A rule for "deploy" also matches targets nested
	// below it, such as "deploy:ships".
	Namespace string `json:"namespace,omitempty"`

	// Alias matches the alias through which the target was requested,
	// case-insensitively.
	Alias string `json:"alias,omitempty"`

	// Args matches when the invocation arguments are equal to this list,
	// element by element and in order.
	Args []string `json:"args,omitempty"`

	// ArgsPrefix matches when the invocation arguments start with this
	// list.
	ArgsPrefix []string `json:"argsPrefix,omitempty"`

	// Dir matches the working directory with shell-style wildcards
	// (path.Match semantics) against the cleaned absolute path, e.g.
	// "/ci/work/*".
	Dir string `json:"dir,omitempty"`

	// Reason is an optional human-readable explanation that is surfaced in
	// the structured denial.
	Reason string `json:"reason,omitempty"`
}

// Policy is an ordered target admission policy. The first matching rule
// wins; when no rule matches, DefaultAction decides. A nil policy admits
// every request, which keeps mage backwards compatible when no policy is
// configured.
type Policy struct {
	// DefaultAction is "allow" (the default) or "deny".
	DefaultAction string `json:"defaultAction,omitempty"`

	// Rules are evaluated in order; the first matching rule decides.
	Rules []Rule `json:"rules"`
}

// LoadPolicy reads, parses and validates the admission policy at path.
func LoadPolicy(path string) (*Policy, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening admission policy %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	return ParsePolicy(f)
}

// ParsePolicy parses an admission policy from r and validates it.
func ParsePolicy(r io.Reader) (*Policy, error) {
	p := &Policy{}
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(p); err != nil {
		return nil, fmt.Errorf("parsing admission policy: %w", err)
	}
	if err := p.validate(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *Policy) validate() error {
	switch strings.ToLower(strings.TrimSpace(p.DefaultAction)) {
	case "", "allow", "deny":
	default:
		return fmt.Errorf("admission policy: defaultAction must be \"allow\" or \"deny\", got %q", p.DefaultAction)
	}
	for i := range p.Rules {
		r := &p.Rules[i]
		switch action := strings.ToLower(strings.TrimSpace(r.Action)); action {
		case "allow", "deny":
			r.Action = action
		default:
			return fmt.Errorf("admission policy rule %d: action must be \"allow\" or \"deny\", got %q", i+1, r.Action)
		}
		if r.Func != "" {
			if _, err := filepath.Match(r.Func, ""); err != nil {
				return fmt.Errorf("admission policy rule %d: invalid func pattern %q: %w", i+1, r.Func, err)
			}
		}
		if r.Dir != "" {
			if _, err := filepath.Match(r.Dir, ""); err != nil {
				return fmt.Errorf("admission policy rule %d: invalid dir pattern %q: %w", i+1, r.Dir, err)
			}
		}
	}
	return nil
}

func (p *Policy) defaultDenies() bool {
	return strings.EqualFold(strings.TrimSpace(p.DefaultAction), "deny")
}

// Admit reports whether req is allowed to run. It returns nil when the
// request is admitted, otherwise a *Denial.
func (p *Policy) Admit(req TargetRequest) *Denial {
	for i := range p.Rules {
		r := &p.Rules[i]
		if !r.matches(req) {
			continue
		}
		if r.Action == actionDeny {
			return &Denial{Request: req, Rule: *r, RuleIndex: i + 1}
		}
		return nil
	}
	if p.defaultDenies() {
		return &Denial{
			Request:   req,
			Rule:      Rule{Action: actionDeny, Reason: "default action is deny"},
			RuleIndex: 0,
		}
	}
	return nil
}

const (
	actionAllow = "allow"
	actionDeny  = "deny"
)

func (r *Rule) matches(req TargetRequest) bool {
	if r.Target != "" && !strings.EqualFold(r.Target, req.Target) {
		return false
	}
	if r.Func != "" {
		if ok, _ := filepath.Match(r.Func, req.Func); !ok {
			return false
		}
	}
	if r.Namespace != "" && !namespaceMatches(r.Namespace, req.Namespace) {
		return false
	}
	if r.Alias != "" && !strings.EqualFold(r.Alias, req.Alias) {
		return false
	}
	if r.Args != nil && !equalStrings(r.Args, req.Args) {
		return false
	}
	if len(r.ArgsPrefix) > 0 && !hasPrefixArgs(req.Args, r.ArgsPrefix) {
		return false
	}
	if r.Dir != "" {
		dir := filepath.Clean(req.Dir)
		if ok, _ := filepath.Match(r.Dir, dir); !ok {
			return false
		}
	}
	return true
}

// namespaceMatches reports whether the rule namespace covers the request
// namespace. An exact match works, as does a parent namespace such as
// "deploy" covering "deploy:ships".
func namespaceMatches(ruleNS, reqNS string) bool {
	ruleNS = strings.ToLower(ruleNS)
	reqNS = strings.ToLower(reqNS)
	if ruleNS == reqNS {
		return true
	}
	return strings.HasPrefix(reqNS, ruleNS+":")
}

func equalStrings(want, got []string) bool {
	if len(want) != len(got) {
		return false
	}
	for i := range want {
		if want[i] != got[i] {
			return false
		}
	}
	return true
}

func hasPrefixArgs(args, prefix []string) bool {
	if len(args) < len(prefix) {
		return false
	}
	for i := range prefix {
		if args[i] != prefix[i] {
			return false
		}
	}
	return true
}

// ChainLink is one hop in the structured explanation leading from the
// requested root target to the node whose admission was rejected.
type ChainLink struct {
	// Request is the resolved identity of this node.
	Request TargetRequest

	// Rule is a copy of the rule that rejected this node; it is nil for
	// ancestors that were admitted.
	Rule *Rule

	// RuleIndex is the 1-based index of the matching rule, or 0 when the
	// default action rejected the node.
	RuleIndex int
}

// Denial is the structured result of a rejected admission. It implements
// error and carries an ExitStatus so it flows through mage's existing error
// handling unchanged.
type Denial struct {
	// Request is the identity that was rejected.
	Request TargetRequest

	// Rule is the rule that rejected the request, or a synthetic default
	// rule when the policy's defaultAction is "deny".
	Rule Rule

	// RuleIndex is the 1-based rule index, or 0 for the default action.
	RuleIndex int

	// Chain is the path from the requested root target to the rejected
	// node. The last link is always the rejected node.
	Chain []ChainLink
}

// Error renders the denial as a deterministic, human-readable explanation
// from the requested target to the rejected node.
func (d *Denial) Error() string {
	var b strings.Builder
	b.WriteString("target admission denied: ")
	b.WriteString(describeRequest(d.Request))
	b.WriteString(" rejected by ")
	b.WriteString(d.describeRule())
	if d.Rule.Reason != "" {
		b.WriteString(": ")
		b.WriteString(d.Rule.Reason)
	}
	if len(d.Chain) > 1 {
		b.WriteString("\ncall chain: ")
		for i, link := range d.Chain {
			if i > 0 {
				b.WriteString(" -> ")
			}
			b.WriteString(describeRequest(link.Request))
		}
	}
	return b.String()
}

// ExitStatus implements the mage exit status interface.
func (d *Denial) ExitStatus() int { return AdmissionDeniedExitCode }

func (d *Denial) describeRule() string {
	if d.RuleIndex > 0 {
		return fmt.Sprintf("%s rule %d", d.Rule.Action, d.RuleIndex)
	}
	return "default action"
}

// admissionError aggregates several denials from one parallel dependency
// batch. All nodes in the batch are evaluated before any node starts, so the
// message is deterministic.
type admissionError struct {
	denials []*Denial
}

func (e *admissionError) Error() string {
	parts := make([]string, len(e.denials))
	for i, d := range e.denials {
		parts[i] = d.Error()
	}
	return strings.Join(parts, "\n")
}

func (*admissionError) ExitStatus() int { return AdmissionDeniedExitCode }

func describeRequest(req TargetRequest) string {
	var b strings.Builder
	switch {
	case req.Target != "":
		fmt.Fprintf(&b, "target %q", req.Target)
	default:
		fmt.Fprintf(&b, "dependency %q", req.Func)
	}
	if req.Alias != "" {
		fmt.Fprintf(&b, " (alias %q)", req.Alias)
	}
	if req.Func != "" {
		fmt.Fprintf(&b, " [func %s]", req.Func)
	}
	if len(req.Args) > 0 {
		fmt.Fprintf(&b, " args=%v", req.Args)
	}
	if req.Dir != "" {
		fmt.Fprintf(&b, " dir=%q", req.Dir)
	}
	return b.String()
}

// TargetInfo describes the parsed CLI identity of a target. The generated
// main file registers every target so that transitive dependencies admitted
// through mg.Deps can be matched by the same target/namespace selectors as
// roots typed on the command line.
type TargetInfo struct {
	// Target is the lower-cased CLI target name, e.g. "build" or
	// "deploy:ships".
	Target string

	// Namespace is the namespace prefix of Target, e.g. "deploy".
	Namespace string

	// Aliases are the CLI aliases that resolve to this target.
	Aliases []string
}

var (
	registryMu sync.RWMutex
	registry   = map[string]TargetInfo{}

	envRegistryOnce sync.Once
	envRegistry     map[string]TargetInfo
)

// RegisterTargets merges target identities into the process-wide target
// registry. It is called by in-process callers and tests. Later registrations
// for the same function name replace earlier ones.
func RegisterTargets(targets map[string]TargetInfo) {
	registryMu.Lock()
	defer registryMu.Unlock()
	for name, info := range targets {
		registry[name] = info
	}
}

// LookupTarget returns the registered identity of a runtime function.
func LookupTarget(funcName string) (TargetInfo, bool) {
	registryMu.RLock()
	info, ok := registry[funcName]
	registryMu.RUnlock()
	if ok {
		return info, true
	}
	return envRegistryLookup(funcName)
}

func envRegistryLookup(funcName string) (TargetInfo, bool) {
	envRegistryOnce.Do(func() {
		raw := os.Getenv(AdmissionTargetsEnv)
		if raw == "" {
			return
		}
		if err := json.Unmarshal([]byte(raw), &envRegistry); err != nil {
			envRegistry = nil
		}
	})
	info, ok := envRegistry[funcName]
	return info, ok
}

// rootChainFromEnv returns the single-link chain of the root target currently
// running, as published by the generated main file. It is the starting point
// of the explanation when the root goroutine itself did not install an
// ambient chain (the generated file admits roots with an inline, stdlib-only
// implementation).
func rootChainFromEnv() []ChainLink {
	raw := os.Getenv(AdmissionRootEnv)
	if raw == "" {
		return nil
	}
	var req TargetRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		return nil
	}
	return []ChainLink{{Request: withWorkDir(req)}}
}

// parentChain returns the admission chain of the calling target: the ambient
// chain installed by the scheduler, or the published root identity when the
// call starts directly inside a root body.
func parentChain() []ChainLink {
	if chain := ambientChain(); len(chain) > 0 {
		return chain
	}
	return rootChainFromEnv()
}

// activePolicy resolution: an explicit policy set with SetPolicy wins,
// otherwise the policy referenced by AdmissionPolicyEnv is loaded once. A
// set-but-empty policy can be represented with a non-nil *Policy that has no
// rules, so "unconfigured" is tracked separately with policyOverridden.
var (
	policyMu         sync.Mutex
	policyOverride   *Policy
	policyOverridden bool

	loadOnce  sync.Once
	envPolicy *Policy
	envErr    error
)

// SetPolicy installs the policy used by every admission decision in this
// process, replacing any policy loaded from the environment. Pass nil to
// remove an override and fall back to the AdmissionPolicyEnv configuration.
// This is primarily intended for in-process callers and tests; the mage CLI
// and embedded mage.Invoke configure the policy file through the environment
// instead.
func SetPolicy(p *Policy) {
	policyMu.Lock()
	defer policyMu.Unlock()
	policyOverride = p
	policyOverridden = true
}

// activePolicy returns the configured policy or nil when admission is
// disabled. A configured-but-unreadable policy fails closed.
func activePolicy() *Policy {
	policyMu.Lock()
	if policyOverridden {
		p := policyOverride
		policyMu.Unlock()
		return p
	}
	policyMu.Unlock()

	loadOnce.Do(func() {
		path := os.Getenv(AdmissionPolicyEnv)
		if path == "" {
			return
		}
		envPolicy, envErr = LoadPolicy(path)
	})
	if envErr != nil {
		return loadErrorPolicy(envErr)
	}
	return envPolicy
}

func loadErrorPolicy(err error) *Policy {
	return &Policy{
		DefaultAction: actionDeny,
		Rules: []Rule{{
			Action: actionDeny,
			Reason: "admission policy could not be loaded: " + err.Error(),
		}},
	}
}

// admit evaluates req against the active policy and attaches an explanation
// chain extending parent. It returns nil when no policy is configured or the
// request is allowed.
func admit(parent []ChainLink, req TargetRequest) *Denial {
	p := activePolicy()
	if p == nil {
		return nil
	}
	req = withWorkDir(req)
	d := p.Admit(req)
	if d == nil {
		return nil
	}
	chain := make([]ChainLink, 0, len(parent)+1)
	chain = append(chain, parent...)
	rule := d.Rule
	chain = append(chain, ChainLink{Request: req, Rule: &rule, RuleIndex: d.RuleIndex})
	d.Chain = chain
	return d
}

// withWorkDir fills in and normalizes the working directory of a request:
// it is made absolute and symlinks are resolved so a rule path supplied by
// the caller compares against the physical directory the target runs in.
func withWorkDir(req TargetRequest) TargetRequest {
	dir := req.Dir
	if dir == "" {
		if wd, err := os.Getwd(); err == nil {
			dir = wd
		}
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	req.Dir = dir
	return req
}

// RunAdmitted admits req under the active policy and only runs run when the
// request is allowed; a denied request returns a *Denial without starting the
// target or any of its dependencies. When no policy is configured it is a
// thin wrapper around run, preserving the legacy scheduling behavior.
//
// In-process callers (and tests) use RunAdmitted as the single root entry
// point. The generated mage main file cannot import this package (its imports
// must stay standard-library only), so it contains an inline copy with
// identical semantics instead; both publish the running root identity
// through AdmissionRootEnv, which is why dependency chains explained by the
// scheduler start at the requested target in either case.
func RunAdmitted(ctx context.Context, req TargetRequest, run func(context.Context) error) error {
	p := activePolicy()
	if p == nil {
		return run(ctx)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	req = withWorkDir(req)
	d := p.Admit(req)
	if d != nil {
		rule := d.Rule
		d.Chain = []ChainLink{{Request: req, Rule: &rule, RuleIndex: d.RuleIndex}}
		return d
	}
	gid := goroutineID()
	setAmbientChain(gid, []ChainLink{{Request: req}})
	defer clearAmbientChain(gid)
	return run(ctx)
}

// dependencyRequest builds the TargetRequest for a transitive dependency,
// enriching the runtime identity with parsed CLI metadata when available.
func dependencyRequest(f Fn) TargetRequest {
	req := TargetRequest{
		Func: f.Name(),
		Args: decodeFnArgs(f.ID()),
	}
	if info, ok := LookupTarget(f.Name()); ok {
		req.Target = info.Target
		req.Namespace = info.Namespace
	} else {
		deriveIdentity(&req)
	}
	return req
}

// deriveIdentity fills a best-effort CLI identity from a runtime function
// name when no parsed registry is available (for example a cached compiled
// binary). Main-package targets and namespaces are recovered exactly in the
// common case; functions in imported packages without registry data remain
// matchable by their fully qualified Func name.
func deriveIdentity(req *TargetRequest) {
	pkg, rest, ok := strings.Cut(req.Func, ".")
	if !ok || pkg != "main" {
		return
	}
	if receiver, name, found := strings.Cut(rest, "."); found {
		req.Namespace = strings.ToLower(receiver)
		req.Target = req.Namespace + ":" + strings.ToLower(name)
		return
	}
	req.Target = strings.ToLower(rest)
}

// decodeFnArgs decodes the JSON argument array produced by mg.F into the raw
// string form used by policy argument selectors. String arguments are
// unquoted; other supported values (int, bool, time.Duration) keep their JSON
// literal form.
func decodeFnArgs(id string) []string {
	id = strings.TrimSpace(id)
	if id == "" || id == "null" || id == "[]" {
		return nil
	}
	var raw []json.RawMessage
	if err := json.Unmarshal([]byte(id), &raw); err != nil {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		var s string
		if err := json.Unmarshal(r, &s); err == nil {
			out = append(out, s)
			continue
		}
		out = append(out, string(r))
	}
	return out
}

// Ambient chains track the request -> running node path on each goroutine so
// mg.Deps, which receives no context on its plain form, can explain how a
// rejected transitive dependency was reached. The chain is installed by the
// scheduler when a target goroutine starts and removed when it ends.
var (
	ambientMu sync.Mutex
	ambient   = map[int64][]ChainLink{}
)

func setAmbientChain(gid int64, chain []ChainLink) {
	ambientMu.Lock()
	ambient[gid] = chain
	ambientMu.Unlock()
}

func clearAmbientChain(gid int64) {
	ambientMu.Lock()
	delete(ambient, gid)
	ambientMu.Unlock()
}

func ambientChain() []ChainLink {
	ambientMu.Lock()
	chain := ambient[goroutineID()]
	ambientMu.Unlock()
	return chain
}

// goroutineID returns the ID of the current goroutine. It is only used to
// correlate the synchronous Deps call nesting inside a running target.
func goroutineID() int64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	// stack text starts with "goroutine 123 [".
	b := buf[len("goroutine "):n]
	end := bytes.IndexByte(b, ' ')
	if end < 0 {
		return 0
	}
	id, err := strconv.ParseInt(string(b[:end]), 10, 64)
	if err != nil {
		return 0
	}
	return id
}
