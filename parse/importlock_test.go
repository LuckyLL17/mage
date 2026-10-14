package parse

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	mageimportDir = "../mage/testdata/mageimport"
	importLockDir = "../mage/testdata/importlock/basic"

	subdir1Path = "github.com/magefile/mage/mage/testdata/mageimport/subdir1"
	subdir2Path = "github.com/magefile/mage/mage/testdata/mageimport/subdir2"
	depPath     = "github.com/magefile/mage/mage/testdata/importlock/basic/dep"
	dep2Path    = "github.com/magefile/mage/mage/testdata/importlock/basic/dep2"
)

func snapshotFor(t *testing.T, dir string) *LockFile {
	t.Helper()
	info, err := PrimaryPackage("go", dir, nil, false)
	if err != nil {
		t.Fatalf("PrimaryPackage(%q): %v", dir, err)
	}
	lf, errs := ImportLockSnapshot("go", info)
	if len(errs) != 0 {
		t.Fatalf("ImportLockSnapshot(%q) returned errors: %v", dir, errs)
	}
	return lf
}

func findImport(t *testing.T, lf *LockFile, path string) LockedImport {
	t.Helper()
	for _, imp := range lf.Imports {
		if imp.Path == path {
			return imp
		}
	}
	t.Fatalf("import %q not found in lock snapshot (have %v)", path, importPaths(lf))
	return LockedImport{}
}

func importPaths(lf *LockFile) []string {
	var out []string
	for _, imp := range lf.Imports {
		out = append(out, imp.Path)
	}
	return out
}

func TestImportLockSnapshotDirect(t *testing.T) {
	lf := snapshotFor(t, mageimportDir)

	if lf.Version != LockVersion {
		t.Fatalf("lock version = %d, want %d", lf.Version, LockVersion)
	}
	if len(lf.Imports) != 2 {
		t.Fatalf("expected 2 imports, got %d: %v", len(lf.Imports), importPaths(lf))
	}

	root := findImport(t, lf, subdir1Path)
	if root.Alias != "" {
		t.Errorf("subdir1 alias = %q, want empty", root.Alias)
	}
	if root.Transitive {
		t.Error("subdir1 should be a direct import")
	}
	if root.UniqueName == "" {
		t.Error("subdir1 should have a unique generated name")
	}
	if root.Source.Hash == "" || !strings.HasPrefix(root.Source.Hash, "sha256:") {
		t.Errorf("subdir1 source hash = %q, want sha256 hash", root.Source.Hash)
	}
	if root.Source.Module != "github.com/magefile/mage" {
		t.Errorf("subdir1 source module = %q", root.Source.Module)
	}
	if len(root.Chain) != 1 || root.Chain[0].Path != subdir1Path {
		t.Errorf("subdir1 chain = %v", root.Chain)
	}

	named := findImport(t, lf, subdir2Path)
	if named.Alias != "zz" {
		t.Errorf("subdir2 alias = %q, want zz", named.Alias)
	}
	if named.UniqueName == "" || named.UniqueName == root.UniqueName {
		t.Errorf("same-name packages need distinct unique names: %q vs %q", named.UniqueName, root.UniqueName)
	}

	wantTargets := map[string]LockTarget{
		"root":            {Name: "Root"},
		"buildsubdir":     {Import: subdir1Path, Name: "BuildSubdir"},
		"ns:deploy":       {Import: subdir1Path, Receiver: "NS", Name: "Deploy"},
		"zz:buildsubdir2": {Import: subdir2Path, Name: "BuildSubdir2"},
		"zz:ns:deploy2":   {Import: subdir2Path, Receiver: "NS", Name: "Deploy2"},
	}
	for name, want := range wantTargets {
		got, ok := lf.Targets[name]
		if !ok {
			t.Errorf("missing resolved target %q in %v", name, lf.Targets)
			continue
		}
		if got != want {
			t.Errorf("target %q = %+v, want %+v", name, got, want)
		}
	}
	if got := lf.Aliases["nsd2"]; got != (LockTarget{Import: subdir2Path, Receiver: "NS", Name: "Deploy2"}) {
		t.Errorf("alias nsd2 = %+v", got)
	}
}

func TestImportLockSnapshotTransitive(t *testing.T) {
	lf := snapshotFor(t, importLockDir)

	direct := findImport(t, lf, depPath)
	if direct.Transitive {
		t.Error("dep should be direct")
	}
	if direct.Alias != "lockdep" {
		t.Errorf("dep alias = %q, want lockdep", direct.Alias)
	}

	trans := findImport(t, lf, dep2Path)
	if !trans.Transitive {
		t.Fatal("dep2 should be transitive")
	}
	wantChain := []ImportEdge{
		{Path: depPath, Alias: "lockdep"},
		{Path: dep2Path},
	}
	if len(trans.Chain) != 2 || trans.Chain[0] != wantChain[0] || trans.Chain[1] != wantChain[1] {
		t.Fatalf("dep2 chain = %v, want %v", trans.Chain, wantChain)
	}
	if trans.Source.Hash == "" {
		t.Error("dep2 source hash empty")
	}
	if got := len(trans.Targets); got != 1 || trans.Targets[0].Name != "Second" {
		t.Errorf("dep2 targets = %v, want just Second", trans.Targets)
	}

	if _, ok := lf.Targets["lockdep:build"]; !ok {
		t.Errorf("lockdep:build missing from resolved targets: %v", lf.Targets)
	}
	if _, ok := lf.Targets["root"]; !ok {
		t.Errorf("root missing from resolved targets: %v", lf.Targets)
	}
	// transitive mage:imports are recorded but not exposed under current
	// mage:import semantics.
	if _, ok := lf.Targets["second"]; ok {
		t.Errorf("transitive target must not be exposed: %v", lf.Targets)
	}
}

func TestLockFileRoundTripAndSelfValidate(t *testing.T) {
	lf := snapshotFor(t, importLockDir)
	if err := lf.Validate(lf); err != nil {
		t.Fatalf("self validation failed: %v", err)
	}

	path := filepath.Join(t.TempDir(), "mage.lock.json")
	if err := WriteLockFile(path, lf); err != nil {
		t.Fatalf("WriteLockFile: %v", err)
	}
	loaded, err := ReadLockFile(path)
	if err != nil {
		t.Fatalf("ReadLockFile: %v", err)
	}
	if err := loaded.Validate(lf); err != nil {
		t.Fatalf("written lock failed to validate against the snapshot: %v", err)
	}

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("temporary lock file left behind: %s", e.Name())
		}
	}
}

func cloneLock(lf *LockFile) *LockFile {
	n := &LockFile{
		Version: lf.Version,
		Targets: map[string]LockTarget{},
		Aliases: map[string]LockTarget{},
	}
	n.Imports = make([]LockedImport, len(lf.Imports))
	for i, imp := range lf.Imports {
		imp.Chain = append([]ImportEdge(nil), imp.Chain...)
		imp.Targets = append([]LockTarget(nil), imp.Targets...)
		n.Imports[i] = imp
	}
	for k, v := range lf.Targets {
		n.Targets[k] = v
	}
	for k, v := range lf.Aliases {
		n.Aliases[k] = v
	}
	return n
}

func TestLockValidateSourceIdentityChange(t *testing.T) {
	current := snapshotFor(t, mageimportDir)
	locked := cloneLock(current)
	locked.Imports[1].Source.Hash = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	locked.Imports[1].Source.Version = "v9.9.9"

	err := locked.Validate(current)
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "source identity changed") {
		t.Errorf("error should mention source identity change: %s", msg)
	}
	if !strings.Contains(msg, "root magefile -> "+subdir2Path+" (alias zz)") {
		t.Errorf("error should contain the import chain: %s", msg)
	}
	if !strings.Contains(msg, "v9.9.9") {
		t.Errorf("error should mention the changed version: %s", msg)
	}
}

func TestLockValidateAliasChange(t *testing.T) {
	current := snapshotFor(t, mageimportDir)
	locked := cloneLock(current)
	locked.Imports[1].Alias = "yy"

	err := locked.Validate(current)
	if err == nil || !strings.Contains(err.Error(), "alias changed") {
		t.Fatalf("expected alias changed error, got %v", err)
	}
}

func TestLockValidateTargetResolvesElsewhere(t *testing.T) {
	current := snapshotFor(t, mageimportDir)
	locked := cloneLock(current)
	// the same exposed name now claims to come from the other import
	locked.Targets["zz:buildsubdir2"] = LockTarget{Import: subdir1Path, Name: "BuildSubdir2"}

	err := locked.Validate(current)
	if err == nil {
		t.Fatal("expected conflict error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, `target "zz:buildsubdir2" resolves to a different implementation`) {
		t.Errorf("expected target conflict, got: %s", msg)
	}
	if !strings.Contains(msg, subdir1Path) || !strings.Contains(msg, subdir2Path) {
		t.Errorf("conflict should name both implementations: %s", msg)
	}
}

func TestLockValidateTargetRemoved(t *testing.T) {
	current := snapshotFor(t, mageimportDir)
	locked := cloneLock(current)
	locked.Targets["gone:thing"] = LockTarget{Import: subdir2Path, Name: "Thing"}

	err := locked.Validate(current)
	if err == nil || !strings.Contains(err.Error(), `target "gone:thing"`) {
		t.Fatalf("expected missing target error, got %v", err)
	}
}

func TestLockValidateExtraImport(t *testing.T) {
	current := snapshotFor(t, mageimportDir)
	locked := cloneLock(current)
	locked.Imports = locked.Imports[:1]

	err := locked.Validate(current)
	if err == nil || !strings.Contains(err.Error(), "not recorded in the lock file") {
		t.Fatalf("expected unrecorded import error, got %v", err)
	}
}

func TestLockValidateMissingImport(t *testing.T) {
	current := snapshotFor(t, mageimportDir)
	locked := cloneLock(current)
	// simulate the package disappearing after a dependency change: the
	// current snapshot no longer resolves it.
	current.Imports = current.Imports[:1]
	delete(current.Targets, "zz:buildsubdir2")
	delete(current.Targets, "zz:ns:deploy2")

	err := locked.Validate(current)
	if err == nil || !strings.Contains(err.Error(), "can no longer be resolved") {
		t.Fatalf("expected missing import error, got %v", err)
	}
}

func TestLockValidateIncomplete(t *testing.T) {
	current := snapshotFor(t, mageimportDir)

	empty := &LockFile{Version: LockVersion}
	if err := empty.Validate(current); err == nil ||
		!strings.Contains(err.Error(), "records no mage:import entries") {
		t.Fatalf("expected incomplete lock error, got %v", err)
	}

	noHash := cloneLock(current)
	noHash.Imports[0].Source.Hash = ""
	if err := noHash.Validate(current); err == nil ||
		!strings.Contains(err.Error(), "has no source identity") {
		t.Fatalf("expected missing identity error, got %v", err)
	}

	badVersion := cloneLock(current)
	badVersion.Version = 99
	if err := badVersion.Validate(current); err == nil ||
		!strings.Contains(err.Error(), "version") {
		t.Fatalf("expected version error, got %v", err)
	}
}

func TestLockSnapshotWithoutImports(t *testing.T) {
	info, err := PrimaryPackage("go", "../mage/testdata/alias", nil, false)
	if err != nil {
		t.Fatalf("PrimaryPackage: %v", err)
	}
	lf, snapErrs := ImportLockSnapshot("go", info)
	if len(snapErrs) != 0 {
		t.Fatalf("unexpected snapshot errors: %v", snapErrs)
	}
	if len(lf.Imports) != 0 {
		t.Fatalf("expected no imports, got %v", importPaths(lf))
	}
	if _, ok := lf.Targets["status"]; !ok {
		t.Fatalf("root target missing: %v", lf.Targets)
	}
	if err := lf.Validate(lf); err != nil {
		t.Fatalf("an empty import lock should validate: %v", err)
	}
}

func TestLockValidateSourceDirIgnored(t *testing.T) {
	current := snapshotFor(t, mageimportDir)
	locked := cloneLock(current)
	locked.Imports[0].Source.Dir = "/some/other/machine/path"
	if err := locked.Validate(current); err != nil {
		t.Fatalf("machine-specific dir must not invalidate the lock: %v", err)
	}
}

func TestCollectTransitiveMissingReportsChain(t *testing.T) {
	src := `package dep

import (
	//mage:import
	_ "github.com/magefile/mage/mage/testdata/importlock/does/not/exist"
)
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "dep.go", src, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	pkg := &ast.Package{Name: "dep", Files: map[string]*ast.File{"dep.go": f}}
	parent := &Import{
		Path:  depPath,
		Alias: "lockdep",
		Info:  PkgInfo{AstPkg: pkg},
	}
	chain := []ImportEdge{{Path: depPath, Alias: "lockdep"}}
	var nodes []LockedImport
	var errs []error
	collectTransitive("go", parent, chain, map[string]bool{}, &nodes, &errs)

	if len(errs) != 1 {
		t.Fatalf("expected 1 resolution error, got %d: %v", len(errs), errs)
	}
	msg := errs[0].Error()
	if !strings.Contains(msg, "could not be resolved") {
		t.Errorf("error should explain the missing import: %s", msg)
	}
	if !strings.Contains(msg, "root magefile -> "+depPath+" (alias lockdep) -> github.com/magefile/mage/mage/testdata/importlock/does/not/exist") {
		t.Errorf("error should report the full import chain: %s", msg)
	}
}
