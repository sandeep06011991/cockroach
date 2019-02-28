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

package rt

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"log"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"strings"
	"syscall"

	"github.com/cockroachdb/cockroach/pkg/cmd/eddie/ext"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

type Result struct {
	Msg string
}

type Results map[token.Position][]*Result

// Enforcer is the main entrypoint for a generated linter binary.
// The generated code will just configure an instance of Enforcer
// and call its Main() method.
type Enforcer struct {
	// If true, we will only consider types to implement an interface
	// if there is an explicit assertion of the form:
	//   var _ Intf = &Impl{}
	AssertedInterfaces bool
	// Contracts contains providers for the various Contract types.
	// This map is the primary point of code-generation.
	Contracts ext.ContractProviders
	// Allows the working directory to be overridden.
	Dir string
	// The name of the generated linter.
	Name string
	// An optional Logger to receive diagnostic messages.
	Logger *log.Logger
	// The package-patterns to enforce contracts upon.
	Packages []string
	// If true, Main() will call os.Exit(1) if any reports are generated.
	SetExitStatus bool
	// If true, the test sources for the package will be included.
	Tests bool

	aliases      targetAliases
	allPackages  map[string]*packages.Package
	assertions   []*assertion
	contractType *types.Interface
	pkgs         []*packages.Package
	reports      chan report
	ssaPgm       *ssa.Program
	targets      []*target
}

// Execute allows an Enforcer to be called programmatically.
func (e *Enforcer) Execute(ctx context.Context) (Results, error) {
	if len(e.Packages) == 0 {
		return nil, errors.New("no packages specified")
	}
	// Load the source
	cfg := &packages.Config{
		Dir:   e.Dir,
		Fset:  token.NewFileSet(),
		Mode:  packages.LoadAllSyntax,
		Tests: e.Tests,
	}
	pkgs, err := packages.Load(cfg, e.Packages...)
	if err != nil {
		return nil, err
	}
	e.pkgs = pkgs

	e.allPackages = flattenImports(pkgs)

	// If the user has imported the ext package, they may have declared
	// contract aliases.  We'll need to find the underlying interface type.
	if extPkg := e.allPackages["github.com/cockroachdb/cockroach/pkg/cmd/eddie/ext"]; extPkg != nil {
		if obj := extPkg.Types.Scope().Lookup("Contract"); obj != nil {
			e.contractType = obj.Type().Underlying().(*types.Interface)
		}
	}

	// Prep SSA program. We'll defer building the packages until we
	// need to present a function to a Contract.
	e.ssaPgm, _ = ssautil.AllPackages(pkgs, 0 /* mode */)

	// Look for contract declarations on the AST side before we go through
	// the bother of converting to SSA form
	if err := e.findContracts(ctx); err != nil {
		return nil, err
	}

	// Expand aliases and initialize Contract instances.
	if err := e.expandAll(ctx); err != nil {
		return nil, err
	}

	e.reports = make(chan report)
	ret := make(Results)
	doneReporting := syncutil.Mutex{}
	doneReporting.Lock()
	go func() {
		for r := range e.reports {
			pos := cfg.Fset.Position(r.pos)
			sb := &strings.Builder{}
			fmt.Fprintf(sb, "violates contract %s", r.target.contract)
			for _, chunk := range strings.Split(r.info, "\n") {
				sb.WriteString("\n>> ")
				sb.WriteString(chunk)
			}
			ret[pos] = append(ret[pos], &Result{Msg: sb.String()})
		}
		doneReporting.Unlock()
	}()

	// Defer building SSA nodes until we know that all configuration is good.
	e.ssaPgm.Build()

	// Now, we can run the contracts.
	err = e.enforceAll(ctx)
	close(e.reports)
	doneReporting.Lock()
	return ret, err
}

// Main is called by the generated main() code.
func (e *Enforcer) Main() {
	verbose := false
	enforce := &cobra.Command{
		Use:           "enforce [packages]",
		Short:         "Enforce contracts defined in the given packages",
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sig := make(chan os.Signal, 1)
			defer close(sig)

			signal.Notify(sig, syscall.SIGINT)
			defer signal.Stop(sig)

			go func() {
				select {
				case _, open := <-sig:
					if open {
						cmd.Println("Interrupted")
						cancel()
					}
				}
			}()

			e.Packages = args
			if verbose {
				e.Logger = log.New(os.Stdout, "" /* prefix */, 0 /* flags */)
			}
			results, err := e.Execute(ctx)
			for pos, slice := range results {
				for _, result := range slice {
					cmd.Printf("%s: %s\n\n", pos, result.Msg)
				}
			}
			if err == nil && e.SetExitStatus {
				err = errors.New("reports generated")
			}
			return err
		},
	}
	enforce.Flags().BoolVar(&e.AssertedInterfaces, "asserted_only",
		false, "only consider explicit type assertions")
	enforce.Flags().StringVarP(&e.Dir, "dir", "d",
		".", "override the current working directory")
	enforce.Flags().BoolVar(&e.SetExitStatus, "set_exit_status",
		false, "return a non-zero exit code if errors are reported")
	enforce.Flags().BoolVarP(&e.Tests, "tests", "t",
		false, "include test sources in the analysis")
	enforce.Flags().BoolVarP(&verbose, "verbose", "v",
		false, "enable additional diagnostic messages")

	root := &cobra.Command{
		Use: e.Name,
	}
	root.AddCommand(
		enforce,
		&cobra.Command{
			Use:   "contracts",
			Short: "Lists all defined contracts",
			RunE: func(cmd *cobra.Command, _ []string) error {
				for name, provider := range e.Contracts {
					if provider.Help == "" {
						cmd.Println("contract:" + name)
					}
					if provider.Help != "" {
						cmd.Println(provider.Help)
					}
					cmd.Println()
				}
				return nil
			},
		})

	if err := root.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	os.Exit(0)
}

// enforce performs enforcement on a single target. This method
// resolves the target into the various objects that we want to
// pass into the contract implementation and then invokes the
// contracts for validation.
func (e *Enforcer) enforce(ctx context.Context, tgt *target) error {
	assertions := make(ext.Assertions, len(e.assertions))
	for _, a := range e.assertions {
		assertions[a.intf] = append(assertions[a.intf], a.impl)
	}
	impl := &contextImpl{
		Context: ctx,
		oracle:  ext.NewOracle(e.ssaPgm, assertions),
		program: e.ssaPgm,
		reports: e.reports,
		target:  tgt,
	}

	switch tgt.kind {
	case ext.KindInterface:
		decl := e.ssaPgm.Package(tgt.object.Pkg()).Type(tgt.object.Name())
		impl.declaration = decl
		intf := decl.Type().Underlying().(*types.Interface)
		for _, obj := range impl.Oracle().TypeImplementors(intf, e.AssertedInterfaces) {
			impl.objects = append(impl.objects, e.ssaPgm.Package(obj.Pkg()).Type(obj.Name()))
		}

	case ext.KindInterfaceMethod:
		intf := tgt.enclosing.Type().Underlying().(*types.Interface)
		impl.declaration = e.ssaPgm.Package(tgt.enclosing.Pkg()).Type(tgt.enclosing.Name())
		for _, i := range impl.Oracle().MethodImplementors(intf, tgt.object.Name(), e.AssertedInterfaces) {
			impl.objects = append(impl.objects, i)
		}

	case ext.KindFunction, ext.KindMethod:
		fn := tgt.object.(*types.Func)
		impl.declaration = e.ssaPgm.FuncValue(fn)

	case ext.KindType:
		impl.declaration = e.ssaPgm.Package(tgt.object.Pkg()).Type(tgt.object.Name())

	default:
		panic(errors.Errorf("unimplemented: %s", tgt.kind))
	}

	e.printf("enforcing %s: %s %s (%d objects)",
		e.ssaPgm.Fset.Position(tgt.pos), impl.Kind(), impl.Declaration(), len(impl.Objects()))

	if err := tgt.impl.Enforce(impl); err != nil {
		impl.Reportf(impl.declaration, "%s: %v", tgt.contract, err)
	}
	return nil
}

// enforceAll executes all targets.
func (e *Enforcer) enforceAll(ctx context.Context) error {
	g, ctx := errgroup.WithContext(ctx)
	ch := make(chan *target, 1)

	for i := 0; i < runtime.NumCPU(); i++ {
		g.Go(func() error {
			for {
				select {
				case tgt, open := <-ch:
					if !open {
						return nil
					}
					if err := e.enforce(ctx, tgt); err != nil {
						return err
					}
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		})
	}

sendLoop:
	for i, tgt := range e.targets {
		select {
		case ch <- tgt:
			// Nullify the reference to the target once dispatched to
			// allow completed targets to be garbage-collected.
			e.targets[i] = nil
		case <-ctx.Done():
			break sendLoop
		}
	}
	close(ch)

	return g.Wait()
}

// expand expands alias targets into their final form or returns
// terminal targets as-is.
func (e *Enforcer) expand(base *target) ([]*target, error) {
	// Non-terminal targets, which need to be further expanded
	nonTerm := e.aliases[base.contract]
	if nonTerm == nil {
		return targets{base}, nil
	}

	// The terminal targets, which we will want to return.
	var term targets
	// Detect recursively-defined contracts.  This would only be an
	// issue with contract aliases that are mutually-referent.
	seen := map[*target]bool{base: true}

	for len(nonTerm) > 0 {
		work := append(targets(nil), nonTerm...)
		nonTerm = nonTerm[:0]
		for _, alias := range work {
			if seen[alias] {
				return nil, errors.Errorf("%s detected recursive contract %q",
					base.fset.Position(base.Pos()), alias.contract)
			}
			seen[alias] = true
			if moreExpansions := e.aliases[alias.contract]; moreExpansions != nil {
				nonTerm = append(nonTerm, moreExpansions...)
			} else {
				dup := *base
				dup.contract = alias.contract
				dup.config = alias.config
				term = append(term, &dup)
			}
		}
	}
	sort.Sort(term)
	return term, nil
}

// expandAll resolves all targets to actual Contract implementations,
// performing alias expansion and configuration. Once this method has
// finished, the Enforcer.targets field will be populated with all work
// to perform.
func (e *Enforcer) expandAll(ctx context.Context) error {
	expanded := make(targets, 0, len(e.targets))
	for _, tgt := range e.targets {
		expansion, err := e.expand(tgt)
		if err != nil {
			return err
		}
		expanded = append(expanded, expansion...)
	}

	for _, tgt := range expanded {
		provider := e.Contracts[tgt.contract]
		if provider == nil {
			return errors.Errorf("%s: cannot find contract named %s",
				tgt.fset.Position(tgt.Pos()), tgt.contract)
		}
		tgt.impl = provider.New()
		if tgt.config != "" {
			// Disallow unknown fields to help with typos.
			d := json.NewDecoder(strings.NewReader(tgt.config))
			d.DisallowUnknownFields()
			if err := d.Decode(&tgt.impl); err != nil {
				return errors.Wrap(err, tgt.fset.Position(tgt.Pos()).String())
			}
		}
	}
	e.targets = expanded
	return nil
}

// findContracts performs AST-level extraction.  Specifically, it will
// find AST nodes which have been annotated with a contract declaration
// as well as type-assertion assignments.
//
// Since we're operating on a per-ast.File basis, we want to operate as
// concurrently as possible. We'll set up a limited number of goroutines
// and feed them (package, file) pairs.
func (e *Enforcer) findContracts(ctx context.Context) error {
	// mu protects the variables shared between goroutines.
	mu := struct {
		syncutil.Mutex
		aliases    targetAliases
		assertions assertions
		targets    targets
	}{
		aliases: make(targetAliases),
	}

	// addAssertion updates mu.assertions in a safe manner.
	addAssertion := func(a *assertion) {
		e.println("assertion", a)
		mu.Lock()
		mu.assertions = append(mu.assertions, a)
		mu.Unlock()
	}

	// contract will update mu.targets if the provided comments contain
	// a contract declaration. It will also extract contract aliases.
	contract := func(
		pkg *packages.Package,
		comments []*ast.CommentGroup,
		object types.Object,
		enclosing types.Object,
		kind ext.Kind,
	) {
		for _, group := range comments {
			for _, comment := range group.List {
				matches := commentSyntax.FindAllStringSubmatch(comment.Text, -1)
				for _, match := range matches {
					tgt := &target{
						config:    strings.TrimSpace(match[2]),
						contract:  match[1],
						enclosing: enclosing,
						fset:      pkg.Fset,
						kind:      kind,
						object:    object,
						pos:       comment.Pos(),
					}

					e.println("target", tgt)
					mu.Lock()
					// Special case for contract aliases of the form
					//   //contract:Foo { ... }
					//   type Alias ext.Contract
					if named, ok := tgt.object.Type().(*types.Named); ok && named.Underlying() == e.contractType {
						name := named.Obj().Name()
						e.println("alias", name, ":=", tgt)
						mu.aliases[name] = append(mu.aliases[name], tgt)
					} else {
						mu.targets = append(mu.targets, tgt)
					}
					mu.Unlock()
				}
			}
		}
	}

	// process performs the bulk of the work in this method.
	process := func(ctx context.Context, pkg *packages.Package, file *ast.File) error {
		// CommentMap associates each node in the file with
		// its surrounding comments.
		comments := ast.NewCommentMap(pkg.Fset, file, file.Comments)

		// Track current-X's in the visitation below.
		var enclosing types.Object

		// Now we'll inspect the ast.File and look for our magic
		// comment syntax.
		ast.Inspect(file, func(node ast.Node) bool {
			// We'll see a node==nil as the very last call.
			if node == nil {
				return false
			}

			switch t := node.(type) {
			case *ast.Field:
				// Methods of an interface type, such as
				//   type I interface { Foo() }
				// surface as fields with a function type.
				if types.IsInterface(enclosing.Type()) {
					if _, ok := t.Type.(*ast.FuncType); ok {
						contract(pkg, comments[t], pkg.TypesInfo.ObjectOf(t.Names[0]), enclosing, ext.KindInterfaceMethod)
					}
				}
				return false

			case *ast.FuncDecl:
				// Top-level function or method declarations, such as
				//   func Foo() { .... }
				//   func (r Receiver) Bar() { ... }
				kind := ext.KindFunction
				if t.Recv != nil {
					kind = ext.KindMethod
				}
				contract(pkg, comments[t], pkg.TypesInfo.ObjectOf(t.Name), nil, kind)
				// We don't need to descend into function bodies.
				return false

			case *ast.GenDecl:
				switch t.Tok {
				case token.TYPE:
					// Type declarations, such as
					//   type Foo struct { ... }
					//   type Bar interface { ... }
					for _, spec := range t.Specs {
						tSpec := spec.(*ast.TypeSpec)
						enclosing = pkg.TypesInfo.ObjectOf(tSpec.Name)
						kind := ext.KindType
						if _, ok := tSpec.Type.(*ast.InterfaceType); ok {
							kind = ext.KindInterface
						}

						// Handle the usual case where contract is associated
						// with the type keyword.
						contract(pkg, comments[t], enclosing, nil, kind)
						// Handle unusual case where a type() block is being used
						// and a contract is specified on the entry.
						contract(pkg, comments[tSpec], enclosing, nil, kind)
						// We do need to descend into interfaces to pick up on
						// contracts applied only to interface methods.
						return kind == ext.KindInterface
					}

				case token.VAR:
					// Assertion declarations, such as
					//   var _ Intf = &Impl{}
					//   var _ Intf = Impl{}
					for _, spec := range t.Specs {
						v := spec.(*ast.ValueSpec)
						if len(v.Values) == 1 && v.Names[0].Name == "_" {
							named, ok := pkg.TypesInfo.TypeOf(v.Type).(*types.Named)
							if !ok || !types.IsInterface(named) {
								continue
							}
							a := &assertion{
								intf: named.Obj(),
								fset: e.ssaPgm.Fset,
								pos:  v.Pos(),
							}
							switch v := pkg.TypesInfo.TypeOf(v.Values[0]).(type) {
							case *types.Named:
								if _, ok := v.Underlying().(*types.Struct); ok {
									a.impl = v.Obj()
								}
							case *types.Pointer:
								if named, ok := v.Elem().(*types.Named); ok {
									if _, ok := named.Underlying().(*types.Struct); ok {
										a.impl = named.Obj()
									}
								}
							}
							if a.impl != nil {
								addAssertion(a)
							}
						}
					}
				}
				return false
			default:
				return true
			}
		})
		return nil
	}

	type work struct {
		pkg  *packages.Package
		file *ast.File
	}
	workCh := make(chan work, 1)

	g, ctx := errgroup.WithContext(ctx)
	for i := 0; i < runtime.NumCPU(); i++ {
		g.Go(func() error {
			for {
				select {
				case work, open := <-workCh:
					if !open {
						return nil
					}
					if err := process(ctx, work.pkg, work.file); err != nil {
						return err
					}
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		})
	}

sendLoop:
	for _, pkg := range e.pkgs {
		// See discussion on package.Config type for the naming scheme.
		if e.Tests && !strings.HasSuffix(pkg.ID, ".test]") {
			continue
		}
		if pkg.Errors != nil {
			return errors.Wrap(pkg.Errors[0], "could not load source due to error(s)")
		}

		for _, file := range pkg.Syntax {
			select {
			case workCh <- work{pkg, file}:
			case <-ctx.Done():
				break sendLoop
			}
		}
	}
	close(workCh)

	// Wait for all the goroutines to exit.
	if err := g.Wait(); err != nil {
		return err
	}

	// Produce stable output.
	for _, aliases := range mu.aliases {
		sort.Sort(aliases)
	}
	sort.Sort(mu.assertions)
	sort.Sort(mu.targets)

	e.aliases = mu.aliases
	e.assertions = mu.assertions
	e.targets = mu.targets
	return nil
}

// printf will emit a diagnostic message via e.Logger, if one is configured.
func (e *Enforcer) printf(format string, args ...interface{}) {
	if l := e.Logger; l != nil {
		l.Printf(format, args...)
	}
}

// println will emit a diagnostic message via e.Logger, if one is configured.
func (e *Enforcer) println(args ...interface{}) {
	if l := e.Logger; l != nil {
		l.Println(args...)
	}
}
