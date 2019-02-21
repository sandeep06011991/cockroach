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

// Package ext defines the extension points for creating new contracts.
package ext

import (
	"context"
	"go/token"

	"golang.org/x/tools/go/ssa"
)

// A Located object is associated with an opaque source location.
// Most types from the ssa package will implement this interface,
// as do all of the underlying AST objects.
type Located interface{ Pos() token.Pos }

// Context defines the interface between a Contract and the supporting
// framework.
type Context interface {
	context.Context

	// Declaration returns the object that the contract declaration is
	// defined on. See additional discussion on the Contract type.
	Declaration() ssa.Member
	// Objects returns a collection of objects that a specific contract
	// declaration maps to. See additional discussion on the Contract
	// type.
	Objects() []ssa.Member
	// Program returns the SSA Program object which is driving the
	// analysis.
	Program() *ssa.Program
	// Report adds an error message to the output that is associated
	// with the given object.
	Report(l Located, msg string)
	// Reportf is a printf-style variant of Report.
	Reportf(l Located, msg string, args ...interface{})
}

// A Contract implements some correctness-checking logic.
//
// Contracts are associated with a specific object by using a
// magic comment of the form
//   contract:SomeContractName
//
// It is acceptable for multiple contracts to be applied to the same
// object.
//
// Additional configuration may be provided to contract instances
// by writing a json object, which will be unmarshalled into an
// instance of the contract struct.
//   contract:ConfigurableContract { "someKey" : "someValue", ... }
//
// The entirety of the json literal must occur within the same comment.
// A multiline configuration can be specified when using the /* comment
// syntax:
//   /*
//     contract:ConfigurableContract {
//       "someKey" : "someValue"
//     }
//   */
//
//
// There is a one-to-one mapping of an instance of a Contract with a
// contract declaration in the underlying source code. The specific
// objects returned from Context.Declaration() and Context.Objects()
// will vary based on where the contract declaration occurs.
//
// In the simplest case, a contract declared directly upon a struct
// type will have a single *ssa.Type upon which the contract is
// enforced.
//   // contract:FooContract
//   type SomeStruct struct { ... }
//   context.Declaration() := SomeStruct
//   context.Objects() := [ SomeStruct ]
//
// Similarly, contract declarations placed upon individual function or
// method declarations will have a singleton *ssa.Function presented.
//   // contract:SomeContract
//   func (r Receiver) MyMethod() { ... }
//   context.Declaration() := MyMethod
//   context.Objects() := [ MyMethod ]
//
// In the case of interfaces, all structs that implement the interface
// and which have a type-asserting assignment will be aggregated. In
// the following example, the objects presented would be *ssa.Type
// instances for both Impl1 and Impl2.  Note that because there is no
// explicit type assertion for NotSeen, it will not be part of the
// collection.
//   // contract:FooContract
//   type SomeIntf interface { ... }
//   var (
//     _ SomeIntf = Impl1{}
//     _ SomeIntf = Impl2{}
//   )
//   type Impl1 struct { ... }
//   type Impl2 struct { ... }
//   type NotSeen struct { ... }
//   context.Declaration() := SomeIntf
//   context.Objects() := [ Impl1, Impl2 ]
//
// Contract declarations placed upon an interface method declaration
// will aggregate all implementing methods and present them as a slice
// of *ssa.Function.  As with the interface case above, only
// (*Impl1).SomeMethod() and (*Impl2).SomeMethod() will be presented,
// because an explicit type assertion for those structs exist.
//   type SomeIntf {
//     // controct:FooContract
//     SomeMethod()
//   }
//   var (
//     _ SomeIntf = &Impl1{}
//     _ SomeIntf = &Impl2{}
//   )
//   func (*Impl1) SomeMethod() { ... }
//   func (*Impl2) SomeMethod() { ... }
//   func (*NotSeen) SomeMethod() { ... }
//   context.Declaration() := (SomeIntf).SomeMethod()
//   context.Objects() := [ (*Impl1).SomeMethod(), (*Impl2).SomeMethod() ]
//
// Reusable contracts may be declared by declaring a type derived from
// Contract.
//   //contract:RetLint { "AllowedTypeNames" : ["github.../pgerror.Error"], "TargetInterface":"error" }
//   type PgErrLinter Contract
//
// TODO: Contracts may be placed on a package
//
// Lastly, failing to abide by a contract results in BigEddie being
// unhappy. You wouldn't want BigEddie to be unhappy, would you?
type Contract interface {
	// Enforce will be called on an instance of the Contract automatically
	// by the runtime.
	Enforce(ctx Context)
}
