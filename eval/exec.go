package eval

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"github.com/elves/elvish/errutil"
)

const (
	// FdNil is a special impossible fd value. Used for "close fd" in
	// syscall.ProcAttr.Files.
	fdNil uintptr = ^uintptr(0)
)

var (
	arityMismatch = errors.New("arity mismatch")
	cdNoArg       = errors.New("implicit cd accepts no arguments")
	evalFailure   = errors.New("generic eval failure")
)

func maybeThrow(err error) {
	if err != nil {
		throw(err)
	}
}

func (ec *evalCtx) pcall(f Caller, args []Value) (ex error) {
	defer errutil.Catch(&ex)
	f.Call(ec, args)
	return nil
}

func (ec *evalCtx) resolveNonSpecial(cmd Value) Caller {
	// Closure
	if cl, ok := cmd.(Caller); ok {
		return cl
	}

	cmdStr := ToString(cmd)

	// Defined callable
	splice, ns, name := parseVariable(cmdStr)
	if !splice {
		if v := ec.ResolveVar(ns, FnPrefix+name); v != nil {
			if clb, ok := v.Get().(Caller); ok {
				return clb
			}
		}
	}

	// External command
	return ExternalCmd{cmdStr}
}

// Call calls a builtin function.
func (b *BuiltinFn) Call(ec *evalCtx, args []Value) {
	b.Impl(ec, args)
}

// Call calls a closure.
func (c *Closure) Call(ec *evalCtx, args []Value) {
	// TODO Support optional/rest argument
	// TODO Support keyword arguments
	if !c.Variadic && len(args) != len(c.ArgNames) {
		throw(arityMismatch)
	}

	// This evalCtx is dedicated to the current form, so we modify it in place.
	// BUG(xiaq): When evaluating closures, async access to global variables
	// and ports can be problematic.

	// Make upvalue namespace and capture variables.
	ec.up = make(map[string]Variable)
	for name, variable := range c.Captured {
		ec.up[name] = variable
	}
	// Make local namespace and pass arguments.
	ec.local = make(map[string]Variable)
	if !c.Variadic {
		for i, name := range c.ArgNames {
			ec.local[name] = newPtrVariable(args[i])
		}
	}
	ec.local["args"] = newPtrVariable(List{&args})
	ec.local["kwargs"] = newPtrVariable(Map{&map[Value]Value{}})

	// TODO(xiaq): Also change ec.name and ec.text since the closure being
	// called can come from another source.

	defer ec.closePorts()
	c.Op(ec)
}

// waitStatusToError converts syscall.WaitStatus to an Error.
func waitStatusToError(ws syscall.WaitStatus) error {
	switch {
	case ws.Exited():
		es := ws.ExitStatus()
		if es == 0 {
			return nil
		}
		return errors.New(fmt.Sprint(es))
	case ws.Signaled():
		msg := fmt.Sprintf("signaled %v", ws.Signal())
		if ws.CoreDump() {
			msg += " (core dumped)"
		}
		return errors.New(msg)
	case ws.Stopped():
		msg := fmt.Sprintf("stopped %v", ws.StopSignal())
		trap := ws.TrapCause()
		if trap != -1 {
			msg += fmt.Sprintf(" (trapped %v)", trap)
		}
		return errors.New(msg)
	/*
		case ws.Continued():
			return newUnexitedStateUpdate("continued")
	*/
	default:
		return fmt.Errorf("unknown WaitStatus", ws)
	}
}

// Call calls an external command.
func (e ExternalCmd) Call(ec *evalCtx, argVals []Value) {
	if DontSearch(e.Name) {
		stat, err := os.Stat(e.Name)
		if err == nil && stat.IsDir() {
			// implicit cd
			cdInner(e.Name, ec)
			return
		}
	}

	files := make([]uintptr, len(ec.ports))
	for i, port := range ec.ports {
		if port == nil || port.f == nil {
			files[i] = fdNil
		} else {
			files[i] = port.f.Fd()
		}
	}

	args := make([]string, len(argVals)+1)
	for i, a := range argVals {
		// NOTE Maybe we should enfore string arguments instead of coercing all
		// args into string
		args[i+1] = ToString(a)
	}

	sys := syscall.SysProcAttr{}
	attr := syscall.ProcAttr{Env: os.Environ(), Files: files[:], Sys: &sys}

	path, err := ec.Search(e.Name)
	if err != nil {
		throw(errors.New("search: " + err.Error()))
	}

	args[0] = path
	pid, err := syscall.ForkExec(path, args, &attr)
	if err != nil {
		throw(errors.New("forkExec: " + err.Error()))
	}

	var ws syscall.WaitStatus
	_, err = syscall.Wait4(pid, &ws, 0, nil)
	if err != nil {
		throw(fmt.Errorf("wait:", err.Error()))
	} else {
		maybeThrow(waitStatusToError(ws))
	}
}

func (t *List) Call(ec *evalCtx, argVals []Value) {
	var v Value = t
	for _, idx := range argVals {
		// XXX the positions are obviously wrong.
		v = evalIndex(ec, v, idx, 0, 0)
	}
	ec.ports[1].ch <- v
}

// XXX duplicate
func (t Map) Call(ec *evalCtx, argVals []Value) {
	var v Value = t
	for _, idx := range argVals {
		// XXX the positions are obviously wrong.
		v = evalIndex(ec, v, idx, 0, 0)
	}
	ec.ports[1].ch <- v
}
