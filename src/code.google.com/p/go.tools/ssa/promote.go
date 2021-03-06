// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssa

// This file defines utilities for method-set computation including
// synthesis of wrapper methods.
//
// Wrappers include:
// - indirection/promotion wrappers for methods of embedded fields.
// - interface method wrappers for expressions I.f.
// - bound method wrappers, for uncalled obj.Method closures.

// TODO(adonovan): rename to wrappers.go.

import (
	"fmt"
	"go/token"

	"code.google.com/p/go.tools/go/types"
)

// recvType returns the receiver type of method obj.
func recvType(obj *types.Func) types.Type {
	return obj.Type().(*types.Signature).Recv().Type()
}

// Method returns the Function implementing method meth, building
// wrapper methods on demand.
//
// Thread-safe.
//
// EXCLUSIVE_LOCKS_ACQUIRED(prog.methodsMu)
//
func (prog *Program) Method(meth *types.Selection) *Function {
	if meth == nil {
		panic("Method(nil)")
	}
	typ := meth.Recv()
	if prog.mode&LogSource != 0 {
		defer logStack("Method %s %v", typ, meth)()
	}

	prog.methodsMu.Lock()
	defer prog.methodsMu.Unlock()

	type methodSet map[string]*Function
	mset, _ := prog.methodSets.At(typ).(methodSet)
	if mset == nil {
		mset = make(methodSet)
		prog.methodSets.Set(typ, mset)
	}

	id := meth.Obj().Id()
	fn := mset[id]
	if fn == nil {
		fn = findMethod(prog, meth)
		mset[id] = fn
	}
	return fn
}

// declaredFunc returns the concrete function/method denoted by obj.
// Panic ensues if there is none.
//
func (prog *Program) declaredFunc(obj *types.Func) *Function {
	if v := prog.packageLevelValue(obj); v != nil {
		return v.(*Function)
	}
	panic("no concrete method: " + obj.String())
}

// findMethod returns the concrete Function for the method meth,
// synthesizing wrappers as needed.
//
// EXCLUSIVE_LOCKS_REQUIRED(prog.methodsMu)
//
func findMethod(prog *Program, meth *types.Selection) *Function {
	needsPromotion := len(meth.Index()) > 1
	obj := meth.Obj().(*types.Func)
	needsIndirection := !isPointer(recvType(obj)) && isPointer(meth.Recv())

	if needsPromotion || needsIndirection {
		return makeWrapper(prog, meth.Recv(), meth)
	}

	if _, ok := meth.Recv().Underlying().(*types.Interface); ok {
		return interfaceMethodWrapper(prog, meth.Recv(), obj)
	}

	return prog.declaredFunc(obj)
}

// makeWrapper returns a synthetic wrapper Function that optionally
// performs receiver indirection, implicit field selections and then a
// tailcall of a "promoted" method.  For example, given these decls:
//
//    type A struct {B}
//    type B struct {*C}
//    type C ...
//    func (*C) f()
//
// then makeWrapper(typ=A, obj={Func:(*C).f, Indices=[B,C,f]})
// synthesize this wrapper method:
//
//    func (a A) f() { return a.B.C->f() }
//
// prog is the program to which the synthesized method will belong.
// typ is the receiver type of the wrapper method.  obj is the
// type-checker's object for the promoted method; its Func may be a
// concrete or an interface method.
//
// EXCLUSIVE_LOCKS_REQUIRED(prog.methodsMu)
//
func makeWrapper(prog *Program, typ types.Type, meth *types.Selection) *Function {
	obj := meth.Obj().(*types.Func)
	oldsig := obj.Type().(*types.Signature)
	recv := types.NewVar(token.NoPos, nil, "recv", typ)

	description := fmt.Sprintf("wrapper for %s", obj)
	if prog.mode&LogSource != 0 {
		defer logStack("make %s to (%s)", description, typ)()
	}
	fn := &Function{
		name:      obj.Name(),
		method:    meth,
		Signature: changeRecv(oldsig, recv),
		Synthetic: description,
		Prog:      prog,
		Pkg:       prog.packages[obj.Pkg()],
		pos:       obj.Pos(),
	}
	fn.startBody()
	fn.addSpilledParam(recv)
	createParams(fn)

	var v Value = fn.Locals[0] // spilled receiver
	if isPointer(typ) {
		// TODO(adonovan): consider emitting a nil-pointer check here
		// with a nice error message, like gc does.
		v = emitLoad(fn, v)
	}

	// Invariant: v is a pointer, either
	//   value of *A receiver param, or
	// address of  A spilled receiver.

	// We use pointer arithmetic (FieldAddr possibly followed by
	// Load) in preference to value extraction (Field possibly
	// preceded by Load).

	indices := meth.Index()
	v = emitImplicitSelections(fn, v, indices[:len(indices)-1])

	// Invariant: v is a pointer, either
	//   value of implicit *C field, or
	// address of implicit  C field.

	var c Call
	if _, ok := oldsig.Recv().Type().Underlying().(*types.Interface); !ok { // concrete method
		if !isPointer(oldsig.Recv().Type()) {
			v = emitLoad(fn, v)
		}
		c.Call.Value = prog.declaredFunc(obj)
		c.Call.Args = append(c.Call.Args, v)
	} else {
		c.Call.Method = obj
		c.Call.Value = emitLoad(fn, v)
	}
	for _, arg := range fn.Params[1:] {
		c.Call.Args = append(c.Call.Args, arg)
	}
	emitTailCall(fn, &c)
	fn.finishBody()
	return fn
}

// createParams creates parameters for wrapper method fn based on its
// Signature.Params, which do not include the receiver.
//
func createParams(fn *Function) {
	var last *Parameter
	tparams := fn.Signature.Params()
	for i, n := 0, tparams.Len(); i < n; i++ {
		last = fn.addParamObj(tparams.At(i))
	}
	if fn.Signature.IsVariadic() {
		last.typ = types.NewSlice(last.typ)
	}
}

// Wrappers for standalone interface methods ----------------------------------

// interfaceMethodWrapper returns a synthetic wrapper function
// permitting an abstract method obj to be called like a standalone
// function, e.g.:
//
//   type I interface { f(x int) R }
//   m := I.f  // wrapper
//   var i I
//   m(i, 0)
//
// The wrapper is defined as if by:
//
//   func (i I) f(x int, ...) R {
//     return i.f(x, ...)
//   }
//
// typ is the type of the receiver (I here).  It isn't necessarily
// equal to the recvType(obj) because one interface may embed another.
// TODO(adonovan): more tests.
//
// TODO(adonovan): opt: currently the stub is created even when used
// in call position: I.f(i, 0).  Clearly this is suboptimal.
//
// EXCLUSIVE_LOCKS_REQUIRED(prog.methodsMu)
//
func interfaceMethodWrapper(prog *Program, typ types.Type, obj *types.Func) *Function {
	// If one interface embeds another they'll share the same
	// wrappers for common methods.  This is safe, but it might
	// confuse some tools because of the implicit interface
	// conversion applied to the first argument.  If this becomes
	// a problem, we should include 'typ' in the memoization key.
	fn, ok := prog.ifaceMethodWrappers[obj]
	if !ok {
		description := "interface method wrapper"
		if prog.mode&LogSource != 0 {
			defer logStack("(%s).%s, %s", typ, obj.Name(), description)()
		}
		fn = &Function{
			name:      obj.Name(),
			object:    obj,
			Signature: obj.Type().(*types.Signature),
			Synthetic: description,
			pos:       obj.Pos(),
			Prog:      prog,
			Pkg:       prog.packages[obj.Pkg()],
		}
		fn.startBody()
		fn.addParam("recv", typ, token.NoPos)
		createParams(fn)
		var c Call

		c.Call.Method = obj
		c.Call.Value = fn.Params[0]
		for _, arg := range fn.Params[1:] {
			c.Call.Args = append(c.Call.Args, arg)
		}
		emitTailCall(fn, &c)
		fn.finishBody()

		prog.ifaceMethodWrappers[obj] = fn
	}
	return fn
}

// Wrappers for bound methods -------------------------------------------------

// boundMethodWrapper returns a synthetic wrapper function that
// delegates to a concrete or interface method.
// The wrapper has one free variable, the method's receiver.
// Use MakeClosure with such a wrapper to construct a bound-method
// closure.  e.g.:
//
//   type T int          or:  type T interface { meth() }
//   func (t T) meth()
//   var t T
//   f := t.meth
//   f() // calls t.meth()
//
// f is a closure of a synthetic wrapper defined as if by:
//
//   f := func() { return t.meth() }
//
// EXCLUSIVE_LOCKS_ACQUIRED(meth.Prog.methodsMu)
//
func boundMethodWrapper(prog *Program, obj *types.Func) *Function {
	prog.methodsMu.Lock()
	defer prog.methodsMu.Unlock()
	fn, ok := prog.boundMethodWrappers[obj]
	if !ok {
		description := fmt.Sprintf("bound method wrapper for %s", obj)
		if prog.mode&LogSource != 0 {
			defer logStack("%s", description)()
		}
		fn = &Function{
			name:      "bound$" + obj.FullName(),
			Signature: changeRecv(obj.Type().(*types.Signature), nil), // drop receiver
			Synthetic: description,
			Prog:      prog,
			Pkg:       prog.packages[obj.Pkg()],
			pos:       obj.Pos(),
		}

		cap := &Capture{name: "recv", typ: recvType(obj), parent: fn}
		fn.FreeVars = []*Capture{cap}
		fn.startBody()
		createParams(fn)
		var c Call

		if _, ok := recvType(obj).Underlying().(*types.Interface); !ok { // concrete
			c.Call.Value = prog.declaredFunc(obj)
			c.Call.Args = []Value{cap}
		} else {
			c.Call.Value = cap
			c.Call.Method = obj
		}
		for _, arg := range fn.Params {
			c.Call.Args = append(c.Call.Args, arg)
		}
		emitTailCall(fn, &c)
		fn.finishBody()

		prog.boundMethodWrappers[obj] = fn
	}
	return fn
}

func changeRecv(s *types.Signature, recv *types.Var) *types.Signature {
	return types.NewSignature(nil, recv, s.Params(), s.Results(), s.IsVariadic())
}
