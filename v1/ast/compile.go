// Copyright 2016 The OPA Authors.  All rights reserved.
// Use of this source code is governed by an Apache2
// license that can be found in the LICENSE file.

package ast

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/open-policy-agent/opa/internal/debug"
	"github.com/open-policy-agent/opa/internal/gojsonschema"
	"github.com/open-policy-agent/opa/v1/ast/location"
	"github.com/open-policy-agent/opa/v1/metrics"
	"github.com/open-policy-agent/opa/v1/types"
	"github.com/open-policy-agent/opa/v1/util"
)

// CompileErrorLimitDefault is the default number errors a compiler will allow before
// exiting.
const CompileErrorLimitDefault = 10

var (
	errLimitReached = NewError(CompileErr, nil, "error limit reached")

	doubleEq = Equal.Ref()
)

// Compiler contains the state of a compilation process.
type Compiler struct {

	// Errors contains errors that occurred during the compilation process.
	// If there are one or more errors, the compilation process is considered
	// "failed".
	Errors Errors

	// Modules contains the compiled modules. The compiled modules are the
	// output of the compilation process. If the compilation process failed,
	// there is no guarantee about the state of the modules.
	Modules map[string]*Module

	// ModuleTree organizes the modules into a tree where each node is keyed by
	// an element in the module's package path. E.g., given modules containing
	// the following package directives: "a", "a.b", "a.c", and "a.b", the
	// resulting module tree would be:
	//
	//  root
	//    |
	//    +--- data (no modules)
	//           |
	//           +--- a (1 module)
	//                |
	//                +--- b (2 modules)
	//                |
	//                +--- c (1 module)
	//
	ModuleTree *ModuleTreeNode

	// RuleTree organizes rules into a tree where each node is keyed by an
	// element in the rule's path. The rule path is the concatenation of the
	// containing package and the stringified rule name. E.g., given the
	// following module:
	//
	//  package ex
	//  p[1] { true }
	//  p[2] { true }
	//  q = true
	//  a.b.c = 3
	//
	//  root
	//    |
	//    +--- data (no rules)
	//           |
	//           +--- ex (no rules)
	//                |
	//                +--- p (2 rules)
	//                |
	//                +--- q (1 rule)
	//                |
	//                +--- a
	//                     |
	//                     +--- b
	//                          |
	//                          +--- c (1 rule)
	//
	// Another example with general refs containing vars at arbitrary locations:
	//
	//  package ex
	//  a.b[x].d { x := "c" }            # R1
	//  a.b.c[x] { x := "d" }            # R2
	//  a.b[x][y] { x := "c"; y := "d" } # R3
	//  p := true                        # R4
	//
	//  root
	//    |
	//    +--- data (no rules)
	//           |
	//           +--- ex (no rules)
	//                |
	//                +--- a
	//                |    |
	//                |    +--- b (R1, R3)
	//                |         |
	//                |         +--- c (R2)
	//                |
	//                +--- p (R4)
	RuleTree *TreeNode

	// Graph contains dependencies between rules. An edge (u,v) is added to the
	// graph if rule 'u' refers to the virtual document defined by 'v'.
	Graph *Graph

	// TypeEnv holds type information for values inferred by the compiler.
	TypeEnv *TypeEnv

	// RewrittenVars is a mapping of variables that have been rewritten
	// with the key being the generated name and value being the original.
	RewrittenVars map[Var]Var

	// Capabilities required by the modules that were compiled.
	Required *Capabilities

	localvargen                *localVarGenerator
	moduleLoader               ModuleLoader
	ruleIndices                *util.HasherMap[Ref, RuleIndex]
	stages                     []stage
	maxErrs                    int
	sorted                     []string // list of sorted module names
	pathExists                 func([]string) (bool, error)
	pathConflictCheckRoots     []string
	after                      map[string][]CompilerStageDefinition
	metrics                    metrics.Metrics
	capabilities               *Capabilities                 // user-supplied capabilities
	imports                    map[string][]*Import          // saved imports from stripping
	builtins                   map[string]*Builtin           // universe of built-in functions
	customBuiltins             map[string]*Builtin           // user-supplied custom built-in functions (deprecated: use capabilities)
	unsafeBuiltinsMap          map[string]struct{}           // user-supplied set of unsafe built-ins functions to block (deprecated: use capabilities)
	deprecatedBuiltinsMap      map[string]struct{}           // set of deprecated, but not removed, built-in functions
	enablePrintStatements      bool                          // indicates if print statements should be elided (default)
	comprehensionIndices       map[*Term]*ComprehensionIndex // comprehension key index
	initialized                bool                          // indicates if init() has been called
	debug                      debug.Debug                   // emits debug information produced during compilation
	schemaSet                  *SchemaSet                    // user-supplied schemas for input and data documents
	inputType                  types.Type                    // global input type retrieved from schema set
	annotationSet              *AnnotationSet                // hierarchical set of annotations
	strict                     bool                          // enforce strict compilation checks
	keepModules                bool                          // whether to keep the unprocessed, parse modules (below)
	parsedModules              map[string]*Module            // parsed, but otherwise unprocessed modules, kept track of when keepModules is true
	useTypeCheckAnnotations    bool                          // whether to provide annotated information (schemas) to the type checker
	allowUndefinedFuncCalls    bool                          // don't error on calls to unknown functions.
	evalMode                   CompilerEvalMode              //
	rewriteTestRulesForTracing bool                          // rewrite test rules to capture dynamic values for tracing.
	defaultRegoVersion         RegoVersion
}

func (c *Compiler) DefaultRegoVersion() RegoVersion {
	return c.defaultRegoVersion
}

// CompilerStage defines the interface for stages in the compiler.
type CompilerStage func(*Compiler) *Error

// CompilerEvalMode allows toggling certain stages that are only
// needed for certain modes, Concretely, only "topdown" mode will
// have the compiler build comprehension and rule indices.
type CompilerEvalMode int

const (
	// EvalModeTopdown (default) instructs the compiler to build rule
	// and comprehension indices used by topdown evaluation.
	EvalModeTopdown CompilerEvalMode = iota

	// EvalModeIR makes the compiler skip the stages for comprehension
	// and rule indices.
	EvalModeIR
)

// CompilerStageDefinition defines a compiler stage
type CompilerStageDefinition struct {
	Name       string
	MetricName string
	Stage      CompilerStage
}

// RulesOptions defines the options for retrieving rules by Ref from the
// compiler.
type RulesOptions struct {
	// IncludeHiddenModules determines if the result contains hidden modules,
	// currently only the "system" namespace, i.e. "data.system.*".
	IncludeHiddenModules bool
}

// QueryContext contains contextual information for running an ad-hoc query.
//
// Ad-hoc queries can be run in the context of a package and imports may be
// included to provide concise access to data.
type QueryContext struct {
	Package *Package
	Imports []*Import
}

// NewQueryContext returns a new QueryContext object.
func NewQueryContext() *QueryContext {
	return &QueryContext{}
}

// WithPackage sets the pkg on qc.
func (qc *QueryContext) WithPackage(pkg *Package) *QueryContext {
	if qc == nil {
		qc = NewQueryContext()
	}
	qc.Package = pkg
	return qc
}

// WithImports sets the imports on qc.
func (qc *QueryContext) WithImports(imports []*Import) *QueryContext {
	if qc == nil {
		qc = NewQueryContext()
	}
	qc.Imports = imports
	return qc
}

// Copy returns a deep copy of qc.
func (qc *QueryContext) Copy() *QueryContext {
	if qc == nil {
		return nil
	}
	cpy := *qc
	if cpy.Package != nil {
		cpy.Package = qc.Package.Copy()
	}
	cpy.Imports = make([]*Import, len(qc.Imports))
	for i := range qc.Imports {
		cpy.Imports[i] = qc.Imports[i].Copy()
	}
	return &cpy
}

// QueryCompiler defines the interface for compiling ad-hoc queries.
type QueryCompiler interface {

	// Compile should be called to compile ad-hoc queries. The return value is
	// the compiled version of the query.
	Compile(q Body) (Body, error)

	// TypeEnv returns the type environment built after running type checking
	// on the query.
	TypeEnv() *TypeEnv

	// WithContext sets the QueryContext on the QueryCompiler. Subsequent calls
	// to Compile will take the QueryContext into account.
	WithContext(qctx *QueryContext) QueryCompiler

	// WithEnablePrintStatements enables print statements in queries compiled
	// with the QueryCompiler.
	WithEnablePrintStatements(yes bool) QueryCompiler

	// WithUnsafeBuiltins sets the built-in functions to treat as unsafe and not
	// allow inside of queries. By default the query compiler inherits the
	// compiler's unsafe built-in functions. This function allows callers to
	// override that set. If an empty (non-nil) map is provided, all built-ins
	// are allowed.
	WithUnsafeBuiltins(unsafe map[string]struct{}) QueryCompiler

	// WithStageAfter registers a stage to run during query compilation after
	// the named stage.
	WithStageAfter(after string, stage QueryCompilerStageDefinition) QueryCompiler

	// RewrittenVars maps generated vars in the compiled query to vars from the
	// parsed query. For example, given the query "input := 1" the rewritten
	// query would be "__local0__ = 1". The mapping would then be {__local0__: input}.
	RewrittenVars() map[Var]Var

	// ComprehensionIndex returns an index data structure for the given comprehension
	// term. If no index is found, returns nil.
	ComprehensionIndex(term *Term) *ComprehensionIndex

	// WithStrict enables strict mode for the query compiler.
	WithStrict(strict bool) QueryCompiler
}

// QueryCompilerStage defines the interface for stages in the query compiler.
type QueryCompilerStage func(QueryCompiler, Body) (Body, error)

// QueryCompilerStageDefinition defines a QueryCompiler stage
type QueryCompilerStageDefinition struct {
	Name       string
	MetricName string
	Stage      QueryCompilerStage
}

type stage struct {
	name       string
	metricName string
	f          func()
}

// NewCompiler returns a new empty compiler.
func NewCompiler() *Compiler {

	c := &Compiler{
		Modules:               map[string]*Module{},
		RewrittenVars:         map[Var]Var{},
		Required:              &Capabilities{},
		ruleIndices:           util.NewHasherMap[Ref, RuleIndex](RefEqual),
		maxErrs:               CompileErrorLimitDefault,
		after:                 map[string][]CompilerStageDefinition{},
		unsafeBuiltinsMap:     map[string]struct{}{},
		deprecatedBuiltinsMap: map[string]struct{}{},
		comprehensionIndices:  map[*Term]*ComprehensionIndex{},
		debug:                 debug.Discard(),
		defaultRegoVersion:    DefaultRegoVersion,
	}

	c.ModuleTree = NewModuleTree(nil)
	c.RuleTree = NewRuleTree(c.ModuleTree)

	c.stages = []stage{
		// Reference resolution should run first as it may be used to lazily
		// load additional modules. If any stages run before resolution, they
		// need to be re-run after resolution.
		{"ResolveRefs", "compile_stage_resolve_refs", c.resolveAllRefs},
		// The local variable generator must be initialized after references are
		// resolved and the dynamic module loader has run but before subsequent
		// stages that need to generate variables.
		{"InitLocalVarGen", "compile_stage_init_local_var_gen", c.initLocalVarGen},
		{"RewriteRuleHeadRefs", "compile_stage_rewrite_rule_head_refs", c.rewriteRuleHeadRefs},
		{"CheckKeywordOverrides", "compile_stage_check_keyword_overrides", c.checkKeywordOverrides},
		{"CheckDuplicateImports", "compile_stage_check_imports", c.checkImports},
		{"RemoveImports", "compile_stage_remove_imports", c.removeImports},
		{"SetModuleTree", "compile_stage_set_module_tree", c.setModuleTree},
		{"SetRuleTree", "compile_stage_set_rule_tree", c.setRuleTree}, // depends on RewriteRuleHeadRefs
		{"RewriteLocalVars", "compile_stage_rewrite_local_vars", c.rewriteLocalVars},
		{"CheckVoidCalls", "compile_stage_check_void_calls", c.checkVoidCalls},
		{"RewritePrintCalls", "compile_stage_rewrite_print_calls", c.rewritePrintCalls},
		{"RewriteExprTerms", "compile_stage_rewrite_expr_terms", c.rewriteExprTerms},
		{"ParseMetadataBlocks", "compile_stage_parse_metadata_blocks", c.parseMetadataBlocks},
		{"SetAnnotationSet", "compile_stage_set_annotationset", c.setAnnotationSet},
		{"RewriteRegoMetadataCalls", "compile_stage_rewrite_rego_metadata_calls", c.rewriteRegoMetadataCalls},
		{"SetGraph", "compile_stage_set_graph", c.setGraph},
		{"RewriteComprehensionTerms", "compile_stage_rewrite_comprehension_terms", c.rewriteComprehensionTerms},
		{"RewriteRefsInHead", "compile_stage_rewrite_refs_in_head", c.rewriteRefsInHead},
		{"RewriteWithValues", "compile_stage_rewrite_with_values", c.rewriteWithModifiers},
		{"CheckRuleConflicts", "compile_stage_check_rule_conflicts", c.checkRuleConflicts},
		{"CheckUndefinedFuncs", "compile_stage_check_undefined_funcs", c.checkUndefinedFuncs},
		{"CheckSafetyRuleHeads", "compile_stage_check_safety_rule_heads", c.checkSafetyRuleHeads},
		{"CheckSafetyRuleBodies", "compile_stage_check_safety_rule_bodies", c.checkSafetyRuleBodies},
		{"RewriteEquals", "compile_stage_rewrite_equals", c.rewriteEquals},
		{"RewriteDynamicTerms", "compile_stage_rewrite_dynamic_terms", c.rewriteDynamicTerms},
		{"RewriteTestRulesForTracing", "compile_stage_rewrite_test_rules_for_tracing", c.rewriteTestRuleEqualities}, // must run after RewriteDynamicTerms
		{"CheckRecursion", "compile_stage_check_recursion", c.checkRecursion},
		{"CheckTypes", "compile_stage_check_types", c.checkTypes}, // must be run after CheckRecursion
		{"CheckUnsafeBuiltins", "compile_state_check_unsafe_builtins", c.checkUnsafeBuiltins},
		{"CheckDeprecatedBuiltins", "compile_state_check_deprecated_builtins", c.checkDeprecatedBuiltins},
		{"BuildRuleIndices", "compile_stage_rebuild_indices", c.buildRuleIndices},
		{"BuildComprehensionIndices", "compile_stage_rebuild_comprehension_indices", c.buildComprehensionIndices},
		{"BuildRequiredCapabilities", "compile_stage_build_required_capabilities", c.buildRequiredCapabilities},
	}

	return c
}

// SetErrorLimit sets the number of errors the compiler can encounter before it
// quits. Zero or a negative number indicates no limit.
func (c *Compiler) SetErrorLimit(limit int) *Compiler {
	c.maxErrs = limit
	return c
}

// WithEnablePrintStatements enables print statements inside of modules compiled
// by the compiler. If print statements are not enabled, calls to print() are
// erased at compile-time.
func (c *Compiler) WithEnablePrintStatements(yes bool) *Compiler {
	c.enablePrintStatements = yes
	return c
}

// WithPathConflictsCheck enables base-virtual document conflict
// detection. The compiler will check that rules don't overlap with
// paths that exist as determined by the provided callable.
func (c *Compiler) WithPathConflictsCheck(fn func([]string) (bool, error)) *Compiler {
	c.pathExists = fn
	return c
}

// WithPathConflictsCheckRoots enables checking path conflicts from the specified root instead
// of the top root node. Limiting conflict checks to a known set of roots, such as bundle roots,
// improves performance. Each root has the format of a "/"-delimited string, excluding the "data"
// root document.
func (c *Compiler) WithPathConflictsCheckRoots(rootPaths []string) *Compiler {
	c.pathConflictCheckRoots = rootPaths
	return c
}

// WithStageAfter registers a stage to run during compilation after
// the named stage.
func (c *Compiler) WithStageAfter(after string, stage CompilerStageDefinition) *Compiler {
	c.after[after] = append(c.after[after], stage)
	return c
}

// WithMetrics will set a metrics.Metrics and be used for profiling
// the Compiler instance.
func (c *Compiler) WithMetrics(metrics metrics.Metrics) *Compiler {
	c.metrics = metrics
	return c
}

// WithCapabilities sets capabilities to enable during compilation. Capabilities allow the caller
// to specify the set of built-in functions available to the policy. In the future, capabilities
// may be able to restrict access to other language features. Capabilities allow callers to check
// if policies are compatible with a particular version of OPA. If policies are a compiled for a
// specific version of OPA, there is no guarantee that _this_ version of OPA can evaluate them
// successfully.
func (c *Compiler) WithCapabilities(capabilities *Capabilities) *Compiler {
	c.capabilities = capabilities
	return c
}

// Capabilities returns the capabilities enabled during compilation.
func (c *Compiler) Capabilities() *Capabilities {
	return c.capabilities
}

// WithDebug sets where debug messages are written to. Passing `nil` has no
// effect.
func (c *Compiler) WithDebug(sink io.Writer) *Compiler {
	if sink != nil {
		c.debug = debug.New(sink)
	}
	return c
}

// WithBuiltins is deprecated.
// Deprecated: Use WithCapabilities instead.
func (c *Compiler) WithBuiltins(builtins map[string]*Builtin) *Compiler {
	c.customBuiltins = maps.Clone(builtins)
	return c
}

// WithUnsafeBuiltins is deprecated.
// Deprecated: Use WithCapabilities instead.
func (c *Compiler) WithUnsafeBuiltins(unsafeBuiltins map[string]struct{}) *Compiler {
	maps.Copy(c.unsafeBuiltinsMap, unsafeBuiltins)
	return c
}

// WithStrict toggles strict mode in the compiler.
func (c *Compiler) WithStrict(strict bool) *Compiler {
	c.strict = strict
	return c
}

// WithKeepModules enables retaining unprocessed modules in the compiler.
// Note that the modules aren't copied on the way in or out -- so when
// accessing them via ParsedModules(), mutations will occur in the module
// map that was passed into Compile().`
func (c *Compiler) WithKeepModules(y bool) *Compiler {
	c.keepModules = y
	return c
}

// WithUseTypeCheckAnnotations use schema annotations during type checking
func (c *Compiler) WithUseTypeCheckAnnotations(enabled bool) *Compiler {
	c.useTypeCheckAnnotations = enabled
	return c
}

func (c *Compiler) WithAllowUndefinedFunctionCalls(allow bool) *Compiler {
	c.allowUndefinedFuncCalls = allow
	return c
}

// WithEvalMode allows setting the CompilerEvalMode of the compiler
func (c *Compiler) WithEvalMode(e CompilerEvalMode) *Compiler {
	c.evalMode = e
	return c
}

// WithRewriteTestRules enables rewriting test rules to capture dynamic values in local variables,
// so they can be accessed by tracing.
func (c *Compiler) WithRewriteTestRules(rewrite bool) *Compiler {
	c.rewriteTestRulesForTracing = rewrite
	return c
}

// ParsedModules returns the parsed, unprocessed modules from the compiler.
// It is `nil` if keeping modules wasn't enabled via `WithKeepModules(true)`.
// The map includes all modules loaded via the ModuleLoader, if one was used.
func (c *Compiler) ParsedModules() map[string]*Module {
	return c.parsedModules
}

func (c *Compiler) QueryCompiler() QueryCompiler {
	c.init()
	c0 := *c
	return newQueryCompiler(&c0)
}

// Compile runs the compilation process on the input modules. The compiled
// version of the modules and associated data structures are stored on the
// compiler. If the compilation process fails for any reason, the compiler will
// contain a slice of errors.
func (c *Compiler) Compile(modules map[string]*Module) {

	c.init()

	c.Modules = make(map[string]*Module, len(modules))
	c.sorted = make([]string, 0, len(modules))

	if c.keepModules {
		c.parsedModules = make(map[string]*Module, len(modules))
	} else {
		c.parsedModules = nil
	}

	for k, v := range modules {
		c.Modules[k] = v.Copy()
		c.sorted = append(c.sorted, k)
		if c.parsedModules != nil {
			c.parsedModules[k] = v
		}
	}

	sort.Strings(c.sorted)

	c.compile()
}

// WithSchemas sets a schemaSet to the compiler
func (c *Compiler) WithSchemas(schemas *SchemaSet) *Compiler {
	c.schemaSet = schemas
	return c
}

// Failed returns true if a compilation error has been encountered.
func (c *Compiler) Failed() bool {
	return len(c.Errors) > 0
}

// ComprehensionIndex returns a data structure specifying how to index comprehension
// results so that callers do not have to recompute the comprehension more than once.
// If no index is found, returns nil.
func (c *Compiler) ComprehensionIndex(term *Term) *ComprehensionIndex {
	return c.comprehensionIndices[term]
}

// GetArity returns the number of args a function referred to by ref takes. If
// ref refers to built-in function, the built-in declaration is consulted,
// otherwise, the ref is used to perform a ruleset lookup.
func (c *Compiler) GetArity(ref Ref) int {
	if bi := c.builtins[ref.String()]; bi != nil {
		return bi.Decl.Arity()
	}
	rules := c.GetRulesExact(ref)
	if len(rules) == 0 {
		return -1
	}
	return len(rules[0].Head.Args)
}

// GetRulesExact returns a slice of rules referred to by the reference.
//
// E.g., given the following module:
//
//		package a.b.c
//
//		p[k] = v { ... }    # rule1
//	 p[k1] = v1 { ... }  # rule2
//
// The following calls yield the rules on the right.
//
//	GetRulesExact("data.a.b.c.p")   => [rule1, rule2]
//	GetRulesExact("data.a.b.c.p.x") => nil
//	GetRulesExact("data.a.b.c")     => nil
func (c *Compiler) GetRulesExact(ref Ref) (rules []*Rule) {
	node := c.RuleTree

	for _, x := range ref {
		if node = node.Child(x.Value); node == nil {
			return nil
		}
	}

	return extractRules(node.Values)
}

// GetRulesForVirtualDocument returns a slice of rules that produce the virtual
// document referred to by the reference.
//
// E.g., given the following module:
//
//		package a.b.c
//
//		p[k] = v { ... }    # rule1
//	 p[k1] = v1 { ... }  # rule2
//
// The following calls yield the rules on the right.
//
//	GetRulesForVirtualDocument("data.a.b.c.p")   => [rule1, rule2]
//	GetRulesForVirtualDocument("data.a.b.c.p.x") => [rule1, rule2]
//	GetRulesForVirtualDocument("data.a.b.c")     => nil
func (c *Compiler) GetRulesForVirtualDocument(ref Ref) (rules []*Rule) {

	node := c.RuleTree

	for _, x := range ref {
		if node = node.Child(x.Value); node == nil {
			return nil
		}
		if len(node.Values) > 0 {
			return extractRules(node.Values)
		}
	}

	return extractRules(node.Values)
}

// GetRulesWithPrefix returns a slice of rules that share the prefix ref.
//
// E.g., given the following module:
//
//	package a.b.c
//
//	p[x] = y { ... }  # rule1
//	p[k] = v { ... }  # rule2
//	q { ... }         # rule3
//
// The following calls yield the rules on the right.
//
//	GetRulesWithPrefix("data.a.b.c.p")   => [rule1, rule2]
//	GetRulesWithPrefix("data.a.b.c.p.a") => nil
//	GetRulesWithPrefix("data.a.b.c")     => [rule1, rule2, rule3]
func (c *Compiler) GetRulesWithPrefix(ref Ref) (rules []*Rule) {

	node := c.RuleTree

	for _, x := range ref {
		if node = node.Child(x.Value); node == nil {
			return nil
		}
	}

	var acc func(node *TreeNode)

	acc = func(node *TreeNode) {
		rules = append(rules, extractRules(node.Values)...)
		for _, child := range node.Children {
			if child.Hide {
				continue
			}
			acc(child)
		}
	}

	acc(node)

	return rules
}

func extractRules(s []any) []*Rule {
	rules := make([]*Rule, len(s))
	for i := range s {
		rules[i] = s[i].(*Rule)
	}
	return rules
}

// GetRules returns a slice of rules that are referred to by ref.
//
// E.g., given the following module:
//
//	package a.b.c
//
//	p[x] = y { q[x] = y; ... } # rule1
//	q[x] = y { ... }           # rule2
//
// The following calls yield the rules on the right.
//
//	GetRules("data.a.b.c.p")	=> [rule1]
//	GetRules("data.a.b.c.p.x")	=> [rule1]
//	GetRules("data.a.b.c.q")	=> [rule2]
//	GetRules("data.a.b.c")		=> [rule1, rule2]
//	GetRules("data.a.b.d")		=> nil
func (c *Compiler) GetRules(ref Ref) (rules []*Rule) {

	set := map[*Rule]struct{}{}

	for _, rule := range c.GetRulesForVirtualDocument(ref) {
		set[rule] = struct{}{}
	}

	for _, rule := range c.GetRulesWithPrefix(ref) {
		set[rule] = struct{}{}
	}

	for rule := range set {
		rules = append(rules, rule)
	}

	return rules
}

// GetRulesDynamic returns a slice of rules that could be referred to by a ref.
//
// Deprecated: use GetRulesDynamicWithOpts
func (c *Compiler) GetRulesDynamic(ref Ref) []*Rule {
	return c.GetRulesDynamicWithOpts(ref, RulesOptions{})
}

// GetRulesDynamicWithOpts returns a slice of rules that could be referred to by
// a ref.
// When parts of the ref are statically known, we use that information to narrow
// down which rules the ref could refer to, but in the most general case this
// will be an over-approximation.
//
// E.g., given the following modules:
//
//	package a.b.c
//
//	r1 = 1  # rule1
//
// and:
//
//	package a.d.c
//
//	r2 = 2  # rule2
//
// The following calls yield the rules on the right.
//
//	GetRulesDynamicWithOpts("data.a[x].c[y]", opts) => [rule1, rule2]
//	GetRulesDynamicWithOpts("data.a[x].c.r2", opts) => [rule2]
//	GetRulesDynamicWithOpts("data.a.b[x][y]", opts) => [rule1]
//
// Using the RulesOptions parameter, the inclusion of hidden modules can be
// controlled:
//
// With
//
//	package system.main
//
//	r3 = 3 # rule3
//
// We'd get this result:
//
//	GetRulesDynamicWithOpts("data[x]", RulesOptions{IncludeHiddenModules: true}) => [rule1, rule2, rule3]
//
// Without the options, it would be excluded.
func (c *Compiler) GetRulesDynamicWithOpts(ref Ref, opts RulesOptions) []*Rule {
	node := c.RuleTree

	set := map[*Rule]struct{}{}
	var walk func(node *TreeNode, i int)
	walk = func(node *TreeNode, i int) {
		switch {
		case i >= len(ref):
			// We've reached the end of the reference and want to collect everything
			// under this "prefix".
			node.DepthFirst(func(descendant *TreeNode) bool {
				insertRules(set, descendant.Values)
				if opts.IncludeHiddenModules {
					return false
				}
				return descendant.Hide
			})

		case i == 0 || IsConstant(ref[i].Value):
			// The head of the ref is always grounded.  In case another part of the
			// ref is also grounded, we can lookup the exact child.  If it's not found
			// we can immediately return...
			if child := node.Child(ref[i].Value); child != nil {
				if len(child.Values) > 0 {
					// Add any rules at this position
					insertRules(set, child.Values)
				}
				// There might still be "sub-rules" contributing key-value "overrides" for e.g. partial object rules, continue walking
				walk(child, i+1)
			} else {
				return
			}

		default:
			// This part of the ref is a dynamic term.  We can't know what it refers
			// to and will just need to try all of the children.
			for _, child := range node.Children {
				if child.Hide && !opts.IncludeHiddenModules {
					continue
				}
				insertRules(set, child.Values)
				walk(child, i+1)
			}
		}
	}

	walk(node, 0)
	rules := make([]*Rule, 0, len(set))
	for rule := range set {
		rules = append(rules, rule)
	}
	return rules
}

// Utility: add all rule values to the set.
func insertRules(set map[*Rule]struct{}, rules []any) {
	for _, rule := range rules {
		set[rule.(*Rule)] = struct{}{}
	}
}

// RuleIndex returns a RuleIndex built for the rule set referred to by path.
// The path must refer to the rule set exactly, i.e., given a rule set at path
// data.a.b.c.p, refs data.a.b.c.p.x and data.a.b.c would not return a
// RuleIndex built for the rule.
func (c *Compiler) RuleIndex(path Ref) RuleIndex {
	r, ok := c.ruleIndices.Get(path)
	if !ok {
		return nil
	}
	return r
}

// PassesTypeCheck determines whether the given body passes type checking
func (c *Compiler) PassesTypeCheck(body Body) bool {
	checker := newTypeChecker().WithSchemaSet(c.schemaSet).WithInputType(c.inputType)
	env := c.TypeEnv
	_, errs := checker.CheckBody(env, body)
	return len(errs) == 0
}

// PassesTypeCheckRules determines whether the given rules passes type checking
func (c *Compiler) PassesTypeCheckRules(rules []*Rule) Errors {
	elems := []util.T{}

	for _, rule := range rules {
		elems = append(elems, rule)
	}

	// Load the global input schema if one was provided.
	if c.schemaSet != nil {
		if schema := c.schemaSet.Get(SchemaRootRef); schema != nil {

			var allowNet []string
			if c.capabilities != nil {
				allowNet = c.capabilities.AllowNet
			}

			tpe, err := loadSchema(schema, allowNet)
			if err != nil {
				return Errors{NewError(TypeErr, nil, err.Error())} //nolint:govet
			}
			c.inputType = tpe
		}
	}

	var as *AnnotationSet
	if c.useTypeCheckAnnotations {
		as = c.annotationSet
	}

	checker := newTypeChecker().WithSchemaSet(c.schemaSet).WithInputType(c.inputType)

	if c.TypeEnv == nil {
		if c.capabilities == nil {
			c.capabilities = CapabilitiesForThisVersion()
		}

		c.builtins = make(map[string]*Builtin, len(c.capabilities.Builtins)+len(c.customBuiltins))

		for _, bi := range c.capabilities.Builtins {
			c.builtins[bi.Name] = bi
		}

		maps.Copy(c.builtins, c.customBuiltins)

		c.TypeEnv = checker.Env(c.builtins)
	}

	_, errs := checker.CheckTypes(c.TypeEnv, elems, as)
	return errs
}

// ModuleLoader defines the interface that callers can implement to enable lazy
// loading of modules during compilation.
type ModuleLoader func(resolved map[string]*Module) (parsed map[string]*Module, err error)

// WithModuleLoader sets f as the ModuleLoader on the compiler.
//
// The compiler will invoke the ModuleLoader after resolving all references in
// the current set of input modules. The ModuleLoader can return a new
// collection of parsed modules that are to be included in the compilation
// process. This process will repeat until the ModuleLoader returns an empty
// collection or an error. If an error is returned, compilation will stop
// immediately.
func (c *Compiler) WithModuleLoader(f ModuleLoader) *Compiler {
	c.moduleLoader = f
	return c
}

// WithDefaultRegoVersion sets the default Rego version to use when a module doesn't specify one;
// such as when it's hand-crafted instead of parsed.
func (c *Compiler) WithDefaultRegoVersion(regoVersion RegoVersion) *Compiler {
	c.defaultRegoVersion = regoVersion
	return c
}

func (c *Compiler) counterAdd(name string, n uint64) {
	if c.metrics == nil {
		return
	}
	c.metrics.Counter(name).Add(n)
}

func (c *Compiler) buildRuleIndices() {

	c.RuleTree.DepthFirst(func(node *TreeNode) bool {
		if len(node.Values) == 0 {
			return false
		}
		rules := extractRules(node.Values)
		hasNonGroundRef := false
		for _, r := range rules {
			hasNonGroundRef = !r.Head.Ref().IsGround()
		}
		if hasNonGroundRef {
			// Collect children to ensure that all rules within the extent of a rule with a general ref
			// are found on the same index. E.g. the following rules should be indexed under data.a.b.c:
			//
			// package a
			// b.c[x].e := 1 { x := input.x }
			// b.c.d := 2
			// b.c.d2.e[x] := 3 { x := input.x }
			for _, child := range node.Children {
				child.DepthFirst(func(c *TreeNode) bool {
					rules = append(rules, extractRules(c.Values)...)
					return false
				})
			}
		}

		index := newBaseDocEqIndex(func(ref Ref) bool {
			return isVirtual(c.RuleTree, ref.GroundPrefix())
		})
		if index.Build(rules) {
			c.ruleIndices.Put(rules[0].Ref().GroundPrefix(), index)
		}
		return hasNonGroundRef // currently, we don't allow those branches to go deeper
	})

}

func (c *Compiler) buildComprehensionIndices() {
	for _, name := range c.sorted {
		WalkRules(c.Modules[name], func(r *Rule) bool {
			candidates := ReservedVars.Copy()
			if len(r.Head.Args) > 0 {
				candidates.Update(r.Head.Args.Vars())
			}
			n := buildComprehensionIndices(c.debug, c.GetArity, candidates, c.RewrittenVars, r.Body, c.comprehensionIndices)
			c.counterAdd(compileStageComprehensionIndexBuild, n)
			return false
		})
	}
}

var futureKeywordsPrefix = Ref{FutureRootDocument, InternedTerm("keywords")}

// buildRequiredCapabilities updates the required capabilities on the compiler
// to include any keyword and feature dependencies present in the modules. The
// built-in function dependencies will have already been added by the type
// checker.
func (c *Compiler) buildRequiredCapabilities() {

	features := map[string]struct{}{}

	// extract required keywords from modules

	keywords := map[string]struct{}{}

	for _, name := range c.sorted {
		for _, imp := range c.imports[name] {
			mod := c.Modules[name]
			path := imp.Path.Value.(Ref)
			switch {
			case path.Equal(RegoV1CompatibleRef):
				if !c.moduleIsRegoV1(mod) {
					features[FeatureRegoV1Import] = struct{}{}
				}
			case path.HasPrefix(futureKeywordsPrefix):
				if len(path) == 2 {
					if c.moduleIsRegoV1(mod) {
						for kw := range futureKeywords {
							keywords[kw] = struct{}{}
						}
					} else {
						for kw := range allFutureKeywords {
							keywords[kw] = struct{}{}
						}
					}
				} else {
					kw := string(path[2].Value.(String))
					if c.moduleIsRegoV1(mod) {
						for allowedKw := range futureKeywords {
							if kw == allowedKw {
								keywords[kw] = struct{}{}
								break
							}
						}
					} else {
						for allowedKw := range allFutureKeywords {
							if kw == allowedKw {
								keywords[kw] = struct{}{}
								break
							}
						}
					}
				}
			}
		}
	}

	c.Required.FutureKeywords = util.KeysSorted(keywords)

	// extract required features from modules

	for _, name := range c.sorted {
		mod := c.Modules[name]

		if c.moduleIsRegoV1(mod) {
			features[FeatureRegoV1] = struct{}{}
		} else {
			for _, rule := range mod.Rules {
				refLen := len(rule.Head.Reference)
				if refLen >= 3 {
					if refLen > len(rule.Head.Reference.ConstantPrefix()) {
						features[FeatureRefHeads] = struct{}{}
					} else {
						features[FeatureRefHeadStringPrefixes] = struct{}{}
					}
				}
			}
		}
	}

	c.Required.Features = util.KeysSorted(features)

	for i, bi := range c.Required.Builtins {
		c.Required.Builtins[i] = bi.Minimal()
	}
}

// checkRecursion ensures that there are no recursive definitions, i.e., there are
// no cycles in the Graph.
func (c *Compiler) checkRecursion() {
	eq := func(a, b util.T) bool {
		return a.(*Rule) == b.(*Rule)
	}

	c.RuleTree.DepthFirst(func(node *TreeNode) bool {
		for _, rule := range node.Values {
			for node := rule.(*Rule); node != nil; node = node.Else {
				c.checkSelfPath(node.Loc(), eq, node, node)
			}
		}
		return false
	})
}

func (c *Compiler) checkSelfPath(loc *Location, eq func(a, b util.T) bool, a, b util.T) {
	tr := NewGraphTraversal(c.Graph)
	if p := util.DFSPath(tr, eq, a, b); len(p) > 0 {
		n := make([]string, 0, len(p))
		for _, x := range p {
			n = append(n, astNodeToString(x))
		}
		c.err(NewError(RecursionErr, loc, "rule %v is recursive: %v", astNodeToString(a), strings.Join(n, " -> ")))
	}
}

func astNodeToString(x any) string {
	return x.(*Rule).Ref().String()
}

// checkRuleConflicts ensures that rules definitions are not in conflict.
func (c *Compiler) checkRuleConflicts() {
	rw := rewriteVarsInRef(c.RewrittenVars)

	c.RuleTree.DepthFirst(func(node *TreeNode) bool {
		if len(node.Values) == 0 {
			return false // go deeper
		}

		kinds := make(map[RuleKind]struct{}, len(node.Values))
		completeRules := 0
		partialRules := 0
		arities := make(map[int]struct{}, len(node.Values))
		name := ""
		var conflicts []Ref
		defaultRules := make([]*Rule, 0)

		for _, rule := range node.Values {
			r := rule.(*Rule)
			ref := r.Ref()
			name = rw(ref.CopyNonGround()).String() // varRewriter operates in-place
			kinds[r.Head.RuleKind()] = struct{}{}
			arities[len(r.Head.Args)] = struct{}{}
			if r.Default {
				defaultRules = append(defaultRules, r)
			}

			// Single-value rules may not have any other rules in their extent.
			// Rules with vars in their ref are allowed to have rules inside their extent.
			// Only the ground portion (terms before the first var term) of a rule's ref is considered when determining
			// whether it's inside the extent of another (c.RuleTree is organized this way already).
			// These pairs are invalid:
			//
			//   data.p.q.r { true }          # data.p.q is { "r": true }
			//   data.p.q.r.s { true }
			//
			//   data.p.q.r { true }
			//   data.p.q.r[s].t { s = input.key }
			//
			// But this is allowed:
			//
			//   data.p.q.r { true }
			//   data.p.q[r].s.t { r = input.key }
			//
			//   data.p[r] := x { r = input.key; x = input.bar }
			//   data.p.q[r] := x { r = input.key; x = input.bar }
			//
			//   data.p.q[r] { r := input.r }
			//   data.p.q.r.s { true }
			//
			//   data.p.q[r] = 1 { r := "r" }
			//   data.p.q.s = 2
			//
			//   data.p[q][r] { q := input.q; r := input.r }
			//   data.p.q.r { true }
			//
			//   data.p.q[r] { r := input.r }
			//   data.p[q].r { q := input.q }
			//
			//   data.p.q[r][s] { r := input.r; s := input.s }
			//   data.p[q].r.s { q := input.q }

			if ref.IsGround() && len(node.Children) > 0 {
				conflicts = node.flattenChildren()
			}

			if r.Head.RuleKind() == SingleValue && r.Head.Ref().IsGround() {
				completeRules++
			} else {
				partialRules++
			}
		}

		switch {
		case conflicts != nil:
			c.err(NewError(TypeErr, node.Values[0].(*Rule).Loc(), "rule %v conflicts with %v", name, conflicts))

		case len(kinds) > 1 || len(arities) > 1 || (completeRules >= 1 && partialRules >= 1):
			c.err(NewError(TypeErr, node.Values[0].(*Rule).Loc(), "conflicting rules %v found", name))

		case len(defaultRules) > 1:

			defaultRuleLocations := strings.Builder{}
			defaultRuleLocations.WriteString(defaultRules[0].Loc().String())
			for i := 1; i < len(defaultRules); i++ {
				defaultRuleLocations.WriteString(", ")
				defaultRuleLocations.WriteString(defaultRules[i].Loc().String())
			}

			c.err(NewError(
				TypeErr,
				defaultRules[0].Module.Package.Loc(),
				"multiple default rules %s found at %s",
				name, defaultRuleLocations.String()),
			)
		}

		return false
	})

	if c.pathExists != nil {
		for _, err := range CheckPathConflicts(c, c.pathExists) {
			c.err(err)
		}
	}

	// NOTE(sr): depthfirst might better use sorted for stable errs?
	c.ModuleTree.DepthFirst(func(node *ModuleTreeNode) bool {
		for _, mod := range node.Modules {
			for _, rule := range mod.Rules {
				ref := rule.Head.Ref().GroundPrefix()
				// Rules with a dynamic portion in their ref are exempted, as a conflict within the dynamic portion
				// can only be detected at eval-time.
				if len(ref) < len(rule.Head.Ref()) {
					continue
				}

				childNode, tail := node.find(ref)
				if childNode != nil && len(tail) == 0 {
					for _, childMod := range childNode.Modules {
						// Avoid recursively checking a module for equality unless we know it's a possible self-match.
						if childMod.Equal(mod) {
							continue // don't self-conflict
						}
						msg := fmt.Sprintf("%v conflicts with rule %v defined at %v", childMod.Package, rule.Head.Ref(), rule.Loc())
						c.err(NewError(TypeErr, mod.Package.Loc(), msg)) //nolint:govet
					}
				}
			}
		}
		return false
	})
}

func (c *Compiler) checkUndefinedFuncs() {
	for _, name := range c.sorted {
		m := c.Modules[name]
		for _, err := range checkUndefinedFuncs(c.TypeEnv, m, c.GetArity, c.RewrittenVars) {
			c.err(err)
		}
	}
}

func checkUndefinedFuncs(env *TypeEnv, x any, arity func(Ref) int, rwVars map[Var]Var) Errors {

	var errs Errors

	WalkExprs(x, func(expr *Expr) bool {
		if !expr.IsCall() {
			return false
		}
		ref := expr.Operator()
		if arity := arity(ref); arity >= 0 {
			operands := len(expr.Operands())
			if expr.Generated { // an output var was added
				if !expr.IsEquality() && operands != arity+1 {
					ref = rewriteVarsInRef(rwVars)(ref)
					errs = append(errs, arityMismatchError(env, ref, expr, arity, operands-1))
					return true
				}
			} else { // either output var or not
				if operands != arity && operands != arity+1 {
					ref = rewriteVarsInRef(rwVars)(ref)
					errs = append(errs, arityMismatchError(env, ref, expr, arity, operands))
					return true
				}
			}
			return false
		}
		ref = rewriteVarsInRef(rwVars)(ref)
		errs = append(errs, NewError(TypeErr, expr.Loc(), "undefined function %v", ref))
		return true
	})

	return errs
}

func arityMismatchError(env *TypeEnv, f Ref, expr *Expr, exp, act int) *Error {
	if want, ok := env.Get(f).(*types.Function); ok { // generate richer error for built-in functions
		have := make([]types.Type, len(expr.Operands()))
		for i, op := range expr.Operands() {
			have[i] = env.Get(op)
		}
		return newArgError(expr.Loc(), f, "arity mismatch", have, want.NamedFuncArgs())
	}
	if act != 1 {
		return NewError(TypeErr, expr.Loc(), "function %v has arity %d, got %d arguments", f, exp, act)
	}
	return NewError(TypeErr, expr.Loc(), "function %v has arity %d, got %d argument", f, exp, act)
}

// checkSafetyRuleBodies ensures that variables appearing in negated expressions or non-target
// positions of built-in expressions will be bound when evaluating the rule from left
// to right, re-ordering as necessary.
func (c *Compiler) checkSafetyRuleBodies() {
	for _, name := range c.sorted {
		m := c.Modules[name]
		WalkRules(m, func(r *Rule) bool {
			safe := ReservedVars.Copy()
			if len(r.Head.Args) > 0 {
				safe.Update(r.Head.Args.Vars())
			}
			r.Body = c.checkBodySafety(safe, r.Body)
			return false
		})
	}
}

func (c *Compiler) checkBodySafety(safe VarSet, b Body) Body {
	reordered, unsafe := reorderBodyForSafety(c.builtins, c.GetArity, safe, b)
	if errs := safetyErrorSlice(unsafe, c.RewrittenVars); len(errs) > 0 {
		for _, err := range errs {
			c.err(err)
		}
		return b
	}
	return reordered
}

// SafetyCheckVisitorParams defines the AST visitor parameters to use for collecting
// variables during the safety check. This has to be exported because it's relied on
// by the copy propagation implementation in topdown.
var SafetyCheckVisitorParams = VarVisitorParams{
	SkipRefCallHead: true,
	SkipClosures:    true,
}

// checkSafetyRuleHeads ensures that variables appearing in the head of a
// rule also appear in the body.
func (c *Compiler) checkSafetyRuleHeads() {
	for _, name := range c.sorted {
		WalkRules(c.Modules[name], func(r *Rule) bool {
			safe := r.Body.Vars(SafetyCheckVisitorParams)
			if len(r.Head.Args) > 0 {
				safe.Update(r.Head.Args.Vars())
			}
			if headMayHaveVars(r.Head) {
				vars := r.Head.Vars()
				if vars.DiffCount(safe) > 0 {
					unsafe := vars.Diff(safe)
					for v := range unsafe {
						if w, ok := c.RewrittenVars[v]; ok {
							v = w
						}
						if !v.IsGenerated() {
							c.err(NewError(UnsafeVarErr, r.Loc(), "var %v is unsafe", v))
						}
					}
				}
			}
			return false
		})
	}
}

func compileSchema(goSchema any, allowNet []string) (*gojsonschema.Schema, error) {
	gojsonschema.SetAllowNet(allowNet)

	var refLoader gojsonschema.JSONLoader
	sl := gojsonschema.NewSchemaLoader()

	if goSchema != nil {
		refLoader = gojsonschema.NewGoLoader(goSchema)
	} else {
		return nil, errors.New("no schema as input to compile")
	}
	schemasCompiled, err := sl.Compile(refLoader)
	if err != nil {
		return nil, fmt.Errorf("unable to compile the schema: %w", err)
	}
	return schemasCompiled, nil
}

func mergeSchemas(schemas ...*gojsonschema.SubSchema) (*gojsonschema.SubSchema, error) {
	if len(schemas) == 0 {
		return nil, nil
	}
	var result = schemas[0]

	for i := range schemas {
		if len(schemas[i].PropertiesChildren) > 0 {
			if !schemas[i].Types.Contains("object") {
				if err := schemas[i].Types.Add("object"); err != nil {
					return nil, errors.New("unable to set the type in schemas")
				}
			}
		} else if len(schemas[i].ItemsChildren) > 0 {
			if !schemas[i].Types.Contains("array") {
				if err := schemas[i].Types.Add("array"); err != nil {
					return nil, errors.New("unable to set the type in schemas")
				}
			}
		}
	}

	for i := 1; i < len(schemas); i++ {
		if result.Types.String() != schemas[i].Types.String() {
			return nil, fmt.Errorf("unable to merge these schemas: type mismatch: %v and %v", result.Types.String(), schemas[i].Types.String())
		} else if result.Types.Contains("object") && len(result.PropertiesChildren) > 0 && schemas[i].Types.Contains("object") && len(schemas[i].PropertiesChildren) > 0 {
			result.PropertiesChildren = append(result.PropertiesChildren, schemas[i].PropertiesChildren...)
		} else if result.Types.Contains("array") && len(result.ItemsChildren) > 0 && schemas[i].Types.Contains("array") && len(schemas[i].ItemsChildren) > 0 {
			for j := range len(schemas[i].ItemsChildren) {
				if len(result.ItemsChildren)-1 < j && !(len(schemas[i].ItemsChildren)-1 < j) {
					result.ItemsChildren = append(result.ItemsChildren, schemas[i].ItemsChildren[j])
				}
				if result.ItemsChildren[j].Types.String() != schemas[i].ItemsChildren[j].Types.String() {
					return nil, errors.New("unable to merge these schemas")
				}
			}
		}
	}
	return result, nil
}

type schemaParser struct {
	definitionCache map[string]*cachedDef
}

type cachedDef struct {
	properties []*types.StaticProperty
}

func newSchemaParser() *schemaParser {
	return &schemaParser{
		definitionCache: map[string]*cachedDef{},
	}
}

func (parser *schemaParser) parseSchema(schema any) (types.Type, error) {
	return parser.parseSchemaWithPropertyKey(schema, "")
}

func (parser *schemaParser) parseSchemaWithPropertyKey(schema any, propertyKey string) (types.Type, error) {
	subSchema, ok := schema.(*gojsonschema.SubSchema)
	if !ok {
		return nil, fmt.Errorf("unexpected schema type %v", subSchema)
	}

	// Handle referenced schemas, returns directly when a $ref is found
	if subSchema.RefSchema != nil {
		if existing, ok := parser.definitionCache[subSchema.Ref.String()]; ok {
			return types.NewObject(existing.properties, nil), nil
		}
		return parser.parseSchemaWithPropertyKey(subSchema.RefSchema, subSchema.Ref.String())
	}

	// Handle anyOf
	if subSchema.AnyOf != nil {
		var orType types.Type

		// If there is a core schema, find its type first
		if subSchema.Types.IsTyped() {
			copySchema := *subSchema
			copySchemaRef := &copySchema
			copySchemaRef.AnyOf = nil
			coreType, err := parser.parseSchema(copySchemaRef)
			if err != nil {
				return nil, fmt.Errorf("unexpected schema type %v: %w", subSchema, err)
			}

			// Only add Object type with static props to orType
			if objType, ok := coreType.(*types.Object); ok {
				if objType.StaticProperties() != nil && objType.DynamicProperties() == nil {
					orType = types.Or(orType, coreType)
				}
			}
		}

		// Iterate through every property of AnyOf and add it to orType
		for _, pSchema := range subSchema.AnyOf {
			newtype, err := parser.parseSchema(pSchema)
			if err != nil {
				return nil, fmt.Errorf("unexpected schema type %v: %w", pSchema, err)
			}
			orType = types.Or(newtype, orType)
		}

		return orType, nil
	}

	if subSchema.AllOf != nil {
		subSchemaArray := subSchema.AllOf
		allOfResult, err := mergeSchemas(subSchemaArray...)
		if err != nil {
			return nil, err
		}

		if subSchema.Types.IsTyped() {
			if (subSchema.Types.Contains("object") && allOfResult.Types.Contains("object")) || (subSchema.Types.Contains("array") && allOfResult.Types.Contains("array")) {
				objectOrArrayResult, err := mergeSchemas(allOfResult, subSchema)
				if err != nil {
					return nil, err
				}
				return parser.parseSchema(objectOrArrayResult)
			} else if subSchema.Types.String() != allOfResult.Types.String() {
				return nil, errors.New("unable to merge these schemas")
			}
		}
		return parser.parseSchema(allOfResult)
	}

	if subSchema.Types.IsTyped() {
		if subSchema.Types.Contains("boolean") {
			return types.B, nil

		} else if subSchema.Types.Contains("string") {
			return types.S, nil

		} else if subSchema.Types.Contains("integer") || subSchema.Types.Contains("number") {
			return types.N, nil

		} else if subSchema.Types.Contains("object") {
			if len(subSchema.PropertiesChildren) > 0 {
				def := &cachedDef{
					properties: make([]*types.StaticProperty, 0, len(subSchema.PropertiesChildren)),
				}
				for _, pSchema := range subSchema.PropertiesChildren {
					def.properties = append(def.properties, types.NewStaticProperty(pSchema.Property, nil))
				}
				if propertyKey != "" {
					parser.definitionCache[propertyKey] = def
				}
				for _, pSchema := range subSchema.PropertiesChildren {
					newtype, err := parser.parseSchema(pSchema)
					if err != nil {
						return nil, fmt.Errorf("unexpected schema type %v: %w", pSchema, err)
					}
					for i, prop := range def.properties {
						if prop.Key == pSchema.Property {
							def.properties[i].Value = newtype
							break
						}
					}
				}
				return types.NewObject(def.properties, nil), nil
			}
			return types.NewObject(nil, types.NewDynamicProperty(types.A, types.A)), nil

		} else if subSchema.Types.Contains("array") {
			if len(subSchema.ItemsChildren) > 0 {
				if subSchema.ItemsChildrenIsSingleSchema {
					iSchema := subSchema.ItemsChildren[0]
					newtype, err := parser.parseSchema(iSchema)
					if err != nil {
						return nil, fmt.Errorf("unexpected schema type %v", iSchema)
					}
					return types.NewArray(nil, newtype), nil
				}
				newTypes := make([]types.Type, 0, len(subSchema.ItemsChildren))
				for i := 0; i != len(subSchema.ItemsChildren); i++ {
					iSchema := subSchema.ItemsChildren[i]
					newtype, err := parser.parseSchema(iSchema)
					if err != nil {
						return nil, fmt.Errorf("unexpected schema type %v", iSchema)
					}
					newTypes = append(newTypes, newtype)
				}
				return types.NewArray(newTypes, nil), nil
			}
			return types.NewArray(nil, types.A), nil
		}
	}

	// Assume types if not specified in schema
	if len(subSchema.PropertiesChildren) > 0 {
		if err := subSchema.Types.Add("object"); err == nil {
			return parser.parseSchema(subSchema)
		}
	} else if len(subSchema.ItemsChildren) > 0 {
		if err := subSchema.Types.Add("array"); err == nil {
			return parser.parseSchema(subSchema)
		}
	}

	return types.A, nil
}

func (c *Compiler) setAnnotationSet() {
	// Sorting modules by name for stable error reporting
	sorted := make([]*Module, 0, len(c.Modules))
	for _, mName := range c.sorted {
		sorted = append(sorted, c.Modules[mName])
	}

	as, errs := BuildAnnotationSet(sorted)
	for _, err := range errs {
		c.err(err)
	}
	c.annotationSet = as
}

// checkTypes runs the type checker on all rules. The type checker builds a
// TypeEnv that is stored on the compiler.
func (c *Compiler) checkTypes() {
	// Recursion is caught in earlier step, so this cannot fail.
	sorted, _ := c.Graph.Sort()
	checker := newTypeChecker().
		WithAllowNet(c.capabilities.AllowNet).
		WithSchemaSet(c.schemaSet).
		WithInputType(c.inputType).
		WithBuiltins(c.builtins).
		WithRequiredCapabilities(c.Required).
		WithVarRewriter(rewriteVarsInRef(c.RewrittenVars)).
		WithAllowUndefinedFunctionCalls(c.allowUndefinedFuncCalls)
	var as *AnnotationSet
	if c.useTypeCheckAnnotations {
		as = c.annotationSet
	}
	env, errs := checker.CheckTypes(c.TypeEnv, sorted, as)
	for _, err := range errs {
		c.err(err)
	}
	c.TypeEnv = env
}

func (c *Compiler) checkUnsafeBuiltins() {
	if len(c.unsafeBuiltinsMap) == 0 {
		return
	}

	for _, name := range c.sorted {
		errs := checkUnsafeBuiltins(c.unsafeBuiltinsMap, c.Modules[name])
		for _, err := range errs {
			c.err(err)
		}
	}
}

func (c *Compiler) checkDeprecatedBuiltins() {
	checkNeeded := false
	for _, b := range c.Required.Builtins {
		if _, found := c.deprecatedBuiltinsMap[b.Name]; found {
			checkNeeded = true
			break
		}
	}
	if !checkNeeded {
		return
	}

	for _, name := range c.sorted {
		mod := c.Modules[name]
		if c.strict || mod.regoV1Compatible() {
			errs := checkDeprecatedBuiltins(c.deprecatedBuiltinsMap, mod)
			for _, err := range errs {
				c.err(err)
			}
		}
	}
}

func (c *Compiler) runStage(metricName string, f func()) {
	if c.metrics != nil {
		c.metrics.Timer(metricName).Start()
		defer c.metrics.Timer(metricName).Stop()
	}
	f()
}

func (c *Compiler) runStageAfter(metricName string, s CompilerStage) *Error {
	if c.metrics != nil {
		c.metrics.Timer(metricName).Start()
		defer c.metrics.Timer(metricName).Stop()
	}
	return s(c)
}

func (c *Compiler) compile() {

	defer func() {
		if r := recover(); r != nil && r != errLimitReached {
			panic(r)
		}
	}()

	for _, s := range c.stages {
		if c.evalMode == EvalModeIR {
			switch s.name {
			case "BuildRuleIndices", "BuildComprehensionIndices":
				continue // skip these stages
			}
		}

		if c.allowUndefinedFuncCalls && (s.name == "CheckUndefinedFuncs" || s.name == "CheckSafetyRuleBodies") {
			continue
		}

		c.runStage(s.metricName, s.f)
		if c.Failed() {
			return
		}
		for _, a := range c.after[s.name] {
			if err := c.runStageAfter(a.MetricName, a.Stage); err != nil {
				c.err(err)
				return
			}
		}
	}
}

func (c *Compiler) init() {

	if c.initialized {
		return
	}

	if defaultModuleLoader != nil {
		if c.moduleLoader == nil {
			c.moduleLoader = defaultModuleLoader
		} else {
			first := c.moduleLoader
			c.moduleLoader = func(res map[string]*Module) (map[string]*Module, error) {
				res0, err := first(res)
				if err != nil {
					return nil, err
				}
				res1, err := defaultModuleLoader(res)
				if err != nil {
					return nil, err
				}
				// merge res1 into res0, based on module "file" names, to avoid clashes
				for k, v := range res1 {
					if _, ok := res0[k]; !ok {
						res0[k] = v
					}
				}
				return res0, nil
			}
		}
	}

	if c.capabilities == nil {
		c.capabilities = CapabilitiesForThisVersion()
	}

	c.builtins = make(map[string]*Builtin, len(c.capabilities.Builtins)+len(c.customBuiltins))

	for _, bi := range c.capabilities.Builtins {
		c.builtins[bi.Name] = bi
		if bi.IsDeprecated() {
			c.deprecatedBuiltinsMap[bi.Name] = struct{}{}
		}
	}

	maps.Copy(c.builtins, c.customBuiltins)

	// Load the global input schema if one was provided.
	if c.schemaSet != nil {
		if schema := c.schemaSet.Get(SchemaRootRef); schema != nil {
			tpe, err := loadSchema(schema, c.capabilities.AllowNet)
			if err != nil {
				c.err(NewError(TypeErr, nil, err.Error())) //nolint:govet
			} else {
				c.inputType = tpe
			}
		}
	}

	c.TypeEnv = newTypeChecker().
		WithSchemaSet(c.schemaSet).
		WithInputType(c.inputType).
		Env(c.builtins)

	c.initialized = true
}

func (c *Compiler) err(err *Error) {
	if c.maxErrs > 0 && len(c.Errors) >= c.maxErrs {
		c.Errors = append(c.Errors, errLimitReached)
		panic(errLimitReached)
	}
	c.Errors = append(c.Errors, err)
}

func (c *Compiler) getExports() *util.HasherMap[Ref, []Ref] {

	rules := util.NewHasherMap[Ref, []Ref](RefEqual)

	for _, name := range c.sorted {
		mod := c.Modules[name]

		for _, rule := range mod.Rules {
			hashMapAdd(rules, mod.Package.Path, rule.Head.Ref().GroundPrefix())
		}
	}

	return rules
}

func refSliceEqual(a, b []Ref) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !a[i].Equal(b[i]) {
			return false
		}
	}
	return true
}

func hashMapAdd(rules *util.HasherMap[Ref, []Ref], pkg, rule Ref) {
	prev, ok := rules.Get(pkg)
	if !ok {
		rules.Put(pkg, []Ref{rule})
		return
	}
	for _, p := range prev {
		if p.Equal(rule) {
			return
		}
	}
	rules.Put(pkg, append(prev, rule))
}

func (c *Compiler) GetAnnotationSet() *AnnotationSet {
	return c.annotationSet
}

func (c *Compiler) checkImports() {
	modules := make([]*Module, 0, len(c.Modules))

	supportsRegoV1Import := c.capabilities.ContainsFeature(FeatureRegoV1Import) ||
		c.capabilities.ContainsFeature(FeatureRegoV1)

	for _, name := range c.sorted {
		mod := c.Modules[name]

		for _, imp := range mod.Imports {
			if !supportsRegoV1Import && RegoV1CompatibleRef.Equal(imp.Path.Value) {
				c.err(NewError(CompileErr, imp.Loc(), "rego.v1 import is not supported"))
			}
		}

		if c.strict || c.moduleIsRegoV1Compatible(mod) {
			modules = append(modules, mod)
		}
	}

	errs := checkDuplicateImports(modules)
	for _, err := range errs {
		c.err(err)
	}
}

func (c *Compiler) checkKeywordOverrides() {
	for _, name := range c.sorted {
		mod := c.Modules[name]
		if c.strict || c.moduleIsRegoV1Compatible(mod) {
			errs := checkRootDocumentOverrides(mod)
			for _, err := range errs {
				c.err(err)
			}
		}
	}
}

func (c *Compiler) moduleIsRegoV1(mod *Module) bool {
	if mod.regoVersion == RegoUndefined {
		switch c.defaultRegoVersion {
		case RegoUndefined:
			c.err(NewError(CompileErr, mod.Package.Loc(), "cannot determine rego version for module"))
			return false
		case RegoV1:
			return true
		}
		return false
	}
	return mod.regoVersion == RegoV1
}

func (c *Compiler) moduleIsRegoV1Compatible(mod *Module) bool {
	if mod.regoVersion == RegoUndefined {
		switch c.defaultRegoVersion {
		case RegoUndefined:
			c.err(NewError(CompileErr, mod.Package.Loc(), "cannot determine rego version for module"))
			return false
		case RegoV1, RegoV0CompatV1:
			return true
		}
		return false
	}
	return mod.regoV1Compatible()
}

// resolveAllRefs resolves references in expressions to their fully qualified values.
//
// For instance, given the following module:
//
// package a.b
// import data.foo.bar
// p[x] { bar[_] = x }
//
// The reference "bar[_]" would be resolved to "data.foo.bar[_]".
//
// Ref rules are resolved, too:
//
// package a.b
// q { c.d.e == 1 }
// c.d[e] := 1 if e := "e"
//
// The reference "c.d.e" would be resolved to "data.a.b.c.d.e".
func (c *Compiler) resolveAllRefs() {

	rules := c.getExports()

	for _, name := range c.sorted {
		mod := c.Modules[name]

		var ruleExports []Ref
		if x, ok := rules.Get(mod.Package.Path); ok {
			ruleExports = x
		}

		globals := getGlobals(mod.Package, ruleExports, mod.Imports)

		WalkRules(mod, func(rule *Rule) bool {
			err := resolveRefsInRule(globals, rule)
			if err != nil {
				c.err(NewError(CompileErr, rule.Location, err.Error())) //nolint:govet
			}
			return false
		})

		if c.strict { // check for unused imports
			for _, imp := range mod.Imports {
				path := imp.Path.Value.(Ref)
				if FutureRootDocument.Equal(path[0]) || RegoRootDocument.Equal(path[0]) {
					continue // ignore future and rego imports
				}

				for v, u := range globals {
					if v.Equal(imp.Name()) && !u.used {
						c.err(NewError(CompileErr, imp.Location, "%s unused", imp.String()))
					}
				}
			}
		}
	}

	if c.moduleLoader != nil {

		parsed, err := c.moduleLoader(c.Modules)
		if err != nil {
			c.err(NewError(CompileErr, nil, err.Error())) //nolint:govet
			return
		}

		if len(parsed) == 0 {
			return
		}

		for id, module := range parsed {
			c.Modules[id] = module.Copy()
			c.sorted = append(c.sorted, id)
			if c.parsedModules != nil {
				c.parsedModules[id] = module
			}
		}

		sort.Strings(c.sorted)
		c.resolveAllRefs()
	}
}

func (c *Compiler) removeImports() {
	c.imports = make(map[string][]*Import, len(c.Modules))
	for name := range c.Modules {
		c.imports[name] = c.Modules[name].Imports
		c.Modules[name].Imports = nil
	}
}

func (c *Compiler) initLocalVarGen() {
	c.localvargen = newLocalVarGeneratorForModuleSet(c.sorted, c.Modules)
}

func (c *Compiler) rewriteComprehensionTerms() {
	f := newEqualityFactory(c.localvargen)
	for _, name := range c.sorted {
		mod := c.Modules[name]
		_, _ = rewriteComprehensionTerms(f, mod) // ignore error
	}
}

func (c *Compiler) rewriteExprTerms() {
	for _, name := range c.sorted {
		mod := c.Modules[name]
		WalkRules(mod, func(rule *Rule) bool {
			rewriteExprTermsInHead(c.localvargen, rule)
			rule.Body = rewriteExprTermsInBody(c.localvargen, rule.Body)
			return false
		})
	}
}

func (c *Compiler) rewriteRuleHeadRefs() {
	f := newEqualityFactory(c.localvargen)
	for _, name := range c.sorted {
		WalkRules(c.Modules[name], func(rule *Rule) bool {

			ref := rule.Head.Ref()
			// NOTE(sr): We're backfilling Refs here -- all parser code paths would have them, but
			//           it's possible to construct Module{} instances from Golang code, so we need
			//           to accommodate for that, too.
			if len(rule.Head.Reference) == 0 {
				rule.Head.Reference = ref
			}

			cannotSpeakStringPrefixRefs := true
			cannotSpeakGeneralRefs := true
			for _, f := range c.capabilities.Features {
				switch f {
				case FeatureRefHeadStringPrefixes:
					cannotSpeakStringPrefixRefs = false
				case FeatureRefHeads:
					cannotSpeakGeneralRefs = false
				case FeatureRegoV1:
					cannotSpeakStringPrefixRefs = false
					cannotSpeakGeneralRefs = false
				}
			}

			if cannotSpeakStringPrefixRefs && cannotSpeakGeneralRefs && rule.Head.Name == "" {
				c.err(NewError(CompileErr, rule.Loc(), "rule heads with refs are not supported: %v", rule.Head.Reference))
				return true
			}

			for i := 1; i < len(ref); i++ {
				if cannotSpeakGeneralRefs && (rule.Head.RuleKind() == MultiValue || i != len(ref)-1) { // last
					if _, ok := ref[i].Value.(String); !ok {
						c.err(NewError(TypeErr, rule.Loc(), "rule heads with general refs (containing variables) are not supported: %v", rule.Head.Reference))
						continue
					}
				}

				// Rewrite so that any non-scalar elements in the rule's ref are vars:
				//     p.q.r[y.z] { ... }  =>  p.q.r[__local0__] { __local0__ = y.z }
				//     p.q[a.b][c.d] { ... }  =>  p.q[__local0__] { __local0__ = a.b; __local1__ = c.d }
				// because that's what the RuleTree knows how to deal with.
				if _, ok := ref[i].Value.(Var); !ok && !IsScalar(ref[i].Value) {
					expr := f.Generate(ref[i])
					if i == len(ref)-1 && rule.Head.Key.Equal(ref[i]) {
						rule.Head.Key = expr.Operand(0)
					}
					rule.Head.Reference[i] = expr.Operand(0)
					rule.Body.Append(expr)
				}
			}

			return true
		})
	}
}

func (c *Compiler) checkVoidCalls() {
	for _, name := range c.sorted {
		mod := c.Modules[name]
		for _, err := range checkVoidCalls(c.TypeEnv, mod) {
			c.err(err)
		}
	}
}

func (c *Compiler) rewritePrintCalls() {
	var modified bool
	if !c.enablePrintStatements {
		for _, name := range c.sorted {
			if erasePrintCalls(c.Modules[name]) {
				modified = true
			}
		}
	} else {
		for _, name := range c.sorted {
			mod := c.Modules[name]
			WalkRules(mod, func(r *Rule) bool {
				safe := r.Head.Args.Vars()
				safe.Update(ReservedVars)
				vis := func(b Body) bool {
					modrec, errs := rewritePrintCalls(c.localvargen, c.GetArity, safe, b)
					if modrec {
						modified = true
					}
					for _, err := range errs {
						c.err(err)
					}
					return false
				}
				WalkBodies(r.Head, vis)
				WalkBodies(r.Body, vis)
				return false
			})
		}
	}
	if modified {
		c.Required.addBuiltinSorted(Print)
	}
}

// checkVoidCalls returns errors for any expressions that treat void function
// calls as values. The only void functions in Rego are specific built-ins like
// print().
func checkVoidCalls(env *TypeEnv, x any) Errors {
	var errs Errors
	WalkTerms(x, func(x *Term) bool {
		if call, ok := x.Value.(Call); ok {
			if tpe, ok := env.Get(call[0]).(*types.Function); ok && tpe.Result() == nil {
				errs = append(errs, NewError(TypeErr, x.Loc(), "%v used as value", call))
			}
		}
		return false
	})
	return errs
}

// rewritePrintCalls will rewrite the body so that print operands are captured
// in local variables and their evaluation occurs within a comprehension.
// Wrapping the terms inside of a comprehension ensures that undefined values do
// not short-circuit evaluation.
//
// For example, given the following print statement:
//
//	print("the value of x is:", input.x)
//
// The expression would be rewritten to:
//
//	print({__local0__ | __local0__ = "the value of x is:"}, {__local1__ | __local1__ = input.x})
func rewritePrintCalls(gen *localVarGenerator, getArity func(Ref) int, globals VarSet, body Body) (bool, Errors) {

	var errs Errors
	var modified bool

	// Visit comprehension bodies recursively to ensure print statements inside
	// those bodies only close over variables that are safe.
	for i := range body {
		if ContainsClosures(body[i]) {
			safe := outputVarsForBody(body[:i], getArity, globals)
			safe.Update(globals)
			WalkClosures(body[i], func(x any) bool {
				var modrec bool
				var errsrec Errors
				switch x := x.(type) {
				case *SetComprehension:
					modrec, errsrec = rewritePrintCalls(gen, getArity, safe, x.Body)
				case *ArrayComprehension:
					modrec, errsrec = rewritePrintCalls(gen, getArity, safe, x.Body)
				case *ObjectComprehension:
					modrec, errsrec = rewritePrintCalls(gen, getArity, safe, x.Body)
				case *Every:
					safe.Update(x.KeyValueVars())
					modrec, errsrec = rewritePrintCalls(gen, getArity, safe, x.Body)
				}
				if modrec {
					modified = true
				}
				errs = append(errs, errsrec...)
				return true
			})
			if len(errs) > 0 {
				return false, errs
			}
		}
	}

	for i := range body {

		if !isPrintCall(body[i]) {
			continue
		}

		modified = true

		var errs Errors
		safe := outputVarsForBody(body[:i], getArity, globals)
		safe.Update(globals)
		args := body[i].Operands()

		var vis *VarVisitor
		for j := range args {
			vis = vis.ClearOrNew().WithParams(SafetyCheckVisitorParams)
			vis.Walk(args[j])
			vars := vis.Vars()
			if vars.DiffCount(safe) > 0 {
				unsafe := vars.Diff(safe)
				for _, v := range unsafe.Sorted() {
					errs = append(errs, NewError(CompileErr, args[j].Loc(), "var %v is undeclared", v))
				}
			}
		}

		if len(errs) > 0 {
			return false, errs
		}

		terms := make([]*Term, 0, len(args))

		for j := range args {
			x := NewTerm(gen.Generate()).SetLocation(args[j].Loc())
			capture := Equality.Expr(x, args[j]).SetLocation(args[j].Loc())
			terms = append(terms, SetComprehensionTerm(x, NewBody(capture)).SetLocation(args[j].Loc()))
		}

		body.Set(NewExpr([]*Term{
			NewTerm(InternalPrint.Ref()).SetLocation(body[i].Loc()),
			ArrayTerm(terms...).SetLocation(body[i].Loc()),
		}).SetLocation(body[i].Loc()), i)
	}

	return modified, nil
}

func erasePrintCalls(node any) bool {
	var modified bool
	NewGenericVisitor(func(x any) bool {
		var modrec bool
		switch x := x.(type) {
		case *Rule:
			modrec, x.Body = erasePrintCallsInBody(x.Body)
		case *ArrayComprehension:
			modrec, x.Body = erasePrintCallsInBody(x.Body)
		case *SetComprehension:
			modrec, x.Body = erasePrintCallsInBody(x.Body)
		case *ObjectComprehension:
			modrec, x.Body = erasePrintCallsInBody(x.Body)
		case *Every:
			modrec, x.Body = erasePrintCallsInBody(x.Body)
		}
		if modrec {
			modified = true
		}
		return false
	}).Walk(node)
	return modified
}

func erasePrintCallsInBody(x Body) (bool, Body) {

	if !containsPrintCall(x) {
		return false, x
	}

	var cpy Body

	for i := range x {

		// Recursively visit any comprehensions contained in this expression.
		erasePrintCalls(x[i])

		if !isPrintCall(x[i]) {
			cpy.Append(x[i])
		}
	}

	if len(cpy) == 0 {
		term := BooleanTerm(true).SetLocation(x.Loc())
		expr := NewExpr(term).SetLocation(x.Loc())
		cpy.Append(expr)
	}

	return true, cpy
}

func containsPrintCall(x any) bool {
	var found bool
	WalkExprs(x, func(expr *Expr) bool {
		if !found {
			if isPrintCall(expr) {
				found = true
			}
		}
		return found
	})
	return found
}

var printRef = Print.Ref()

func isPrintCall(x *Expr) bool {
	return x.IsCall() && x.Operator().Equal(printRef)
}

// rewriteRefsInHead will rewrite rules so that the head does not contain any
// terms that require evaluation (e.g., refs or comprehensions). If the key or
// value contains one or more of these terms, the key or value will be moved
// into the body and assigned to a new variable. The new variable will replace
// the key or value in the head.
//
// For instance, given the following rule:
//
// p[{"foo": data.foo[i]}] { i < 100 }
//
// The rule would be re-written as:
//
// p[__local0__] { i < 100; __local0__ = {"foo": data.foo[i]} }
func (c *Compiler) rewriteRefsInHead() {
	f := newEqualityFactory(c.localvargen)
	for _, name := range c.sorted {
		mod := c.Modules[name]
		WalkRules(mod, func(rule *Rule) bool {
			if requiresEval(rule.Head.Key) {
				expr := f.Generate(rule.Head.Key)
				rule.Head.Key = expr.Operand(0)
				rule.Body.Append(expr)
			}
			if requiresEval(rule.Head.Value) {
				expr := f.Generate(rule.Head.Value)
				rule.Head.Value = expr.Operand(0)
				rule.Body.Append(expr)
			}
			for i := 0; i < len(rule.Head.Args); i++ {
				if requiresEval(rule.Head.Args[i]) {
					expr := f.Generate(rule.Head.Args[i])
					rule.Head.Args[i] = expr.Operand(0)
					rule.Body.Append(expr)
				}
			}
			return false
		})
	}
}

func (c *Compiler) rewriteEquals() {
	modified := false
	for _, name := range c.sorted {
		modified = rewriteEquals(c.Modules[name]) || modified
	}
	if modified {
		c.Required.addBuiltinSorted(Equal)
	}
}

func (c *Compiler) rewriteDynamicTerms() {
	f := newEqualityFactory(c.localvargen)
	for _, name := range c.sorted {
		WalkRules(c.Modules[name], func(rule *Rule) bool {
			rule.Body = rewriteDynamics(f, rule.Body)
			return false
		})
	}
}

// rewriteTestRuleEqualities rewrites equality expressions in test rule bodies to create local vars for statements that would otherwise
// not have their values captured through tracing, such as refs and comprehensions not unified/assigned to a local var.
// For example, given the following module:
//
//	package test
//
//	p.q contains v if {
//		some v in numbers.range(1, 3)
//	}
//
//	p.r := "foo"
//
//	test_rule {
//		p == {
//			"q": {4, 5, 6}
//		}
//	}
//
// `p` in `test_rule` resolves to `data.test.p`, which won't be an entry in the virtual-cache and must therefore be calculated after-the-fact.
// If `p` isn't captured in a local var, there is no trivial way to retrieve its value for test reporting.
func (c *Compiler) rewriteTestRuleEqualities() {
	if !c.rewriteTestRulesForTracing {
		return
	}

	f := newEqualityFactory(c.localvargen)
	for _, name := range c.sorted {
		mod := c.Modules[name]
		WalkRules(mod, func(rule *Rule) bool {
			if strings.HasPrefix(string(rule.Head.Name), "test_") {
				rule.Body = rewriteTestEqualities(f, rule.Body)
			}
			return false
		})
	}
}

func (c *Compiler) parseMetadataBlocks() {
	// Only parse annotations if rego.metadata built-ins are called
	regoMetadataCalled := false
	for _, name := range c.sorted {
		mod := c.Modules[name]
		WalkExprs(mod, func(expr *Expr) bool {
			if isRegoMetadataChainCall(expr) || isRegoMetadataRuleCall(expr) {
				regoMetadataCalled = true
			}
			return regoMetadataCalled
		})

		if regoMetadataCalled {
			break
		}
	}

	if regoMetadataCalled {
		// NOTE: Possible optimization: only parse annotations for modules on the path of rego.metadata-calling module
		for _, name := range c.sorted {
			mod := c.Modules[name]

			if len(mod.Annotations) == 0 {
				var errs Errors
				mod.Annotations, errs = parseAnnotations(mod.Comments)
				errs = append(errs, attachAnnotationsNodes(mod)...)
				for _, err := range errs {
					c.err(err)
				}

				attachRuleAnnotations(mod)
			}
		}
	}
}

func (c *Compiler) rewriteRegoMetadataCalls() {
	eqFactory := newEqualityFactory(c.localvargen)

	_, chainFuncAllowed := c.builtins[RegoMetadataChain.Name]
	_, ruleFuncAllowed := c.builtins[RegoMetadataRule.Name]

	for _, name := range c.sorted {
		mod := c.Modules[name]

		WalkRules(mod, func(rule *Rule) bool {
			var firstChainCall *Expr
			var firstRuleCall *Expr

			WalkExprs(rule, func(expr *Expr) bool {
				if chainFuncAllowed && firstChainCall == nil && isRegoMetadataChainCall(expr) {
					firstChainCall = expr
				} else if ruleFuncAllowed && firstRuleCall == nil && isRegoMetadataRuleCall(expr) {
					firstRuleCall = expr
				}
				return firstChainCall != nil && firstRuleCall != nil
			})

			chainCalled := firstChainCall != nil
			ruleCalled := firstRuleCall != nil

			if chainCalled || ruleCalled {
				body := make(Body, 0, len(rule.Body)+2)

				var metadataChainVar Var
				if chainCalled {
					// Create and inject metadata chain for rule

					chain, err := createMetadataChain(c.annotationSet.Chain(rule))
					if err != nil {
						c.err(err)
						return false
					}

					chain.Location = firstChainCall.Location
					eq := eqFactory.Generate(chain)
					metadataChainVar = eq.Operands()[0].Value.(Var)
					body.Append(eq)
				}

				var metadataRuleVar Var
				if ruleCalled {
					// Create and inject metadata for rule

					var metadataRuleTerm *Term

					a := getPrimaryRuleAnnotations(c.annotationSet, rule)
					if a != nil {
						annotObj, err := a.toObject()
						if err != nil {
							c.err(err)
							return false
						}
						metadataRuleTerm = NewTerm(*annotObj)
					} else {
						// If rule has no annotations, assign an empty object
						metadataRuleTerm = ObjectTerm()
					}

					metadataRuleTerm.Location = firstRuleCall.Location
					eq := eqFactory.Generate(metadataRuleTerm)
					metadataRuleVar = eq.Operands()[0].Value.(Var)
					body.Append(eq)
				}

				for _, expr := range rule.Body {
					body.Append(expr)
				}
				rule.Body = body

				vis := func(b Body) bool {
					for _, err := range rewriteRegoMetadataCalls(&metadataChainVar, &metadataRuleVar, b, &c.RewrittenVars) {
						c.err(err)
					}
					return false
				}
				WalkBodies(rule.Head, vis)
				WalkBodies(rule.Body, vis)
			}

			return false
		})
	}
}

func getPrimaryRuleAnnotations(as *AnnotationSet, rule *Rule) *Annotations {
	annots := as.GetRuleScope(rule)

	if len(annots) == 0 {
		return nil
	}

	// Sort by annotation location; chain must start with annotations declared closest to rule, then going outward
	slices.SortStableFunc(annots, func(a, b *Annotations) int {
		return -a.Location.Compare(b.Location)
	})

	return annots[0]
}

func rewriteRegoMetadataCalls(metadataChainVar *Var, metadataRuleVar *Var, body Body, rewrittenVars *map[Var]Var) Errors {
	var errs Errors

	WalkClosures(body, func(x any) bool {
		switch x := x.(type) {
		case *ArrayComprehension:
			errs = rewriteRegoMetadataCalls(metadataChainVar, metadataRuleVar, x.Body, rewrittenVars)
		case *SetComprehension:
			errs = rewriteRegoMetadataCalls(metadataChainVar, metadataRuleVar, x.Body, rewrittenVars)
		case *ObjectComprehension:
			errs = rewriteRegoMetadataCalls(metadataChainVar, metadataRuleVar, x.Body, rewrittenVars)
		case *Every:
			errs = rewriteRegoMetadataCalls(metadataChainVar, metadataRuleVar, x.Body, rewrittenVars)
		}
		return true
	})

	for i := range body {
		expr := body[i]
		var metadataVar Var

		if metadataChainVar != nil && isRegoMetadataChainCall(expr) {
			metadataVar = *metadataChainVar
		} else if metadataRuleVar != nil && isRegoMetadataRuleCall(expr) {
			metadataVar = *metadataRuleVar
		} else {
			continue
		}

		// NOTE(johanfylling): An alternative strategy would be to walk the body and replace all operands[0]
		// usages with *metadataChainVar
		operands := expr.Operands()
		var newExpr *Expr
		if len(operands) > 0 { // There is an output var to rewrite
			rewrittenVar := operands[0]
			newExpr = Equality.Expr(rewrittenVar, NewTerm(metadataVar))
		} else { // No output var, just rewrite expr to metadataVar
			newExpr = NewExpr(NewTerm(metadataVar))
		}

		newExpr.Generated = true
		newExpr.Location = expr.Location
		body.Set(newExpr, i)
	}

	return errs
}

var regoMetadataChainRef = RegoMetadataChain.Ref()
var regoMetadataRuleRef = RegoMetadataRule.Ref()

func isRegoMetadataChainCall(x *Expr) bool {
	return x.IsCall() && x.Operator().Equal(regoMetadataChainRef)
}

func isRegoMetadataRuleCall(x *Expr) bool {
	return x.IsCall() && x.Operator().Equal(regoMetadataRuleRef)
}

func createMetadataChain(chain []*AnnotationsRef) (*Term, *Error) {

	metaArray := NewArray()
	for _, link := range chain {
		// Dropping leading 'data' element of path
		p := link.Path[1:].toArray()
		obj := NewObject(Item(InternedTerm("path"), NewTerm(p)))
		if link.Annotations != nil {
			annotObj, err := link.Annotations.toObject()
			if err != nil {
				return nil, err
			}
			obj.Insert(InternedTerm("annotations"), NewTerm(*annotObj))
		}
		metaArray = metaArray.Append(NewTerm(obj))
	}

	return NewTerm(metaArray), nil
}

func (c *Compiler) rewriteLocalVars() {
	var assignment bool

	args := NewVarVisitor()
	argsStack := newLocalDeclaredVars()

	for _, name := range c.sorted {
		mod := c.Modules[name]
		gen := c.localvargen

		WalkRules(mod, func(rule *Rule) bool {
			args.Clear()
			argsStack.Clear()

			if c.strict && len(rule.Head.Args) > 0 {
				args.WalkArgs(rule.Head.Args)
			}
			unusedArgs := args.Vars()

			c.rewriteLocalArgVars(gen, argsStack, rule)

			// Rewrite local vars in each else-branch of the rule.
			// Note: this is done instead of a walk so that we can capture any unused function arguments
			// across else-branches.
			for rule := rule; rule != nil; rule = rule.Else {
				stack, errs := c.rewriteLocalVarsInRule(rule, unusedArgs, argsStack, gen)
				if stack.assignment {
					assignment = true
				}

				for arg := range unusedArgs {
					if stack.Count(arg) > 1 {
						delete(unusedArgs, arg)
					}
				}

				for _, err := range errs {
					c.err(err)
				}
			}

			if c.strict {
				// Report an error for each unused function argument
				for arg := range unusedArgs {
					if !arg.IsWildcard() {
						c.err(NewError(CompileErr, rule.Head.Location, "unused argument %v. (hint: use _ (wildcard variable) instead)", arg))
					}
				}
			}

			return true
		})
	}

	if assignment {
		c.Required.addBuiltinSorted(Assign)
	}
}

func (c *Compiler) rewriteLocalVarsInRule(rule *Rule, unusedArgs VarSet, argsStack *localDeclaredVars, gen *localVarGenerator) (*localDeclaredVars, Errors) {
	onlyScalars := !headMayHaveVars(rule.Head)

	var used VarSet

	if !onlyScalars {
		// Rewrite assignments contained in head of rule. Assignments can
		// occur in rule head if they're inside a comprehension. Note,
		// assigned vars in comprehensions in the head will be rewritten
		// first to preserve scoping rules. For example:
		//
		// p = [x | x := 1] { x := 2 } becomes p = [__local0__ | __local0__ = 1] { __local1__ = 2 }
		//
		// This behaviour is consistent scoping inside the body. For example:
		//
		// p = xs { x := 2; xs = [x | x := 1] } becomes p = xs { __local0__ = 2; xs = [__local1__ | __local1__ = 1] }
		nestedXform := &rewriteNestedHeadVarLocalTransform{
			gen:           gen,
			RewrittenVars: c.RewrittenVars,
			strict:        c.strict,
		}

		NewGenericVisitor(nestedXform.Visit).Walk(rule.Head)

		for _, err := range nestedXform.errs {
			c.err(err)
		}

		// Rewrite assignments in body.
		used = NewVarSet()

		for _, t := range rule.Head.Ref()[1:] {
			used.Update(t.Vars())
		}

		if rule.Head.Key != nil {
			used.Update(rule.Head.Key.Vars())
		}

		if rule.Head.Value != nil {
			valueVars := rule.Head.Value.Vars()
			used.Update(valueVars)
			for arg := range unusedArgs {
				if valueVars.Contains(arg) {
					delete(unusedArgs, arg)
				}
			}
		}
	}

	stack := argsStack.Copy()

	body, declared, errs := rewriteLocalVars(gen, stack, used, rule.Body, c.strict)

	// For rewritten vars use the collection of all variables that
	// were in the stack at some point in time.
	maps.Copy(c.RewrittenVars, stack.rewritten)

	rule.Body = body

	if onlyScalars {
		return stack, errs
	}

	// Rewrite vars in head that refer to locally declared vars in the body.
	localXform := rewriteHeadVarLocalTransform{declared: declared}

	for i := range rule.Head.Args {
		rule.Head.Args[i], _ = transformTerm(localXform, rule.Head.Args[i])
	}

	for i := 1; i < len(rule.Head.Ref()); i++ {
		rule.Head.Reference[i], _ = transformTerm(localXform, rule.Head.Ref()[i])
	}
	if rule.Head.Key != nil {
		rule.Head.Key, _ = transformTerm(localXform, rule.Head.Key)
	}

	if rule.Head.Value != nil {
		rule.Head.Value, _ = transformTerm(localXform, rule.Head.Value)
	}
	return stack, errs
}

func headMayHaveVars(head *Head) bool {
	if head == nil {
		return false
	}
	for i := range head.Args {
		if !IsScalar(head.Args[i].Value) {
			return true
		}
	}
	if head.Key != nil && !IsScalar(head.Key.Value) {
		return true
	}
	if head.Value != nil && !IsScalar(head.Value.Value) {
		return true
	}
	ref := head.Ref()[1:]
	for i := range ref {
		if !IsScalar(ref[i].Value) {
			return true
		}
	}
	return false
}

type rewriteNestedHeadVarLocalTransform struct {
	gen           *localVarGenerator
	errs          Errors
	RewrittenVars map[Var]Var
	strict        bool
}

func (xform *rewriteNestedHeadVarLocalTransform) Visit(x any) bool {
	if term, ok := x.(*Term); ok {
		stop := false
		stack := newLocalDeclaredVars()

		switch x := term.Value.(type) {
		case *object:
			cpy, _ := x.Map(func(k, v *Term) (*Term, *Term, error) {
				kcpy := k.Copy()
				NewGenericVisitor(xform.Visit).Walk(kcpy)
				vcpy := v.Copy()
				NewGenericVisitor(xform.Visit).Walk(vcpy)
				return kcpy, vcpy, nil
			})
			term.Value = cpy
			stop = true
		case *set:
			cpy, _ := x.Map(func(v *Term) (*Term, error) {
				vcpy := v.Copy()
				NewGenericVisitor(xform.Visit).Walk(vcpy)
				return vcpy, nil
			})
			term.Value = cpy
			stop = true
		case *ArrayComprehension:
			xform.errs = rewriteDeclaredVarsInArrayComprehension(xform.gen, stack, x, xform.errs, xform.strict)
			stop = true
		case *SetComprehension:
			xform.errs = rewriteDeclaredVarsInSetComprehension(xform.gen, stack, x, xform.errs, xform.strict)
			stop = true
		case *ObjectComprehension:
			xform.errs = rewriteDeclaredVarsInObjectComprehension(xform.gen, stack, x, xform.errs, xform.strict)
			stop = true
		}

		maps.Copy(xform.RewrittenVars, stack.rewritten)

		return stop
	}

	return false
}

type rewriteHeadVarLocalTransform struct {
	declared map[Var]Var
}

func (xform rewriteHeadVarLocalTransform) Transform(x any) (any, error) {
	if v, ok := x.(Var); ok {
		if gv, ok := xform.declared[v]; ok {
			return gv, nil
		}
	}
	return x, nil
}

func (c *Compiler) rewriteLocalArgVars(gen *localVarGenerator, stack *localDeclaredVars, rule *Rule) {

	vis := &ruleArgLocalRewriter{
		stack: stack,
		gen:   gen,
	}

	for i := range rule.Head.Args {
		Walk(vis, rule.Head.Args[i])
	}

	for i := range vis.errs {
		c.err(vis.errs[i])
	}
}

type ruleArgLocalRewriter struct {
	stack *localDeclaredVars
	gen   *localVarGenerator
	errs  []*Error
}

func (vis *ruleArgLocalRewriter) Visit(x any) Visitor {

	t, ok := x.(*Term)
	if !ok {
		return vis
	}

	switch v := t.Value.(type) {
	case Var:
		gv, ok := vis.stack.Declared(v)
		if ok {
			vis.stack.Seen(v)
		} else {
			gv = vis.gen.Generate()
			vis.stack.Insert(v, gv, argVar)
		}
		t.Value = gv
		return nil
	case *object:
		if cpy, err := v.Map(func(k, v *Term) (*Term, *Term, error) {
			vcpy := v.Copy()
			Walk(vis, vcpy)
			return k, vcpy, nil
		}); err != nil {
			vis.errs = append(vis.errs, NewError(CompileErr, t.Location, err.Error())) //nolint:govet
		} else {
			t.Value = cpy
		}
		return nil
	case Null, Boolean, Number, String, *ArrayComprehension, *SetComprehension, *ObjectComprehension, Set:
		// Scalars are no-ops. Comprehensions are handled above. Sets must not
		// contain variables.
		return nil
	case Call:
		vis.errs = append(vis.errs, NewError(CompileErr, t.Location, "rule arguments cannot contain calls"))
		return nil
	default:
		// Recurse on refs and arrays. Any embedded
		// variables can be rewritten.
		return vis
	}
}

func (c *Compiler) rewriteWithModifiers() {
	f := newEqualityFactory(c.localvargen)
	for _, name := range c.sorted {
		mod := c.Modules[name]
		t := NewGenericTransformer(func(x any) (any, error) {
			body, ok := x.(Body)
			if !ok {
				return x, nil
			}
			body, err := rewriteWithModifiersInBody(c, c.unsafeBuiltinsMap, f, body)
			if err != nil {
				c.err(err)
			}

			return body, nil
		})
		_, _ = Transform(t, mod) // ignore error
	}
}

func (c *Compiler) setModuleTree() {
	c.ModuleTree = NewModuleTree(c.Modules)
}

func (c *Compiler) setRuleTree() {
	c.RuleTree = NewRuleTree(c.ModuleTree)
}

func (c *Compiler) setGraph() {
	list := func(r Ref) []*Rule {
		return c.GetRulesDynamicWithOpts(r, RulesOptions{IncludeHiddenModules: true})
	}
	c.Graph = NewGraph(c.Modules, list)
}

type queryCompiler struct {
	compiler              *Compiler
	qctx                  *QueryContext
	typeEnv               *TypeEnv
	rewritten             map[Var]Var
	after                 map[string][]QueryCompilerStageDefinition
	unsafeBuiltins        map[string]struct{}
	comprehensionIndices  map[*Term]*ComprehensionIndex
	enablePrintStatements bool
}

func newQueryCompiler(compiler *Compiler) QueryCompiler {
	qc := &queryCompiler{
		compiler:             compiler,
		qctx:                 nil,
		after:                map[string][]QueryCompilerStageDefinition{},
		comprehensionIndices: map[*Term]*ComprehensionIndex{},
	}
	return qc
}

func (qc *queryCompiler) WithStrict(strict bool) QueryCompiler {
	qc.compiler.WithStrict(strict)
	return qc
}

func (qc *queryCompiler) WithEnablePrintStatements(yes bool) QueryCompiler {
	qc.enablePrintStatements = yes
	return qc
}

func (qc *queryCompiler) WithContext(qctx *QueryContext) QueryCompiler {
	qc.qctx = qctx
	return qc
}

func (qc *queryCompiler) WithStageAfter(after string, stage QueryCompilerStageDefinition) QueryCompiler {
	qc.after[after] = append(qc.after[after], stage)
	return qc
}

func (qc *queryCompiler) WithUnsafeBuiltins(unsafe map[string]struct{}) QueryCompiler {
	qc.unsafeBuiltins = unsafe
	return qc
}

func (qc *queryCompiler) RewrittenVars() map[Var]Var {
	return qc.rewritten
}

func (qc *queryCompiler) ComprehensionIndex(term *Term) *ComprehensionIndex {
	if result, ok := qc.comprehensionIndices[term]; ok {
		return result
	} else if result, ok := qc.compiler.comprehensionIndices[term]; ok {
		return result
	}
	return nil
}

func (qc *queryCompiler) runStage(metricName string, qctx *QueryContext, query Body, s func(*QueryContext, Body) (Body, error)) (Body, error) {
	if qc.compiler.metrics != nil {
		qc.compiler.metrics.Timer(metricName).Start()
		defer qc.compiler.metrics.Timer(metricName).Stop()
	}
	return s(qctx, query)
}

func (qc *queryCompiler) runStageAfter(metricName string, query Body, s QueryCompilerStage) (Body, error) {
	if qc.compiler.metrics != nil {
		qc.compiler.metrics.Timer(metricName).Start()
		defer qc.compiler.metrics.Timer(metricName).Stop()
	}
	return s(qc, query)
}

type queryStage = struct {
	name       string
	metricName string
	f          func(*QueryContext, Body) (Body, error)
}

func (qc *queryCompiler) Compile(query Body) (Body, error) {
	if len(query) == 0 {
		return nil, Errors{NewError(CompileErr, nil, "empty query cannot be compiled")}
	}

	query = query.Copy()

	stages := []queryStage{
		{"CheckKeywordOverrides", "query_compile_stage_check_keyword_overrides", qc.checkKeywordOverrides},
		{"ResolveRefs", "query_compile_stage_resolve_refs", qc.resolveRefs},
		{"RewriteLocalVars", "query_compile_stage_rewrite_local_vars", qc.rewriteLocalVars},
		{"CheckVoidCalls", "query_compile_stage_check_void_calls", qc.checkVoidCalls},
		{"RewritePrintCalls", "query_compile_stage_rewrite_print_calls", qc.rewritePrintCalls},
		{"RewriteExprTerms", "query_compile_stage_rewrite_expr_terms", qc.rewriteExprTerms},
		{"RewriteComprehensionTerms", "query_compile_stage_rewrite_comprehension_terms", qc.rewriteComprehensionTerms},
		{"RewriteWithValues", "query_compile_stage_rewrite_with_values", qc.rewriteWithModifiers},
		{"CheckUndefinedFuncs", "query_compile_stage_check_undefined_funcs", qc.checkUndefinedFuncs},
		{"CheckSafety", "query_compile_stage_check_safety", qc.checkSafety},
		{"RewriteDynamicTerms", "query_compile_stage_rewrite_dynamic_terms", qc.rewriteDynamicTerms},
		{"CheckTypes", "query_compile_stage_check_types", qc.checkTypes},
		{"CheckUnsafeBuiltins", "query_compile_stage_check_unsafe_builtins", qc.checkUnsafeBuiltins},
		{"CheckDeprecatedBuiltins", "query_compile_stage_check_deprecated_builtins", qc.checkDeprecatedBuiltins},
	}
	if qc.compiler.evalMode == EvalModeTopdown {
		stages = append(stages, queryStage{"BuildComprehensionIndex", "query_compile_stage_build_comprehension_index", qc.buildComprehensionIndices})
	}

	qctx := qc.qctx.Copy()

	for _, s := range stages {
		var err error
		query, err = qc.runStage(s.metricName, qctx, query, s.f)
		if err != nil {
			return nil, qc.applyErrorLimit(err)
		}
		for _, s := range qc.after[s.name] {
			query, err = qc.runStageAfter(s.MetricName, query, s.Stage)
			if err != nil {
				return nil, qc.applyErrorLimit(err)
			}
		}
	}

	return query, nil
}

func (qc *queryCompiler) TypeEnv() *TypeEnv {
	return qc.typeEnv
}

func (qc *queryCompiler) applyErrorLimit(err error) error {
	var errs Errors
	if errors.As(err, &errs) {
		if qc.compiler.maxErrs > 0 && len(errs) > qc.compiler.maxErrs {
			err = append(errs[:qc.compiler.maxErrs], errLimitReached)
		}
	}
	return err
}

func (qc *queryCompiler) checkKeywordOverrides(_ *QueryContext, body Body) (Body, error) {
	if qc.compiler.strict {
		if errs := checkRootDocumentOverrides(body); len(errs) > 0 {
			return nil, errs
		}
	}
	return body, nil
}

func (qc *queryCompiler) resolveRefs(qctx *QueryContext, body Body) (Body, error) {

	var globals map[Var]*usedRef

	if qctx != nil {
		pkg := qctx.Package
		// Query compiler ought to generate a package if one was not provided and one or more imports were provided.
		// The generated package name could even be an empty string to avoid conflicts (it doesn't have to be valid syntactically)
		if pkg == nil && len(qctx.Imports) > 0 {
			pkg = &Package{Path: RefTerm(VarTerm("")).Value.(Ref)}
		}
		if pkg != nil {
			var ruleExports []Ref
			rules := qc.compiler.getExports()
			if exist, ok := rules.Get(pkg.Path); ok {
				ruleExports = exist
			}

			globals = getGlobals(qctx.Package, ruleExports, qctx.Imports)
			qctx.Imports = nil
		}
	}

	ignore := &declaredVarStack{declaredVars(body)}

	return resolveRefsInBody(globals, ignore, body), nil
}

func (*queryCompiler) rewriteComprehensionTerms(_ *QueryContext, body Body) (Body, error) {
	gen := newLocalVarGenerator("q", body)
	f := newEqualityFactory(gen)
	node, err := rewriteComprehensionTerms(f, body)
	if err != nil {
		return nil, err
	}
	return node.(Body), nil
}

func (*queryCompiler) rewriteDynamicTerms(_ *QueryContext, body Body) (Body, error) {
	gen := newLocalVarGenerator("q", body)
	f := newEqualityFactory(gen)
	return rewriteDynamics(f, body), nil
}

func (*queryCompiler) rewriteExprTerms(_ *QueryContext, body Body) (Body, error) {
	gen := newLocalVarGenerator("q", body)
	return rewriteExprTermsInBody(gen, body), nil
}

func (qc *queryCompiler) rewriteLocalVars(_ *QueryContext, body Body) (Body, error) {
	gen := newLocalVarGenerator("q", body)
	stack := newLocalDeclaredVars()
	body, _, err := rewriteLocalVars(gen, stack, nil, body, qc.compiler.strict)
	if len(err) != 0 {
		return nil, err
	}

	// The vars returned during the rewrite will include all seen vars,
	// even if they're not declared with an assignment operation. We don't
	// want to include these inside the rewritten set though.
	qc.rewritten = maps.Clone(stack.rewritten)

	return body, nil
}

func (qc *queryCompiler) rewritePrintCalls(_ *QueryContext, body Body) (Body, error) {
	if !qc.enablePrintStatements {
		_, cpy := erasePrintCallsInBody(body)
		return cpy, nil
	}
	gen := newLocalVarGenerator("q", body)
	if _, errs := rewritePrintCalls(gen, qc.compiler.GetArity, ReservedVars, body); len(errs) > 0 {
		return nil, errs
	}
	return body, nil
}

func (qc *queryCompiler) checkVoidCalls(_ *QueryContext, body Body) (Body, error) {
	if errs := checkVoidCalls(qc.compiler.TypeEnv, body); len(errs) > 0 {
		return nil, errs
	}
	return body, nil
}

func (qc *queryCompiler) checkUndefinedFuncs(_ *QueryContext, body Body) (Body, error) {
	if errs := checkUndefinedFuncs(qc.compiler.TypeEnv, body, qc.compiler.GetArity, qc.rewritten); len(errs) > 0 {
		return nil, errs
	}
	return body, nil
}

func (qc *queryCompiler) checkSafety(_ *QueryContext, body Body) (Body, error) {
	safe := ReservedVars.Copy()
	reordered, unsafe := reorderBodyForSafety(qc.compiler.builtins, qc.compiler.GetArity, safe, body)
	if errs := safetyErrorSlice(unsafe, qc.RewrittenVars()); len(errs) > 0 {
		return nil, errs
	}
	return reordered, nil
}

func (qc *queryCompiler) checkTypes(_ *QueryContext, body Body) (Body, error) {
	var errs Errors
	checker := newTypeChecker().
		WithSchemaSet(qc.compiler.schemaSet).
		WithInputType(qc.compiler.inputType).
		WithVarRewriter(rewriteVarsInRef(qc.rewritten, qc.compiler.RewrittenVars))
	qc.typeEnv, errs = checker.CheckBody(qc.compiler.TypeEnv, body)
	if len(errs) > 0 {
		return nil, errs
	}

	return body, nil
}

func (qc *queryCompiler) checkUnsafeBuiltins(_ *QueryContext, body Body) (Body, error) {
	errs := checkUnsafeBuiltins(qc.unsafeBuiltinsMap(), body)
	if len(errs) > 0 {
		return nil, errs
	}
	return body, nil
}

func (qc *queryCompiler) unsafeBuiltinsMap() map[string]struct{} {
	if qc.unsafeBuiltins != nil {
		return qc.unsafeBuiltins
	}
	return qc.compiler.unsafeBuiltinsMap
}

func (qc *queryCompiler) checkDeprecatedBuiltins(_ *QueryContext, body Body) (Body, error) {
	if qc.compiler.strict {
		errs := checkDeprecatedBuiltins(qc.compiler.deprecatedBuiltinsMap, body)
		if len(errs) > 0 {
			return nil, errs
		}
	}
	return body, nil
}

func (qc *queryCompiler) rewriteWithModifiers(_ *QueryContext, body Body) (Body, error) {
	f := newEqualityFactory(newLocalVarGenerator("q", body))
	body, err := rewriteWithModifiersInBody(qc.compiler, qc.unsafeBuiltinsMap(), f, body)
	if err != nil {
		return nil, Errors{err}
	}
	return body, nil
}

func (qc *queryCompiler) buildComprehensionIndices(_ *QueryContext, body Body) (Body, error) {
	// NOTE(tsandall): The query compiler does not have a metrics object so we
	// cannot record index metrics currently.
	_ = buildComprehensionIndices(qc.compiler.debug, qc.compiler.GetArity, ReservedVars, qc.RewrittenVars(), body, qc.comprehensionIndices)
	return body, nil
}

// ComprehensionIndex specifies how the comprehension term can be indexed. The keys
// tell the evaluator what variables to use for indexing. In the future, the index
// could be expanded with more information that would allow the evaluator to index
// a larger fragment of comprehensions (e.g., by closing over variables in the outer
// query.)
type ComprehensionIndex struct {
	Term *Term
	Keys []*Term
}

func (ci *ComprehensionIndex) String() string {
	if ci == nil {
		return ""
	}
	return fmt.Sprintf("<keys: %v>", NewArray(ci.Keys...))
}

func buildComprehensionIndices(dbg debug.Debug, arity func(Ref) int, candidates VarSet, rwVars map[Var]Var, node Body, result map[*Term]*ComprehensionIndex) uint64 {
	var n uint64
	cpy := candidates.Copy()
	WalkBodies(node, func(b Body) bool {
		for _, expr := range b {
			index := getComprehensionIndex(dbg, arity, cpy, rwVars, expr)
			if index != nil {
				result[index.Term] = index
				n++
			}
			// Any variables appearing in the expressions leading up to the comprehension
			// are fair-game to be used as index keys.
			cpy.Update(expr.Vars(VarVisitorParams{SkipClosures: true, SkipRefCallHead: true}))
		}
		return false
	})
	return n
}

func getComprehensionIndex(dbg debug.Debug, arity func(Ref) int, candidates VarSet, rwVars map[Var]Var, expr *Expr) *ComprehensionIndex {

	// Ignore everything except <var> = <comprehension> expressions. Extract
	// the comprehension term from the expression.
	if !expr.IsEquality() || expr.Negated || len(expr.With) > 0 {
		// No debug message, these are assumed to be known hinderances
		// to comprehension indexing.
		return nil
	}

	var term *Term

	lhs, rhs := expr.Operand(0), expr.Operand(1)

	if _, ok := lhs.Value.(Var); ok && IsComprehension(rhs.Value) {
		term = rhs
	} else if _, ok := rhs.Value.(Var); ok && IsComprehension(lhs.Value) {
		term = lhs
	}

	if term == nil {
		// no debug for this, it's the ordinary "nothing to do here" case
		return nil
	}

	// Ignore comprehensions that contain expressions that close over variables
	// in the outer body if those variables are not also output variables in the
	// comprehension body. In other words, ignore comprehensions that we cannot
	// safely evaluate without bindings from the outer body. For example:
	//
	// 	x = [1]
	//	[true | data.y[z] = x]     # safe to evaluate w/o outer body
	//	[true | data.y[z] = x[0]]  # NOT safe to evaluate because 'x' would be unsafe.
	//
	// By identifying output variables in the body we also know what to index on by
	// intersecting with candidate variables from the outer query.
	//
	// For example:
	//
	//	x = data.foo[_]
	//	_ = [y | data.bar[y] = x]      # index on 'x'
	//
	// This query goes from O(data.foo*data.bar) to O(data.foo+data.bar).
	var body Body

	switch x := term.Value.(type) {
	case *ArrayComprehension:
		body = x.Body
	case *SetComprehension:
		body = x.Body
	case *ObjectComprehension:
		body = x.Body
	}

	outputs := outputVarsForBody(body, arity, ReservedVars)
	unsafe := body.Vars(SafetyCheckVisitorParams).Diff(outputs).Diff(ReservedVars)

	if len(unsafe) > 0 {
		dbg.Printf("%s: comprehension index: unsafe vars: %v", expr.Location, unsafe)
		return nil
	}

	// Similarly, ignore comprehensions that contain references with output variables
	// that intersect with the candidates. Indexing these comprehensions could worsen
	// performance.
	regressionVis := newComprehensionIndexRegressionCheckVisitor(candidates)
	regressionVis.Walk(body)
	if regressionVis.worse {
		dbg.Printf("%s: comprehension index: output vars intersect candidates", expr.Location)
		return nil
	}

	// Check if any nested comprehensions close over candidates. If any intersection is found
	// the comprehension cannot be cached because it would require closing over the candidates
	// which the evaluator does not support today.
	nestedVis := newComprehensionIndexNestedCandidateVisitor(candidates)
	nestedVis.Walk(body)
	if nestedVis.found {
		dbg.Printf("%s: comprehension index: nested comprehensions close over candidates", expr.Location)
		return nil
	}

	// Make a sorted set of variable names that will serve as the index key set.
	// Sort to ensure deterministic indexing. In future this could be relaxed
	// if we can decide that one ordering is better than another. If the set is
	// empty, there is no indexing to do.
	indexVars := candidates.Intersect(outputs)
	if len(indexVars) == 0 {
		dbg.Printf("%s: comprehension index: no index vars", expr.Location)
		return nil
	}

	result := make([]*Term, 0, len(indexVars))

	for v := range indexVars {
		result = append(result, NewTerm(v))
	}

	slices.SortFunc(result, TermValueCompare)

	debugRes := make([]*Term, len(result))
	for i, r := range result {
		if o, ok := rwVars[r.Value.(Var)]; ok {
			debugRes[i] = NewTerm(o)
		} else {
			debugRes[i] = r
		}
	}
	dbg.Printf("%s: comprehension index: built with keys: %v", expr.Location, debugRes)
	return &ComprehensionIndex{Term: term, Keys: result}
}

type comprehensionIndexRegressionCheckVisitor struct {
	candidates VarSet
	seen       VarSet
	worse      bool
}

// TODO(tsandall): Improve this so that users can either supply this list explicitly
// or the information is maintained on the built-in function declaration. What we really
// need to know is whether the built-in function allows callers to push down output
// values or not. It's unlikely that anything outside of OPA does this today so this
// solution is fine for now.
var comprehensionIndexBlacklist = map[string]int{
	WalkBuiltin.Name: len(WalkBuiltin.Decl.FuncArgs().Args),
}

func newComprehensionIndexRegressionCheckVisitor(candidates VarSet) *comprehensionIndexRegressionCheckVisitor {
	return &comprehensionIndexRegressionCheckVisitor{
		candidates: candidates,
		seen:       NewVarSet(),
	}
}

func (vis *comprehensionIndexRegressionCheckVisitor) Walk(x any) {
	NewGenericVisitor(vis.visit).Walk(x)
}

func (vis *comprehensionIndexRegressionCheckVisitor) visit(x any) bool {
	if !vis.worse {
		switch x := x.(type) {
		case *Expr:
			operands := x.Operands()
			if pos := comprehensionIndexBlacklist[x.Operator().String()]; pos > 0 && pos < len(operands) {
				vis.assertEmptyIntersection(operands[pos].Vars())
			}
		case Ref:
			vis.assertEmptyIntersection(x.OutputVars())
		case Var:
			vis.seen.Add(x)
		// Always skip comprehensions. We do not have to visit their bodies here.
		case *ArrayComprehension, *SetComprehension, *ObjectComprehension:
			return true
		}
	}
	return vis.worse
}

func (vis *comprehensionIndexRegressionCheckVisitor) assertEmptyIntersection(vs VarSet) {
	for v := range vs {
		if vis.candidates.Contains(v) && !vis.seen.Contains(v) {
			vis.worse = true
			return
		}
	}
}

type comprehensionIndexNestedCandidateVisitor struct {
	candidates VarSet
	found      bool
}

func newComprehensionIndexNestedCandidateVisitor(candidates VarSet) *comprehensionIndexNestedCandidateVisitor {
	return &comprehensionIndexNestedCandidateVisitor{
		candidates: candidates,
	}
}

func (vis *comprehensionIndexNestedCandidateVisitor) Walk(x any) {
	NewGenericVisitor(vis.visit).Walk(x)
}

func (vis *comprehensionIndexNestedCandidateVisitor) visit(x any) bool {
	if vis.found {
		return true
	}

	if v, ok := x.(Value); ok && IsComprehension(v) {
		varVis := NewVarVisitor().WithParams(VarVisitorParams{SkipRefHead: true})
		varVis.Walk(v)
		vis.found = len(varVis.Vars().Intersect(vis.candidates)) > 0
		return true
	}

	return false
}

// ModuleTreeNode represents a node in the module tree. The module
// tree is keyed by the package path.
type ModuleTreeNode struct {
	Key      Value
	Modules  []*Module
	Children map[Value]*ModuleTreeNode
	Hide     bool
}

func (n *ModuleTreeNode) String() string {
	var rules []string
	for _, m := range n.Modules {
		for _, r := range m.Rules {
			rules = append(rules, r.Head.String())
		}
	}
	return fmt.Sprintf("<ModuleTreeNode key:%v children:%v rules:%v hide:%v>", n.Key, n.Children, rules, n.Hide)
}

// NewModuleTree returns a new ModuleTreeNode that represents the root
// of the module tree populated with the given modules.
func NewModuleTree(mods map[string]*Module) *ModuleTreeNode {
	root := &ModuleTreeNode{
		Children: map[Value]*ModuleTreeNode{},
	}
	for _, name := range util.KeysSorted(mods) {
		m := mods[name]
		node := root
		for i, x := range m.Package.Path {
			c, ok := node.Children[x.Value]
			if !ok {
				var hide bool
				if i == 1 && x.Value.Compare(SystemDocumentKey) == 0 {
					hide = true
				}
				c = &ModuleTreeNode{
					Key:      x.Value,
					Children: map[Value]*ModuleTreeNode{},
					Hide:     hide,
				}
				node.Children[x.Value] = c
			}
			node = c
		}
		node.Modules = append(node.Modules, m)
	}
	return root
}

// Size returns the number of modules in the tree.
func (n *ModuleTreeNode) Size() int {
	s := len(n.Modules)
	for _, c := range n.Children {
		s += c.Size()
	}
	return s
}

// Child returns n's child with key k.
func (n *ModuleTreeNode) child(k Value) *ModuleTreeNode {
	switch k.(type) {
	case String, Var:
		return n.Children[k]
	}
	return nil
}

// Find dereferences ref along the tree. ref[0] is converted to a String
// for convenience.
func (n *ModuleTreeNode) find(ref Ref) (*ModuleTreeNode, Ref) {
	if v, ok := ref[0].Value.(Var); ok {
		ref = Ref{StringTerm(string(v))}.Concat(ref[1:])
	}
	node := n
	for i, r := range ref {
		next := node.child(r.Value)
		if next == nil {
			tail := make(Ref, len(ref)-i)
			tail[0] = VarTerm(string(ref[i].Value.(String)))
			copy(tail[1:], ref[i+1:])
			return node, tail
		}
		node = next
	}
	return node, nil
}

// DepthFirst performs a depth-first traversal of the module tree rooted at n.
// If f returns true, traversal will not continue to the children of n.
func (n *ModuleTreeNode) DepthFirst(f func(*ModuleTreeNode) bool) {
	if f(n) {
		return
	}
	for _, node := range n.Children {
		node.DepthFirst(f)
	}
}

// TreeNode represents a node in the rule tree. The rule tree is keyed by
// rule path.
type TreeNode struct {
	Key      Value
	Values   []any
	Children map[Value]*TreeNode
	Sorted   []Value
	Hide     bool
}

func (n *TreeNode) String() string {
	return fmt.Sprintf("<TreeNode key:%v values:%v sorted:%v hide:%v>", n.Key, n.Values, n.Sorted, n.Hide)
}

// NewRuleTree returns a new TreeNode that represents the root
// of the rule tree populated with the given rules.
func NewRuleTree(mtree *ModuleTreeNode) *TreeNode {
	root := TreeNode{
		Key: mtree.Key,
	}

	mtree.DepthFirst(func(m *ModuleTreeNode) bool {
		for _, mod := range m.Modules {
			if len(mod.Rules) == 0 {
				root.add(mod.Package.Path, nil)
			}
			for _, rule := range mod.Rules {
				root.add(rule.Ref().GroundPrefix(), rule)
			}
		}
		return false
	})

	// ensure that data.system's TreeNode is hidden
	node, tail := root.find(DefaultRootRef.Append(NewTerm(SystemDocumentKey)))
	if len(tail) == 0 { // found
		node.Hide = true
	}

	root.DepthFirst(func(x *TreeNode) bool {
		x.sort()
		return false
	})

	return &root
}

func (n *TreeNode) add(path Ref, rule *Rule) {
	node, tail := n.find(path)
	if len(tail) > 0 {
		sub := treeNodeFromRef(tail, rule)
		if node.Children == nil {
			node.Children = make(map[Value]*TreeNode, 1)
		}
		node.Children[sub.Key] = sub
		node.Sorted = append(node.Sorted, sub.Key)
	} else if rule != nil {
		node.Values = append(node.Values, rule)
	}
}

// Size returns the number of rules in the tree.
func (n *TreeNode) Size() int {
	s := len(n.Values)
	for _, c := range n.Children {
		s += c.Size()
	}
	return s
}

// Child returns n's child with key k.
func (n *TreeNode) Child(k Value) *TreeNode {
	switch k.(type) {
	case Ref, Call:
		return nil
	default:
		return n.Children[k]
	}
}

// Find dereferences ref along the tree
func (n *TreeNode) Find(ref Ref) *TreeNode {
	node := n
	for _, r := range ref {
		node = node.Child(r.Value)
		if node == nil {
			return nil
		}
	}
	return node
}

// Iteratively dereferences ref along the node's subtree.
// - If matching fails immediately, the tail will contain the full ref.
// - Partial matching will result in a tail of non-zero length.
// - A complete match will result in a 0 length tail.
func (n *TreeNode) find(ref Ref) (*TreeNode, Ref) {
	node := n
	for i := range ref {
		next := node.Child(ref[i].Value)
		if next == nil {
			tail := make(Ref, len(ref)-i)
			copy(tail, ref[i:])
			return node, tail
		}
		node = next
	}
	return node, nil
}

// DepthFirst performs a depth-first traversal of the rule tree rooted at n. If
// f returns true, traversal will not continue to the children of n.
func (n *TreeNode) DepthFirst(f func(*TreeNode) bool) {
	if f(n) {
		return
	}
	for _, node := range n.Children {
		node.DepthFirst(f)
	}
}

func (n *TreeNode) sort() {
	slices.SortFunc(n.Sorted, Value.Compare)
}

func treeNodeFromRef(ref Ref, rule *Rule) *TreeNode {
	depth := len(ref) - 1
	key := ref[depth].Value
	node := &TreeNode{
		Key:      key,
		Children: nil,
	}
	if rule != nil {
		node.Values = []any{rule}
	}

	for i := len(ref) - 2; i >= 0; i-- {
		key := ref[i].Value
		node = &TreeNode{
			Key:      key,
			Children: map[Value]*TreeNode{ref[i+1].Value: node},
			Sorted:   []Value{ref[i+1].Value},
		}
	}
	return node
}

// flattenChildren flattens all children's rule refs into a sorted array.
func (n *TreeNode) flattenChildren() []Ref {
	ret := newRefSet()
	for _, sub := range n.Children { // we only want the children, so don't use n.DepthFirst() right away
		sub.DepthFirst(func(x *TreeNode) bool {
			for _, r := range x.Values {
				rule := r.(*Rule)
				ret.AddPrefix(rule.Ref())
			}
			return false
		})
	}

	slices.SortFunc(ret.s, RefCompare)
	return ret.s
}

// Graph represents the graph of dependencies between rules.
type Graph struct {
	adj    map[util.T]map[util.T]struct{}
	radj   map[util.T]map[util.T]struct{}
	nodes  map[util.T]struct{}
	sorted []util.T
}

// NewGraph returns a new Graph based on modules. The list function must return
// the rules referred to directly by the ref.
func NewGraph(modules map[string]*Module, list func(Ref) []*Rule) *Graph {

	graph := &Graph{
		adj:    map[util.T]map[util.T]struct{}{},
		radj:   map[util.T]map[util.T]struct{}{},
		nodes:  map[util.T]struct{}{},
		sorted: nil,
	}

	// Create visitor to walk a rule AST and add edges to the rule graph for
	// each dependency.
	vis := func(a *Rule) *GenericVisitor {
		stop := false
		return NewGenericVisitor(func(x any) bool {
			switch x := x.(type) {
			case Ref:
				for _, b := range list(x) {
					for node := b; node != nil; node = node.Else {
						graph.addDependency(a, node)
					}
				}
			case *Rule:
				if stop {
					// Do not recurse into else clauses (which will be handled
					// by the outer visitor.)
					return true
				}
				stop = true
			}
			return false
		})
	}

	// Walk over all rules, add them to graph, and build adjacency lists.
	for _, module := range modules {
		WalkRules(module, func(a *Rule) bool {
			graph.addNode(a)
			vis(a).Walk(a)
			return false
		})
	}

	return graph
}

// Dependencies returns the set of rules that x depends on.
func (g *Graph) Dependencies(x util.T) map[util.T]struct{} {
	return g.adj[x]
}

// Dependents returns the set of rules that depend on x.
func (g *Graph) Dependents(x util.T) map[util.T]struct{} {
	return g.radj[x]
}

// Sort returns a slice of rules sorted by dependencies. If a cycle is found,
// ok is set to false.
func (g *Graph) Sort() (sorted []util.T, ok bool) {
	if g.sorted != nil {
		return g.sorted, true
	}

	sorter := &graphSort{
		sorted: make([]util.T, 0, len(g.nodes)),
		deps:   g.Dependencies,
		marked: map[util.T]struct{}{},
		temp:   map[util.T]struct{}{},
	}

	for node := range g.nodes {
		if !sorter.Visit(node) {
			return nil, false
		}
	}

	g.sorted = sorter.sorted
	return g.sorted, true
}

func (g *Graph) addDependency(u util.T, v util.T) {

	if _, ok := g.nodes[u]; !ok {
		g.addNode(u)
	}

	if _, ok := g.nodes[v]; !ok {
		g.addNode(v)
	}

	edges, ok := g.adj[u]
	if !ok {
		edges = map[util.T]struct{}{}
		g.adj[u] = edges
	}

	edges[v] = struct{}{}

	edges, ok = g.radj[v]
	if !ok {
		edges = map[util.T]struct{}{}
		g.radj[v] = edges
	}

	edges[u] = struct{}{}
}

func (g *Graph) addNode(n util.T) {
	g.nodes[n] = struct{}{}
}

type graphSort struct {
	sorted []util.T
	deps   func(util.T) map[util.T]struct{}
	marked map[util.T]struct{}
	temp   map[util.T]struct{}
}

func (sort *graphSort) Marked(node util.T) bool {
	_, marked := sort.marked[node]
	return marked
}

func (sort *graphSort) Visit(node util.T) (ok bool) {
	if _, ok := sort.temp[node]; ok {
		return false
	}
	if sort.Marked(node) {
		return true
	}
	sort.temp[node] = struct{}{}
	for other := range sort.deps(node) {
		if !sort.Visit(other) {
			return false
		}
	}
	sort.marked[node] = struct{}{}
	delete(sort.temp, node)
	sort.sorted = append(sort.sorted, node)
	return true
}

// GraphTraversal is a Traversal that understands the dependency graph
type GraphTraversal struct {
	graph   *Graph
	visited map[util.T]struct{}
}

// NewGraphTraversal returns a Traversal for the dependency graph
func NewGraphTraversal(graph *Graph) *GraphTraversal {
	return &GraphTraversal{
		graph:   graph,
		visited: map[util.T]struct{}{},
	}
}

// Edges lists all dependency connections for a given node
func (g *GraphTraversal) Edges(x util.T) []util.T {
	r := []util.T{}
	for v := range g.graph.Dependencies(x) {
		r = append(r, v)
	}
	return r
}

// Visited returns whether a node has been visited, setting a node to visited if not
func (g *GraphTraversal) Visited(u util.T) bool {
	_, ok := g.visited[u]
	g.visited[u] = struct{}{}
	return ok
}

type unsafePair struct {
	Expr *Expr
	Vars VarSet
}

type unsafeVarLoc struct {
	Var Var
	Loc *Location
}

type unsafeVars map[*Expr]VarSet

func (vs unsafeVars) Add(e *Expr, v Var) {
	if u, ok := vs[e]; ok {
		u[v] = struct{}{}
	} else {
		vs[e] = VarSet{v: struct{}{}}
	}
}

func (vs unsafeVars) Set(e *Expr, s VarSet) {
	vs[e] = s
}

func (vs unsafeVars) Update(o unsafeVars) {
	for k, v := range o {
		if _, ok := vs[k]; !ok {
			vs[k] = VarSet{}
		}
		vs[k].Update(v)
	}
}

func (vs unsafeVars) Vars() (result []unsafeVarLoc) {

	locs := map[Var]*Location{}

	// If var appears in multiple sets then pick first by location.
	for expr, vars := range vs {
		for v := range vars {
			if locs[v].Compare(expr.Location) > 0 {
				locs[v] = expr.Location
			}
		}
	}

	for v, loc := range locs {
		result = append(result, unsafeVarLoc{
			Var: v,
			Loc: loc,
		})
	}

	slices.SortFunc(result, func(a, b unsafeVarLoc) int {
		return a.Loc.Compare(b.Loc)
	})

	return result
}

func (vs unsafeVars) Slice() (result []unsafePair) {
	for expr, vs := range vs {
		result = append(result, unsafePair{
			Expr: expr,
			Vars: vs,
		})
	}
	return
}

// reorderBodyForSafety returns a copy of the body ordered such that
// left to right evaluation of the body will not encounter unbound variables
// in input positions or negated expressions.
//
// Expressions are added to the re-ordered body as soon as they are considered
// safe. If multiple expressions become safe in the same pass, they are added
// in their original order. This results in minimal re-ordering of the body.
//
// If the body cannot be reordered to ensure safety, the second return value
// contains a mapping of expressions to unsafe variables in those expressions.
func reorderBodyForSafety(builtins map[string]*Builtin, arity func(Ref) int, globals VarSet, body Body) (Body, unsafeVars) {
	vis := varVisitorPool.Get().WithParams(SafetyCheckVisitorParams)
	vis.WalkBody(body)

	defer varVisitorPool.Put(vis)

	bodyVars := vis.Vars().Copy()
	safe := bodyVars.Intersect(globals)
	unsafe := make(unsafeVars, len(bodyVars)-len(safe))

	for _, e := range body {
		vis.Clear().WithParams(SafetyCheckVisitorParams).Walk(e)
		for v := range vis.Vars() {
			if _, ok := safe[v]; !ok {
				unsafe.Add(e, v)
			}
		}
	}

	reordered := make(Body, 0, len(body))
	output := VarSet{}

	for {
		n := len(reordered)

		for _, e := range body {
			if reordered.Contains(e) {
				continue
			}

			ovs := outputVarsForExpr(e, arity, safe, output)

			// check closures: is this expression closing over variables that
			// haven't been made safe by what's already included in `reordered`?
			vs := unsafeVarsInClosures(e)
			cv := vs.Intersect(bodyVars).Diff(globals)
			ob := outputVarsForBody(reordered, arity, safe)

			if cv.DiffCount(ob) > 0 {
				uv := cv.Diff(ob)
				if uv.Equal(ovs) { // special case "closure-self"
					continue
				}
				unsafe.Set(e, uv)
			}

			for v := range unsafe[e] {
				if ovs.Contains(v) || safe.Contains(v) {
					delete(unsafe[e], v)
				}
			}

			if len(unsafe[e]) == 0 {
				delete(unsafe, e)
				reordered.Append(e)
				safe.Update(ovs) // this expression's outputs are safe
			}
		}

		if len(reordered) == n { // fixed point, could not add any expr of body
			break
		}
	}

	// Recursively visit closures and perform the safety checks on them.
	// Update the globals at each expression to include the variables that could
	// be closed over.
	g := globals.Copy()
	xform := &bodySafetyTransformer{
		builtins: builtins,
		arity:    arity,
	}
	gvis := &GenericVisitor{}
	for i, e := range reordered {
		if i > 0 {
			vis.Walk(reordered[i-1])
			g.Update(vis.Vars())
			vis.Clear().WithParams(SafetyCheckVisitorParams)
		}
		xform.current = e
		xform.globals = g
		xform.unsafe = unsafe
		gvis.f = xform.Visit
		gvis.Walk(e)
	}

	return reordered, unsafe
}

type bodySafetyTransformer struct {
	builtins map[string]*Builtin
	arity    func(Ref) int
	current  *Expr
	globals  VarSet
	unsafe   unsafeVars
}

func (xform *bodySafetyTransformer) Visit(x any) bool {
	switch term := x.(type) {
	case *Term:
		switch x := term.Value.(type) {
		case *object:
			cpy, _ := x.Map(func(k, v *Term) (*Term, *Term, error) {
				kcpy := k.Copy()
				NewGenericVisitor(xform.Visit).Walk(kcpy)
				vcpy := v.Copy()
				NewGenericVisitor(xform.Visit).Walk(vcpy)
				return kcpy, vcpy, nil
			})
			term.Value = cpy
			return true
		case *set:
			cpy, _ := x.Map(func(v *Term) (*Term, error) {
				vcpy := v.Copy()
				NewGenericVisitor(xform.Visit).Walk(vcpy)
				return vcpy, nil
			})
			term.Value = cpy
			return true
		case *ArrayComprehension:
			xform.reorderArrayComprehensionSafety(x)
			return true
		case *ObjectComprehension:
			xform.reorderObjectComprehensionSafety(x)
			return true
		case *SetComprehension:
			xform.reorderSetComprehensionSafety(x)
			return true
		}
	case *Expr:
		if ev, ok := term.Terms.(*Every); ok {
			xform.globals.Update(ev.KeyValueVars())
			ev.Body = xform.reorderComprehensionSafety(NewVarSet(), ev.Body)
			return true
		}
	}
	return false
}

func (xform *bodySafetyTransformer) reorderComprehensionSafety(tv VarSet, body Body) Body {
	bv := body.Vars(SafetyCheckVisitorParams)
	bv.Update(xform.globals)

	if tv.DiffCount(bv) > 0 {
		uv := tv.Diff(bv)
		for v := range uv {
			xform.unsafe.Add(xform.current, v)
		}
	}

	r, u := reorderBodyForSafety(xform.builtins, xform.arity, xform.globals, body)
	if len(u) == 0 {
		return r
	}

	xform.unsafe.Update(u)
	return body
}

func (xform *bodySafetyTransformer) reorderArrayComprehensionSafety(ac *ArrayComprehension) {
	ac.Body = xform.reorderComprehensionSafety(ac.Term.Vars(), ac.Body)
}

func (xform *bodySafetyTransformer) reorderObjectComprehensionSafety(oc *ObjectComprehension) {
	tv := oc.Key.Vars()
	tv.Update(oc.Value.Vars())
	oc.Body = xform.reorderComprehensionSafety(tv, oc.Body)
}

func (xform *bodySafetyTransformer) reorderSetComprehensionSafety(sc *SetComprehension) {
	sc.Body = xform.reorderComprehensionSafety(sc.Term.Vars(), sc.Body)
}

// unsafeVarsInClosures collects vars that are contained in closures within
// this expression.
func unsafeVarsInClosures(e *Expr) VarSet {
	vs := VarSet{}
	WalkClosures(e, func(x any) bool {
		vis := &VarVisitor{vars: vs}
		if ev, ok := x.(*Every); ok {
			vis.WalkBody(ev.Body)
			return true
		}
		vis.Walk(x)
		return true
	})
	return vs
}

// OutputVarsFromBody returns all variables which are the "output" for
// the given body. For safety checks this means that they would be
// made safe by the body.
func OutputVarsFromBody(c *Compiler, body Body, safe VarSet) VarSet {
	return outputVarsForBody(body, c.GetArity, safe)
}

func outputVarsForBody(body Body, arity func(Ref) int, safe VarSet) VarSet {
	o := safe.Copy()
	output := VarSet{}
	for _, e := range body {
		o.Update(outputVarsForExpr(e, arity, o, output))
	}
	return o.Diff(safe)
}

// OutputVarsFromExpr returns all variables which are the "output" for
// the given expression. For safety checks this means that they would be
// made safe by the expr.
func OutputVarsFromExpr(c *Compiler, expr *Expr, safe VarSet) VarSet {
	return outputVarsForExpr(expr, c.GetArity, safe, VarSet{})
}

func outputVarsForExpr(expr *Expr, arity func(Ref) int, safe VarSet, output VarSet) VarSet {
	// Negated expressions must be safe.
	if expr.Negated {
		return VarSet{}
	}

	var vis *VarVisitor

	// With modifier inputs must be safe.
	for _, with := range expr.With {
		vis = vis.ClearOrNew().WithParams(SafetyCheckVisitorParams)
		vis.Walk(with)
		if vis.Vars().DiffCount(safe) > 0 {
			return VarSet{}
		}
	}

	switch terms := expr.Terms.(type) {
	case *Term:
		return outputVarsForTerms(expr, safe)
	case []*Term:
		if expr.IsEquality() {
			return outputVarsForExprEq(expr, safe, output)
		}

		operator, ok := terms[0].Value.(Ref)
		if !ok {
			return VarSet{}
		}

		ar := arity(operator)
		if ar < 0 {
			return VarSet{}
		}

		return outputVarsForExprCall(expr, ar, safe, terms, vis, output)
	case *Every:
		return outputVarsForTerms(terms.Domain, safe)
	default:
		panic("illegal expression")
	}
}

func outputVarsForExprEq(expr *Expr, safe VarSet, output VarSet) VarSet {
	if !validEqAssignArgCount(expr) {
		return safe
	}

	output.Update(outputVarsForTerms(expr, safe))
	output.Update(safe)
	output.Update(Unify(output, expr.Operand(0), expr.Operand(1)))

	diff := output.Diff(safe)

	clear(output)

	return diff
}

func outputVarsForExprCall(expr *Expr, arity int, safe VarSet, terms []*Term, vis *VarVisitor, output VarSet) VarSet {
	clear(output)

	output.Update(outputVarsForTerms(expr, safe))

	numInputTerms := arity + 1
	if numInputTerms >= len(terms) {
		return output
	}

	params := VarVisitorParams{
		SkipClosures:   true,
		SkipSets:       true,
		SkipObjectKeys: true,
		SkipRefHead:    true,
	}
	vis = vis.ClearOrNew().WithParams(params)
	vis.WalkArgs(Args(terms[:numInputTerms]))

	unsafe := vis.Vars().Diff(output).DiffCount(safe)
	if unsafe > 0 {
		return VarSet{}
	}

	vis = vis.Clear().WithParams(params)
	vis.WalkArgs(Args(terms[numInputTerms:]))
	output.Update(vis.vars)
	return output
}

func outputVarsForTerms(expr any, safe VarSet) VarSet {
	output := VarSet{}
	WalkTerms(expr, func(x *Term) bool {
		switch r := x.Value.(type) {
		case *SetComprehension, *ArrayComprehension, *ObjectComprehension:
			return true
		case Ref:
			if !isRefSafe(r, safe) {
				return true
			}
			if !r.IsGround() {
				// Avoiding r.OutputVars() here as it won't allow reusing the visitor.
				vis := varVisitorPool.Get().WithParams(VarVisitorParams{SkipRefHead: true})
				vis.WalkRef(r)
				output.Update(vis.Vars())
				varVisitorPool.Put(vis)
			}
		}
		return false
	})
	return output
}

type equalityFactory struct {
	gen *localVarGenerator
}

func newEqualityFactory(gen *localVarGenerator) *equalityFactory {
	return &equalityFactory{gen}
}

func (f *equalityFactory) Generate(other *Term) *Expr {
	term := NewTerm(f.gen.Generate()).SetLocation(other.Location)
	expr := Equality.Expr(term, other)
	expr.Generated = true
	expr.Location = other.Location
	return expr
}

// TODO: Move to internal package?
const LocalVarPrefix = "__local"

type localVarGenerator struct {
	exclude VarSet
	suffix  string
	next    int
}

func newLocalVarGeneratorForModuleSet(sorted []string, modules map[string]*Module) *localVarGenerator {
	vis := NewVarVisitor()
	for _, key := range sorted {
		vis.Walk(modules[key])
	}
	return &localVarGenerator{exclude: vis.vars, next: 0}
}

func newLocalVarGenerator(suffix string, node any) *localVarGenerator {
	vis := NewVarVisitor()
	vis.Walk(node)
	return &localVarGenerator{exclude: vis.vars, suffix: suffix, next: 0}
}

func (l *localVarGenerator) Generate() Var {
	for {
		result := Var(LocalVarPrefix + l.suffix + strconv.Itoa(l.next) + "__")
		l.next++
		if !l.exclude.Contains(result) {
			return result
		}
	}
}

func getGlobals(pkg *Package, rules []Ref, imports []*Import) map[Var]*usedRef {
	globals := make(map[Var]*usedRef, len(rules)+len(imports))

	for _, ref := range rules {
		v := ref[0].Value.(Var)
		globals[v] = &usedRef{ref: pkg.Path.Append(StringTerm(string(v)))}
	}

	for _, imp := range imports {
		path := imp.Path.Value.(Ref)
		if FutureRootDocument.Equal(path[0]) || RegoRootDocument.Equal(path[0]) {
			continue
		}
		globals[imp.Name()] = &usedRef{ref: path}
	}

	return globals
}

func requiresEval(x *Term) bool {
	if x == nil {
		return false
	}
	return ContainsRefs(x) || ContainsComprehensions(x)
}

func resolveRef(globals map[Var]*usedRef, ignore *declaredVarStack, ref Ref) Ref {

	r := Ref{}
	for i, x := range ref {
		switch v := x.Value.(type) {
		case Var:
			if g, ok := globals[v]; ok && !ignore.Contains(v) {
				cpy := g.ref.Copy()
				for i := range cpy {
					cpy[i].SetLocation(x.Location)
				}
				if i == 0 {
					r = cpy
				} else {
					r = append(r, NewTerm(cpy).SetLocation(x.Location))
				}
				g.used = true
			} else {
				r = append(r, x)
			}
		case Ref, *Array, Object, Set, *ArrayComprehension, *SetComprehension, *ObjectComprehension, Call:
			r = append(r, resolveRefsInTerm(globals, ignore, x))
		default:
			r = append(r, x)
		}
	}

	return r
}

type usedRef struct {
	ref  Ref
	used bool
}

func resolveRefsInRule(globals map[Var]*usedRef, rule *Rule) error {
	ignore := &declaredVarStack{}

	vars := NewVarSet()
	var vis *GenericVisitor
	var err error

	// Walk args to collect vars and transform body so that callers can shadow
	// root documents.
	vis = NewGenericVisitor(func(x any) bool {
		if err != nil {
			return true
		}
		switch x := x.(type) {
		case Var:
			vars.Add(x)

		// Object keys cannot be pattern matched so only walk values.
		case *object:
			x.Foreach(func(_, v *Term) {
				vis.Walk(v)
			})

		// Skip terms that could contain vars that cannot be pattern matched.
		case Set, *ArrayComprehension, *SetComprehension, *ObjectComprehension, Call:
			return true

		case *Term:
			if _, ok := x.Value.(Ref); ok {
				if RootDocumentRefs.Contains(x) {
					// We could support args named input, data, etc. however
					// this would require rewriting terms in the head and body.
					// Preventing root document shadowing is simpler, and
					// arguably, will prevent confusing names from being used.
					// NOTE: this check is also performed as part of strict-mode in
					// checkRootDocumentOverrides.
					err = fmt.Errorf("args must not shadow %v (use a different variable name)", x)
					return true
				}
			}
		}
		return false
	})

	vis.Walk(rule.Head.Args)

	if err != nil {
		return err
	}

	ignore.Push(vars)
	ignore.Push(declaredVars(rule.Body))

	ref := rule.Head.Ref()
	for i := 1; i < len(ref); i++ {
		ref[i] = resolveRefsInTerm(globals, ignore, ref[i])
	}
	if rule.Head.Key != nil {
		rule.Head.Key = resolveRefsInTerm(globals, ignore, rule.Head.Key)
	}

	if rule.Head.Value != nil {
		rule.Head.Value = resolveRefsInTerm(globals, ignore, rule.Head.Value)
	}

	rule.Body = resolveRefsInBody(globals, ignore, rule.Body)
	return nil
}

func resolveRefsInBody(globals map[Var]*usedRef, ignore *declaredVarStack, body Body) Body {
	r := make([]*Expr, 0, len(body))
	for _, expr := range body {
		r = append(r, resolveRefsInExpr(globals, ignore, expr))
	}
	return r
}

func resolveRefsInExpr(globals map[Var]*usedRef, ignore *declaredVarStack, expr *Expr) *Expr {
	cpy := *expr
	switch ts := expr.Terms.(type) {
	case *Term:
		cpy.Terms = resolveRefsInTerm(globals, ignore, ts)
	case []*Term:
		buf := make([]*Term, len(ts))
		for i := range ts {
			buf[i] = resolveRefsInTerm(globals, ignore, ts[i])
		}
		cpy.Terms = buf
	case *SomeDecl:
		if val, ok := ts.Symbols[0].Value.(Call); ok {
			cpy.Terms = &SomeDecl{
				Symbols:  []*Term{CallTerm(resolveRefsInTermSlice(globals, ignore, val)...)},
				Location: ts.Location,
			}
		}
	case *Every:
		locals := NewVarSet()
		if ts.Key != nil {
			locals.Update(ts.Key.Vars())
		}
		locals.Update(ts.Value.Vars())
		ignore.Push(locals)
		cpy.Terms = &Every{
			Key:    ts.Key.Copy(),   // TODO(sr): do more?
			Value:  ts.Value.Copy(), // TODO(sr): do more?
			Domain: resolveRefsInTerm(globals, ignore, ts.Domain),
			Body:   resolveRefsInBody(globals, ignore, ts.Body),
		}
		ignore.Pop()
	}
	for _, w := range cpy.With {
		w.Target = resolveRefsInTerm(globals, ignore, w.Target)
		w.Value = resolveRefsInTerm(globals, ignore, w.Value)
	}
	return &cpy
}

func resolveRefsInTerm(globals map[Var]*usedRef, ignore *declaredVarStack, term *Term) *Term {
	switch v := term.Value.(type) {
	case Var:
		if g, ok := globals[v]; ok && !ignore.Contains(v) {
			cpy := g.ref.Copy()
			for i := range cpy {
				cpy[i].SetLocation(term.Location)
			}
			g.used = true
			return NewTerm(cpy).SetLocation(term.Location)
		}
		return term
	case Ref:
		fqn := resolveRef(globals, ignore, v)
		cpy := *term
		cpy.Value = fqn
		return &cpy
	case *object:
		cpy := *term
		cpy.Value, _ = v.Map(func(k, v *Term) (*Term, *Term, error) {
			k = resolveRefsInTerm(globals, ignore, k)
			v = resolveRefsInTerm(globals, ignore, v)
			return k, v, nil
		})
		return &cpy
	case *Array:
		cpy := *term
		cpy.Value = NewArray(resolveRefsInTermArray(globals, ignore, v)...)
		return &cpy
	case Call:
		cpy := *term
		cpy.Value = Call(resolveRefsInTermSlice(globals, ignore, v))
		return &cpy
	case Set:
		s, _ := v.Map(func(e *Term) (*Term, error) {
			return resolveRefsInTerm(globals, ignore, e), nil
		})
		cpy := *term
		cpy.Value = s
		return &cpy
	case *ArrayComprehension:
		ac := &ArrayComprehension{}
		ignore.Push(declaredVars(v.Body))
		ac.Term = resolveRefsInTerm(globals, ignore, v.Term)
		ac.Body = resolveRefsInBody(globals, ignore, v.Body)
		cpy := *term
		cpy.Value = ac
		ignore.Pop()
		return &cpy
	case *ObjectComprehension:
		oc := &ObjectComprehension{}
		ignore.Push(declaredVars(v.Body))
		oc.Key = resolveRefsInTerm(globals, ignore, v.Key)
		oc.Value = resolveRefsInTerm(globals, ignore, v.Value)
		oc.Body = resolveRefsInBody(globals, ignore, v.Body)
		cpy := *term
		cpy.Value = oc
		ignore.Pop()
		return &cpy
	case *SetComprehension:
		sc := &SetComprehension{}
		ignore.Push(declaredVars(v.Body))
		sc.Term = resolveRefsInTerm(globals, ignore, v.Term)
		sc.Body = resolveRefsInBody(globals, ignore, v.Body)
		cpy := *term
		cpy.Value = sc
		ignore.Pop()
		return &cpy
	default:
		return term
	}
}

func resolveRefsInTermArray(globals map[Var]*usedRef, ignore *declaredVarStack, terms *Array) []*Term {
	cpy := make([]*Term, terms.Len())
	for i := range terms.Len() {
		cpy[i] = resolveRefsInTerm(globals, ignore, terms.Elem(i))
	}
	return cpy
}

func resolveRefsInTermSlice(globals map[Var]*usedRef, ignore *declaredVarStack, terms []*Term) []*Term {
	cpy := make([]*Term, len(terms))
	for i := range terms {
		cpy[i] = resolveRefsInTerm(globals, ignore, terms[i])
	}
	return cpy
}

type declaredVarStack []VarSet

func (s declaredVarStack) Contains(v Var) bool {
	for i := len(s) - 1; i >= 0; i-- {
		if _, ok := s[i][v]; ok {
			return ok
		}
	}
	return false
}

func (s declaredVarStack) Add(v Var) {
	s[len(s)-1].Add(v)
}

func (s *declaredVarStack) Push(vs VarSet) {
	*s = append(*s, vs)
}

func (s *declaredVarStack) Pop() {
	curr := *s
	*s = curr[:len(curr)-1]
}

func declaredVars(x any) VarSet {
	vars := NewVarSet()
	vis := NewGenericVisitor(func(x any) bool {
		switch x := x.(type) {
		case *Expr:
			if x.IsAssignment() && validEqAssignArgCount(x) {
				WalkVars(x.Operand(0), func(v Var) bool {
					vars.Add(v)
					return false
				})
			} else if decl, ok := x.Terms.(*SomeDecl); ok {
				for i := range decl.Symbols {
					switch val := decl.Symbols[i].Value.(type) {
					case Var:
						vars.Add(val)
					case Call:
						args := val[1:]
						if len(args) == 3 { // some x, y in xs
							WalkVars(args[1], func(v Var) bool {
								vars.Add(v)
								return false
							})
						}
						// some x in xs
						WalkVars(args[0], func(v Var) bool {
							vars.Add(v)
							return false
						})
					}
				}
			}
		case *ArrayComprehension, *SetComprehension, *ObjectComprehension:
			return true
		}
		return false
	})
	vis.Walk(x)
	return vars
}

// rewriteComprehensionTerms will rewrite comprehensions so that the term part
// is bound to a variable in the body. This allows any type of term to be used
// in the term part (even if the term requires evaluation.)
//
// For instance, given the following comprehension:
//
// [x[0] | x = y[_]; y = [1,2,3]]
//
// The comprehension would be rewritten as:
//
// [__local0__ | x = y[_]; y = [1,2,3]; __local0__ = x[0]]
func rewriteComprehensionTerms(f *equalityFactory, node any) (any, error) {
	return TransformComprehensions(node, func(x any) (Value, error) {
		switch x := x.(type) {
		case *ArrayComprehension:
			if requiresEval(x.Term) {
				expr := f.Generate(x.Term)
				x.Term = expr.Operand(0)
				x.Body.Append(expr)
			}
			return x, nil
		case *SetComprehension:
			if requiresEval(x.Term) {
				expr := f.Generate(x.Term)
				x.Term = expr.Operand(0)
				x.Body.Append(expr)
			}
			return x, nil
		case *ObjectComprehension:
			if requiresEval(x.Key) {
				expr := f.Generate(x.Key)
				x.Key = expr.Operand(0)
				x.Body.Append(expr)
			}
			if requiresEval(x.Value) {
				expr := f.Generate(x.Value)
				x.Value = expr.Operand(0)
				x.Body.Append(expr)
			}
			return x, nil
		}
		panic("illegal type")
	})
}

// rewriteEquals will rewrite exprs under x as unification calls instead of ==
// calls. For example:
//
// data.foo == data.bar is rewritten as data.foo = data.bar
//
// This stage should only run the safety check (since == is a built-in with no
// outputs, so the inputs must not be marked as safe.)
//
// This stage is not executed by the query compiler by default because when
// callers specify == instead of = they expect to receive a true/false/undefined
// result back whereas with = the result is only ever true/undefined. For
// partial evaluation cases we do want to rewrite == to = to simplify the
// result.
func rewriteEquals(x any) (modified bool) {
	unifyOp := Equality.Ref()
	t := NewGenericTransformer(func(x any) (any, error) {
		if x, ok := x.(*Expr); ok && x.IsCall() {
			operator := x.Operator()
			if operator.Equal(doubleEq) && len(x.Operands()) == 2 {
				modified = true
				x.SetOperator(NewTerm(unifyOp))
			}
		}
		return x, nil
	})
	_, _ = Transform(t, x) // ignore error
	return modified
}

func rewriteTestEqualities(f *equalityFactory, body Body) Body {
	result := make(Body, 0, len(body))
	for _, expr := range body {
		// We can't rewrite negated expressions; if the extracted term is undefined, evaluation would fail before
		// reaching the negation check.
		if !expr.Negated && !expr.Generated {
			switch {
			case expr.IsEquality():
				terms := expr.Terms.([]*Term)
				result, terms[1] = rewriteDynamicsShallow(expr, f, terms[1], result)
				result, terms[2] = rewriteDynamicsShallow(expr, f, terms[2], result)
			case expr.IsEvery():
				// We rewrite equalities inside of every-bodies as a fail here will be the cause of the test-rule fail.
				// Failures inside other expressions with closures, such as comprehensions, won't cause the test-rule to fail, so we skip those.
				every := expr.Terms.(*Every)
				every.Body = rewriteTestEqualities(f, every.Body)
			}
		}
		result = appendExpr(result, expr)
	}
	return result
}

func rewriteDynamicsShallow(original *Expr, f *equalityFactory, term *Term, result Body) (Body, *Term) {
	switch term.Value.(type) {
	case Ref, *ArrayComprehension, *SetComprehension, *ObjectComprehension:
		generated := f.Generate(term)
		generated.With = original.With
		result.Append(generated)
		connectGeneratedExprs(original, generated)
		return result, result[len(result)-1].Operand(0)
	}
	return result, term
}

// rewriteDynamics will rewrite the body so that dynamic terms (i.e., refs and
// comprehensions) are bound to vars earlier in the query. This translation
// results in eager evaluation.
//
// For instance, given the following query:
//
// foo(data.bar) = 1
//
// The rewritten version will be:
//
// __local0__ = data.bar; foo(__local0__) = 1
func rewriteDynamics(f *equalityFactory, body Body) Body {
	result := make(Body, 0, len(body))
	for _, expr := range body {
		switch {
		case expr.IsEquality():
			result = rewriteDynamicsEqExpr(f, expr, result)
		case expr.IsCall():
			result = rewriteDynamicsCallExpr(f, expr, result)
		case expr.IsEvery():
			result = rewriteDynamicsEveryExpr(f, expr, result)
		default:
			result = rewriteDynamicsTermExpr(f, expr, result)
		}
	}
	return result
}

func appendExpr(body Body, expr *Expr) Body {
	body.Append(expr)
	return body
}

func rewriteDynamicsEqExpr(f *equalityFactory, expr *Expr, result Body) Body {
	if !validEqAssignArgCount(expr) {
		return appendExpr(result, expr)
	}
	terms := expr.Terms.([]*Term)
	result, terms[1] = rewriteDynamicsInTerm(expr, f, terms[1], result)
	result, terms[2] = rewriteDynamicsInTerm(expr, f, terms[2], result)
	return appendExpr(result, expr)
}

func rewriteDynamicsCallExpr(f *equalityFactory, expr *Expr, result Body) Body {
	terms := expr.Terms.([]*Term)
	for i := 1; i < len(terms); i++ {
		result, terms[i] = rewriteDynamicsOne(expr, f, terms[i], result)
	}
	return appendExpr(result, expr)
}

func rewriteDynamicsEveryExpr(f *equalityFactory, expr *Expr, result Body) Body {
	ev := expr.Terms.(*Every)
	result, ev.Domain = rewriteDynamicsOne(expr, f, ev.Domain, result)
	ev.Body = rewriteDynamics(f, ev.Body)
	return appendExpr(result, expr)
}

func rewriteDynamicsTermExpr(f *equalityFactory, expr *Expr, result Body) Body {
	term := expr.Terms.(*Term)
	result, expr.Terms = rewriteDynamicsInTerm(expr, f, term, result)
	return appendExpr(result, expr)
}

func rewriteDynamicsInTerm(original *Expr, f *equalityFactory, term *Term, result Body) (Body, *Term) {
	switch v := term.Value.(type) {
	case Ref:
		for i := 1; i < len(v); i++ {
			result, v[i] = rewriteDynamicsOne(original, f, v[i], result)
		}
	case *ArrayComprehension:
		v.Body = rewriteDynamics(f, v.Body)
	case *SetComprehension:
		v.Body = rewriteDynamics(f, v.Body)
	case *ObjectComprehension:
		v.Body = rewriteDynamics(f, v.Body)
	default:
		result, term = rewriteDynamicsOne(original, f, term, result)
	}
	return result, term
}

func rewriteDynamicsOne(original *Expr, f *equalityFactory, term *Term, result Body) (Body, *Term) {
	switch v := term.Value.(type) {
	case Ref:
		for i := 1; i < len(v); i++ {
			result, v[i] = rewriteDynamicsOne(original, f, v[i], result)
		}
		generated := f.Generate(term)
		generated.With = original.With
		result.Append(generated)
		connectGeneratedExprs(original, generated)
		return result, result[len(result)-1].Operand(0)
	case *Array:
		for i := range v.Len() {
			var t *Term
			result, t = rewriteDynamicsOne(original, f, v.Elem(i), result)
			v.set(i, t)
		}
		return result, term
	case *object:
		cpy := NewObject()
		v.Foreach(func(key, value *Term) {
			result, key = rewriteDynamicsOne(original, f, key, result)
			result, value = rewriteDynamicsOne(original, f, value, result)
			cpy.Insert(key, value)
		})
		return result, NewTerm(cpy).SetLocation(term.Location)
	case Set:
		cpy := NewSet()
		for _, term := range v.Slice() {
			var rw *Term
			result, rw = rewriteDynamicsOne(original, f, term, result)
			cpy.Add(rw)
		}
		return result, NewTerm(cpy).SetLocation(term.Location)
	case *ArrayComprehension:
		var extra *Expr
		v.Body, extra = rewriteDynamicsComprehensionBody(original, f, v.Body, term)
		result.Append(extra)
		connectGeneratedExprs(original, extra)
		return result, result[len(result)-1].Operand(0)
	case *SetComprehension:
		var extra *Expr
		v.Body, extra = rewriteDynamicsComprehensionBody(original, f, v.Body, term)
		result.Append(extra)
		connectGeneratedExprs(original, extra)
		return result, result[len(result)-1].Operand(0)
	case *ObjectComprehension:
		var extra *Expr
		v.Body, extra = rewriteDynamicsComprehensionBody(original, f, v.Body, term)
		result.Append(extra)
		connectGeneratedExprs(original, extra)
		return result, result[len(result)-1].Operand(0)
	}
	return result, term
}

func rewriteDynamicsComprehensionBody(original *Expr, f *equalityFactory, body Body, term *Term) (Body, *Expr) {
	body = rewriteDynamics(f, body)
	generated := f.Generate(term)
	generated.With = original.With
	return body, generated
}

func rewriteExprTermsInHead(gen *localVarGenerator, rule *Rule) {
	for i := range rule.Head.Args {
		support, output := expandExprTerm(gen, rule.Head.Args[i])
		for j := range support {
			rule.Body.Append(support[j])
		}
		rule.Head.Args[i] = output
	}
	if rule.Head.Key != nil {
		support, output := expandExprTerm(gen, rule.Head.Key)
		for i := range support {
			rule.Body.Append(support[i])
		}
		rule.Head.Key = output
	}
	if rule.Head.Value != nil {
		support, output := expandExprTerm(gen, rule.Head.Value)
		for i := range support {
			rule.Body.Append(support[i])
		}
		rule.Head.Value = output
	}
}

func rewriteExprTermsInBody(gen *localVarGenerator, body Body) Body {
	cpy := make(Body, 0, len(body))
	for i := range body {
		for _, expr := range expandExpr(gen, body[i]) {
			cpy.Append(expr)
		}
	}
	return cpy
}

func expandExpr(gen *localVarGenerator, expr *Expr) (result []*Expr) {
	for i := range expr.With {
		extras, value := expandExprTerm(gen, expr.With[i].Value)
		expr.With[i].Value = value
		result = append(result, extras...)
	}
	switch terms := expr.Terms.(type) {
	case *Term:
		extras, term := expandExprTerm(gen, terms)
		if len(expr.With) > 0 {
			for i := range extras {
				extras[i].With = expr.With
			}
		}
		result = append(result, extras...)
		expr.Terms = term
		result = append(result, expr)
	case []*Term:
		for i := 1; i < len(terms); i++ {
			var extras []*Expr
			extras, terms[i] = expandExprTerm(gen, terms[i])
			connectGeneratedExprs(expr, extras...)
			if len(expr.With) > 0 {
				for i := range extras {
					extras[i].With = expr.With
				}
			}
			result = append(result, extras...)
		}
		result = append(result, expr)
	case *Every:
		var extras []*Expr

		term := NewTerm(gen.Generate()).SetLocation(terms.Domain.Location)
		eq := Equality.Expr(term, terms.Domain).SetLocation(terms.Domain.Location)
		eq.Generated = true
		eq.With = expr.With
		extras = expandExpr(gen, eq)
		terms.Domain = term

		terms.Body = rewriteExprTermsInBody(gen, terms.Body)
		result = append(result, extras...)
		result = append(result, expr)
	}
	return
}

func connectGeneratedExprs(parent *Expr, children ...*Expr) {
	for _, child := range children {
		child.generatedFrom = parent
		parent.generates = append(parent.generates, child)
	}
}

func expandExprTerm(gen *localVarGenerator, term *Term) (support []*Expr, output *Term) {
	output = term
	switch v := term.Value.(type) {
	case Call:
		for i := 1; i < len(v); i++ {
			var extras []*Expr
			extras, v[i] = expandExprTerm(gen, v[i])
			support = append(support, extras...)
		}
		output = NewTerm(gen.Generate()).SetLocation(term.Location)
		expr := v.MakeExpr(output).SetLocation(term.Location)
		expr.Generated = true
		support = append(support, expr)
	case Ref:
		support = expandExprRef(gen, v)
	case *Array:
		support = expandExprTermArray(gen, v)
	case *object:
		cpy, _ := v.Map(func(k, v *Term) (*Term, *Term, error) {
			extras1, expandedKey := expandExprTerm(gen, k)
			extras2, expandedValue := expandExprTerm(gen, v)
			support = append(support, extras1...)
			support = append(support, extras2...)
			return expandedKey, expandedValue, nil
		})
		output = NewTerm(cpy).SetLocation(term.Location)
	case Set:
		cpy, _ := v.Map(func(x *Term) (*Term, error) {
			extras, expanded := expandExprTerm(gen, x)
			support = append(support, extras...)
			return expanded, nil
		})
		output = NewTerm(cpy).SetLocation(term.Location)
	case *ArrayComprehension:
		support, term := expandExprTerm(gen, v.Term)
		for i := range support {
			v.Body.Append(support[i])
		}
		v.Term = term
		v.Body = rewriteExprTermsInBody(gen, v.Body)
	case *SetComprehension:
		support, term := expandExprTerm(gen, v.Term)
		for i := range support {
			v.Body.Append(support[i])
		}
		v.Term = term
		v.Body = rewriteExprTermsInBody(gen, v.Body)
	case *ObjectComprehension:
		support, key := expandExprTerm(gen, v.Key)
		for i := range support {
			v.Body.Append(support[i])
		}
		v.Key = key
		support, value := expandExprTerm(gen, v.Value)
		for i := range support {
			v.Body.Append(support[i])
		}
		v.Value = value
		v.Body = rewriteExprTermsInBody(gen, v.Body)
	}
	return
}

func expandExprRef(gen *localVarGenerator, v []*Term) (support []*Expr) {
	// Start by calling a normal expandExprTerm on all terms.
	support = expandExprTermSlice(gen, v)

	// Rewrite references in order to support indirect references.  We rewrite
	// e.g.
	//
	//     [1, 2, 3][i]
	//
	// to
	//
	//     __local_var = [1, 2, 3]
	//     __local_var[i]
	//
	// to support these.  This only impacts the reference subject, i.e. the
	// first item in the slice.
	var subject = v[0]
	switch subject.Value.(type) {
	case *Array, Object, Set, *ArrayComprehension, *SetComprehension, *ObjectComprehension, Call:
		f := newEqualityFactory(gen)
		assignToLocal := f.Generate(subject)
		support = append(support, assignToLocal)
		v[0] = assignToLocal.Operand(0)
	}
	return
}

func expandExprTermArray(gen *localVarGenerator, arr *Array) (support []*Expr) {
	for i := range arr.Len() {
		extras, v := expandExprTerm(gen, arr.Elem(i))
		arr.set(i, v)
		support = append(support, extras...)
	}
	return
}

func expandExprTermSlice(gen *localVarGenerator, v []*Term) (support []*Expr) {
	for i := range v {
		var extras []*Expr
		extras, v[i] = expandExprTerm(gen, v[i])
		support = append(support, extras...)
	}
	return
}

type localDeclaredVars struct {
	vars []*declaredVarSet

	// rewritten contains a mapping of *all* user-defined variables
	// that have been rewritten whereas vars contains the state
	// from the current query (not any nested queries, and all vars
	// seen).
	rewritten map[Var]Var

	// indicates if an assignment (:= operator) has been seen *ever*
	assignment bool
}

type varOccurrence uint8

const (
	newVar varOccurrence = iota
	argVar
	seenVar
	assignedVar
	declaredVar
)

type declaredVarSet struct {
	vs         map[Var]Var
	occurrence map[Var]varOccurrence
	count      map[Var]int
}

func newDeclaredVarSet() *declaredVarSet {
	return &declaredVarSet{
		vs:         map[Var]Var{},
		occurrence: map[Var]varOccurrence{},
		count:      map[Var]int{},
	}
}

func (s *declaredVarSet) clear() *declaredVarSet {
	clear(s.vs)
	clear(s.occurrence)
	clear(s.count)

	return s
}

func newLocalDeclaredVars() *localDeclaredVars {
	return &localDeclaredVars{
		vars:      []*declaredVarSet{newDeclaredVarSet()},
		rewritten: map[Var]Var{},
	}
}

func (s *localDeclaredVars) Clear() {
	var vs *declaredVarSet
	if len(s.vars) > 0 {
		vs = s.vars[0]
	}

	clear(s.vars)
	clear(s.rewritten)

	s.vars = s.vars[:0]

	if vs != nil {
		s.vars = append(s.vars, vs.clear())
	}
	if s.vars[0] == nil {
		s.vars[0] = newDeclaredVarSet()
	}
	s.assignment = false
}

func (s *localDeclaredVars) Copy() *localDeclaredVars {
	stack := &localDeclaredVars{
		vars: make([]*declaredVarSet, 0, len(s.vars)),
	}

	for i := range s.vars {
		stack.vars = append(stack.vars, newDeclaredVarSet())
		maps.Copy(stack.vars[0].vs, s.vars[i].vs)
		maps.Copy(stack.vars[0].occurrence, s.vars[i].occurrence)
		maps.Copy(stack.vars[0].count, s.vars[i].count)
	}

	stack.rewritten = maps.Clone(s.rewritten)

	return stack
}

func (s *localDeclaredVars) Push() {
	s.vars = append(s.vars, newDeclaredVarSet())
}

func (s *localDeclaredVars) Pop() *declaredVarSet {
	sl := s.vars
	curr := sl[len(sl)-1]
	s.vars = sl[:len(sl)-1]
	return curr
}

func (s localDeclaredVars) Peek() *declaredVarSet {
	return s.vars[len(s.vars)-1]
}

func (s localDeclaredVars) Insert(x, y Var, occurrence varOccurrence) {
	elem := s.vars[len(s.vars)-1]
	elem.vs[x] = y
	elem.occurrence[x] = occurrence

	elem.count[x] = 1

	// If the variable has been rewritten (where x != y, with y being
	// the generated value), store it in the map of rewritten vars.
	// Assume that the generated values are unique for the compilation.
	if !x.Equal(y) {
		s.rewritten[y] = x
	}
}

func (s localDeclaredVars) Declared(x Var) (y Var, ok bool) {
	for i := len(s.vars) - 1; i >= 0; i-- {
		if y, ok = s.vars[i].vs[x]; ok {
			return
		}
	}
	return
}

// Occurrence returns a flag that indicates whether x has occurred in the
// current scope.
func (s localDeclaredVars) Occurrence(x Var) varOccurrence {
	return s.vars[len(s.vars)-1].occurrence[x]
}

// GlobalOccurrence returns a flag that indicates whether x has occurred in the
// global scope.
func (s localDeclaredVars) GlobalOccurrence(x Var) (varOccurrence, bool) {
	for i := len(s.vars) - 1; i >= 0; i-- {
		if occ, ok := s.vars[i].occurrence[x]; ok {
			return occ, true
		}
	}
	return newVar, false
}

// Seen marks x as seen by incrementing its counter
func (s localDeclaredVars) Seen(x Var) {
	for i := len(s.vars) - 1; i >= 0; i-- {
		dvs := s.vars[i]
		if c, ok := dvs.count[x]; ok {
			dvs.count[x] = c + 1
			return
		}
	}

	s.vars[len(s.vars)-1].count[x] = 1
}

// Count returns how many times x has been seen
func (s localDeclaredVars) Count(x Var) int {
	for i := len(s.vars) - 1; i >= 0; i-- {
		if c, ok := s.vars[i].count[x]; ok {
			return c
		}
	}

	return 0
}

// rewriteLocalVars rewrites bodies to remove assignment/declaration
// expressions. For example:
//
// a := 1; p[a]
//
// Is rewritten to:
//
// __local0__ = 1; p[__local0__]
//
// During rewriting, assignees are validated to prevent use before declaration.
func rewriteLocalVars(g *localVarGenerator, stack *localDeclaredVars, used VarSet, body Body, strict bool) (Body, map[Var]Var, Errors) {
	var errs Errors
	body, errs = rewriteDeclaredVarsInBody(g, stack, used, body, errs, strict)
	return body, stack.Peek().vs, errs
}

func rewriteDeclaredVarsInBody(g *localVarGenerator, stack *localDeclaredVars, used VarSet, body Body, errs Errors, strict bool) (Body, Errors) {
	var cpy Body

	for i := range body {
		var expr *Expr
		switch {
		case body[i].IsAssignment():
			stack.assignment = true
			expr, errs = rewriteDeclaredAssignment(g, stack, body[i], errs, strict)
		case body[i].IsSome():
			expr, errs = rewriteSomeDeclStatement(g, stack, body[i], errs, strict)
		case body[i].IsEvery():
			expr, errs = rewriteEveryStatement(g, stack, body[i], errs, strict)
		default:
			expr, errs = rewriteDeclaredVarsInExpr(g, stack, body[i], errs, strict)
		}
		if expr != nil {
			cpy.Append(expr)
		}
	}

	// If the body only contained a var statement it will be empty at this
	// point. Append true to the body to ensure that it's non-empty (zero length
	// bodies are not supported.)
	if len(cpy) == 0 {
		cpy.Append(NewExpr(BooleanTerm(true)))
	}

	errs = checkUnusedAssignedVars(body, stack, used, errs, strict)
	return cpy, checkUnusedDeclaredVars(body, stack, used, cpy, errs)
}

func checkUnusedAssignedVars(body Body, stack *localDeclaredVars, used VarSet, errs Errors, strict bool) Errors {
	if !strict || len(errs) > 0 {
		return errs
	}

	dvs := stack.Peek()

	hasAssignedVars := false
	for _, occ := range dvs.occurrence {
		if occ == assignedVar {
			hasAssignedVars = true
		}
	}
	if !hasAssignedVars {
		return errs
	}

	unused := NewVarSet()

	for v, occ := range dvs.occurrence {
		// A var that was assigned in this scope must have been seen (used) more than once (the time of assignment) in
		// the same, or nested, scope to be counted as used.
		if !v.IsWildcard() && stack.Count(v) <= 1 && occ == assignedVar {
			unused.Add(dvs.vs[v])
		}
	}

	rewrittenUsed := NewVarSet()
	for v := range used {
		if gv, ok := stack.Declared(v); ok {
			rewrittenUsed.Add(gv)
		} else {
			rewrittenUsed.Add(v)
		}
	}

	unused = unused.Diff(rewrittenUsed)
	if len(unused) == 0 {
		return errs
	}

	reversed := make(map[Var]Var, len(dvs.vs))
	for k, v := range dvs.vs {
		reversed[v] = k
	}

	for _, gv := range unused.Sorted() {
		found := false
		for i := range body {
			if body[i].Vars(VarVisitorParams{}).Contains(gv) {
				errs = append(errs, NewError(CompileErr, body[i].Loc(), "assigned var %v unused", reversed[gv]))
				found = true
				break
			}
		}
		if !found {
			errs = append(errs, NewError(CompileErr, body[0].Loc(), "assigned var %v unused", reversed[gv]))
		}
	}

	return errs
}

func checkUnusedDeclaredVars(body Body, stack *localDeclaredVars, used VarSet, cpy Body, errs Errors) Errors {

	// NOTE(tsandall): Do not generate more errors if there are existing
	// declaration errors.
	if len(errs) > 0 {
		return errs
	}

	dvs := stack.Peek()

	hasDeclaredVars := false
	for _, occ := range dvs.occurrence {
		if occ == declaredVar {
			hasDeclaredVars = true
		}
	}
	if !hasDeclaredVars {
		return errs
	}

	declared := NewVarSet()

	for v, occ := range dvs.occurrence {
		if occ == declaredVar {
			declared.Add(dvs.vs[v])
		}
	}

	bodyvars := cpy.Vars(VarVisitorParams{})

	for v := range used {
		if gv, ok := stack.Declared(v); ok {
			bodyvars.Add(gv)
		} else {
			bodyvars.Add(v)
		}
	}

	dbv := declared.Diff(bodyvars)
	if dbv.DiffCount(used) == 0 {
		return errs
	}

	reversed := make(map[Var]Var, len(dvs.vs))
	for k, v := range dvs.vs {
		reversed[v] = k
	}

	for _, gv := range dbv.Diff(used).Sorted() {
		rv := reversed[gv]
		if !rv.IsGenerated() {
			// Scan through body exprs, looking for a match between the
			// bad var's original name, and each expr's declared vars.
			foundUnusedVarByName := false
			for i := range body {
				varsDeclaredInExpr := declaredVars(body[i])
				if varsDeclaredInExpr.Contains(rv) {
					// TODO(philipc): Clean up the offset logic here when the parser
					// reports more accurate locations.
					errs = append(errs, NewError(CompileErr, body[i].Loc(), "declared var %v unused", rv))
					foundUnusedVarByName = true
					break
				}
			}
			// Default error location returned.
			if !foundUnusedVarByName {
				errs = append(errs, NewError(CompileErr, body[0].Loc(), "declared var %v unused", rv))
			}
		}
	}

	return errs
}

func rewriteEveryStatement(g *localVarGenerator, stack *localDeclaredVars, expr *Expr, errs Errors, strict bool) (*Expr, Errors) {
	e := expr.Copy()
	every := e.Terms.(*Every)

	errs = rewriteDeclaredVarsInTermRecursive(g, stack, every.Domain, errs, strict)

	stack.Push()
	defer stack.Pop()

	// if the key exists, rewrite
	if every.Key != nil {
		if v := every.Key.Value.(Var); !v.IsWildcard() {
			gv, err := rewriteDeclaredVar(g, stack, v, declaredVar)
			if err != nil {
				return nil, append(errs, NewError(CompileErr, every.Loc(), err.Error())) //nolint:govet
			}
			every.Key.Value = gv
		}
	} else { // if the key doesn't exist, add dummy local
		every.Key = NewTerm(g.Generate())
	}

	// value is always present
	if v := every.Value.Value.(Var); !v.IsWildcard() {
		gv, err := rewriteDeclaredVar(g, stack, v, declaredVar)
		if err != nil {
			return nil, append(errs, NewError(CompileErr, every.Loc(), err.Error())) //nolint:govet
		}
		every.Value.Value = gv
	}

	used := NewVarSet()
	every.Body, errs = rewriteDeclaredVarsInBody(g, stack, used, every.Body, errs, strict)

	return rewriteDeclaredVarsInExpr(g, stack, e, errs, strict)
}

func rewriteSomeDeclStatement(g *localVarGenerator, stack *localDeclaredVars, expr *Expr, errs Errors, strict bool) (*Expr, Errors) {
	e := expr.Copy()
	decl := e.Terms.(*SomeDecl)
	for i := range decl.Symbols {
		switch v := decl.Symbols[i].Value.(type) {
		case Var:
			if _, err := rewriteDeclaredVar(g, stack, v, declaredVar); err != nil {
				return nil, append(errs, NewError(CompileErr, decl.Loc(), err.Error())) //nolint:govet
			}
		case Call:
			var key, val, container *Term
			switch len(v) {
			case 4: // member3
				key = v[1]
				val = v[2]
				container = v[3]
			case 3: // member
				key = NewTerm(g.Generate())
				val = v[1]
				container = v[2]
			}

			var rhs *Term
			switch c := container.Value.(type) {
			case Ref:
				rhs = RefTerm(append(c, key)...)
			default:
				rhs = RefTerm(container, key)
			}
			e.Terms = []*Term{
				RefTerm(VarTerm(Equality.Name)), val, rhs,
			}

			output := VarSet{}

			for _, v0 := range outputVarsForExprEq(e, container.Vars(), output).Sorted() {
				if _, err := rewriteDeclaredVar(g, stack, v0, declaredVar); err != nil {
					return nil, append(errs, NewError(CompileErr, decl.Loc(), err.Error())) //nolint:govet
				}
			}
			return rewriteDeclaredVarsInExpr(g, stack, e, errs, strict)
		}
	}
	return nil, errs
}

func rewriteDeclaredVarsInExpr(g *localVarGenerator, stack *localDeclaredVars, expr *Expr, errs Errors, strict bool) (*Expr, Errors) {
	vis := NewGenericVisitor(func(x any) bool {
		var stop bool
		switch x := x.(type) {
		case *Term:
			stop, errs = rewriteDeclaredVarsInTerm(g, stack, x, errs, strict)
		case *With:
			stop, errs = true, rewriteDeclaredVarsInWithRecursive(g, stack, x, errs, strict)
		}
		return stop
	})
	vis.Walk(expr)
	return expr, errs
}

func rewriteDeclaredAssignment(g *localVarGenerator, stack *localDeclaredVars, expr *Expr, errs Errors, strict bool) (*Expr, Errors) {

	if expr.Negated {
		errs = append(errs, NewError(CompileErr, expr.Location, "cannot assign vars inside negated expression"))
		return expr, errs
	}

	numErrsBefore := len(errs)

	if !validEqAssignArgCount(expr) {
		return expr, errs
	}

	// Rewrite terms on right hand side capture seen vars and recursively
	// process comprehensions before left hand side is processed. Also
	// rewrite with modifier.
	errs = rewriteDeclaredVarsInTermRecursive(g, stack, expr.Operand(1), errs, strict)

	for _, w := range expr.With {
		errs = rewriteDeclaredVarsInTermRecursive(g, stack, w.Value, errs, strict)
	}

	// Rewrite vars on left hand side with unique names. Catch redeclaration
	// and invalid term types here.
	var vis func(t *Term) bool

	vis = func(t *Term) bool {
		switch v := t.Value.(type) {
		case Var:
			if gv, err := rewriteDeclaredVar(g, stack, v, assignedVar); err != nil {
				errs = append(errs, NewError(CompileErr, t.Location, err.Error())) //nolint:govet
			} else {
				t.Value = gv
			}
			return true
		case *Array:
			return false
		case *object:
			v.Foreach(func(_, v *Term) {
				WalkTerms(v, vis)
			})
			return true
		case Ref:
			if RootDocumentRefs.Contains(t) {
				if gv, err := rewriteDeclaredVar(g, stack, v[0].Value.(Var), assignedVar); err != nil {
					errs = append(errs, NewError(CompileErr, t.Location, err.Error())) //nolint:govet
				} else {
					t.Value = gv
				}
				return true
			}
		}
		errs = append(errs, NewError(CompileErr, t.Location, "cannot assign to %v", ValueName(t.Value)))
		return true
	}

	WalkTerms(expr.Operand(0), vis)

	if len(errs) == numErrsBefore {
		loc := expr.Operator()[0].Location
		expr.SetOperator(RefTerm(VarTerm(Equality.Name).SetLocation(loc)).SetLocation(loc))
	}

	return expr, errs
}

func rewriteDeclaredVarsInTerm(g *localVarGenerator, stack *localDeclaredVars, term *Term, errs Errors, strict bool) (bool, Errors) {
	switch v := term.Value.(type) {
	case Var:
		if gv, ok := stack.Declared(v); ok {
			term.Value = gv
			stack.Seen(v)
		} else if stack.Occurrence(v) == newVar {
			stack.Insert(v, v, seenVar)
		}
	case Ref:
		if RootDocumentRefs.Contains(term) {
			x := v[0].Value.(Var)
			if occ, ok := stack.GlobalOccurrence(x); ok && occ != seenVar {
				gv, _ := stack.Declared(x)
				term.Value = gv
			}

			return true, errs
		}
		return false, errs
	case Call:
		ref := v[0]
		WalkVars(ref, func(v Var) bool {
			if gv, ok := stack.Declared(v); ok && !gv.Equal(v) {
				// We will rewrite the ref of a function call, which is never ok since we don't have first-class functions.
				errs = append(errs, NewError(CompileErr, term.Location, "called function %s shadowed", ref))
				return true
			}
			return false
		})
		return false, errs
	case *object:
		cpy, _ := v.Map(func(k, v *Term) (*Term, *Term, error) {
			kcpy := k.Copy()
			errs = rewriteDeclaredVarsInTermRecursive(g, stack, kcpy, errs, strict)
			errs = rewriteDeclaredVarsInTermRecursive(g, stack, v, errs, strict)
			return kcpy, v, nil
		})
		term.Value = cpy
	case Set:
		cpy, _ := v.Map(func(elem *Term) (*Term, error) {
			elemcpy := elem.Copy()
			errs = rewriteDeclaredVarsInTermRecursive(g, stack, elemcpy, errs, strict)
			return elemcpy, nil
		})
		term.Value = cpy
	case *ArrayComprehension:
		errs = rewriteDeclaredVarsInArrayComprehension(g, stack, v, errs, strict)
	case *SetComprehension:
		errs = rewriteDeclaredVarsInSetComprehension(g, stack, v, errs, strict)
	case *ObjectComprehension:
		errs = rewriteDeclaredVarsInObjectComprehension(g, stack, v, errs, strict)
	default:
		return false, errs
	}
	return true, errs
}

func rewriteDeclaredVarsInTermRecursive(g *localVarGenerator, stack *localDeclaredVars, term *Term, errs Errors, strict bool) Errors {
	WalkTerms(term, func(t *Term) bool {
		var stop bool
		stop, errs = rewriteDeclaredVarsInTerm(g, stack, t, errs, strict)
		return stop
	})
	return errs
}

func rewriteDeclaredVarsInWithRecursive(g *localVarGenerator, stack *localDeclaredVars, w *With, errs Errors, strict bool) Errors {
	// NOTE(sr): `with input as` and `with input.a.b.c as` are deliberately skipped here: `input` could
	// have been shadowed by a local variable/argument but should NOT be replaced in the `with` target.
	//
	// We cannot drop `input` from the stack since it's conceivable to do `with input[input] as` where
	// the second input is meant to be the local var. It's a terrible idea, but when you're shadowing
	// `input` those might be your thing.
	errs = rewriteDeclaredVarsInTermRecursive(g, stack, w.Target, errs, strict)
	if sdwInput, ok := stack.Declared(InputRootDocument.Value.(Var)); ok { // Was "input" shadowed...
		switch value := w.Target.Value.(type) {
		case Var:
			if sdwInput.Equal(value) { // ...and replaced? If so, fix it
				w.Target.Value = InputRootRef
			}
		case Ref:
			if sdwInput.Equal(value[0].Value.(Var)) {
				w.Target.Value.(Ref)[0].Value = InputRootDocument.Value
			}
		}
	}
	// No special handling of the `with` value
	return rewriteDeclaredVarsInTermRecursive(g, stack, w.Value, errs, strict)
}

func rewriteDeclaredVarsInArrayComprehension(g *localVarGenerator, stack *localDeclaredVars, v *ArrayComprehension, errs Errors, strict bool) Errors {
	used := NewVarSet()
	used.Update(v.Term.Vars())

	stack.Push()
	v.Body, errs = rewriteDeclaredVarsInBody(g, stack, used, v.Body, errs, strict)
	errs = rewriteDeclaredVarsInTermRecursive(g, stack, v.Term, errs, strict)
	stack.Pop()
	return errs
}

func rewriteDeclaredVarsInSetComprehension(g *localVarGenerator, stack *localDeclaredVars, v *SetComprehension, errs Errors, strict bool) Errors {
	used := NewVarSet()
	used.Update(v.Term.Vars())

	stack.Push()
	v.Body, errs = rewriteDeclaredVarsInBody(g, stack, used, v.Body, errs, strict)
	errs = rewriteDeclaredVarsInTermRecursive(g, stack, v.Term, errs, strict)
	stack.Pop()
	return errs
}

func rewriteDeclaredVarsInObjectComprehension(g *localVarGenerator, stack *localDeclaredVars, v *ObjectComprehension, errs Errors, strict bool) Errors {
	used := NewVarSet()
	used.Update(v.Key.Vars())
	used.Update(v.Value.Vars())

	stack.Push()
	v.Body, errs = rewriteDeclaredVarsInBody(g, stack, used, v.Body, errs, strict)
	errs = rewriteDeclaredVarsInTermRecursive(g, stack, v.Key, errs, strict)
	errs = rewriteDeclaredVarsInTermRecursive(g, stack, v.Value, errs, strict)
	stack.Pop()
	return errs
}

func rewriteDeclaredVar(g *localVarGenerator, stack *localDeclaredVars, v Var, occ varOccurrence) (gv Var, err error) {
	switch stack.Occurrence(v) {
	case seenVar:
		return gv, fmt.Errorf("var %v referenced above", v)
	case assignedVar:
		return gv, fmt.Errorf("var %v assigned above", v)
	case declaredVar:
		return gv, fmt.Errorf("var %v declared above", v)
	case argVar:
		return gv, fmt.Errorf("arg %v redeclared", v)
	}
	gv = g.Generate()
	stack.Insert(v, gv, occ)
	return
}

// rewriteWithModifiersInBody will rewrite the body so that with modifiers do
// not contain terms that require evaluation as values. If this function
// encounters an invalid with modifier target then it will raise an error.
func rewriteWithModifiersInBody(c *Compiler, unsafeBuiltinsMap map[string]struct{}, f *equalityFactory, body Body) (Body, *Error) {
	var result Body
	for i := range body {
		exprs, err := rewriteWithModifier(c, unsafeBuiltinsMap, f, body[i])
		if err != nil {
			return nil, err
		}
		if len(exprs) > 0 {
			for _, expr := range exprs {
				result.Append(expr)
			}
		} else {
			result.Append(body[i])
		}
	}
	return result, nil
}

func rewriteWithModifier(c *Compiler, unsafeBuiltinsMap map[string]struct{}, f *equalityFactory, expr *Expr) ([]*Expr, *Error) {

	var result []*Expr
	for i := range expr.With {
		eval, err := validateWith(c, unsafeBuiltinsMap, expr, i)
		if err != nil {
			return nil, err
		}

		if eval {
			eq := f.Generate(expr.With[i].Value)
			result = append(result, eq)
			expr.With[i].Value = eq.Operand(0)
		}
	}

	return append(result, expr), nil
}

func validateWith(c *Compiler, unsafeBuiltinsMap map[string]struct{}, expr *Expr, i int) (bool, *Error) {
	target, value := expr.With[i].Target, expr.With[i].Value

	// Ensure that values that are built-ins are rewritten to Ref (not Var)
	if v, ok := value.Value.(Var); ok {
		if _, ok := c.builtins[v.String()]; ok {
			value.Value = Ref([]*Term{NewTerm(v)})
		}
	}
	isBuiltinRefOrVar, err := isBuiltinRefOrVar(c.builtins, unsafeBuiltinsMap, target)
	if err != nil {
		return false, err
	}

	isAllowedUnknownFuncCall := false
	if c.allowUndefinedFuncCalls {
		switch target.Value.(type) {
		case Ref, Var:
			isAllowedUnknownFuncCall = true
		}
	}

	switch {
	case isDataRef(target):
		ref := target.Value.(Ref)
		targetNode := c.RuleTree
		for i := range len(ref) - 1 {
			child := targetNode.Child(ref[i].Value)
			if child == nil {
				break
			} else if len(child.Values) > 0 {
				return false, NewError(CompileErr, target.Loc(), "with keyword cannot partially replace virtual document(s)")
			}
			targetNode = child
		}

		if targetNode != nil {
			// NOTE(sr): at this point in the compiler stages, we don't have a fully-populated
			// TypeEnv yet -- so we have to make do with this check to see if the replacement
			// target is a function. It's probably wrong for arity-0 functions, but those are
			// and edge case anyways.
			if child := targetNode.Child(ref[len(ref)-1].Value); child != nil {
				for _, v := range child.Values {
					if len(v.(*Rule).Head.Args) > 0 {
						if ok, err := validateWithFunctionValue(c.builtins, unsafeBuiltinsMap, c.RuleTree, value); err != nil || ok {
							return false, err // err may be nil
						}
					}
				}
			}
		}

		// If the with-value is a ref to a function, but not a call, we can't rewrite it
		if r, ok := value.Value.(Ref); ok {
			// TODO: check that target ref doesn't exist?
			if valueNode := c.RuleTree.Find(r); valueNode != nil {
				for _, v := range valueNode.Values {
					if len(v.(*Rule).Head.Args) > 0 {
						return false, nil
					}
				}
			}
		}
	case isInputRef(target): // ok, valid
	case isBuiltinRefOrVar:

		// NOTE(sr): first we ensure that parsed Var builtins (`count`, `concat`, etc)
		// are rewritten to their proper Ref convention
		if v, ok := target.Value.(Var); ok {
			target.Value = Ref([]*Term{NewTerm(v)})
		}

		targetRef := target.Value.(Ref)
		bi := c.builtins[targetRef.String()] // safe because isBuiltinRefOrVar checked this
		if err := validateWithBuiltinTarget(bi, targetRef, target.Loc()); err != nil {
			return false, err
		}

		if ok, err := validateWithFunctionValue(c.builtins, unsafeBuiltinsMap, c.RuleTree, value); err != nil || ok {
			return false, err // err may be nil
		}
	case isAllowedUnknownFuncCall:
		// The target isn't a ref to the input doc, data doc, or a known built-in, but it might be a ref to an unknown built-in.
		return false, nil
	default:
		return false, NewError(TypeErr, target.Location, "with keyword target must reference existing %v, %v, or a function", InputRootDocument, DefaultRootDocument)
	}
	return requiresEval(value), nil
}

func validateWithBuiltinTarget(bi *Builtin, target Ref, loc *location.Location) *Error {
	switch bi.Name {
	case Equality.Name,
		RegoMetadataChain.Name,
		RegoMetadataRule.Name:
		return NewError(CompileErr, loc, "with keyword replacing built-in function: replacement of %q invalid", bi.Name)
	}

	switch {
	case target.HasPrefix(Ref([]*Term{VarTerm("internal")})):
		return NewError(CompileErr, loc, "with keyword replacing built-in function: replacement of internal function %q invalid", target)

	case bi.Relation:
		return NewError(CompileErr, loc, "with keyword replacing built-in function: target must not be a relation")

	case bi.Decl.Result() == nil:
		return NewError(CompileErr, loc, "with keyword replacing built-in function: target must not be a void function")
	}
	return nil
}

func validateWithFunctionValue(bs map[string]*Builtin, unsafeMap map[string]struct{}, ruleTree *TreeNode, value *Term) (bool, *Error) {
	if v, ok := value.Value.(Ref); ok {
		if ruleTree.Find(v) != nil { // ref exists in rule tree
			return true, nil
		}
	}
	return isBuiltinRefOrVar(bs, unsafeMap, value)
}

func isInputRef(term *Term) bool {
	if ref, ok := term.Value.(Ref); ok {
		if ref.HasPrefix(InputRootRef) {
			return true
		}
	}
	return false
}

func isDataRef(term *Term) bool {
	if ref, ok := term.Value.(Ref); ok {
		if ref.HasPrefix(DefaultRootRef) {
			return true
		}
	}
	return false
}

func isBuiltinRefOrVar(bs map[string]*Builtin, unsafeBuiltinsMap map[string]struct{}, term *Term) (bool, *Error) {
	switch v := term.Value.(type) {
	case Ref, Var:
		if _, ok := unsafeBuiltinsMap[v.String()]; ok {
			return false, NewError(CompileErr, term.Location, "with keyword replacing built-in function: target must not be unsafe: %q", v)
		}
		_, ok := bs[v.String()]
		return ok, nil
	}
	return false, nil
}

func isVirtual(node *TreeNode, ref Ref) bool {
	for i := range ref {
		child := node.Child(ref[i].Value)
		if child == nil {
			return false
		} else if len(child.Values) > 0 {
			return true
		}
		node = child
	}
	return true
}

func safetyErrorSlice(unsafe unsafeVars, rewritten map[Var]Var) (result Errors) {
	if len(unsafe) == 0 {
		return
	}

	for _, pair := range unsafe.Vars() {
		v := pair.Var
		if w, ok := rewritten[v]; ok {
			v = w
		}
		if !v.IsGenerated() {
			if _, ok := allFutureKeywords[string(v)]; ok {
				result = append(result, NewError(UnsafeVarErr, pair.Loc,
					"var %[1]v is unsafe (hint: `import future.keywords.%[1]v` to import a future keyword)", v))
				continue
			}
			result = append(result, NewError(UnsafeVarErr, pair.Loc, "var %v is unsafe", v))
		}
	}

	if len(result) > 0 {
		return
	}

	// If the expression contains unsafe generated variables, report which
	// expressions are unsafe instead of the variables that are unsafe (since
	// the latter are not meaningful to the user.)
	pairs := unsafe.Slice()

	slices.SortFunc(pairs, func(a, b unsafePair) int {
		return a.Expr.Location.Compare(b.Expr.Location)
	})

	// Report at most one error per generated variable.
	seen := NewVarSet()

	for _, expr := range pairs {
		before := len(seen)
		for v := range expr.Vars {
			if v.IsGenerated() {
				seen.Add(v)
			}
		}
		if len(seen) > before {
			result = append(result, NewError(UnsafeVarErr, expr.Expr.Location, "expression is unsafe"))
		}
	}

	return
}

func checkUnsafeBuiltins(unsafeBuiltinsMap map[string]struct{}, node any) Errors {
	var errs Errors
	WalkExprs(node, func(x *Expr) bool {
		if x.IsCall() {
			operator := x.Operator().String()
			if _, ok := unsafeBuiltinsMap[operator]; ok {
				errs = append(errs, NewError(TypeErr, x.Loc(), "unsafe built-in function calls in expression: %v", operator))
			}
		}
		return false
	})
	return errs
}

func rewriteVarsInRef(vars ...map[Var]Var) varRewriter {
	return func(node Ref) Ref {
		i, _ := TransformVars(node, func(v Var) (Value, error) {
			for _, m := range vars {
				if u, ok := m[v]; ok {
					return u, nil
				}
			}
			return v, nil
		})
		return i.(Ref)
	}
}

// NOTE(sr): This is duplicated with compile/compile.go; but moving it into another location
// would cause a circular dependency -- the refSet definition needs ast.Ref. If we make it
// public in the ast package, the compile package could take it from there, but it would also
// increase our public interface. Let's reconsider if we need it in a third place.
type refSet struct {
	s []Ref
}

func newRefSet(x ...Ref) *refSet {
	result := &refSet{}
	for i := range x {
		result.AddPrefix(x[i])
	}
	return result
}

// ContainsPrefix returns true if r is prefixed by any of the existing refs in the set.
func (rs *refSet) ContainsPrefix(r Ref) bool {
	return slices.ContainsFunc(rs.s, r.HasPrefix)
}

// AddPrefix inserts r into the set if r is not prefixed by any existing
// refs in the set. If any existing refs are prefixed by r, those existing
// refs are removed.
func (rs *refSet) AddPrefix(r Ref) {
	if rs.ContainsPrefix(r) {
		return
	}
	cpy := []Ref{r}
	for i := range rs.s {
		if !rs.s[i].HasPrefix(r) {
			cpy = append(cpy, rs.s[i])
		}
	}
	rs.s = cpy
}

// Sorted returns a sorted slice of terms for refs in the set.
func (rs *refSet) Sorted() []*Term {
	terms := make([]*Term, len(rs.s))
	for i := range rs.s {
		terms[i] = NewTerm(rs.s[i])
	}
	slices.SortFunc(terms, TermValueCompare)
	return terms
}
