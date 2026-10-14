package mage

// this template uses the "data"

// var only for tests.
var mageMainfileTplString = `//go:build ignore
// +build ignore

package main

import (
	_context "context"
	_encodingjson "encoding/json"
	_flag "flag"
	_fmt "fmt"
	_io "io"
	_log "log"
	_os "os"
	_signal "os/signal"
	_filepath "path/filepath"
	_runtime "runtime"
	_sort "sort"
	_strconv "strconv"
	_strings "strings"
	_sync "sync"
	_syscall "syscall"
	_tabwriter "text/tabwriter"
	_time "time"
	{{range .Imports}}{{.UniqueName}} "{{.Path}}"
	{{end}}
)

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

	// mageTargetID identifies a command line selected target using only
	// stdlib types.
	type mageTargetID struct {
		Name    string
		Display string
	}

	// The generated main stays stdlib-only and emits structured events as the
	// same JSON schema as mg, even for magefiles that do not import mg. The
	// private _root control line additionally publishes the goroutine and
	// execution identity of a root target, so mg can associate context-free
	// Deps calls with it; the mage driver filters control lines out.
	type mageEvent struct {
		Time            _time.Time ` + "`" + `json:"time"` + "`" + `
		Sequence        uint64     ` + "`" + `json:"seq"` + "`" + `
		Kind            string     ` + "`" + `json:"kind"` + "`" + `
		RunID           string     ` + "`" + `json:"run_id"` + "`" + `
		ExecutionID     string     ` + "`" + `json:"execution_id,omitempty"` + "`" + `
		ParentExecution string     ` + "`" + `json:"parent_execution_id,omitempty"` + "`" + `
		Target          struct {
			Name    string ` + "`" + `json:"name"` + "`" + `
			ID      string ` + "`" + `json:"id,omitempty"` + "`" + `
			Display string ` + "`" + `json:"display,omitempty"` + "`" + `
		} ` + "`" + `json:"target"` + "`" + `
		Error     string ` + "`" + `json:"error,omitempty"` + "`" + `
		ExitCode  int    ` + "`" + `json:"exit_code,omitempty"` + "`" + `
		Reason    string ` + "`" + `json:"reason,omitempty"` + "`" + `
		Goroutine int64  ` + "`" + `json:"goroutine,omitempty"` + "`" + `
	}
	var (
		mageEventFile  *_os.File
		mageEventMu    _sync.Mutex
		mageEventSeq   uint64
		mageEventRunID string
		mageRootCount  uint64
	)
	mageEventInit := func() {
		path := _os.Getenv("MAGEFILE_EVENT_LOG")
		if path == "" {
			return
		}
		mageEventRunID = _os.Getenv("MAGEFILE_RUN_ID")
		if mageEventRunID == "" {
			mageEventRunID = _strconv.FormatInt(_time.Now().UnixNano(), 16) + "-" + _strconv.Itoa(_os.Getpid())
		}
		if file, err := _os.OpenFile(path, _os.O_CREATE|_os.O_WRONLY|_os.O_APPEND, 0o600); err == nil {
			mageEventFile = file
		}
	}
	mageEventWrite := func(event mageEvent) {
		if mageEventFile == nil {
			return
		}
		mageEventMu.Lock()
		defer mageEventMu.Unlock()
		if mageEventFile == nil {
			return
		}
		mageEventSeq++
		event.Sequence = mageEventSeq
		event.Time = _time.Now()
		event.RunID = mageEventRunID
		line, err := _encodingjson.Marshal(event)
		if err != nil {
			return
		}
		line = append(line, '\n')
		if _, err := mageEventFile.Write(line); err != nil {
			_ = mageEventFile.Close()
			mageEventFile = nil
		}
	}
	mageEventExitCode := func(value interface{}) int {
		type coded interface{ ExitStatus() int }
		if code, ok := value.(coded); ok {
			return code.ExitStatus()
		}
		return 1
	}
	mageEventFor := func(kind, execution string, target mageTargetID) mageEvent {
		event := mageEvent{Kind: kind, ExecutionID: execution}
		event.Target.Name = target.Name
		event.Target.Display = target.Display
		return event
	}
	mageGoroutineID := func() int64 {
		var buf [64]byte
		n := _runtime.Stack(buf[:], false)
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
		id, _ := _strconv.ParseInt(line[:end], 10, 64)
		return id
	}
	mageObserveTarget := func(ctx _context.Context, target mageTargetID, fn func(_context.Context) error) (err error) {
		mageEventMu.Lock()
		mageRootCount++
		execution := "root-" + _strconv.FormatUint(mageRootCount, 10)
		mageEventMu.Unlock()
		control := mageEventFor("_root", execution, target)
		control.Goroutine = mageGoroutineID()
		mageEventWrite(control)
		mageEventWrite(mageEventFor("target_selected", execution, target))
		mageEventWrite(mageEventFor("target_started", execution, target))
		func() {
			defer func() {
				if value := recover(); value != nil {
					failure := mageEventFor("target_failed", execution, target)
					failure.Error = _fmt.Sprint(value)
					failure.ExitCode = mageEventExitCode(value)
					mageEventWrite(failure)
					panic(value)
				}
			}()
			err = fn(ctx)
		}()
		if err != nil {
			failure := mageEventFor("target_failed", execution, target)
			failure.Error = err.Error()
			failure.ExitCode = mageEventExitCode(err)
			mageEventWrite(failure)
			return err
		}
		mageEventWrite(mageEventFor("target_completed", execution, target))
		return nil
	}
	mageEmitInterrupt := func(target mageTargetID, cause error) {
		event := mageEventFor("canceled", "", target)
		if cause != nil {
			if cause == _context.DeadlineExceeded {
				event.Kind = "timed_out"
			}
			event.Error = cause.Error()
		}
		mageEventWrite(event)
	}
	mageEmitSelectionError := func(name string, cause error) {
		event := mageEventFor("target_selected", "", mageTargetID{Name: name, Display: name})
		event.Reason = "unknown_target"
		if cause != nil {
			event.Error = cause.Error()
		}
		mageEventWrite(event)
	}
	mageEventInit()
	eventCtx := _context.Background()
	closeEventLog := func() {
		mageEventMu.Lock()
		defer mageEventMu.Unlock()
		if mageEventFile != nil {
			_ = mageEventFile.Close()
			mageEventFile = nil
		}
	}
	exit := func(code int) {
		mageEventWrite(mageEvent{Kind: "exited", ExitCode: code})
		closeEventLog()
		_os.Exit(code)
	}
	defer closeEventLog()

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
		exit(0)
	}
	args.Args = fs.Args()
	if args.Help && len(args.Args) == 0 {
		fs.Usage()
		exit(0)
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
				ctx, ctxCancel = _context.WithTimeout(eventCtx, args.Timeout)
			} else {
				ctx, ctxCancel = _context.WithCancel(eventCtx)
			}
		}

		return ctx, ctxCancel
	}

	runTarget := func(logger *_log.Logger, target mageTargetID, fn func(_context.Context) error) interface{} {
		var err interface{}
		ctx, cancel := getContext()
		d := make(chan interface{})
		go func() {
			defer func() {
				err := recover()
				d <- err
			}()
			d <- mageObserveTarget(ctx, target, fn)
		}()
		sigCh := make(chan _os.Signal, 1)
		_signal.Notify(sigCh, _syscall.SIGINT)
		select {
		case <-sigCh:
			logger.Println("cancelling mage targets, waiting up to 5 seconds for cleanup...")
			mageEmitInterrupt(target, _context.Canceled)
			cancel()
			cleanupCh := _time.After(5 * _time.Second)

			select {
			// target exited by itself
			case err = <-d:
				return err
			// cleanup timeout exceeded
			case <-cleanupCh:
				cleanupErr := _fmt.Errorf("cleanup timeout exceeded")
				mageEmitInterrupt(target, cleanupErr)
				return cleanupErr
			// second SIGINT received
			case <-sigCh:
				logger.Println("exiting mage")
				forcedErr := _fmt.Errorf("exit forced")
				mageEmitInterrupt(target, forcedErr)
				return forcedErr
			}
		case <-ctx.Done():
			cancel()
			e := ctx.Err()
			mageEmitInterrupt(target, e)
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
				exit(c.ExitStatus())
			}
			exit(1)
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
			exit(1)
		}
		exit(0)
	}

	if args.Help {
		if len(args.Args) < 1 {
			logger.Println("no target specified")
			exit(2)
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
				exit(0)
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
				exit(0)
				{{end -}}
			{{end -}}
			default:
				logger.Printf("Unknown target: %q\n", args.Args[0])
				mageEmitSelectionError(args.Args[0], _fmt.Errorf("unknown target: %q", args.Args[0]))
				exit(2)
		}
	}
	if len(args.Args) < 1 {
	{{- if .DefaultFunc.Name}}
		ignoreDefault, _ := _strconv.ParseBool(_os.Getenv("MAGEFILE_IGNOREDEFAULT"))
		if ignoreDefault {
			if err := list(); err != nil {
				logger.Println("Error:", err)
				exit(1)
			}
			exit(0)
		}
		{{.DefaultFunc.ExecCode}}
		handleError(logger, ret)
		exit(0)
	{{- else}}
		if err := list(); err != nil {
			logger.Println("Error:", err)
			exit(1)
		}
		exit(0)
	{{- end}}
	}
	for x := 0; x < len(args.Args); {
		target := args.Args[x]
		x++

		// resolve aliases
		switch _strings.ToLower(target) {
		{{range $alias, $func := .Aliases}}
			case "{{lower $alias}}":
				target = "{{$func.TargetName}}"
		{{- end}}
		}

		switch _strings.ToLower(target) {
		{{range .Funcs }}
			case "{{lower .TargetName}}":
				expected := x + {{.NumRequiredArgs}}
				if expected > len(args.Args) {
					// note that expected and args at this point include the arg for the target itself
					// so we subtract 1 here to show the number of args without the target.
					logger.Printf("not enough arguments for target \"{{.TargetName}}\", expected %v, got %v\n", expected-1, len(args.Args)-1)
					exit(2)
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
						exit(2)
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
			mageEmitSelectionError(target, _fmt.Errorf("unknown target specified: %q", target))
			exit(2)
		}
	}
	exit(0)
}




`
