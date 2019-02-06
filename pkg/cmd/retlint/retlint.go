// Copyright 2019 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"path"

	"github.com/pkg/errors"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

type DirtyFunction interface {
	// Fn returns the dirty function.
	Fn() *ssa.Function
	// String returns a user-consumable representation of why the function
	// is dirty.
	String() string
	// Why returns a value-chain that describes why the function is
	// marked as dirty.
	Why() []DirtyReason
}

type DirtyReason struct {
	Reason string
	Value  ssa.Value
}

func because(value ssa.Value, reason string, args ...interface{}) []DirtyReason {
	return []DirtyReason{{fmt.Sprintf(reason, args...), value}}
}

type RetLint struct {
	AllowedNames []string
	// Override the current working directory.
	Dir string
	// The names of the packages whose exported functions will be
	Packages []string

	// The name of the target interface. This can be an unqualified name
	// like "error", which will be resolved against golang's "Universe"
	// scope, or something like "github.com/myproject/mypkg/SomeType".
	TargetName string

	// The acceptable types which implement the target interface.
	allowed map[*types.Named]bool
	stats   map[*ssa.Function]*funcStat
	// The interfaces that we trigger the behavior on.
	target *types.Named
	work   []*funcStat
}

// Execute performs the analysis and returns the dirty functions which
// match the configured predicate (if any).
func (l *RetLint) Execute() ([]DirtyFunction, error) {
	if l.TargetName == "" {
		return nil, errors.New("no target interface name set")
	}
	l.allowed = make(map[*types.Named]bool)
	l.stats = make(map[*ssa.Function]*funcStat)

	cfg := &packages.Config{
		Dir:  l.Dir,
		Mode: packages.LoadAllSyntax,
	}
	pkgs, err := packages.Load(cfg, l.Packages...)
	if err != nil {
		return nil, err
	}

	pgm, sPkgs := ssautil.AllPackages(pkgs, 0 /* mode flags */)
	pgm.Build()

	// Resolve the input types names.
	if found, err := resolve(pgm, l.TargetName); err == nil {
		l.target = found
	} else {
		return nil, err
	}

	for _, allowed := range l.AllowedNames {
		if found, err := resolve(pgm, allowed); err == nil {
			l.allowed[found] = true
		} else {
			return nil, err
		}
	}

	sPkgMap := make(map[*ssa.Package]bool)

	// Bootstrap the work to perform.
	for _, pkg := range sPkgs {
		sPkgMap[pkg] = true
		for _, m := range pkg.Members {
			switch t := m.(type) {
			case *ssa.Function:
				// Top-level function declarations.
				l.stat(t)
			case *ssa.Type:
				// Methods defined with value receivers.
				methods := pgm.MethodSets.MethodSet(t.Type())
				for i := 0; i < methods.Len(); i++ {
					if fn := pgm.MethodValue(methods.At(i)); fn != nil {
						l.stat(fn)
					}
				}
				// Methods defined with pointer receivers.
				methods = pgm.MethodSets.MethodSet(types.NewPointer(t.Type()))
				for i := 0; i < methods.Len(); i++ {
					if fn := pgm.MethodValue(methods.At(i)); fn != nil {
						l.stat(fn)
					}
				}
			}
		}
	}

	// Loop until we haven't added any new functions.
	for l.work != nil {
		work := l.work
		l.work = nil
		for _, stat := range work {
			l.analyze(stat)
		}
	}

	// Any functions not dirty by now are clean.
	for _, stat := range l.stats {
		if stat.state == stateAnalyzing {
			stat.state = stateClean
		}
	}

	// Returns dirty functions declared in the input package(s).
	var ret []DirtyFunction
	for _, s := range l.stats {
		if s.state == stateDirty && ast.IsExported(s.fn.Name()) && sPkgMap[s.fn.Pkg] {
			ret = append(ret, s)
		}
	}
	return ret, nil
}

// analyze begins the analysis process for a function.  This function
// is a no-op if it has already been called on the stat.
func (l *RetLint) analyze(stat *funcStat) {
	if stat.state != stateUnknown {
		return
	}
	//
	defer func() {
		x := recover()
		if x == nil {
			return
		}
		if err, ok := x.(error); ok {
			panic(errors.Wrap(err, stat.fn.RelString(stat.fn.Pkg.Pkg)))
		}
		panic(errors.Errorf("%s: %v", stat.fn.Name(), x))
	}()
	stat.state = stateAnalyzing
	seen := make(map[ssa.Value]bool)
	for _, ret := range stat.returns {
		for _, idx := range stat.targetIndexes {
			l.decide(stat, ret.Results[idx], seen)

			if stat.state != stateAnalyzing {
				return
			}
		}
	}
}

// decide will mark the given function as dirty if the type of the given
// value is not statically-resolvable to one of the desired concrete types.
func (l *RetLint) decide(stat *funcStat, val ssa.Value, seen map[ssa.Value]bool) {
	if seen[val] {
		return
	}
	seen[val] = true
	switch t := val.(type) {
	case *ssa.Call:
		if callee := t.Call.StaticCallee(); callee != nil {
			next := l.stat(callee)
			l.analyze(next)
			switch next.state {
			case stateClean:
			// Already proven to be clean, ignore.
			case stateDirty:
				// Already proven to be dirty, propagate reason.
				why := make([]DirtyReason, len(next.why)+1)
				why[0] = DirtyReason{"calls", t}
				copy(why[1:], next.why)
				l.markDirty(stat, why)
			default:
				// Mark for future dirtying.
				next.dirties[stat] = t
			}
		} else {
			l.markDirty(stat, because(t, "callee not static"))
		}

	case *ssa.Const:
		// We want to ignore nil values
		if !t.IsNil() && !l.isAllowed(t.Type()) {
			l.markDirty(stat, because(t, "constant of type %q", t.Type()))
		}

	case *ssa.Extract:
		// This is how a (comma,ok) expression or multiple-return call
		// is unpacked.
		l.decide(stat, t.Tuple, seen)

	case *ssa.MakeInterface:
		// A value is being wrapped as an interface.
		l.decide(stat, t.X, seen)

	case *ssa.Phi:
		// A Phi ("phony") value represents the convergence of multiple
		// flows after a branch.  For example:
		//   var a Foo
		//   if condition {
		//     a = someFunc()
		//   } else {
		//     a = otherFunc()
		//   }
		//   doSomethingWith(a)
		//
		// The SSA of the above might look something like:
		//   Call(doSomethingWith, Phi(Call(someFunc), Call(otherFunc)))
		for _, edge := range t.Edges {
			l.decide(stat, edge, seen)
		}

	case *ssa.TypeAssert:
		// x, ok := y.(*Something)
		if !l.isAllowed(t.AssertedType) {
			l.markDirty(stat, because(t, "assertion to %q", t.AssertedType))
		}

	case *ssa.UnOp:
		// This is a dereference operation.
		if t.Op == token.MUL {
			l.decide(stat, t.X, seen)
		}

	default:
		// Otherwise, see if the type is one of our named types or a pointer
		if !l.isAllowed(t.Type()) {
			l.markDirty(stat, because(t, "result of disallowed type %q", t.Type()))
		}
	}
}

// In the first pass, we'll extract all functions in the package.
func (l *RetLint) extract(fn *ssa.Function) {
	// Determine if the function returns a value of the target type.
	results := fn.Signature.Results()
	if results == nil {
		l.stats[fn] = clean
		return
	}

	var targetIndexes []int
	for i, j := 0, results.Len(); i < j; i++ {
		if named, ok := results.At(i).Type().(*types.Named); ok {
			if named == l.target {
				targetIndexes = append(targetIndexes, i)
			}
		}
	}
	if targetIndexes == nil {
		l.stats[fn] = clean
		return
	}

	// Extract all return statements from the function.
	var returns []*ssa.Return
	for _, block := range fn.Blocks {
		for _, inst := range block.Instrs {
			if ret, ok := inst.(*ssa.Return); ok {
				returns = append(returns, ret)
			}
		}
	}

	stat := l.stat(fn)
	stat.returns = returns
	stat.targetIndexes = targetIndexes
}

func (l *RetLint) isAllowed(lookAt types.Type) bool {
	for {
		switch typ := lookAt.(type) {
		case *types.Pointer:
			lookAt = typ.Elem()
		case *types.Named:
			return l.allowed[typ]
		case *types.Tuple:
			panic("should not see a tuple type; unpack in extract()")
		default:
			return false
		}
	}
}

// markDirty will mark the given function as dirty and propagate
// the reason to nodes which depend on this function.
func (l *RetLint) markDirty(stat *funcStat, why []DirtyReason) {
	var changed bool
	// Try to choose a shorter explanation, if we can.
	if stat.why == nil || len(why) < len(stat.why) {
		stat.why = why
		changed = true
	}
	if stat.state == stateDirty && !changed {
		return
	}
	stat.state = stateDirty

	for chained, call := range stat.dirties {
		nextWhy := make([]DirtyReason, len(why)+1)
		nextWhy[0] = DirtyReason{"calls", call}
		copy(nextWhy[1:], why)
		l.markDirty(chained, nextWhy)
	}
}

// resolve looks up a named type from within the collection of packages
func resolve(pgm *ssa.Program, typeName string) (*types.Named, error) {
	tgtPath, tgtName := path.Split(typeName)
	var found types.Type
	if tgtPath == "" {
		tgtObject := types.Universe.Lookup(tgtName)
		if tgtObject != nil {
			found = tgtObject.Type()
		}
		found = tgtObject.Type()
	} else {
		tgtPath = tgtPath[:len(tgtPath)-1]
		for _, pkg := range pgm.AllPackages() {
			if pkg.Pkg.Path() == tgtPath {
				if ptr := pkg.Type(tgtName); ptr != nil {
					found = ptr.Type()
				}
				break
			}
		}
	}
	if found == nil {
		return nil, fmt.Errorf("unable to find type %q", typeName)
	}
	if named, ok := found.(*types.Named); ok {
		return named, nil
	} else {
		return nil, fmt.Errorf("%q was not a named type", tgtName)
	}
}

// stat creates a memoized funcStat to hold extracted information about
// the provided function.
func (l *RetLint) stat(fn *ssa.Function) *funcStat {
	ret := l.stats[fn]
	if ret == nil {
		ret = &funcStat{
			dirties: make(map[*funcStat]*ssa.Call),
			fn:      fn,
		}
		l.stats[fn] = ret
		l.work = append(l.work, ret)
		l.extract(fn)
	}
	return ret
}
