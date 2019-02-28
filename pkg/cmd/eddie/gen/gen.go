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

// Package gen contains the code-generator.
package gen

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/token"
	"go/types"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"text/template"

	"github.com/pkg/errors"
	"golang.org/x/tools/go/packages"
)

// Eddie generates a contract-enforcer binary.  See discussion on the
// public API for details on the patterns that it looks for.
type Eddie struct {
	BuildFlags []string
	Dir        string
	KeepTemp   bool
	Logger     *log.Logger
	Name       string
	Outfile    string
	Packages   []string

	contracts []types.Object
	extPkg    string
	rtPkg     string

	// For testing only, causes the compiled enforcer to be emitted
	// as a golang plugin.
	Plugin bool
}

func (e *Eddie) Execute() error {
	if e.Logger == nil {
		e.Logger = log.New(os.Stdout, "", 0 /* no flags */)
	}

	if e.Name == "" {
		return errors.New("no name was set for the output binary")
	}

	if e.Outfile == "" {
		e.Outfile = e.Name
	}

	// Look up the package name using reflection to prevent any weirdness
	// if the code gets moved to a new package.
	myPkg := reflect.TypeOf(Eddie{}).PkgPath()
	myPkg = path.Dir(myPkg)
	e.extPkg = path.Join(myPkg, "ext")
	e.rtPkg = path.Join(myPkg, "rt")

	if err := e.findContracts(); err != nil {
		return err
	}
	if err := e.writeBinary(); err != nil {
		return err
	}
	return nil
}

// findContracts looks for some very specific patterns in the original
// source code, so we're just going to run through the AST nodes, rather
// than try to back this out of the SSA form of the implicit init()
// function.
//
// Specifically, it looks for:
//   var _ Contract = MyContract{}
//   var _ Contract = &MyContract{}
func (e *Eddie) findContracts() error {
	cfg := &packages.Config{
		BuildFlags: e.BuildFlags,
		Dir:        e.Dir,
		Mode:       packages.LoadAllSyntax,
	}
	pkgs, err := packages.Load(cfg, e.Packages...)
	if err != nil {
		return err
	}

	// Look for the type assertion.
	for _, pkg := range pkgs {
		for _, f := range pkg.Syntax {
			for _, d := range f.Decls {
				if v, ok := d.(*ast.GenDecl); ok && v.Tok == token.VAR {
					for _, s := range v.Specs {
						if v, ok := s.(*ast.ValueSpec); ok &&
							len(v.Values) == 1 &&
							v.Names[0].Name == "_" {
							// assignmentType is the LHS type.
							assignmentType, _ := pkg.TypesInfo.TypeOf(v.Type).(*types.Named)
							if assignmentType == nil ||
								assignmentType.Obj().Pkg().Path() != e.extPkg ||
								assignmentType.Obj().Name() != "Contract" {
								continue
							}
							// value:Type is the type of the RHS. It should be a named
							// type or a pointer thereto.
							valueType := pkg.TypesInfo.TypeOf(v.Values[0])
							if ptr, ok := valueType.(*types.Pointer); ok {
								valueType = ptr.Elem()
							}
							if named, ok := valueType.(*types.Named); ok {
								e.contracts = append(e.contracts, named.Obj())
							}
						}
					}
				}
			}
		}
	}

	return nil
}

// writeBinary generates a file containing a main function which
// configures the runtime and then compiles in into an executable
// binary.
func (e *Eddie) writeBinary() error {
	tempDir, err := ioutil.TempDir("", "eddie")
	if err != nil {
		return err
	}
	if e.KeepTemp {
		e.Logger.Printf("writing to temporary directory %s", tempDir)
	} else {
		defer func() {
			if err := os.RemoveAll(tempDir); err != nil {
				panic(err)
			}
		}()
	}

	fnMap := template.FuncMap{
		"Contracts": func() []types.Object { return e.contracts },
		"ExtPkg":    func() string { return e.extPkg },
		"Help": func(obj types.Object) string {
			// Ideally, this would emit the struct definition and
			// additional documentation. For now, we'll create a godoc
			// link for users to follow.
			return fmt.Sprintf("`contract:%s\n\thttps://godoc.org/%s#%s`",
				obj.Name(), obj.Pkg().Path(), obj.Name())
		},
		"Name":  func() string { return e.Name },
		"RtPkg": func() string { return e.rtPkg },
	}

	t, err := template.New("root").Funcs(fnMap).Parse(`
// File generated by eddie; DO NOT EDIT
package main

import (
	ext "{{ ExtPkg }}"
	rt "{{ RtPkg }}"
	{{ range $idx, $c := Contracts -}}
		c{{ $idx }} "{{ $c.Pkg.Path }}"
	{{ end }}
)

var Enforcer = rt.Enforcer{
	Contracts: ext.ContractProviders {
	{{ range $idx, $c := Contracts -}}
		"{{ $c.Name }}" : {
			New: func() ext.Contract { return &c{{ $idx }}.{{ $c.Name}}{} },
			Help: {{ Help $c }},
		},
	{{ end }}
	},
	Name: "{{ Name }}",
}

func main() {
	Enforcer.Main()
}
`)
	if err != nil {
		return err
	}

	var src bytes.Buffer
	if err := t.Execute(&src, nil); err != nil {
		return err
	}

	// Formatting this code isn't strictly necessary, but it does
	// make it easier to inspect.
	if formatted, err := format.Source(src.Bytes()); err != nil {
		e.Logger.Print(src.String())
		return err
	} else {
		src.Reset()
		src.Write(formatted)
	}

	main := filepath.Join(tempDir, "main.go")
	if err := ioutil.WriteFile(main, src.Bytes(), 0644); err != nil {
		return err
	}

	exe, err := filepath.Abs(e.Outfile)
	if err != nil {
		return err
	}

	args := []string{"build", "-o", exe}
	if e.Plugin {
		args = append(args, "-buildmode=plugin")
	}
	args = append(args, main)
	args = append(args, e.BuildFlags...)

	build := exec.Command("go", args...)
	build.Dir = e.Dir
	build.Env = os.Environ()
	if output, err := build.CombinedOutput(); err != nil {
		return errors.Wrapf(err, "%s", output)
	}

	e.Logger.Printf("wrote output to %s", exe)
	return nil
}
