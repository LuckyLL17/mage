package mage

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const admissionDir = "testdata/admission"

func admissionPolicy(name string) string {
	abs, err := filepath.Abs(filepath.Join(admissionDir, "policies", name))
	if err != nil {
		panic(err)
	}
	return abs
}

func runAdmission(t *testing.T, policy string, workDir string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	inv := Invocation{
		Dir:             admissionDir,
		WorkDir:         workDir,
		Stdout:          out,
		Stderr:          errOut,
		Args:            args,
		AdmissionPolicy: policy,
	}
	code = Invoke(inv)
	return code, out.String(), errOut.String()
}

func TestAdmissionUnconfiguredRunsEverything(t *testing.T) {
	out := &bytes.Buffer{}
	inv := Invocation{
		Dir:    admissionDir,
		Stdout: out,
		Stderr: io.Discard,
		Args:   []string{"danger"},
	}
	if code := Invoke(inv); code != 0 {
		t.Fatalf("unconfigured policy must keep exit code 0, got %d: %s", code, out)
	}
	if !strings.Contains(out.String(), "danger ran") {
		t.Fatalf("unconfigured policy must not block targets, got %q", out.String())
	}
}

func TestAdmissionDenyTarget(t *testing.T) {
	code, stdout, stderr := runAdmission(t, admissionPolicy("deny-danger.json"), "", "danger")
	if code != 77 {
		t.Fatalf("denied target exit code = %d, want 77\nstdout:%s\nstderr:%s", code, stdout, stderr)
	}
	if strings.Contains(stdout, "danger ran") {
		t.Fatalf("denied target must not run, stdout: %s", stdout)
	}
	if !strings.Contains(stderr, "target admission denied") ||
		!strings.Contains(stderr, `target "danger"`) ||
		!strings.Contains(stderr, "danger is forbidden in shared CI") {
		t.Fatalf("stderr must explain the denial, got %q", stderr)
	}

	// an allowed target under the same policy still runs with code 0.
	code, stdout, _ = runAdmission(t, admissionPolicy("deny-danger.json"), "", "build")
	if code != 0 || !strings.Contains(stdout, "build ran") {
		t.Fatalf("allowed target must run, code=%d stdout=%q", code, stdout)
	}
}

func TestAdmissionDenyFuncIdentity(t *testing.T) {
	code, stdout, stderr := runAdmission(t, admissionPolicy("deny-func.json"), "", "danger")
	if code != 77 {
		t.Fatalf("exit = %d, want 77: %s", code, stderr)
	}
	if strings.Contains(stdout, "danger ran") {
		t.Fatal("deny by fully qualified func identity must block the target")
	}
	if !strings.Contains(stderr, "[func main.Danger]") {
		t.Fatalf("denial must show resolved func identity: %s", stderr)
	}
}

func TestAdmissionDenyAlias(t *testing.T) {
	code, stdout, stderr := runAdmission(t, admissionPolicy("deny-alias.json"), "", "b")
	if code != 77 {
		t.Fatalf("exit = %d, want 77: %s", code, stderr)
	}
	if strings.Contains(stdout, "build ran") {
		t.Fatal("alias invocation must be blocked before the target runs")
	}
	if !strings.Contains(stderr, `alias "b"`) {
		t.Fatalf("denial must identify the alias used: %s", stderr)
	}
	// the same target through its canonical name is allowed.
	code, stdout, _ = runAdmission(t, admissionPolicy("deny-alias.json"), "", "build")
	if code != 0 || !strings.Contains(stdout, "build ran") {
		t.Fatalf("canonical name must still work, code=%d stdout=%q", code, stdout)
	}
}

func TestAdmissionNamespace(t *testing.T) {
	for _, target := range []string{"ns:task", "ns:nested"} {
		code, stdout, stderr := runAdmission(t, admissionPolicy("deny-namespace.json"), "", target)
		if code != 77 {
			t.Fatalf("%s: exit = %d, want 77: %s", target, code, stderr)
		}
		if strings.Contains(stdout, "ran") {
			t.Fatalf("%s: namespaced target must not run: %s", target, stdout)
		}
		if !strings.Contains(stderr, `target "`+target+`"`) {
			t.Fatalf("%s: denial must name the resolved target: %s", target, stderr)
		}
	}
	code, _, _ := runAdmission(t, admissionPolicy("deny-namespace.json"), "", "build")
	if code != 0 {
		t.Fatalf("top-level targets stay allowed, code=%d", code)
	}
}

func TestAdmissionArgs(t *testing.T) {
	code, stdout, _ := runAdmission(t, admissionPolicy("deny-args.json"), "", "greet", "admin")
	if code != 77 || strings.Contains(stdout, "greet ran") {
		t.Fatalf("greet admin must be denied, code=%d stdout=%q", code, stdout)
	}
	code, stdout, _ = runAdmission(t, admissionPolicy("deny-args.json"), "", "greet", "bob")
	if code != 0 || !strings.Contains(stdout, "greet ran bob") {
		t.Fatalf("greet bob must be allowed, code=%d stdout=%q", code, stdout)
	}
}

func TestAdmissionDefaultDeny(t *testing.T) {
	code, _, _ := runAdmission(t, admissionPolicy("allowlist.json"), "", "danger")
	if code != 77 {
		t.Fatalf("default deny must reject unlisted targets, got %d", code)
	}
	code, stdout, _ := runAdmission(t, admissionPolicy("allowlist.json"), "", "build")
	if code != 0 || !strings.Contains(stdout, "build ran") {
		t.Fatalf("explicitly allowed target must run, code=%d stdout=%q", code, stdout)
	}
}

func TestAdmissionTransitiveChain(t *testing.T) {
	code, stdout, stderr := runAdmission(t, admissionPolicy("deny-leaf.json"), "", "chain")
	if code != 77 {
		t.Fatalf("exit = %d, want 77: %s", code, stderr)
	}
	for _, ran := range []string{"leaf ran", "mid ran", "chain ran"} {
		if strings.Contains(stdout, ran) {
			t.Fatalf("rejected dependency and its downstream nodes must not run; stdout contains %q:\n%s", ran, stdout)
		}
	}
	if !strings.Contains(stderr, "call chain:") ||
		!strings.Contains(stderr, `target "chain"`) ||
		!strings.Contains(stderr, `target "mid"`) ||
		!strings.Contains(stderr, `target "leaf"`) {
		t.Fatalf("stderr must explain chain -> mid -> leaf:\n%s", stderr)
	}
}

func TestAdmissionParallelBatchIsDeterministic(t *testing.T) {
	for i := 0; i < 5; i++ {
		code, stdout, stderr := runAdmission(t, admissionPolicy("deny-pb.json"), "", "parallel")
		if code != 77 {
			t.Fatalf("iteration %d: exit = %d, want 77: %s", i, code, stderr)
		}
		if strings.Contains(stdout, "pa ran") || strings.Contains(stdout, "pb ran") || strings.Contains(stdout, "parallel ran") {
			t.Fatalf("iteration %d: no node of a partially denied parallel batch may start:\n%s", i, stdout)
		}
	}
}

func TestAdmissionSharedDependencyTwoRoots(t *testing.T) {
	// allowed: shared dep runs exactly once for two roots in one invocation.
	code, stdout, stderr := runAdmission(t, "", "", "share1", "share2")
	if code != 0 {
		t.Fatalf("allowed shared dep: %s", stderr)
	}
	if strings.Count(stdout, "shared ran") != 1 {
		t.Fatalf("shared dep must run exactly once, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "share1 ran") || !strings.Contains(stdout, "share2 ran") {
		t.Fatalf("both roots must complete:\n%s", stdout)
	}

	// denied: the first root fails with 77 and the second root never starts.
	code, stdout, stderr = runAdmission(t, admissionPolicy("deny-shared.json"), "", "share1", "share2")
	if code != 77 {
		t.Fatalf("denied shared dep exit = %d, want 77: %s", code, stderr)
	}
	if strings.Contains(stdout, "shared ran") {
		t.Fatalf("denied shared dep must never run:\n%s", stdout)
	}
	if strings.Contains(stdout, "share2 ran") {
		t.Fatalf("the second root must not start after the first was rejected:\n%s", stdout)
	}
}

func TestAdmissionWorkDirRule(t *testing.T) {
	allowedDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	policyPath := filepath.Join(t.TempDir(), "deny-dir.json")
	policy := fmt.Sprintf(`{
		"defaultAction": "allow",
		"rules": [{"action": "deny", "dir": "%s", "reason": "workdir not authorized"}]
	}`, allowedDir)
	if err := os.WriteFile(policyPath, []byte(policy), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runAdmission(t, policyPath, allowedDir, "build")
	if code != 77 {
		t.Fatalf("expected denial in %s, code=%d: %s", allowedDir, code, stderr)
	}
	if strings.Contains(stdout, "build ran") {
		t.Fatal("target in denied workdir must not run")
	}
	otherDir := t.TempDir()
	code, stdout, _ = runAdmission(t, policyPath, otherDir, "build")
	if code != 0 || !strings.Contains(stdout, "build ran") {
		t.Fatalf("target outside denied workdir must run, code=%d stdout=%q", code, stdout)
	}
}

func TestAdmissionDiscoveryUnaffected(t *testing.T) {
	policy := admissionPolicy("allowlist.json")

	// list still shows every target and exits 0 even with a deny-all policy.
	out := &bytes.Buffer{}
	inv := Invocation{
		Dir: admissionDir, Stdout: out, Stderr: io.Discard,
		List: true, AdmissionPolicy: policy,
	}
	if code := Invoke(inv); code != 0 {
		t.Fatalf("list must keep exit code 0 under policy, got %d", code)
	}
	for _, name := range []string{"build", "danger", "chain", "ns:task", "greet"} {
		if !strings.Contains(out.String(), name) {
			t.Fatalf("target discovery must still list %q:\n%s", name, out.String())
		}
	}

	// help keeps working and exits 0.
	out.Reset()
	inv = Invocation{
		Dir: admissionDir, Stdout: out, Stderr: io.Discard,
		Help: true, Args: []string{"build"}, AdmissionPolicy: policy,
	}
	if code := Invoke(inv); code != 0 {
		t.Fatalf("help must keep exit code 0 under policy, got %d", code)
	}

	// unknown targets still exit 2, not the admission exit code.
	code, _, stderr := runAdmission(t, policy, "", "does-not-exist")
	if code != 2 {
		t.Fatalf("unknown target exit = %d, want 2: %s", code, stderr)
	}
}

func TestAdmissionBadPolicyFailsClosed(t *testing.T) {
	policyPath := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(policyPath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runAdmission(t, policyPath, "", "build")
	if code != 77 {
		t.Fatalf("unusable policy must fail closed with 77, got %d: %s", code, stderr)
	}
	if strings.Contains(stdout, "build ran") {
		t.Fatal("no target may run when the policy cannot be loaded")
	}
}

func TestAdmissionCLIFlag(t *testing.T) {
	// the -admission flag is the same configuration path as the embedded
	// Invocation field.
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	code := ParseAndRun(stdout, stderr, nil, []string{
		"-d", admissionDir,
		"-admission", admissionPolicy("deny-danger.json"),
		"danger",
	})
	if code != 77 {
		t.Fatalf("CLI -admission flag must enforce policy, code=%d:\n%s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "danger ran") {
		t.Fatal("target must be blocked when policy is supplied through the CLI flag")
	}

	// a relative policy path is resolved from the caller's working
	// directory, not from the magefile directory (where the compiled
	// binary runs).
	stdout.Reset()
	stderr.Reset()
	code = ParseAndRun(stdout, stderr, nil, []string{
		"-d", admissionDir,
		"-admission", filepath.Join("testdata", "admission", "policies", "deny-danger.json"),
		"danger",
	})
	if code != 77 {
		t.Fatalf("relative -admission path must resolve from the caller dir, code=%d:\n%s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "danger ran") {
		t.Fatal("target must be blocked with a relative policy path")
	}
}
