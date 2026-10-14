package mage

// this template uses the "data"

// var only for tests.
var mageMainfileTplString = `//go:build ignore
// +build ignore

package main

import (
	_context "context"
	_flag "flag"
	_fmt "fmt"
	_io "io"
	_log "log"
	_os "os"
	_signal "os/signal"
	_filepath "path/filepath"
	_sort "sort"
	_strconv "strconv"
	_strings "strings"
	_syscall "syscall"
	_tabwriter "text/tabwriter"
	_time "time"
	_json "encoding/json"
	_sync "sync"
	{{range .Imports}}{{.UniqueName}} "{{.Path}}"
	{{end}}
)

// Target admission is inlined here (stdlib only) so the generated main file
// never introduces a new non-stdlib import. The logic is required to stay
// identical to github.com/magefile/mage/mg's policy implementation: the same
// MAGEFILE_ADMISSION_POLICY file, selectors, denial messages and exit status.
// Transitive dependencies are gated by the mg scheduler itself, using the same
// policy file; the running root identity is published for mg via
// MAGEFILE_ADMISSION_ROOT.
const _mageAdmissionDeniedExitCode = 77

type _mageTargetRequest struct {
	Target    string
	Func      string
	Namespace string
	Alias     string
	Args      []string
	Dir       string
}

type _mageRule struct {
	Action     string
	Target     string
	Func       string
	Namespace  string
	Alias      string
	Args       []string
	ArgsPrefix []string
	Dir        string
	Reason     string
}

type _magePolicy struct {
	DefaultAction string
	Rules         []_mageRule
}

type _mageDenied struct {
	req   _mageTargetRequest
	rule  _mageRule
	index int
}

func (d *_mageDenied) Error() string {
	var b _strings.Builder
	b.WriteString("target admission denied: ")
	b.WriteString(_mageDescribeRequest(d.req))
	b.WriteString(" rejected by ")
	if d.index > 0 {
		_fmt.Fprintf(&b, "%s rule %d", d.rule.Action, d.index)
	} else {
		b.WriteString("default action")
	}
	if d.rule.Reason != "" {
		b.WriteString(": ")
		b.WriteString(d.rule.Reason)
	}
	return b.String()
}

func (d *_mageDenied) ExitStatus() int { return _mageAdmissionDeniedExitCode }

func _mageDescribeRequest(req _mageTargetRequest) string {
	var b _strings.Builder
	if req.Target != "" {
		_fmt.Fprintf(&b, "target %q", req.Target)
	} else {
		_fmt.Fprintf(&b, "dependency %q", req.Func)
	}
	if req.Alias != "" {
		_fmt.Fprintf(&b, " (alias %q)", req.Alias)
	}
	if req.Func != "" {
		_fmt.Fprintf(&b, " [func %s]", req.Func)
	}
	if len(req.Args) > 0 {
		_fmt.Fprintf(&b, " args=%v", req.Args)
	}
	if req.Dir != "" {
		_fmt.Fprintf(&b, " dir=%q", req.Dir)
	}
	return b.String()
}

var (
	_magePolicyOnce _sync.Once
	_magePolicyData *_magePolicy
)

func _mageLoadPolicy() *_magePolicy {
	_magePolicyOnce.Do(func() {
		path := _os.Getenv("MAGEFILE_ADMISSION_POLICY")
		if path == "" {
			return
		}
		fail := func(msg string) {
			_magePolicyData = &_magePolicy{DefaultAction: "deny"}
			_magePolicyData.Rules = append(_magePolicyData.Rules, _mageRule{Action: "deny", Reason: msg})
		}
		f, err := _os.Open(path)
		if err != nil {
			fail("admission policy could not be loaded: " + err.Error())
			return
		}
		var p _magePolicy
		dec := _json.NewDecoder(f)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&p); err != nil {
			_ = f.Close()
			fail("admission policy could not be loaded: " + err.Error())
			return
		}
		_ = f.Close()
		if p.DefaultAction != "" && p.DefaultAction != "allow" && p.DefaultAction != "deny" {
			fail("admission policy could not be loaded: defaultAction must be \"allow\" or \"deny\"")
			return
		}
		for i := range p.Rules {
			if p.Rules[i].Action != "allow" && p.Rules[i].Action != "deny" {
				fail("admission policy could not be loaded: every rule needs an \"allow\" or \"deny\" action")
				return
			}
		}
		_magePolicyData = &p
	})
	return _magePolicyData
}

func _mageNamespaceMatches(ruleNS, reqNS string) bool {
	ruleNS = _strings.ToLower(ruleNS)
	reqNS = _strings.ToLower(reqNS)
	return ruleNS == reqNS || _strings.HasPrefix(reqNS, ruleNS+":")
}

func _mageEqualStrings(want, got []string) bool {
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

func _mageHasPrefixArgs(args, prefix []string) bool {
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

func (r *_mageRule) matches(req _mageTargetRequest) bool {
	if r.Target != "" && !_strings.EqualFold(r.Target, req.Target) {
		return false
	}
	if r.Func != "" {
		if ok, _ := _filepath.Match(r.Func, req.Func); !ok {
			return false
		}
	}
	if r.Namespace != "" && !_mageNamespaceMatches(r.Namespace, req.Namespace) {
		return false
	}
	if r.Alias != "" && !_strings.EqualFold(r.Alias, req.Alias) {
		return false
	}
	if r.Args != nil && !_mageEqualStrings(r.Args, req.Args) {
		return false
	}
	if len(r.ArgsPrefix) > 0 && !_mageHasPrefixArgs(req.Args, r.ArgsPrefix) {
		return false
	}
	if r.Dir != "" {
		if ok, _ := _filepath.Match(r.Dir, _filepath.Clean(req.Dir)); !ok {
			return false
		}
	}
	return true
}

// _magePublishRoot publishes the admitted root identity so the mg dependency
// scheduler can explain a rejected transitive node from the requested target.
func _magePublishRoot(req _mageTargetRequest) {
	root, err := _json.Marshal(req)
	if err == nil {
		_ = _os.Setenv("MAGEFILE_ADMISSION_ROOT", string(root))
	}
}

func _mageAdmitRun(ctx _context.Context, req _mageTargetRequest, run func(_context.Context) error) error {
	p := _mageLoadPolicy()
	if p == nil {
		_magePublishRoot(req)
		return run(ctx)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if wd, err := _os.Getwd(); err == nil {
		if abs, err := _filepath.Abs(wd); err == nil {
			wd = abs
		}
		if real, err := _filepath.EvalSymlinks(wd); err == nil {
			wd = real
		}
		req.Dir = wd
	}
	for i := range p.Rules {
		r := &p.Rules[i]
		if !r.matches(req) {
			continue
		}
		if r.Action == "deny" {
			return &_mageDenied{req: req, rule: *r, index: i + 1}
		}
		_magePublishRoot(req)
		return run(ctx)
	}
	if _strings.EqualFold(p.DefaultAction, "deny") {
		return &_mageDenied{req: req, rule: _mageRule{Action: "deny", Reason: "default action is deny"}}
	}
	_magePublishRoot(req)
	return run(ctx)
}

func main() {
	// Use local types and functions in order to avoid name conflicts with additional magefiles.
	type arguments struct {
		Verbose       bool          // print out log statements
		List          bool          // print out a list of targets
		Help          bool          // print out help for a specific target
		Timeout       _time.Duration // set a timeout to running the targets
		Args          []string      // args contain the non-flag command-line arguments
	}

	parseBool := func(env string) bool {
		val := _os.Getenv(env)
		if val == "" {
			return false
		}		
		b, err := _strconv.ParseBool(val)
		if err != nil {
			_log.Printf("warning: environment variable %s is not a valid bool value: %v", env, val)
			return false
		}
		return b
	}

	parseDuration := func(env string) _time.Duration {
		val := _os.Getenv(env)
		if val == "" {
			return 0
		}		
		d, err := _time.ParseDuration(val)
		if err != nil {
			_log.Printf("warning: environment variable %s is not a valid duration value: %v", env, val)
			return 0
		}
		return d
	}
	args := arguments{}
	fs := _flag.FlagSet{}
	fs.SetOutput(_os.Stdout)

	// default flag set with ExitOnError and auto generated PrintDefaults should be sufficient
	fs.BoolVar(&args.Verbose, "v", parseBool("MAGEFILE_VERBOSE"), "show verbose output when running targets")
	fs.BoolVar(&args.List, "l", parseBool("MAGEFILE_LIST"), "list targets for this binary")
	fs.BoolVar(&args.Help, "h", parseBool("MAGEFILE_HELP"), "print out help for a specific target")
	fs.DurationVar(&args.Timeout, "t", parseDuration("MAGEFILE_TIMEOUT"), "timeout in duration parsable format (e.g. 5m30s)")
	fs.Usage = func() {
		_fmt.Fprintf(_os.Stdout, ` + "`" + `
%s [options] [target]

Commands:
  -l    list targets in this binary
  -h    show this help

Options:
  -h    show description of a target
  -t <string>
        timeout in duration parsable format (e.g. 5m30s)
  -v    show verbose output when running targets
 ` + "`" + `[1:], _filepath.Base(_os.Args[0]))
	}
	if err := fs.Parse(_os.Args[1:]); err != nil {
		// flag will have printed out an error already.
		return
	}
	args.Args = fs.Args()
	if args.Help && len(args.Args) == 0 {
		fs.Usage()
		return
	}
		
	{{.PrintNameFunc}}

	list := func() error {
		{{with .Description}}_fmt.Println(` + "`{{.}}\n`" + `)
		{{- end}}
		{{- $default := .DefaultFunc}}
		targets := map[string]string{
		{{- range .Funcs}}
			"{{lowerFirst .TargetName}}{{if and (eq .Name $default.Name) (eq .Receiver $default.Receiver)}}*{{end}}": {{printf "%q" .Synopsis}},
		{{- end}}
		{{- range .Imports}}{{$imp := .}}
			{{- range .Info.Funcs}}
			"{{lowerFirst .TargetName}}{{if and (eq .Name $default.Name) (eq .Receiver $default.Receiver)}}*{{end}}": {{printf "%q" .Synopsis}},
			{{- end}}
		{{- end}}
		}

		keys := make([]string, 0, len(targets))
		for name := range targets {
			keys = append(keys, name)
		}
		_sort.Strings(keys)

		_fmt.Println("Targets:")
		w := _tabwriter.NewWriter(_os.Stdout, 0, 4, 4, ' ', 0)
		for _, name := range keys {
			_fmt.Fprintf(w, "  %v\t%v\n", printName(name), targets[name])
		}
		err := w.Flush()
		{{- if .DefaultFunc.Name}}
			if err == nil {
				_fmt.Println("\n* default target")
			}
		{{- end}}
		return err
	}

	var ctx _context.Context
	ctxCancel := func(){}

	// by deferring in a closure, we let the cancel function get replaced
	// by the getContext function.
	defer func() {
		ctxCancel()
	}()

	getContext := func() (_context.Context, func()) {
		if ctx == nil {
			if args.Timeout != 0 {
				ctx, ctxCancel = _context.WithTimeout(_context.Background(), args.Timeout)
			} else {
				ctx, ctxCancel = _context.WithCancel(_context.Background())
			}
		}

		return ctx, ctxCancel
	}

	runTarget := func(logger *_log.Logger, fn func(_context.Context) error) interface{} {
		var err interface{}
		ctx, cancel := getContext()
		d := make(chan interface{})
		go func() {
			defer func() {
				err := recover()
				d <- err
			}()
			err := fn(ctx)
			d <- err
		}()
		sigCh := make(chan _os.Signal, 1)
		_signal.Notify(sigCh, _syscall.SIGINT)
		select {
		case <-sigCh:
			logger.Println("cancelling mage targets, waiting up to 5 seconds for cleanup...")
			cancel()
			cleanupCh := _time.After(5 * _time.Second)

			select {
			// target exited by itself
			case err = <-d:
				return err
			// cleanup timeout exceeded
			case <-cleanupCh:
				return _fmt.Errorf("cleanup timeout exceeded")
			// second SIGINT received
			case <-sigCh:
				logger.Println("exiting mage")
				return _fmt.Errorf("exit forced")
			}
		case <-ctx.Done():
			cancel()
			e := ctx.Err()
			_fmt.Printf("ctx err: %v\n", e)
			return e
		case err = <-d:
			// we intentionally don't cancel the context here, because
			// the next target will need to run with the same _context.
			return err
		}
	}
	// This is necessary in case there aren't any targets, to avoid an unused
	// variable error.
	_ = runTarget

	handleError := func(logger *_log.Logger, err interface{}) {
		if err != nil {
			logger.Printf("Error: %+v\n", err)
			type code interface {
				ExitStatus() int
			}
			if c, ok := err.(code); ok {
				_os.Exit(c.ExitStatus())
			}
			_os.Exit(1)
		}
	}
	_ = handleError

	// Set MAGEFILE_VERBOSE so mg.Verbose() reflects the flag value.
	if args.Verbose {
		_os.Setenv("MAGEFILE_VERBOSE", "1")
	} else {
		_os.Setenv("MAGEFILE_VERBOSE", "0")
	}

	_log.SetFlags(0)
	if !args.Verbose {
		_log.SetOutput(_io.Discard)
	}
	logger := _log.New(_os.Stderr, "", 0)
	if args.List {
		if err := list(); err != nil {
			_log.Println(err)
			_os.Exit(1)
		}
		return
	}

	if args.Help {
		if len(args.Args) < 1 {
			logger.Println("no target specified")
			_os.Exit(2)
		}
		switch _strings.ToLower(args.Args[0]) {
			{{range .Funcs -}}
			case "{{lower .TargetName}}":
				{{if ne .Comment "" -}}
				_fmt.Println({{printf "%q" .Comment}})
				_fmt.Println()
				{{end}}
				_fmt.Print("Usage:\n\n\t{{$.BinaryName}} {{lower .TargetName}}{{range .RequiredArgs}} <{{.Name}}>{{end}}{{if .MultipleOptionalArgs}} [<flags>]{{else}}{{range .OptionalArgs}} [-{{.Name}}=<{{.Type}}>]{{end}}{{end}}\n\n")
				{{if .ShowFlagDocs}}_fmt.Print({{printf "%q" .FlagDocsString}})
				{{end -}}
				var aliases []string
				{{- $name := .Name -}}
				{{- $recv := .Receiver -}}
				{{range $alias, $func := $.Aliases}}
				{{if and (eq $name $func.Name) (eq $recv $func.Receiver)}}aliases = append(aliases, "{{$alias}}"){{end -}}
				{{- end}}
				if len(aliases) > 0 {
					_sort.Strings(aliases)
					_fmt.Printf("Aliases: %s\n\n", _strings.Join(aliases, ", "))
				}
				return
			{{end -}}
			{{range .Imports -}}
				{{range .Info.Funcs -}}
			case "{{lower .TargetName}}":
				{{if ne .Comment "" -}}
				_fmt.Println({{printf "%q" .Comment}})
				_fmt.Println()
				{{end}}
				_fmt.Print("Usage:\n\n\t{{$.BinaryName}} {{lower .TargetName}}{{range .RequiredArgs}} <{{.Name}}>{{end}}{{if .MultipleOptionalArgs}} [<flags>]{{else}}{{range .OptionalArgs}} [-{{.Name}}=<{{.Type}}>]{{end}}{{end}}\n\n")
				{{if .ShowFlagDocs}}_fmt.Print({{printf "%q" .FlagDocsString}})
				{{end -}}
				var aliases []string
				{{- $name := .Name -}}
				{{- $recv := .Receiver -}}
				{{range $alias, $func := $.Aliases}}
				{{if and (eq $name $func.Name) (eq $recv $func.Receiver)}}aliases = append(aliases, "{{$alias}}"){{end -}}
				{{- end}}
				if len(aliases) > 0 {
					_sort.Strings(aliases)
					_fmt.Printf("Aliases: %s\n\n", _strings.Join(aliases, ", "))
				}
				return
				{{end -}}
			{{end -}}
			default:
				logger.Printf("Unknown target: %q\n", args.Args[0])
				_os.Exit(2)
		}
	}
	_mageAdmitAlias := ""
	_mageArgsStart := 0
	_, _ = _mageAdmitAlias, _mageArgsStart
	if len(args.Args) < 1 {
	{{- if .DefaultFunc.Name}}
		ignoreDefault, _ := _strconv.ParseBool(_os.Getenv("MAGEFILE_IGNOREDEFAULT"))
		if ignoreDefault {
			if err := list(); err != nil {
				logger.Println("Error:", err)
				_os.Exit(1)
			}
			return
		}
		x := 0
		{{.DefaultFunc.ExecCode}}
		handleError(logger, ret)
		return
	{{- else}}
		if err := list(); err != nil {
			logger.Println("Error:", err)
			_os.Exit(1)
		}
		return
	{{- end}}
	}
	for x := 0; x < len(args.Args); {
		target := args.Args[x]
		x++

		// resolve aliases
		requestedTarget := _strings.ToLower(target)
		switch _strings.ToLower(target) {
		{{range $alias, $func := .Aliases}}
			case "{{lower $alias}}":
				target = "{{$func.TargetName}}"
		{{- end}}
		}
		// record whether the target was requested through an alias, so the
		// admission policy can match aliases as well as resolved targets.
		if requestedTarget != _strings.ToLower(target) {
			_mageAdmitAlias = requestedTarget
		} else {
			_mageAdmitAlias = ""
		}
		_mageArgsStart = x

		switch _strings.ToLower(target) {
		{{range .Funcs }}
			case "{{lower .TargetName}}":
				expected := x + {{.NumRequiredArgs}}
				if expected > len(args.Args) {
					// note that expected and args at this point include the arg for the target itself
					// so we subtract 1 here to show the number of args without the target.
					logger.Printf("not enough arguments for target \"{{.TargetName}}\", expected %v, got %v\n", expected-1, len(args.Args)-1)
					_os.Exit(2)
				}
				if args.Verbose {
					logger.Println("Running target:", "{{.TargetName}}")
				}
				{{.ExecCode}}
				handleError(logger, ret)
		{{- end}}
		{{range .Imports}}
		{{$imp := .}}
			{{range .Info.Funcs }}
				case "{{lower .TargetName}}":
					expected := x + {{.NumRequiredArgs}}
					if expected > len(args.Args) {
						// note that expected and args at this point include the arg for the target itself
						// so we subtract 1 here to show the number of args without the target.
						logger.Printf("not enough arguments for target \"{{.TargetName}}\", expected %v, got %v\n", expected-1, len(args.Args)-1)
						_os.Exit(2)
					}
					if args.Verbose {
						logger.Println("Running target:", "{{.TargetName}}")
					}
					{{.ExecCode}}
					handleError(logger, ret)
			{{- end}}
		{{- end}}
		default:
			logger.Printf("Unknown target specified: %q\n", target)
			_os.Exit(2)
		}
	}
}




`
