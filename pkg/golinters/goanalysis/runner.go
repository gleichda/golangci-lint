// checker is a partial copy of https://github.com/golang/tools/blob/master/go/analysis/internal/checker
// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package checker defines the implementation of the checker commands.
// The same code drives the multi-analysis driver, the single-analysis
// driver that is conventionally provided for convenience along with
// each analysis package, and the test driver.
package goanalysis

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"go/types"
	"log"
	"os"
	"reflect"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/tools/go/types/objectpath"

	"golang.org/x/tools/go/gcexportdata"

	"github.com/golangci/golangci-lint/internal/errorutil"
	"github.com/golangci/golangci-lint/internal/pkgcache"

	"github.com/golangci/golangci-lint/pkg/golinters/goanalysis/load"
	"github.com/golangci/golangci-lint/pkg/logutils"

	"github.com/pkg/errors"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/packages"
)

var (
	// Debug is a set of single-letter flags:
	//
	//	f	show [f]acts as they are created
	// 	p	disable [p]arallel execution of analyzers
	//	s	do additional [s]anity checks on fact types and serialization
	//	t	show [t]iming info (NB: use 'p' flag to avoid GC/scheduler noise)
	//	v	show [v]erbose logging
	//

	debugf  = logutils.Debug("goanalysis")
	isDebug = logutils.HaveDebugTag("goanalysis")

	factsDebugf  = logutils.Debug("goanalysis/facts")
	isFactsDebug = logutils.HaveDebugTag("goanalysis/facts")

	factsCacheDebugf = logutils.Debug("goanalysis/facts/cache")
	analyzeDebugf    = logutils.Debug("goanalysis/analyze")

	Debug = os.Getenv("GL_GOANALYSIS_DEBUG")

	unsafePkgName = "unsafe"
)

type Diagnostic struct {
	analysis.Diagnostic
	Analyzer *analysis.Analyzer
	Position token.Position
}

type runner struct {
	log       logutils.Log
	prefix    string // ensure unique analyzer names
	pkgCache  *pkgcache.Cache
	loadGuard *load.Guard
}

func newRunner(prefix string, logger logutils.Log, pkgCache *pkgcache.Cache, loadGuard *load.Guard) *runner {
	return &runner{
		prefix:    prefix,
		log:       logger,
		pkgCache:  pkgCache,
		loadGuard: loadGuard,
	}
}

// Run loads the packages specified by args using go/packages,
// then applies the specified analyzers to them.
// Analysis flags must already have been set.
// It provides most of the logic for the main functions of both the
// singlechecker and the multi-analysis commands.
// It returns the appropriate exit code.
//nolint:gocyclo
func (r *runner) run(analyzers []*analysis.Analyzer, initialPackages []*packages.Package) ([]Diagnostic, []error) {
	defer r.pkgCache.Trim()

	roots, err := r.analyze(initialPackages, analyzers)
	if err != nil {
		return nil, []error{err}
	}

	return extractDiagnostics(roots)
}

func (r *runner) analyze(pkgs []*packages.Package, analyzers []*analysis.Analyzer) ([]*action, error) {
	// Construct the action graph.

	// Each graph node (action) is one unit of analysis.
	// Edges express package-to-package (vertical) dependencies,
	// and analysis-to-analysis (horizontal) dependencies.
	type key struct {
		*analysis.Analyzer
		*packages.Package
	}
	actions := make(map[key]*action)

	initialPkgs := map[*packages.Package]bool{}
	for _, pkg := range pkgs {
		initialPkgs[pkg] = true
	}

	var mkAction func(a *analysis.Analyzer, pkg *packages.Package) *action
	mkAction = func(a *analysis.Analyzer, pkg *packages.Package) *action {
		k := key{a, pkg}
		act, ok := actions[k]
		if !ok {
			act = &action{
				a:                 a,
				pkg:               pkg,
				log:               r.log,
				prefix:            r.prefix,
				pkgCache:          r.pkgCache,
				isInitialPkg:      initialPkgs[pkg],
				needAnalyzeSource: initialPkgs[pkg],
				analysisDoneCh:    make(chan struct{}),
				objectFacts:       make(map[objectFactKey]analysis.Fact),
				packageFacts:      make(map[packageFactKey]analysis.Fact),
			}

			// Add a dependency on each required analyzers.
			for _, req := range a.Requires {
				act.deps = append(act.deps, mkAction(req, pkg))
			}

			// An analysis that consumes/produces facts
			// must run on the package's dependencies too.
			if len(a.FactTypes) > 0 {
				paths := make([]string, 0, len(pkg.Imports))
				for path := range pkg.Imports {
					paths = append(paths, path)
				}
				sort.Strings(paths) // for determinism
				for _, path := range paths {
					dep := mkAction(a, pkg.Imports[path])
					act.deps = append(act.deps, dep)
				}

				// Need to register fact types for pkgcache proper gob encoding.
				for _, f := range a.FactTypes {
					gob.Register(f)
				}
			}

			actions[k] = act
		}
		return act
	}

	// Build nodes for initial packages.
	var roots []*action
	for _, a := range analyzers {
		for _, pkg := range pkgs {
			root := mkAction(a, pkg)
			root.isroot = true
			roots = append(roots, root)
		}
	}

	allActions := make([]*action, 0, len(actions))
	for _, act := range actions {
		allActions = append(allActions, act)
	}

	if err := r.loadPackagesAndFacts(allActions, initialPkgs); err != nil {
		return nil, errors.Wrap(err, "failed to load packages")
	}

	r.runActionsAnalysis(allActions)

	return roots, nil
}

func (r *runner) loadPackagesAndFacts(actions []*action, initialPkgs map[*packages.Package]bool) error {
	defer func(from time.Time) {
		debugf("Loading packages and facts took %s", time.Since(from))
	}(time.Now())

	actionPerPkg := map[*packages.Package][]*action{}
	for _, act := range actions {
		actionPerPkg[act.pkg] = append(actionPerPkg[act.pkg], act)
	}

	// Fill Imports field.
	loadingPackages := map[*packages.Package]*loadingPackage{}
	var dfs func(pkg *packages.Package)
	dfs = func(pkg *packages.Package) {
		if loadingPackages[pkg] != nil {
			return
		}

		imports := map[string]*loadingPackage{}
		for impPath, imp := range pkg.Imports {
			dfs(imp)
			imports[impPath] = loadingPackages[imp]
		}

		loadingPackages[pkg] = &loadingPackage{
			pkg:       pkg,
			imports:   imports,
			isInitial: initialPkgs[pkg],
			doneCh:    make(chan struct{}),
			log:       r.log,
			actions:   actionPerPkg[pkg],
			loadGuard: r.loadGuard,
		}
	}
	for _, act := range actions {
		dfs(act.pkg)
	}

	// Limit IO.
	loadSem := make(chan struct{}, runtime.GOMAXPROCS(-1))

	var wg sync.WaitGroup
	wg.Add(len(loadingPackages))
	errCh := make(chan error, len(loadingPackages))
	for _, lp := range loadingPackages {
		go func(lp *loadingPackage) {
			defer wg.Done()

			lp.waitUntilImportsLoaded()
			loadSem <- struct{}{}

			if err := lp.loadWithFacts(); err != nil {
				errCh <- errors.Wrapf(err, "failed to load package %s", lp.pkg.Name)
			}
			<-loadSem
		}(lp)
	}
	wg.Wait()

	close(errCh)
	for err := range errCh {
		return err
	}

	return nil
}

func (r *runner) runActionsAnalysis(actions []*action) {
	// Execute the graph in parallel.
	debugf("Running %d actions in parallel", len(actions))
	var wg sync.WaitGroup
	wg.Add(len(actions))
	panicsCh := make(chan error, len(actions))
	for _, act := range actions {
		go func(act *action) {
			defer func() {
				if p := recover(); p != nil {
					panicsCh <- errorutil.NewPanicError(fmt.Sprintf("%s: package %q (isInitialPkg: %t, needAnalyzeSource: %t): %s",
						act.a.Name, act.pkg.Name, act.isInitialPkg, act.needAnalyzeSource, p), debug.Stack())
				}
				wg.Done()
			}()
			act.analyze()
		}(act)
	}
	wg.Wait()
	close(panicsCh)

	for p := range panicsCh {
		panic(p)
	}
}

//nolint:nakedret
func extractDiagnostics(roots []*action) (retDiags []Diagnostic, retErrors []error) {
	extracted := make(map[*action]bool)
	var extract func(*action)
	var visitAll func(actions []*action)
	visitAll = func(actions []*action) {
		for _, act := range actions {
			if !extracted[act] {
				extracted[act] = true
				visitAll(act.deps)
				extract(act)
			}
		}
	}

	// De-duplicate diagnostics by position (not token.Pos) to
	// avoid double-reporting in source files that belong to
	// multiple packages, such as foo and foo.test.
	type key struct {
		token.Position
		*analysis.Analyzer
		message string
	}
	seen := make(map[key]bool)

	extract = func(act *action) {
		if act.err != nil {
			retErrors = append(retErrors, errors.Wrap(act.err, act.a.Name))
			return
		}

		if act.isroot {
			for _, diag := range act.diagnostics {
				// We don't display a.Name/f.Category
				// as most users don't care.

				posn := act.pkg.Fset.Position(diag.Pos)
				k := key{posn, act.a, diag.Message}
				if seen[k] {
					continue // duplicate
				}
				seen[k] = true

				retDiags = append(retDiags, Diagnostic{Diagnostic: diag, Analyzer: act.a, Position: posn})
			}
		}
	}
	visitAll(roots)
	return
}

// NeedFacts reports whether any analysis required by the specified set
// needs facts.  If so, we must load the entire program from source.
func NeedFacts(analyzers []*analysis.Analyzer) bool {
	seen := make(map[*analysis.Analyzer]bool)
	var q []*analysis.Analyzer // for BFS
	q = append(q, analyzers...)
	for len(q) > 0 {
		a := q[0]
		q = q[1:]
		if !seen[a] {
			seen[a] = true
			if len(a.FactTypes) > 0 {
				return true
			}
			q = append(q, a.Requires...)
		}
	}
	return false
}

// An action represents one unit of analysis work: the application of
// one analysis to one package. Actions form a DAG, both within a
// package (as different analyzers are applied, either in sequence or
// parallel), and across packages (as dependencies are analyzed).
type action struct {
	a                   *analysis.Analyzer
	pkg                 *packages.Package
	pass                *analysis.Pass
	isroot              bool
	isInitialPkg        bool
	needAnalyzeSource   bool
	deps                []*action
	objectFacts         map[objectFactKey]analysis.Fact
	packageFacts        map[packageFactKey]analysis.Fact
	result              interface{}
	diagnostics         []analysis.Diagnostic
	err                 error
	duration            time.Duration
	log                 logutils.Log
	prefix              string
	pkgCache            *pkgcache.Cache
	analysisDoneCh      chan struct{}
	loadCachedFactsDone bool
	loadCachedFactsOk   bool
}

type objectFactKey struct {
	obj types.Object
	typ reflect.Type
}

type packageFactKey struct {
	pkg *types.Package
	typ reflect.Type
}

func (act *action) String() string {
	return fmt.Sprintf("%s@%s", act.a, act.pkg)
}

func (act *action) loadCachedFacts() bool {
	if act.loadCachedFactsDone { // can't be set in parallel
		return act.loadCachedFactsOk
	}

	res := func() bool {
		if act.isInitialPkg {
			return true // load cached facts only for non-initial packages
		}

		if len(act.a.FactTypes) == 0 {
			return true // no need to load facts
		}

		return act.loadPersistedFacts()
	}()
	act.loadCachedFactsDone = true
	act.loadCachedFactsOk = res
	return res
}

func (act *action) analyze() {
	defer close(act.analysisDoneCh) // unblock actions depending from this action

	if !act.needAnalyzeSource {
		return
	}

	// Analyze dependencies.
	for _, dep := range act.deps {
		<-dep.analysisDoneCh
	}

	// TODO(adonovan): uncomment this during profiling.
	// It won't build pre-go1.11 but conditional compilation
	// using build tags isn't warranted.
	//
	// ctx, task := trace.NewTask(context.Background(), "exec")
	// trace.Log(ctx, "pass", act.String())
	// defer task.End()

	// Record time spent in this node but not its dependencies.
	// In parallel mode, due to GC/scheduler contention, the
	// time is 5x higher than in sequential mode, even with a
	// semaphore limiting the number of threads here.
	// So use -debug=tp.
	if isDebug {
		t0 := time.Now()
		defer func() { act.duration = time.Since(t0) }()
	}
	defer func(now time.Time) {
		analyzeDebugf("go/analysis: %s: %s: analyzed package %q in %s", act.prefix, act.a.Name, act.pkg.Name, time.Since(now))
	}(time.Now())

	// Report an error if any dependency failed.
	var failed []string
	for _, dep := range act.deps {
		if dep.err != nil {
			failed = append(failed, dep.String())
		}
	}
	if failed != nil {
		sort.Strings(failed)
		act.err = fmt.Errorf("failed prerequisites: %s", strings.Join(failed, ", "))
		return
	}

	// Plumb the output values of the dependencies
	// into the inputs of this action.  Also facts.
	inputs := make(map[*analysis.Analyzer]interface{})
	for _, dep := range act.deps {
		if dep.pkg == act.pkg {
			// Same package, different analysis (horizontal edge):
			// in-memory outputs of prerequisite analyzers
			// become inputs to this analysis pass.
			inputs[dep.a] = dep.result
		} else if dep.a == act.a { // (always true)
			// Same analysis, different package (vertical edge):
			// serialized facts produced by prerequisite analysis
			// become available to this analysis pass.
			inheritFacts(act, dep)
		}
	}

	// Run the analysis.
	pass := &analysis.Pass{
		Analyzer:          act.a,
		Fset:              act.pkg.Fset,
		Files:             act.pkg.Syntax,
		OtherFiles:        act.pkg.OtherFiles,
		Pkg:               act.pkg.Types,
		TypesInfo:         act.pkg.TypesInfo,
		TypesSizes:        act.pkg.TypesSizes,
		ResultOf:          inputs,
		Report:            func(d analysis.Diagnostic) { act.diagnostics = append(act.diagnostics, d) },
		ImportObjectFact:  act.importObjectFact,
		ExportObjectFact:  act.exportObjectFact,
		ImportPackageFact: act.importPackageFact,
		ExportPackageFact: act.exportPackageFact,
		AllObjectFacts:    act.allObjectFacts,
		AllPackageFacts:   act.allPackageFacts,
	}
	act.pass = pass

	var err error
	if act.pkg.IllTyped && !pass.Analyzer.RunDespiteErrors {
		err = fmt.Errorf("analysis skipped due to errors in package")
	} else {
		act.result, err = pass.Analyzer.Run(pass)
		if err == nil {
			if got, want := reflect.TypeOf(act.result), pass.Analyzer.ResultType; got != want {
				err = fmt.Errorf(
					"internal error: on package %s, analyzer %s returned a result of type %v, but declared ResultType %v",
					pass.Pkg.Path(), pass.Analyzer, got, want)
			}
		}
	}
	act.err = err

	// disallow calls after Run
	pass.ExportObjectFact = nil
	pass.ExportPackageFact = nil

	if err := act.persistFactsToCache(); err != nil {
		act.log.Warnf("Failed to persist facts to cache: %s", err)
	}
}

// inheritFacts populates act.facts with
// those it obtains from its dependency, dep.
func inheritFacts(act, dep *action) {
	serialize := false

	for key, fact := range dep.objectFacts {
		// Filter out facts related to objects
		// that are irrelevant downstream
		// (equivalently: not in the compiler export data).
		if !exportedFrom(key.obj, dep.pkg.Types) {
			factsDebugf("%v: discarding %T fact from %s for %s: %s", act, fact, dep, key.obj, fact)
			continue
		}

		// Optionally serialize/deserialize fact
		// to verify that it works across address spaces.
		if serialize {
			var err error
			fact, err = codeFact(fact)
			if err != nil {
				log.Panicf("internal error: encoding of %T fact failed in %v", fact, act)
			}
		}

		factsDebugf("%v: inherited %T fact for %s: %s", act, fact, key.obj, fact)
		act.objectFacts[key] = fact
	}

	for key, fact := range dep.packageFacts {
		// TODO: filter out facts that belong to
		// packages not mentioned in the export data
		// to prevent side channels.

		// Optionally serialize/deserialize fact
		// to verify that it works across address spaces
		// and is deterministic.
		if serialize {
			var err error
			fact, err = codeFact(fact)
			if err != nil {
				log.Panicf("internal error: encoding of %T fact failed in %v", fact, act)
			}
		}

		factsDebugf("%v: inherited %T fact for %s: %s", act, fact, key.pkg.Path(), fact)
		act.packageFacts[key] = fact
	}
}

// codeFact encodes then decodes a fact,
// just to exercise that logic.
func codeFact(fact analysis.Fact) (analysis.Fact, error) {
	// We encode facts one at a time.
	// A real modular driver would emit all facts
	// into one encoder to improve gob efficiency.
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(fact); err != nil {
		return nil, err
	}

	// Encode it twice and assert that we get the same bits.
	// This helps detect nondeterministic Gob encoding (e.g. of maps).
	var buf2 bytes.Buffer
	if err := gob.NewEncoder(&buf2).Encode(fact); err != nil {
		return nil, err
	}
	if !bytes.Equal(buf.Bytes(), buf2.Bytes()) {
		return nil, fmt.Errorf("encoding of %T fact is nondeterministic", fact)
	}

	newFact := reflect.New(reflect.TypeOf(fact).Elem()).Interface().(analysis.Fact)
	if err := gob.NewDecoder(&buf).Decode(newFact); err != nil {
		return nil, err
	}
	return newFact, nil
}

// exportedFrom reports whether obj may be visible to a package that imports pkg.
// This includes not just the exported members of pkg, but also unexported
// constants, types, fields, and methods, perhaps belonging to oether packages,
// that find there way into the API.
// This is an overapproximation of the more accurate approach used by
// gc export data, which walks the type graph, but it's much simpler.
//
// TODO(adonovan): do more accurate filtering by walking the type graph.
func exportedFrom(obj types.Object, pkg *types.Package) bool {
	switch obj := obj.(type) {
	case *types.Func:
		return obj.Exported() && obj.Pkg() == pkg ||
			obj.Type().(*types.Signature).Recv() != nil
	case *types.Var:
		return obj.Exported() && obj.Pkg() == pkg ||
			obj.IsField()
	case *types.TypeName, *types.Const:
		return true
	}
	return false // Nil, Builtin, Label, or PkgName
}

// importObjectFact implements Pass.ImportObjectFact.
// Given a non-nil pointer ptr of type *T, where *T satisfies Fact,
// importObjectFact copies the fact value to *ptr.
func (act *action) importObjectFact(obj types.Object, ptr analysis.Fact) bool {
	if obj == nil {
		panic("nil object")
	}
	key := objectFactKey{obj, factType(ptr)}
	if v, ok := act.objectFacts[key]; ok {
		reflect.ValueOf(ptr).Elem().Set(reflect.ValueOf(v).Elem())
		return true
	}
	return false
}

// exportObjectFact implements Pass.ExportObjectFact.
func (act *action) exportObjectFact(obj types.Object, fact analysis.Fact) {
	if act.pass.ExportObjectFact == nil {
		log.Panicf("%s: Pass.ExportObjectFact(%s, %T) called after Run", act, obj, fact)
	}

	if obj.Pkg() != act.pkg.Types {
		log.Panicf("internal error: in analysis %s of package %s: Fact.Set(%s, %T): can't set facts on objects belonging another package",
			act.a, act.pkg, obj, fact)
	}

	key := objectFactKey{obj, factType(fact)}
	act.objectFacts[key] = fact // clobber any existing entry
	if isFactsDebug {
		objstr := types.ObjectString(obj, (*types.Package).Name)
		factsDebugf("%s: object %s has fact %s\n",
			act.pkg.Fset.Position(obj.Pos()), objstr, fact)
	}
}

func (act *action) allObjectFacts() []analysis.ObjectFact {
	out := make([]analysis.ObjectFact, 0, len(act.objectFacts))
	for key, fact := range act.objectFacts {
		out = append(out, analysis.ObjectFact{
			Object: key.obj,
			Fact:   fact,
		})
	}
	return out
}

// importPackageFact implements Pass.ImportPackageFact.
// Given a non-nil pointer ptr of type *T, where *T satisfies Fact,
// fact copies the fact value to *ptr.
func (act *action) importPackageFact(pkg *types.Package, ptr analysis.Fact) bool {
	if pkg == nil {
		panic("nil package")
	}
	key := packageFactKey{pkg, factType(ptr)}
	if v, ok := act.packageFacts[key]; ok {
		reflect.ValueOf(ptr).Elem().Set(reflect.ValueOf(v).Elem())
		return true
	}
	return false
}

// exportPackageFact implements Pass.ExportPackageFact.
func (act *action) exportPackageFact(fact analysis.Fact) {
	if act.pass.ExportPackageFact == nil {
		log.Panicf("%s: Pass.ExportPackageFact(%T) called after Run", act, fact)
	}

	key := packageFactKey{act.pass.Pkg, factType(fact)}
	act.packageFacts[key] = fact // clobber any existing entry
	factsDebugf("%s: package %s has fact %s\n",
		act.pkg.Fset.Position(act.pass.Files[0].Pos()), act.pass.Pkg.Path(), fact)
}

func (act *action) allPackageFacts() []analysis.PackageFact {
	out := make([]analysis.PackageFact, 0, len(act.packageFacts))
	for key, fact := range act.packageFacts {
		out = append(out, analysis.PackageFact{
			Package: key.pkg,
			Fact:    fact,
		})
	}
	return out
}

func factType(fact analysis.Fact) reflect.Type {
	t := reflect.TypeOf(fact)
	if t.Kind() != reflect.Ptr {
		log.Fatalf("invalid Fact type: got %T, want pointer", t)
	}
	return t
}

type Fact struct {
	Path string // non-empty only for object facts
	Fact analysis.Fact
}

func (act *action) persistFactsToCache() error {
	analyzer := act.a
	if len(analyzer.FactTypes) == 0 {
		return nil
	}

	// Merge new facts into the package and persist them.
	var facts []Fact
	for key, fact := range act.packageFacts {
		if key.pkg != act.pkg.Types {
			// The fact is from inherited facts from another package
			continue
		}
		facts = append(facts, Fact{
			Path: "",
			Fact: fact,
		})
	}
	for key, fact := range act.objectFacts {
		obj := key.obj
		if obj.Pkg() != act.pkg.Types {
			// The fact is from inherited facts from another package
			continue
		}

		path, err := objectpath.For(obj)
		if err != nil {
			// The object is not globally addressable
			continue
		}

		facts = append(facts, Fact{
			Path: string(path),
			Fact: fact,
		})
	}

	factsCacheDebugf("Caching %d facts for package %q and analyzer %s", len(facts), act.pkg.Name, act.a.Name)

	key := fmt.Sprintf("%s/facts", analyzer.Name)
	return act.pkgCache.Put(act.pkg, key, facts)
}

func (act *action) loadPersistedFacts() bool {
	var facts []Fact
	key := fmt.Sprintf("%s/facts", act.a.Name)
	if err := act.pkgCache.Get(act.pkg, key, &facts); err != nil {
		if err != pkgcache.ErrMissing {
			act.log.Warnf("Failed to get persisted facts: %s", err)
		}

		factsCacheDebugf("No cached facts for package %q and analyzer %s", act.pkg.Name, act.a.Name)
		return false
	}

	factsCacheDebugf("Loaded %d cached facts for package %q and analyzer %s", len(facts), act.pkg.Name, act.a.Name)

	for _, f := range facts {
		if f.Path == "" { // this is a package fact
			key := packageFactKey{act.pkg.Types, factType(f.Fact)}
			act.packageFacts[key] = f.Fact
			continue
		}
		obj, err := objectpath.Object(act.pkg.Types, objectpath.Path(f.Path))
		if err != nil {
			// Be lenient about these errors. For example, when
			// analyzing io/ioutil from source, we may get a fact
			// for methods on the devNull type, and objectpath
			// will happily create a path for them. However, when
			// we later load io/ioutil from export data, the path
			// no longer resolves.
			//
			// If an exported type embeds the unexported type,
			// then (part of) the unexported type will become part
			// of the type information and our path will resolve
			// again.
			continue
		}
		factKey := objectFactKey{obj, factType(f.Fact)}
		act.objectFacts[factKey] = f.Fact
	}

	return true
}

type loadingPackage struct {
	pkg       *packages.Package
	imports   map[string]*loadingPackage
	isInitial bool
	doneCh    chan struct{}
	log       logutils.Log
	actions   []*action // all actions with this package
	wasLoaded bool
	loadGuard *load.Guard
}

func (lp *loadingPackage) loadFromSource() error {
	pkg := lp.pkg

	// Call NewPackage directly with explicit name.
	// This avoids skew between golist and go/types when the files'
	// package declarations are inconsistent.
	// Subtle: we populate all Types fields with an empty Package
	// before loading export data so that export data processing
	// never has to create a types.Package for an indirect dependency,
	// which would then require that such created packages be explicitly
	// inserted back into the Import graph as a final step after export data loading.
	pkg.Types = types.NewPackage(pkg.PkgPath, pkg.Name)

	pkg.IllTyped = true

	// Many packages have few files, much fewer than there
	// are CPU cores. Additionally, parsing each individual file is
	// very fast. A naive parallel implementation of this loop won't
	// be faster, and tends to be slower due to extra scheduling,
	// bookkeeping and potentially false sharing of cache lines.
	pkg.Syntax = make([]*ast.File, len(pkg.CompiledGoFiles))
	for i, file := range pkg.CompiledGoFiles {
		f, err := parser.ParseFile(pkg.Fset, file, nil, parser.ParseComments)
		if err != nil {
			pkg.Errors = append(pkg.Errors, lp.convertError(err)...)
			return err
		}
		pkg.Syntax[i] = f
	}
	pkg.TypesInfo = &types.Info{
		Types:      make(map[ast.Expr]types.TypeAndValue),
		Defs:       make(map[*ast.Ident]types.Object),
		Uses:       make(map[*ast.Ident]types.Object),
		Implicits:  make(map[ast.Node]types.Object),
		Scopes:     make(map[ast.Node]*types.Scope),
		Selections: make(map[*ast.SelectorExpr]*types.Selection),
	}

	importer := func(path string) (*types.Package, error) {
		if path == unsafePkgName {
			return types.Unsafe, nil
		}
		if path == "C" {
			// go/packages doesn't tell us that cgo preprocessing
			// failed. When we subsequently try to parse the package,
			// we'll encounter the raw C import.
			return nil, errors.New("cgo preprocessing failed")
		}
		imp := pkg.Imports[path]
		if imp == nil {
			return nil, nil
		}
		if len(imp.Errors) > 0 {
			return nil, imp.Errors[0]
		}
		return imp.Types, nil
	}
	tc := &types.Config{
		Importer: importerFunc(importer),
		Error: func(err error) {
			pkg.Errors = append(pkg.Errors, lp.convertError(err)...)
		},
	}
	err := types.NewChecker(tc, pkg.Fset, pkg.Types, pkg.TypesInfo).Files(pkg.Syntax)
	if err != nil {
		return err
	}
	pkg.IllTyped = false
	return nil
}

func (lp *loadingPackage) loadFromExportData() error {
	// Because gcexportdata.Read has the potential to create or
	// modify the types.Package for each node in the transitive
	// closure of dependencies of lpkg, all exportdata operations
	// must be sequential. (Finer-grained locking would require
	// changes to the gcexportdata API.)
	//
	// The exportMu lock guards the Package.Pkg field and the
	// types.Package it points to, for each Package in the graph.
	//
	// Not all accesses to Package.Pkg need to be protected by this mutex:
	// graph ordering ensures that direct dependencies of source
	// packages are fully loaded before the importer reads their Pkg field.
	mu := lp.loadGuard.MutexForExportData()
	mu.Lock()
	defer mu.Unlock()

	pkg := lp.pkg

	// Call NewPackage directly with explicit name.
	// This avoids skew between golist and go/types when the files'
	// package declarations are inconsistent.
	// Subtle: we populate all Types fields with an empty Package
	// before loading export data so that export data processing
	// never has to create a types.Package for an indirect dependency,
	// which would then require that such created packages be explicitly
	// inserted back into the Import graph as a final step after export data loading.
	pkg.Types = types.NewPackage(pkg.PkgPath, pkg.Name)

	pkg.IllTyped = true
	for path, pkg := range pkg.Imports {
		if pkg.Types == nil {
			return fmt.Errorf("dependency %q hasn't been loaded yet", path)
		}
	}
	if pkg.ExportFile == "" {
		return fmt.Errorf("no export data for %q", pkg.ID)
	}
	f, err := os.Open(pkg.ExportFile)
	if err != nil {
		return err
	}
	defer f.Close()

	r, err := gcexportdata.NewReader(f)
	if err != nil {
		return err
	}

	view := make(map[string]*types.Package)  // view seen by gcexportdata
	seen := make(map[*packages.Package]bool) // all visited packages
	var visit func(pkgs map[string]*packages.Package)
	visit = func(pkgs map[string]*packages.Package) {
		for _, pkg := range pkgs {
			if !seen[pkg] {
				seen[pkg] = true
				view[pkg.PkgPath] = pkg.Types
				visit(pkg.Imports)
			}
		}
	}
	visit(pkg.Imports)
	tpkg, err := gcexportdata.Read(r, pkg.Fset, view, pkg.PkgPath)
	if err != nil {
		return err
	}
	pkg.Types = tpkg
	pkg.IllTyped = false
	return nil
}

func (lp *loadingPackage) waitUntilImportsLoaded() {
	// Imports must be loaded before loading the package.
	for _, imp := range lp.imports {
		<-imp.doneCh
	}
}

func (lp *loadingPackage) loadWithFacts() error {
	defer close(lp.doneCh)
	defer func() {
		lp.wasLoaded = true
	}()

	pkg := lp.pkg

	if pkg.PkgPath == unsafePkgName {
		// Fill in the blanks to avoid surprises.
		pkg.Types = types.Unsafe
		pkg.Syntax = []*ast.File{}
		pkg.TypesInfo = new(types.Info)
		return nil
	}

	markDepsForAnalyzingSource := func(act *action) {
		// Horizontal deps (analyzer.Requires) must be loaded from source and analyzed before analyzing
		// this action.
		for _, dep := range act.deps {
			if dep.pkg == act.pkg {
				// Analyze source only for horizontal dependencies, e.g. from "buildssa".
				dep.needAnalyzeSource = true // can't be set in parallel
			}
		}
	}

	if pkg.TypesInfo != nil {
		// Already loaded package, e.g. because another not go/analysis linter required types for deps.
		// Try load cached facts for it.

		if !lp.wasLoaded { // wasLoaded can't be set in parallel
			for _, act := range lp.actions {
				if !act.loadCachedFacts() {
					// Cached facts loading failed: analyze later the action from source.
					act.needAnalyzeSource = true
					markDepsForAnalyzingSource(act)
				}
			}
		}
		return nil
	}

	if lp.isInitial {
		// No need to load cached facts: the package will be analyzed from source
		// because it's the initial.
		return lp.loadFromSource()
	}

	// Load package from export data
	if err := lp.loadFromExportData(); err != nil {
		// We asked Go to give us up to date export data, yet
		// we can't load it. There must be something wrong.
		//
		// Attempt loading from source. This should fail (because
		// otherwise there would be export data); we just want to
		// get the compile errors. If loading from source succeeds
		// we discard the result, anyway. Otherwise we'll fail
		// when trying to reload from export data later.

		// Otherwise it panics because uses already existing (from exported data) types.
		pkg.Types = types.NewPackage(pkg.PkgPath, pkg.Name)
		if srcErr := lp.loadFromSource(); srcErr != nil {
			return srcErr
		}
		// Make sure this package can't be imported successfully
		pkg.Errors = append(pkg.Errors, packages.Error{
			Pos:  "-",
			Msg:  fmt.Sprintf("could not load export data: %s", err),
			Kind: packages.ParseError,
		})
		return errors.Wrap(err, "could not load export data")
	}

	needLoadFromSource := false
	for _, act := range lp.actions {
		if act.loadCachedFacts() {
			continue
		}

		// Cached facts loading failed: analyze later the action from source.
		factsCacheDebugf("Loading of facts for %s:%s failed, analyze it from source later", act.a.Name, pkg.Name)
		act.needAnalyzeSource = true // can't be set in parallel
		needLoadFromSource = true

		markDepsForAnalyzingSource(act)
	}

	if needLoadFromSource {
		// Cached facts loading failed: analyze later the action from source. To perform
		// the analysis we need to load the package from source code.

		// Otherwise it panics because uses already existing (from exported data) types.
		pkg.Types = types.NewPackage(pkg.PkgPath, pkg.Name)
		return lp.loadFromSource()
	}

	return nil
}

func (lp *loadingPackage) convertError(err error) []packages.Error {
	var errs []packages.Error
	// taken from go/packages
	switch err := err.(type) {
	case packages.Error:
		// from driver
		errs = append(errs, err)

	case *os.PathError:
		// from parser
		errs = append(errs, packages.Error{
			Pos:  err.Path + ":1",
			Msg:  err.Err.Error(),
			Kind: packages.ParseError,
		})

	case scanner.ErrorList:
		// from parser
		for _, err := range err {
			errs = append(errs, packages.Error{
				Pos:  err.Pos.String(),
				Msg:  err.Msg,
				Kind: packages.ParseError,
			})
		}

	case types.Error:
		// from type checker
		errs = append(errs, packages.Error{
			Pos:  err.Fset.Position(err.Pos).String(),
			Msg:  err.Msg,
			Kind: packages.TypeError,
		})

	default:
		// unexpected impoverished error from parser?
		errs = append(errs, packages.Error{
			Pos:  "-",
			Msg:  err.Error(),
			Kind: packages.UnknownError,
		})

		// If you see this error message, please file a bug.
		lp.log.Warnf("Internal error: error %q (%T) without position", err, err)
	}
	return errs
}

type importerFunc func(path string) (*types.Package, error)

func (f importerFunc) Import(path string) (*types.Package, error) { return f(path) }
