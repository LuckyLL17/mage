package mg

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func resetAdmission(t *testing.T) {
	t.Helper()
	clearAdmission()
	t.Cleanup(clearAdmission)
}

func clearAdmission() {
	policyMu.Lock()
	policyOverride = nil
	policyOverridden = false
	policyMu.Unlock()
	loadOnce = sync.Once{}
	envPolicy = nil
	envErr = nil
	registryMu.Lock()
	registry = map[string]TargetInfo{}
	registryMu.Unlock()
	envRegistryOnce = sync.Once{}
	envRegistry = nil
	ResetDeps()
}

// stubFn is an mg.Fn with a controllable runtime identity so scheduler tests
// can exercise the same registry enrichment the generated main file provides.
type stubFn struct {
	name string
	id   string
	ran  func()
}

func (s *stubFn) Name() string { return s.name }
func (s *stubFn) ID() string   { return s.id }
func (s *stubFn) Run(context.Context) error {
	if s.ran != nil {
		s.ran()
	}
	return nil
}

func denyPolicy() *Policy {
	return &Policy{Rules: []Rule{{Action: actionDeny}}}
}

func TestPolicyDisabledByDefault(t *testing.T) {
	resetAdmission(t)
	if p := activePolicy(); p != nil {
		t.Fatalf("expected no policy, got %#v", p)
	}
	ran := false
	err := RunAdmitted(context.Background(), TargetRequest{Func: "main.Any"}, func(context.Context) error {
		ran = true
		return nil
	})
	if err != nil || !ran {
		t.Fatalf("disabled policy must run the target, got err=%v ran=%v", err, ran)
	}
}

func TestPolicySelectors(t *testing.T) {
	req := TargetRequest{
		Target:    "deploy:ships",
		Func:      "main.Deploy.Ships",
		Namespace: "deploy",
		Args:      []string{"prod", "--fast"},
		Dir:       "/ci/work",
	}
	cases := []struct {
		name string
		rule Rule
		want bool
	}{
		{"target match", Rule{Target: "Deploy:Ships"}, true},
		{"target mismatch", Rule{Target: "build"}, false},
		{"func exact", Rule{Func: "main.Deploy.Ships"}, true},
		{"func glob", Rule{Func: "main.Deploy.*"}, true},
		{"func glob mismatch", Rule{Func: "main.Build.*"}, false},
		{"namespace exact", Rule{Namespace: "Deploy"}, true},
		{"namespace unrelated prefix", Rule{Namespace: "de"}, false},
		{"namespace nested", Rule{Namespace: "deploy"}, true},
		{"alias miss", Rule{Alias: "ship"}, false},
		{"args exact", Rule{Args: []string{"prod", "--fast"}}, true},
		{"args mismatch", Rule{Args: []string{"prod"}}, false},
		{"args prefix", Rule{ArgsPrefix: []string{"prod"}}, true},
		{"args prefix mismatch", Rule{ArgsPrefix: []string{"dev"}}, false},
		{"dir glob", Rule{Dir: "/ci/*"}, true},
		{"dir mismatch", Rule{Dir: "/other/*"}, false},
		{"combined all match", Rule{Target: "deploy:ships", Func: "main.Deploy.*", Namespace: "deploy", ArgsPrefix: []string{"prod"}, Dir: "/ci/*"}, true},
		{"combined one mismatch", Rule{Target: "deploy:ships", Args: []string{"dev"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.rule.Action = actionDeny
			if got := tc.rule.matches(req); got != tc.want {
				t.Fatalf("matches = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNamespaceRuleCoversNested(t *testing.T) {
	p := &Policy{Rules: []Rule{{Action: actionDeny, Namespace: "ns"}}}
	for _, target := range []string{"ns:task", "ns:sub:task"} {
		req := TargetRequest{Target: target, Namespace: strings.SplitN(target, ":", 2)[0]}
		if d := p.Admit(req); d == nil {
			t.Fatalf("expected %s to be denied by namespace rule", target)
		}
	}
	if d := p.Admit(TargetRequest{Target: "nsx:task", Namespace: "nsx"}); d != nil {
		t.Fatalf("sibling namespace must not be denied, got %v", d)
	}
}

func TestRuleOrderAndDefaultAction(t *testing.T) {
	// deny-all default with an explicit allow for one target.
	p := &Policy{
		DefaultAction: actionDeny,
		Rules: []Rule{
			{Action: actionAllow, Target: "build"},
			{Action: actionDeny, Func: "main.*"},
		},
	}
	if d := p.Admit(TargetRequest{Target: "build", Func: "main.Build"}); d != nil {
		t.Fatalf("first matching allow rule must win, got %v", d)
	}
	d := p.Admit(TargetRequest{Target: "release", Func: "main.Release"})
	if d == nil || d.RuleIndex != 2 {
		t.Fatalf("expected deny rule #2, got %v", d)
	}
	d = p.Admit(TargetRequest{Target: "other", Func: "ext.Other"})
	if d == nil || d.RuleIndex != 0 {
		t.Fatalf("expected default deny, got %v", d)
	}
}

func TestParsePolicy(t *testing.T) {
	doc := `{
		"defaultAction": "deny",
		"rules": [
			{"action": "allow", "target": "build"},
			{"action": "deny", "alias": "b", "reason": "no shortcut in CI"}
		]
	}`
	p, err := ParsePolicy(strings.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	if !p.defaultDenies() || len(p.Rules) != 2 {
		t.Fatalf("unexpected policy: %#v", p)
	}
	if _, err := ParsePolicy(strings.NewReader(`{"defaultAction":"maybe"}`)); err == nil {
		t.Fatal("expected validation error for bad defaultAction")
	}
	if _, err := ParsePolicy(strings.NewReader(`{"rules":[{"action":"wat"}]}`)); err == nil {
		t.Fatal("expected validation error for bad action")
	}
	if _, err := ParsePolicy(strings.NewReader(`{"unknown":true}`)); err == nil {
		t.Fatal("expected error on unknown fields")
	}
}

func TestPolicyFromEnvFailsClosed(t *testing.T) {
	resetAdmission(t)
	t.Setenv(AdmissionPolicyEnv, filepath.Join(t.TempDir(), "missing.json"))
	p := activePolicy()
	if p == nil {
		t.Fatal("missing policy file must fail closed, not disable admission")
	}
	d := p.Admit(TargetRequest{Target: "build", Func: "main.Build"})
	if d == nil || !strings.Contains(d.Error(), "could not be loaded") {
		t.Fatalf("expected load error denial, got %v", d)
	}
	if d.ExitStatus() != AdmissionDeniedExitCode {
		t.Fatalf("exit status = %d, want %d", d.ExitStatus(), AdmissionDeniedExitCode)
	}
}

func TestRunAdmittedAllowAndDeny(t *testing.T) {
	resetAdmission(t)
	SetPolicy(&Policy{Rules: []Rule{{Action: actionDeny, Target: "danger"}}})

	allowed := false
	err := RunAdmitted(context.Background(), TargetRequest{Target: "build", Func: "main.Build"}, func(context.Context) error {
		allowed = true
		return nil
	})
	if err != nil || !allowed {
		t.Fatalf("allowed target should run, err=%v ran=%v", err, allowed)
	}

	err = RunAdmitted(context.Background(), TargetRequest{Target: "danger", Func: "main.Danger"}, func(context.Context) error {
		t.Fatal("denied root target must never run")
		return nil
	})
	var d *Denial
	if !errors.As(err, &d) {
		t.Fatalf("expected *Denial, got %T %v", err, err)
	}
	if d.ExitStatus() != AdmissionDeniedExitCode {
		t.Fatalf("exit = %d, want %d", d.ExitStatus(), AdmissionDeniedExitCode)
	}
	if len(d.Chain) != 1 || d.Chain[0].Request.Target != "danger" {
		t.Fatalf("unexpected chain: %#v", d.Chain)
	}
	if !strings.Contains(err.Error(), `target "danger"`) {
		t.Fatalf("denial must explain the rejected target: %v", err)
	}
}

func TestDepsDeniedNeverRuns(t *testing.T) {
	resetAdmission(t)
	SetPolicy(denyPolicy())
	var ran int64
	dep := &stubFn{name: "main.Dep", ran: func() { atomic.AddInt64(&ran, 1) }}
	err := recoverDeps(func() { Deps(dep) })
	if err == nil {
		t.Fatal("expected panic from Deps")
	}
	if atomic.LoadInt64(&ran) != 0 {
		t.Fatal("denied dependency must not run")
	}
	var ae *admissionError
	if !errors.As(err, &ae) {
		t.Fatalf("expected *admissionError, got %T: %v", err, err)
	}
	if ae.ExitStatus() != AdmissionDeniedExitCode {
		t.Fatalf("exit = %d", ae.ExitStatus())
	}
}

func TestDepsBatchRejectsAllSiblings(t *testing.T) {
	resetAdmission(t)
	SetPolicy(&Policy{Rules: []Rule{{Action: actionDeny, Target: "bad"}}})
	RegisterTargets(map[string]TargetInfo{
		"main.Good": {Target: "good"},
		"main.Bad":  {Target: "bad"},
	})
	var goodRan, badRan int64
	good := &stubFn{name: "main.Good", ran: func() { atomic.AddInt64(&goodRan, 1) }}
	bad := &stubFn{name: "main.Bad", ran: func() { atomic.AddInt64(&badRan, 1) }}
	err := recoverDeps(func() { Deps(good, bad) })
	if err == nil {
		t.Fatal("expected panic")
	}
	if atomic.LoadInt64(&goodRan) != 0 || atomic.LoadInt64(&badRan) != 0 {
		t.Fatalf("no node in a partially denied batch may start, good=%d bad=%d", goodRan, badRan)
	}
}

func TestDepsDenialChain(t *testing.T) {
	resetAdmission(t)
	SetPolicy(&Policy{Rules: []Rule{{Action: actionDeny, Target: "leaf"}}})
	RegisterTargets(map[string]TargetInfo{
		"main.Root": {Target: "root"},
		"main.Mid":  {Target: "mid"},
		"main.Leaf": {Target: "leaf"},
	})
	var leafRan, midRan, rootRan int64
	leaf := &stubFn{name: "main.Leaf", ran: func() { atomic.AddInt64(&leafRan, 1) }}
	mid := &stubFn{name: "main.Mid", ran: func() {
		Deps(leaf)
		atomic.AddInt64(&midRan, 1)
	}}
	root := &stubFn{name: "main.Root", ran: func() {
		Deps(mid)
		atomic.AddInt64(&rootRan, 1)
	}}
	var err error
	func() {
		defer func() { err = recoverErr(recover()) }()
		_ = RunAdmitted(context.Background(), TargetRequest{Target: "root", Func: "main.Root"}, func(context.Context) error {
			root.Run(context.Background())
			return nil
		})
	}()
	if err == nil {
		t.Fatal("expected denied chain to surface")
	}
	msg := fmt.Sprint(err)
	for _, want := range []string{"call chain:", `target "root"`, `target "mid"`, `target "leaf"`} {
		if !strings.Contains(msg, want) {
			t.Fatalf("denial must contain %q, got:\n%s", want, msg)
		}
	}
	if atomic.LoadInt64(&leafRan) != 0 {
		t.Fatal("rejected node leaf must never run")
	}
	if atomic.LoadInt64(&midRan) != 0 {
		t.Fatal("mid must not continue after its Deps was rejected")
	}
	if atomic.LoadInt64(&rootRan) != 0 {
		t.Fatal("root must not continue after its Deps was rejected")
	}
}

func TestDepsParallelAllowedBranchesComplete(t *testing.T) {
	resetAdmission(t)
	SetPolicy(&Policy{DefaultAction: actionAllow})
	RegisterTargets(map[string]TargetInfo{
		"main.PA": {Target: "pa"},
		"main.PB": {Target: "pb"},
	})
	var a, b int64
	pa := &stubFn{name: "main.PA", ran: func() { atomic.AddInt64(&a, 1) }}
	pb := &stubFn{name: "main.PB", ran: func() { atomic.AddInt64(&b, 1) }}
	Deps(pa, pb)
	if atomic.LoadInt64(&a) != 1 || atomic.LoadInt64(&b) != 1 {
		t.Fatalf("both admitted parallel deps must run, a=%d b=%d", a, b)
	}
}

func TestSharedDependencyAcrossRoots(t *testing.T) {
	resetAdmission(t)
	SetPolicy(&Policy{DefaultAction: actionAllow})
	RegisterTargets(map[string]TargetInfo{"main.Shared": {Target: "shared"}})
	var shared int64
	sharedFn := &stubFn{name: "main.Shared", ran: func() { atomic.AddInt64(&shared, 1) }}
	runRoot := func(name string) {
		_ = RunAdmitted(context.Background(), TargetRequest{Target: name, Func: "main." + upperFirst(name)}, func(context.Context) error {
			Deps(sharedFn)
			return nil
		})
	}
	runRoot("r1")
	runRoot("r2")
	if got := atomic.LoadInt64(&shared); got != 1 {
		t.Fatalf("shared admitted dependency must run exactly once across roots, got %d", got)
	}
}

func TestSharedDeniedDependencyBlocksEveryRoot(t *testing.T) {
	resetAdmission(t)
	SetPolicy(&Policy{Rules: []Rule{{Action: actionDeny, Target: "shared"}}})
	RegisterTargets(map[string]TargetInfo{"main.Shared": {Target: "shared"}})
	var shared int64
	sharedFn := &stubFn{name: "main.Shared", ran: func() { atomic.AddInt64(&shared, 1) }}
	root := func(name string) error {
		return RunAdmitted(context.Background(), TargetRequest{Target: name, Func: "main." + upperFirst(name)}, func(context.Context) error {
			Deps(sharedFn)
			return nil
		})
	}
	runRoot := func(name string) (err error) {
		// scheduler failures surface as panics, exactly like in the
		// generated main file; recover them to inspect the result.
		defer func() { err = recoverErr(recover()) }()
		return root(name)
	}
	if err := runRoot("r1"); err == nil {
		t.Fatal("first root must fail on denied shared dependency")
	}
	if err := runRoot("r2"); err == nil {
		t.Fatal("second root must fail on the same denied shared dependency")
	}
	if atomic.LoadInt64(&shared) != 0 {
		t.Fatal("denied shared dependency must never run")
	}
}

func TestCanceledContextDoesNotStartDeps(t *testing.T) {
	resetAdmission(t)
	SetPolicy(&Policy{DefaultAction: actionAllow})
	var ran int64
	dep := &stubFn{name: "main.Dep", ran: func() { atomic.AddInt64(&ran, 1) }}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := recoverDeps(func() { CtxDeps(ctx, dep) })
	if err == nil {
		t.Fatal("expected panic on canceled context")
	}
	if atomic.LoadInt64(&ran) != 0 {
		t.Fatal("no dependency may start after cancellation")
	}
}

func TestDependencyRequestEnrichmentAndArgs(t *testing.T) {
	resetAdmission(t)
	RegisterTargets(map[string]TargetInfo{
		"main.Greet": {Target: "greet", Namespace: "", Aliases: []string{"g"}},
	})
	fn := &stubFn{name: "main.Greet", id: `["Susan",42,true]`}
	req := dependencyRequest(fn)
	if req.Target != "greet" || req.Namespace != "" {
		t.Fatalf("expected registered target enrichment, got %#v", req)
	}
	want := []string{"Susan", "42", "true"}
	if len(req.Args) != len(want) {
		t.Fatalf("args = %#v, want %#v", req.Args, want)
	}
	for i := range want {
		if req.Args[i] != want[i] {
			t.Fatalf("args = %#v, want %#v", req.Args, want)
		}
	}
	info, _ := LookupTarget("main.Greet")
	if info.Aliases[0] != "g" {
		t.Fatal("registered aliases must be visible")
	}
}

func TestWorkDirSelector(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	p := &Policy{Rules: []Rule{{Action: actionDeny, Dir: filepath.Join(dir, "*")}}}
	if d := p.Admit(TargetRequest{Target: "build", Func: "main.Build", Dir: filepath.Join(dir, "sub")}); d == nil {
		t.Fatal("expected working directory rule to deny a directory below the matched prefix")
	}
	if d := p.Admit(TargetRequest{Target: "build", Func: "main.Build", Dir: "/other/work"}); d != nil {
		t.Fatal("working directory rule must not match unrelated directories")
	}
}

func recoverDeps(f func()) (err error) {
	defer func() { err = recoverErr(recover()) }()
	f()
	return nil
}

func recoverErr(v interface{}) error {
	if v == nil {
		return nil
	}
	if e, ok := v.(error); ok {
		return e
	}
	return fmt.Errorf("%v", v)
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
