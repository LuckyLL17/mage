package mage

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/magefile/mage/mg"
	"github.com/magefile/mage/parse"
)

const importLockFixture = "./testdata/importlock/basic"

const (
	importLockDepPath  = "github.com/magefile/mage/mage/testdata/importlock/basic/dep"
	importLockDep2Path = "github.com/magefile/mage/mage/testdata/importlock/basic/dep2"
)

func generateLock(t *testing.T, dir string) string {
	t.Helper()
	lockPath := filepath.Join(t.TempDir(), "mage.lock.json")
	t.Setenv(mg.ImportLockFileEnv, lockPath)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	code := Invoke(Invocation{
		Dir:            dir,
		Stdout:         stdout,
		Stderr:         stderr,
		ImportLockMode: ImportLockUpdate,
	})
	if code != 0 {
		t.Fatalf("import lock update exited %d: stdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock file not written: %v", err)
	}
	return lockPath
}

func readLock(t *testing.T, path string) *parse.LockFile {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lf := &parse.LockFile{}
	if err := json.Unmarshal(data, lf); err != nil {
		t.Fatalf("lock file is not valid json: %v\n%s", err, data)
	}
	return lf
}

func TestImportLockUpdateRecordsGraph(t *testing.T) {
	lockPath := generateLock(t, importLockFixture)
	lf := readLock(t, lockPath)

	if lf.Version != parse.LockVersion {
		t.Fatalf("lock version = %d, want %d", lf.Version, parse.LockVersion)
	}
	var direct, transitive *parse.LockedImport
	for i := range lf.Imports {
		switch lf.Imports[i].Path {
		case importLockDepPath:
			direct = &lf.Imports[i]
		case importLockDep2Path:
			transitive = &lf.Imports[i]
		default:
			// no other imports are expected
		}
	}
	if direct == nil {
		t.Fatalf("direct import not recorded: %v", lf.Imports)
	}
	if direct.Alias != "lockdep" || direct.Transitive {
		t.Errorf("direct import = %+v", direct)
	}
	if direct.Source.Hash == "" || direct.Source.Module != "github.com/magefile/mage" {
		t.Errorf("direct import source = %+v", direct.Source)
	}
	if transitive == nil {
		t.Fatalf("transitive import not recorded: %v", lf.Imports)
	}
	if !transitive.Transitive || len(transitive.Chain) != 2 {
		t.Fatalf("transitive import = %+v", transitive)
	}
	if transitive.Chain[0].Path != direct.Path || transitive.Chain[1].Path != transitive.Path {
		t.Errorf("transitive chain = %v", transitive.Chain)
	}
	if _, ok := lf.Targets["lockdep:build"]; !ok {
		t.Errorf("lockdep:build missing from targets: %v", lf.Targets)
	}
}

func TestImportLockAllowsEveryOperation(t *testing.T) {
	lockPath := generateLock(t, importLockFixture)

	t.Run("list", func(t *testing.T) {
		t.Setenv(mg.ImportLockFileEnv, lockPath)
		stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
		inv := Invocation{Dir: importLockFixture, Stdout: stdout, Stderr: stderr, List: true, ImportLockMode: ImportLockValidate}
		if code := Invoke(inv); code != 0 {
			t.Fatalf("code %d, stderr: %s", code, stderr)
		}
		if !strings.Contains(stdout.String(), "lockdep:build") {
			t.Fatalf("list output missing imported target:\n%s", stdout)
		}
	})

	t.Run("help", func(t *testing.T) {
		t.Setenv(mg.ImportLockFileEnv, lockPath)
		stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
		inv := Invocation{
			Dir:            importLockFixture,
			Stdout:         stdout,
			Stderr:         stderr,
			Help:           true,
			Args:           []string{"lockdep:ns:deploy"},
			ImportLockMode: ImportLockValidate,
		}
		if code := Invoke(inv); code != 0 {
			t.Fatalf("code %d, stderr: %s", code, stderr)
		}
		if !strings.Contains(stdout.String(), "mage lockdep:ns:deploy") {
			t.Fatalf("help output:\n%s", stdout)
		}
	})

	t.Run("autocomplete", func(t *testing.T) {
		t.Setenv(mg.ImportLockFileEnv, lockPath)
		stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
		inv := Invocation{Dir: importLockFixture, Stdout: stdout, Stderr: stderr, Autocomplete: true, ImportLockMode: ImportLockValidate}
		if code := Invoke(inv); code != 0 {
			t.Fatalf("code %d, stderr: %s", code, stderr)
		}
		found := false
		for _, l := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
			if l == "lockdep:build" {
				found = true
			}
		}
		if !found {
			t.Fatalf("autocomplete missing lockdep:build:\n%s", stdout)
		}
	})

	t.Run("execute root", func(t *testing.T) {
		t.Setenv(mg.ImportLockFileEnv, lockPath)
		stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
		inv := Invocation{Dir: importLockFixture, Stdout: stdout, Stderr: stderr, Args: []string{"root"}, ImportLockMode: ImportLockValidate}
		if code := Invoke(inv); code != 0 {
			t.Fatalf("code %d, stderr: %s", code, stderr)
		}
		if stdout.String() != "root\n" {
			t.Fatalf("output = %q", stdout.String())
		}
	})

	t.Run("execute imported", func(t *testing.T) {
		t.Setenv(mg.ImportLockFileEnv, lockPath)
		stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
		inv := Invocation{Dir: importLockFixture, Stdout: stdout, Stderr: stderr, Args: []string{"lockdep:build"}, ImportLockMode: ImportLockValidate}
		if code := Invoke(inv); code != 0 {
			t.Fatalf("code %d, stderr: %s", code, stderr)
		}
		if stdout.String() != "dep build\n" {
			t.Fatalf("output = %q", stdout.String())
		}
	})

	t.Run("compile", func(t *testing.T) {
		t.Setenv(mg.ImportLockFileEnv, lockPath)
		out := filepath.Join(t.TempDir(), "magebinary")
		stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
		inv := Invocation{
			Dir:            importLockFixture,
			Stdout:         stdout,
			Stderr:         stderr,
			CompileOut:     out,
			Force:          true,
			ImportLockMode: ImportLockValidate,
		}
		if code := Invoke(inv); code != 0 {
			t.Fatalf("code %d, stderr: %s", code, stderr)
		}
		if _, err := os.Stat(out); err != nil {
			t.Fatalf("compiled binary missing: %v", err)
		}
	})
}

func TestImportLockDriftBlocksAllOperations(t *testing.T) {
	good := generateLock(t, importLockFixture)

	// tamper with the recorded source identity
	data, err := os.ReadFile(good)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	tampered := false
	for i, l := range lines {
		if strings.Contains(l, `"hash": "sha256:`) {
			lines[i] = strings.Replace(l, "sha256:", "sha256:0", 1)
			tampered = true
			break
		}
	}
	if !tampered {
		t.Fatal("could not find a hash entry in lock file")
	}
	badPath := filepath.Join(t.TempDir(), "mage.lock.json")
	if err := os.WriteFile(badPath, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(mg.ImportLockFileEnv, badPath)

	check := func(t *testing.T, inv Invocation) {
		t.Helper()
		stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
		inv.Dir = importLockFixture
		inv.Stdout = stdout
		inv.Stderr = stderr
		inv.ImportLockMode = ImportLockValidate
		if code := Invoke(inv); code != 1 {
			t.Fatalf("expected exit code 1 on drift, got %d (stderr: %s)", code, stderr)
		}
		msg := stderr.String()
		if !strings.Contains(msg, "source identity changed") {
			t.Fatalf("expected identity drift error, got: %s", msg)
		}
		if !strings.Contains(msg, "root magefile -> "+importLockDepPath) {
			t.Fatalf("error must report the import chain: %s", msg)
		}
	}

	t.Run("execute", func(t *testing.T) { check(t, Invocation{Args: []string{"root"}}) })
	t.Run("list", func(t *testing.T) { check(t, Invocation{List: true}) })
	t.Run("help", func(t *testing.T) { check(t, Invocation{Help: true, Args: []string{"root"}}) })
	t.Run("autocomplete", func(t *testing.T) { check(t, Invocation{Autocomplete: true}) })
	t.Run("compile", func(t *testing.T) {
		check(t, Invocation{CompileOut: filepath.Join(t.TempDir(), "out")})
	})
}

func TestImportLockMissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "mage.lock.json")
	t.Setenv(mg.ImportLockFileEnv, missing)
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	inv := Invocation{
		Dir:            importLockFixture,
		Stdout:         stdout,
		Stderr:         stderr,
		List:           true,
		ImportLockMode: ImportLockValidate,
	}
	if code := Invoke(inv); code != 1 {
		t.Fatalf("expected code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "not found") || !strings.Contains(stderr.String(), "-importlockupdate") {
		t.Fatalf("expected a message explaining how to create the lock, got: %s", stderr)
	}
}

func TestImportLockDisabledLeavesNoLock(t *testing.T) {
	lockPath := filepath.Join(importLockFixture, defaultImportLockFile)
	if _, err := os.Stat(lockPath); err == nil {
		t.Fatalf("precondition failed: stray lock file at %s", lockPath)
	}
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	code := Invoke(Invocation{Dir: importLockFixture, Stdout: stdout, Stderr: stderr, List: true})
	if code != 0 {
		t.Fatalf("code %d, stderr: %s", code, stderr)
	}
	if _, err := os.Stat(lockPath); err == nil {
		_ = os.Remove(lockPath)
		t.Fatal("a lock file was created although locking was not enabled")
	}
}

func TestParseImportLockFlags(t *testing.T) {
	t.Setenv(mg.ImportLockFileEnv, "")
	stderr, stdout := &bytes.Buffer{}, &bytes.Buffer{}

	inv, cmd, err := Parse(stderr, stdout, []string{"-importlock", "-importlockfile", "/tmp/x.lock"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd != None || inv.ImportLockMode != ImportLockValidate || mg.ImportLockFile() != "/tmp/x.lock" {
		t.Fatalf("inv = %+v, cmd = %v, file = %q", inv, cmd, mg.ImportLockFile())
	}

	_, cmd, err = Parse(stderr, stdout, []string{"-importlockupdate"})
	if err != nil || cmd != UpdateImportLock {
		t.Fatalf("cmd = %v, err = %v", cmd, err)
	}

	_, _, err = Parse(stderr, stdout, []string{"-importlockupdate", "-autocomplete"})
	if err == nil || !strings.Contains(err.Error(), "cannot be used simultaneously") {
		t.Fatalf("expected mutual exclusion error, got %v", err)
	}
}
