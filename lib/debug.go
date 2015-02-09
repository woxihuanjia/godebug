package godebug

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"

	"github.com/jtolds/gls"
)

// Scope represents a lexical scope for variable bindings.
type Scope struct {
	vars   map[string]interface{}
	parent *Scope
}

// EnteringNewScope returns a new Scope and internally sets
// the current scope to be the returned scope.
func EnteringNewScope() *Scope {
	return &Scope{vars: make(map[string]interface{})}
}

// EnteringNewChildScope returns a new Scope that is the
// child of s and internally sets the current scope to be
// the returned scope.
func (s *Scope) EnteringNewChildScope() *Scope {
	return &Scope{
		vars:   make(map[string]interface{}),
		parent: s,
	}
}

func (s *Scope) getVar(name string) (i interface{}, ok bool) {
	for scope := s; scope != nil; scope = scope.parent {
		if i, ok = scope.vars[name]; ok {
			return i, true
		}
	}
	return nil, false
}

// Declare creates new variable bindings in s from a list of name, value pairs.
// The values should be pointers to the values in the program rather than copies
// of them so that s can track changes to them.
func (s *Scope) Declare(namevalue ...interface{}) {
	var i int
	for i = 0; i+1 < len(namevalue); i += 2 {
		name, ok := namevalue[i].(string)
		if !ok {
			panic("programming error: got odd-numbered argument to RecordVars that was not a string")
		}
		s.vars[name] = namevalue[i+1]
	}
	if i != len(namevalue) {
		panic("programming error: called RecordVars with odd number of arguments")
	}
}

const (
	run int32 = iota
	next
	step
)

var (
	currentState     int32
	currentDepth     int
	debuggerDepth    int
	context          = getPreferredContextManager()
	goroutineKey     = gls.GenSym()
	currentGoroutine uint32
	ids              idPool
)

// EnterFunc marks the beginning of a function. Calling fn should be equivalent to running
// the function that is being entered. If proceed is false, EnterFunc did in fact call
// fn, and so the caller of EnterFunc should return immediately rather than proceed to
// duplicate the effects of fn.
func EnterFunc(fn func()) (ctx *Context, proceed bool) {
	// We've entered a new function. If we're in step or next mode we have some bookkeeping to do,
	// but only if the current goroutine is the one the debugger is following.
	//
	// We consult context to determine whether we are the goroutine the debugger is following. If
	// context has not seen our goroutine before, the ok it returns is false. Why would that happen?
	// godebug supports generating debug code for a library that is later built into a binary. If that
	// happens, then context will not see any goroutines until they call code from the debugged library.
	// context will also not see any goroutines while the debugger is in state run.
	val, ok := context.GetValue(goroutineKey)
	if !ok {
		// This is the first time context has seen the current goroutine.
		//
		// Or, more accurately and precisely: This is the first frame in the current stack that contains
		// code that has been generated by godebug.
		//
		// We record some bookkeeping information with context and then continue running. This means we will
		// invoke fn, which means the caller should not proceed. After running it, return false.
		id := uint32(ids.Acquire())
		defer ids.Release(uint(id))
		context.SetValues(gls.Values{goroutineKey: id}, fn)
		return nil, false
	}
	if val.(uint32) == atomic.LoadUint32(&currentGoroutine) && currentState != run {
		currentDepth++
	}
	return &Context{goroutine: val.(uint32)}, true
}

// EnterFuncLit is like EnterFunc, but intended for function literals. The passed callback takes a *Context rather than no input.
func EnterFuncLit(fn func(*Context)) (ctx *Context, proceed bool) {
	val, ok := context.GetValue(goroutineKey)
	if !ok {
		id := uint32(ids.Acquire())
		defer ids.Release(uint(id))
		context.SetValues(gls.Values{goroutineKey: id}, func() {
			fn(&Context{goroutine: id})
		})
		return nil, false
	}
	if val.(uint32) == atomic.LoadUint32(&currentGoroutine) && currentState != run {
		currentDepth++
	}
	return &Context{goroutine: val.(uint32)}, true
}

// ExitFunc marks the end of a function.
func ExitFunc() {
	if atomic.LoadInt32(&currentState) == run {
		return
	}
	val, ok := context.GetValue(goroutineKey)
	if !ok {
		panic("Logic error in the debugger. Sorry! Let me know about this in the github issue tracker.")
	}
	if val.(uint32) != atomic.LoadUint32(&currentGoroutine) {
		return
	}
	if currentState == next && currentDepth == debuggerDepth {
		debuggerDepth--
	}
	currentDepth--
}

// Context contains debugging context information.
type Context struct {
	goroutine uint32
}

// Line marks a normal line where the debugger might pause.
func Line(c *Context, s *Scope) {
	if atomic.LoadUint32(&currentGoroutine) != c.goroutine {
		return
	}
	if currentState == run || (currentState == next && currentDepth != debuggerDepth) {
		return
	}
	debuggerDepth = currentDepth
	printLine()
	waitForInput(s)
}

var skipNextElseIfExpr bool

// ElseIfSimpleStmt marks a simple statement preceding an "else if" expression.
func ElseIfSimpleStmt(c *Context, s *Scope, line string) {
	SLine(c, s, line)
	if currentState == next {
		skipNextElseIfExpr = true
	}
}

// ElseIfExpr marks an "else if" expression.
func ElseIfExpr(c *Context, s *Scope, line string) {
	if skipNextElseIfExpr {
		skipNextElseIfExpr = false
		return
	}
	SLine(c, s, line)
}

// SLine is like Line, except that the debugger should print the provided line rather than
// reading the next line from the source code.
func SLine(c *Context, s *Scope, line string) {
	if currentState == run || (currentState == next && currentDepth != debuggerDepth) {
		return
	}
	debuggerDepth = currentDepth
	fmt.Println("->", line)
	waitForInput(s)
}

// SetTrace is the entrypoint to the debugger. The code generator converts
// this call to a call to SetTraceGen.
func SetTrace() {
}

// SetTraceGen is the generated entrypoint to the debugger.
func SetTraceGen(ctx *Context) {
	if atomic.LoadInt32(&currentState) != run {
		return
	}
	atomic.StoreUint32(&currentGoroutine, ctx.goroutine)
	currentState = step
}

var input *bufio.Scanner

func init() {
	input = bufio.NewScanner(os.Stdin)
}

func waitForInput(scope *Scope) {
	for {
		fmt.Print("(godebug) ")
		if !input.Scan() {
			fmt.Println("quitting session")
			currentState = run
			return
		}
		s := input.Text()
		switch s {
		case "n", "next":
			currentState = next
			return
		case "s", "step":
			currentState = step
			return
		}
		if v, ok := scope.getVar(strings.TrimSpace(s)); ok {
			fmt.Println(dereference(v))
			continue
		}
		var cmd, name string
		n, _ := fmt.Sscan(s, &cmd, &name)
		if n == 2 && (cmd == "p" || cmd == "print") {
			if v, ok := scope.getVar(strings.TrimSpace(name)); ok {
				fmt.Println(dereference(v))
				continue
			}
		}
		fmt.Printf("Command not recognized, sorry! You typed: %q\n", s)
	}
}

func dereference(i interface{}) interface{} {
	return reflect.ValueOf(i).Elem().Interface()
}

func printLine() {
	_, file, line, ok := runtime.Caller(2)
	if !ok {
		fmt.Println("Hmm, something is broken. Failed to identify current source line.")
		return
	}
	printLineFromFile(line, file)
}

var parsedFiles map[string][]string

func init() {
	parsedFiles = make(map[string][]string)
}

func printLineFromFile(line int, file string) {
	f, ok := parsedFiles[file]
	if !ok {
		f = parseFile(file)
		parsedFiles[file] = f
	}
	if line >= len(f) {
		fmt.Printf("Hmm, something is broken. Current source line = %v, current file = %v, length of file = %v\n", line, file, len(f))
		return
	}
	fmt.Println("->", f[line])
}

func parseFile(file string) []string {
	f, err := os.Open(file)
	if err != nil {
		fmt.Println("Failed to open current source file:", err)
		return nil
	}
	var lines []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := string(bytes.TrimSpace(scanner.Bytes()))
		line = strings.Replace(line, "<-(<-_godebug_recover_chan_)", "recover()", -1)
		lines = append(lines, line)
	}
	if err = scanner.Err(); err != nil {
		fmt.Println("Error reading current source file:", err)
	}
	return lines
}
