package parse

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/magefile/mage/internal"
)

// LockVersion is the version of the import lock file format understood by
// this version of mage.
const LockVersion = 1

// LockFile is the on-disk representation of mage:import resolution. It records
// every direct and transitive mage:import, the source identity of the package
// it resolves to, the alias/namespace it is exposed under, the targets it
// exposes and the conflict resolution (unique generated package name and
// final target name to implementation mapping) that was in effect when the
// lock was generated.
type LockFile struct {
	Version int                   `json:"version"`
	Imports []LockedImport        `json:"imports"`
	Targets map[string]LockTarget `json:"targets"`
	Aliases map[string]LockTarget `json:"aliases,omitempty"`
}

// LockedImport is a single resolved mage:import node.
type LockedImport struct {
	// Path is the Go import path of the imported package.
	Path string `json:"path"`
	// Alias is the mage:import alias the importing package assigned to it
	// (empty for root imports).
	Alias string `json:"alias,omitempty"`
	// Transitive reports whether the import was declared by a package other
	// than the root magefile.
	Transitive bool `json:"transitive,omitempty"`
	// Package is the Go package name of the imported package.
	Package string `json:"package"`
	// UniqueName is the generated identifier mage assigned when multiple
	// imports share the same package name. It is only set for direct imports,
	// which are compiled into the generated magefile.
	UniqueName string `json:"uniqueName,omitempty"`
	// Chain is the mage:import chain from the root magefile to this node,
	// inclusive of the node itself.
	Chain []ImportEdge `json:"chain"`
	// Source identifies the concrete source the path resolved to.
	Source LockSource `json:"source"`
	// Targets are the targets exposed by this package (receiver is the
	// namespace, Name the target function).
	Targets []LockTarget `json:"targets"`
}

// ImportEdge is one hop in an mage:import chain.
type ImportEdge struct {
	Path  string `json:"path"`
	Alias string `json:"alias,omitempty"`
}

// LockSource identifies the concrete source an import path resolved to.
// Dir is informational (it is machine specific) and never validated.
type LockSource struct {
	Dir            string `json:"dir,omitempty"`
	Module         string `json:"module,omitempty"`
	Version        string `json:"version,omitempty"`
	ReplaceModule  string `json:"replaceModule,omitempty"`
	ReplaceVersion string `json:"replaceVersion,omitempty"`
	ReplaceDir     string `json:"replaceDir,omitempty"`
	// Hash is a sha256 over the package's Go source files, as selected by
	// "go list" (thus respecting build tags).
	Hash string `json:"hash"`
}

// LockTarget identifies one exposed target implementation. Import is empty
// for targets defined by the root magefile.
type LockTarget struct {
	Import   string `json:"import,omitempty"`
	Receiver string `json:"receiver,omitempty"`
	Name     string `json:"name"`
}

// goListPackage is the subset of "go list -json" output that we use.
type goListPackage struct {
	Dir     string
	Name    string
	GoFiles []string
	Module  *goListModule
}

type goListModule struct {
	Path    string
	Version string
	Dir     string
	Replace *goListModule
}

// ImportLockSnapshot resolves the full mage:import graph reachable from the
// root package and records it, including transitive mage:import directives
// found inside imported packages. Direct imports are taken from root.Imports,
// i.e. the exact resolution used to execute, list, autocomplete, show help
// and compile, so all of those operations validate against the same data.
//
// Resolution errors for transitive imports (for example an import that can no
// longer be found after a dependency upgrade or replace change) are returned
// as errors while the rest of the snapshot is still populated.
func ImportLockSnapshot(gocmd string, root *PkgInfo) (*LockFile, []error) {
	lf := &LockFile{
		Version: LockVersion,
		Targets: map[string]LockTarget{},
		Aliases: map[string]LockTarget{},
	}

	for _, f := range root.Funcs {
		lf.Targets[strings.ToLower(f.TargetName())] = LockTarget{Receiver: f.Receiver, Name: f.Name}
	}
	for alias, f := range root.Aliases {
		lf.Aliases[strings.ToLower(alias)] = LockTarget{Import: f.ImportPath, Receiver: f.Receiver, Name: f.Name}
	}

	// visit every path only once; direct imports seed the set so a package
	// imported both directly and transitively is recorded exactly once, as a
	// direct import.
	visited := map[string]bool{}
	var errs []error

	direct := make([]*Import, len(root.Imports))
	copy(direct, root.Imports)
	sort.Sort(Imports(direct))

	for _, imp := range direct {
		visited[imp.Path] = true
		node := LockedImport{
			Path:       imp.Path,
			Alias:      imp.Alias,
			Package:    imp.Name,
			UniqueName: imp.UniqueName,
			Chain:      []ImportEdge{{Path: imp.Path, Alias: imp.Alias}},
		}
		if src, err := resolveSource(gocmd, imp.Path); err != nil {
			errs = append(errs, fmt.Errorf("import %q could not be resolved (import chain: %s): %w", imp.Path, formatChain(node.Chain), err))
		} else {
			node.Source = src
		}
		for _, f := range imp.Info.Funcs {
			node.Targets = append(node.Targets, LockTarget{Receiver: f.Receiver, Name: f.Name})
			lf.Targets[strings.ToLower(f.TargetName())] = LockTarget{Import: imp.Path, Receiver: f.Receiver, Name: f.Name}
		}
		sortTargets(node.Targets)
		lf.Imports = append(lf.Imports, node)
		collectTransitive(gocmd, imp, node.Chain, visited, &lf.Imports, &errs)
	}

	sort.SliceStable(lf.Imports, func(i, j int) bool {
		// direct imports first, matching the order mage generates code in
		if lf.Imports[i].Transitive != lf.Imports[j].Transitive {
			return !lf.Imports[i].Transitive
		}
		if lf.Imports[i].Path != lf.Imports[j].Path {
			return lf.Imports[i].Path < lf.Imports[j].Path
		}
		return lf.Imports[i].Alias < lf.Imports[j].Alias
	})
	return lf, errs
}

func collectTransitive(gocmd string, parent *Import, chain []ImportEdge, visited map[string]bool, out *[]LockedImport, errs *[]error) {
	for _, spec := range mageImportSpecs(parent.Info.AstPkg) {
		childChain := make([]ImportEdge, 0, len(chain)+1)
		childChain = append(childChain, chain...)
		childChain = append(childChain, ImportEdge(spec))

		if visited[spec.Path] {
			// diamond dependency or import cycle: the node is already
			// recorded through another chain.
			continue
		}
		child, err := getImport(gocmd, spec.Path, spec.Alias, parent.Info.Multiline)
		if err != nil {
			*errs = append(*errs, fmt.Errorf("import %q could not be resolved (import chain: %s): %w", spec.Path, formatChain(childChain), err))
			continue
		}
		visited[spec.Path] = true

		node := LockedImport{
			Path:       spec.Path,
			Alias:      spec.Alias,
			Transitive: true,
			Package:    child.Name,
			Chain:      childChain,
		}
		src, err := resolveSource(gocmd, spec.Path)
		if err != nil {
			*errs = append(*errs, fmt.Errorf("import %q could not be resolved (import chain: %s): %w", spec.Path, formatChain(childChain), err))
			continue
		}
		node.Source = src
		for _, f := range child.Info.Funcs {
			node.Targets = append(node.Targets, LockTarget{Receiver: f.Receiver, Name: f.Name})
		}
		sortTargets(node.Targets)
		*out = append(*out, node)
		collectTransitive(gocmd, child, childChain, visited, out, errs)
	}
}

func resolveSource(gocmd, importpath string) (LockSource, error) {
	out, err := internal.OutputDebug(gocmd, "list", "-json", importpath)
	if err != nil {
		return LockSource{}, err
	}
	var pkg goListPackage
	if err := json.NewDecoder(strings.NewReader(out)).Decode(&pkg); err != nil {
		return LockSource{}, fmt.Errorf("parsing go list output for %q: %w", importpath, err)
	}

	src := LockSource{Dir: pkg.Dir}
	if m := pkg.Module; m != nil {
		src.Module = m.Path
		src.Version = m.Version
		if r := m.Replace; r != nil {
			src.ReplaceModule = r.Path
			src.ReplaceVersion = r.Version
			src.ReplaceDir = r.Dir
		}
	}

	files := append([]string(nil), pkg.GoFiles...)
	sort.Strings(files)
	h := sha256.New()
	for _, name := range files {
		f, err := os.Open(filepath.Join(pkg.Dir, name))
		if err != nil {
			return LockSource{}, fmt.Errorf("hashing import source %s: %w", importpath, err)
		}
		_, _ = io.WriteString(h, "file:"+name+"\n")
		if _, err := io.Copy(h, f); err != nil {
			_ = f.Close()
			return LockSource{}, fmt.Errorf("hashing import source %s: %w", importpath, err)
		}
		_ = f.Close()
	}
	src.Hash = "sha256:" + hex.EncodeToString(h.Sum(nil))
	return src, nil
}

func sortTargets(t []LockTarget) {
	sort.Slice(t, func(i, j int) bool {
		if t[i].Receiver != t[j].Receiver {
			return t[i].Receiver < t[j].Receiver
		}
		return t[i].Name < t[j].Name
	})
}

func formatChain(chain []ImportEdge) string {
	var b strings.Builder
	_, _ = b.WriteString("root magefile")
	for _, e := range chain {
		_, _ = b.WriteString(" -> ")
		_, _ = b.WriteString(e.Path)
		if e.Alias != "" {
			_, _ = b.WriteString(" (alias ")
			_, _ = b.WriteString(e.Alias)
			_, _ = b.WriteString(")")
		}
	}
	return b.String()
}

// ReadLockFile reads and parses a lock file.
func ReadLockFile(path string) (*LockFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lf := &LockFile{}
	if err := json.Unmarshal(data, lf); err != nil {
		return nil, fmt.Errorf("lock file %q is incomplete or unreadable: %w", path, err)
	}
	return lf, nil
}

// WriteLockFile atomically writes the lock file: data is rendered to a
// temporary file in the destination directory and then renamed into place, so
// concurrent readers never observe a partially written lock.
func WriteLockFile(path string, lf *LockFile) error {
	data, err := json.MarshalIndent(lf, "", "  ")
	if err != nil {
		return fmt.Errorf("rendering import lock: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	if dir == "" {
		dir = "."
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("creating import lock temp file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("writing import lock: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("writing import lock: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("writing import lock: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("writing import lock: %w", err)
	}
	return nil
}

// Validate checks the locked resolution against a freshly resolved snapshot.
// It returns an aggregated error describing every drift detected, each one
// annotated with the import chain that leads to it.
func (l *LockFile) Validate(current *LockFile) error {
	if l == nil {
		return errors.New("no import lock data")
	}
	var problems []string
	add := func(format string, args ...interface{}) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	if l.Version != LockVersion {
		add("lock file version is %d, but this mage version supports version %d; regenerate the lock file", l.Version, LockVersion)
	}
	if len(l.Imports) == 0 && len(current.Imports) > 0 {
		add("lock file records no mage:import entries, but the magefile currently imports %d package(s); regenerate the lock file", len(current.Imports))
	}

	currentByPath := map[string]LockedImport{}
	for _, imp := range current.Imports {
		if _, ok := currentByPath[imp.Path]; !ok {
			currentByPath[imp.Path] = imp
		}
	}
	lockedPaths := map[string]bool{}
	for _, imp := range l.Imports {
		if imp.Path == "" {
			add("lock file is incomplete: an import entry is missing its path")
			continue
		}
		lockedPaths[imp.Path] = true
		if len(imp.Chain) == 0 {
			add("lock file is incomplete: import %q has no import chain", imp.Path)
		}
		if imp.Source.Hash == "" {
			add("lock file is incomplete: import %q (import chain: %s) has no source identity", imp.Path, formatChain(imp.Chain))
		}
		for _, t := range imp.Targets {
			if t.Name == "" {
				add("lock file is incomplete: import %q records a target without a name", imp.Path)
			}
		}

		cur, ok := currentByPath[imp.Path]
		if !ok {
			add("import %q recorded in the lock can no longer be resolved (import chain: %s)", imp.Path, formatChain(imp.Chain))
			continue
		}
		if imp.Alias != cur.Alias {
			add("import %q alias changed (import chain: %s): lock records %q, current resolution uses %q",
				imp.Path, formatChain(imp.Chain), displayAlias(imp.Alias), displayAlias(cur.Alias))
		}
		if imp.Package != cur.Package {
			add("import %q package name changed (import chain: %s): lock records %q, current resolution uses %q",
				imp.Path, formatChain(imp.Chain), imp.Package, cur.Package)
		}
		if imp.UniqueName != cur.UniqueName {
			add("import %q conflict resolution changed (import chain: %s): lock assigned generated name %q, current resolution assigns %q",
				imp.Path, formatChain(imp.Chain), imp.UniqueName, cur.UniqueName)
		}
		if d := diffSource(imp.Path, imp.Chain, imp.Source, cur.Source); d != "" {
			add("%s", d)
		}
		if d := diffTargetSet(imp.Path, imp.Chain, imp.Targets, cur.Targets); d != "" {
			add("%s", d)
		}
	}
	for _, imp := range current.Imports {
		if !lockedPaths[imp.Path] {
			add("import %q is resolved (import chain: %s) but is not recorded in the lock file; regenerate the lock file",
				imp.Path, formatChain(imp.Chain))
		}
	}

	compareTargetMaps("target", l.Targets, current.Targets, currentByPath, add)
	compareTargetMaps("alias", l.Aliases, current.Aliases, currentByPath, add)

	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return errors.New("import lock validation failed:\n  " + strings.Join(problems, "\n  "))
}

func compareTargetMaps(kind string, locked, current map[string]LockTarget, chains map[string]LockedImport, add func(string, ...interface{})) {
	for name, lt := range locked {
		if lt.Name == "" {
			add("lock file is incomplete: %s %q has no target name", kind, name)
			continue
		}
		cur, ok := current[name]
		if !ok {
			add("%s %q recorded in the lock (provided by %s) is no longer exposed (import chain: %s)",
				kind, name, targetProvider(lt), formatChain(chainFor(chains, lt.Import)))
			continue
		}
		if cur != lt {
			add("%s %q resolves to a different implementation: lock records %s, current resolution provides %s (import chain: %s)",
				kind, name, targetProvider(lt), targetProvider(cur), formatChain(chainFor(chains, cur.Import)))
		}
	}
	for name, cur := range current {
		if _, ok := locked[name]; !ok {
			add("%s %q is exposed by the current resolution (provided by %s) but is not recorded in the lock file; regenerate the lock file",
				kind, name, targetProvider(cur))
		}
	}
}

func chainFor(chains map[string]LockedImport, importPath string) []ImportEdge {
	if importPath == "" {
		return nil
	}
	if imp, ok := chains[importPath]; ok {
		return imp.Chain
	}
	return []ImportEdge{{Path: importPath}}
}

func targetProvider(t LockTarget) string {
	s := t.Import
	if s == "" {
		s = "<root magefile>"
	}
	if t.Receiver != "" {
		return fmt.Sprintf("%s.%s.%s", s, t.Receiver, t.Name)
	}
	return fmt.Sprintf("%s.%s", s, t.Name)
}

func displayAlias(alias string) string {
	if alias == "" {
		return "<root>"
	}
	return alias
}

func diffSource(path string, chain []ImportEdge, locked, current LockSource) string {
	var details []string
	if locked.Module != current.Module {
		details = append(details, fmt.Sprintf("module %q -> %q", locked.Module, current.Module))
	}
	if locked.Version != current.Version {
		details = append(details, fmt.Sprintf("version %q -> %q", locked.Version, current.Version))
	}
	if locked.ReplaceModule != current.ReplaceModule || locked.ReplaceVersion != current.ReplaceVersion || locked.ReplaceDir != current.ReplaceDir {
		details = append(details, fmt.Sprintf("replace %s@%s (%s) -> %s@%s (%s)",
			locked.ReplaceModule, locked.ReplaceVersion, locked.ReplaceDir,
			current.ReplaceModule, current.ReplaceVersion, current.ReplaceDir))
	}
	if locked.Hash != current.Hash && current.Hash != "" {
		details = append(details, fmt.Sprintf("source hash %s -> %s", locked.Hash, current.Hash))
	}
	if len(details) == 0 {
		return ""
	}
	return fmt.Sprintf("import %q source identity changed (import chain: %s): %s", path, formatChain(chain), strings.Join(details, ", "))
}

func diffTargetSet(path string, chain []ImportEdge, locked, current []LockTarget) string {
	lockedSet := map[string]bool{}
	for _, t := range locked {
		lockedSet[t.Receiver+"."+t.Name] = true
	}
	currentSet := map[string]bool{}
	for _, t := range current {
		currentSet[t.Receiver+"."+t.Name] = true
	}
	var missing, extra []string
	for k := range lockedSet {
		if !currentSet[k] {
			missing = append(missing, k)
		}
	}
	for k := range currentSet {
		if !lockedSet[k] {
			extra = append(extra, k)
		}
	}
	if len(missing) == 0 && len(extra) == 0 {
		return ""
	}
	sort.Strings(missing)
	sort.Strings(extra)
	var b strings.Builder
	fmt.Fprintf(&b, "import %q exposed targets changed (import chain: %s)", path, formatChain(chain))
	if len(missing) > 0 {
		fmt.Fprintf(&b, ": removed since lock: %s", strings.Join(missing, ", "))
	}
	if len(extra) > 0 {
		if len(missing) > 0 {
			_, _ = b.WriteString(";")
		}
		fmt.Fprintf(&b, ": added since lock: %s", strings.Join(extra, ", "))
	}
	return b.String()
}
