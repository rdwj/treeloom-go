package graph

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rdwj/treeloom-go/internal/model"
)

// Default directory patterns excluded from AddDirectory walks.
var defaultExcludes = []string{
	"__pycache__",
	"node_modules",
	".git",
	"venv",
	".venv",
}

// LanguageVisitor is the interface that language-specific visitors implement.
// The graph package depends on this interface rather than importing the lang
// package, keeping the dependency arrow from lang -> graph (not circular).
type LanguageVisitor interface {
	Name() string
	Extensions() []string
	Parse(source []byte, filename string) (interface{}, error)
	Visit(tree interface{}, filePath string, emitter NodeEmitter)
	ResolveCalls(cpg *CodePropertyGraph, functionNodes, callNodes []*model.CpgNode) []CallResolution
}

// CallResolution records a resolved call: a call site linked to a function definition.
type CallResolution struct {
	CallID model.NodeId
	FuncID model.NodeId
}

// NodeEmitter is the callback interface that language visitors use to emit
// nodes and edges into the CPG during the visit phase.
type NodeEmitter interface {
	EmitModule(name string, path string, endLoc *model.SourceLocation, sourceText string) model.NodeId
	EmitClass(name string, loc model.SourceLocation, scope model.NodeId, bases []string, endLoc *model.SourceLocation, sourceText string) model.NodeId
	EmitFunction(name string, loc model.SourceLocation, scope model.NodeId, isAsync bool, decorators []string, endLoc *model.SourceLocation, sourceText string) model.NodeId
	EmitParameter(name string, loc model.SourceLocation, function model.NodeId, typeAnnotation string, position int, endLoc *model.SourceLocation, sourceText string) model.NodeId
	EmitVariable(name string, loc model.SourceLocation, scope model.NodeId, inferredType string, endLoc *model.SourceLocation, sourceText string) model.NodeId
	EmitCall(targetName string, loc model.SourceLocation, scope model.NodeId, args []string, receiverInferredType string, endLoc *model.SourceLocation, sourceText string) model.NodeId
	EmitLiteral(value string, literalType string, loc model.SourceLocation, scope model.NodeId, endLoc *model.SourceLocation, sourceText string) model.NodeId
	EmitReturn(loc model.SourceLocation, scope model.NodeId, endLoc *model.SourceLocation, sourceText string) model.NodeId
	EmitImport(module string, names []string, loc model.SourceLocation, scope model.NodeId, isFrom bool, aliases map[string]string, endLoc *model.SourceLocation, sourceText string) model.NodeId
	EmitBranchNode(branchType string, loc model.SourceLocation, scope model.NodeId, hasElse bool, endLoc *model.SourceLocation, sourceText string) model.NodeId
	EmitLoopNode(loopType string, loc model.SourceLocation, scope model.NodeId, iteratorVar string, endLoc *model.SourceLocation, sourceText string) model.NodeId
	EmitDataFlow(source, target model.NodeId, attrs map[string]interface{})
	EmitDefinition(variable, definedBy model.NodeId)
	EmitUsage(variable, usedAt model.NodeId)
	EmitControlFlow(from, to model.NodeId)
	EmitBranch(from, trueBranch model.NodeId, falseBranch *model.NodeId)
}

// VisitorRegistry provides visitor lookup by file extension.
// This is a simple interface so the builder doesn't depend on the lang package.
type VisitorRegistry interface {
	GetVisitor(extension string) LanguageVisitor
}

// FunctionSummary captures how data flows through a function: which parameters
// reach the return value and which reach internal sinks.
type FunctionSummary struct {
	FunctionID    model.NodeId
	FunctionName  string
	ParamsToReturn []int                       // parameter positions (0-based) that flow to return
	ParamsToSinks  map[int][]model.NodeId      // param position -> sink node IDs
}

// BuildProgressCallback is called between build phases with (phase, detail).
type BuildProgressCallback func(phase, detail string)

// queuedSource is a source file queued for parsing, either from disk or memory.
type queuedSource struct {
	path     string // normalized file path (used in node IDs)
	source   []byte // raw source bytes (nil if reading from disk)
	language string // explicit language override (empty = detect from extension)
}

// CPGBuilder constructs a CodePropertyGraph from source files.
type CPGBuilder struct {
	cpg           *CodePropertyGraph
	counter       int
	queued        []queuedSource
	relativeRoot  string
	includeSource bool
	registry      VisitorRegistry
	progress      BuildProgressCallback
	timeout       time.Duration
}

// BuilderOption is a functional option for NewBuilder.
type BuilderOption func(*CPGBuilder)

// WithRegistry sets the visitor registry used to find language visitors.
func WithRegistry(r VisitorRegistry) BuilderOption {
	return func(b *CPGBuilder) { b.registry = r }
}

// WithProgress sets the progress callback invoked between build phases.
func WithProgress(cb BuildProgressCallback) BuilderOption {
	return func(b *CPGBuilder) { b.progress = cb }
}

// WithTimeout sets a wall-clock timeout for the build. If exceeded between
// phases, Build returns an error.
func WithTimeout(d time.Duration) BuilderOption {
	return func(b *CPGBuilder) { b.timeout = d }
}

// WithRelativeRoot causes all file paths in the CPG to be stored relative
// to the given directory.
func WithRelativeRoot(root string) BuilderOption {
	return func(b *CPGBuilder) {
		abs, err := filepath.Abs(root)
		if err == nil {
			b.relativeRoot = abs
		} else {
			b.relativeRoot = root
		}
	}
}

// WithIncludeSource includes source text snippets in emitted nodes.
func WithIncludeSource(include bool) BuilderOption {
	return func(b *CPGBuilder) { b.includeSource = include }
}

// NewBuilder creates a CPGBuilder with the given options.
func NewBuilder(opts ...BuilderOption) *CPGBuilder {
	b := &CPGBuilder{
		cpg: NewCPG(),
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// AddFile queues a single file for parsing.
func (b *CPGBuilder) AddFile(path string) *CPGBuilder {
	b.queued = append(b.queued, queuedSource{path: path})
	return b
}

// AddDirectory recursively walks a directory and queues all files that have
// a visitor registered for their extension. Patterns in exclude are matched
// against directory and file base names using filepath.Match semantics. If
// exclude is nil, defaultExcludes is used.
func (b *CPGBuilder) AddDirectory(path string, exclude []string) *CPGBuilder {
	if exclude == nil {
		exclude = defaultExcludes
	}

	var files []string
	_ = filepath.Walk(path, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		base := info.Name()

		if info.IsDir() {
			if shouldExclude(base, exclude) {
				return filepath.SkipDir
			}
			return nil
		}

		if shouldExclude(base, exclude) {
			return nil
		}

		files = append(files, p)
		return nil
	})

	sort.Strings(files)
	for _, f := range files {
		b.queued = append(b.queued, queuedSource{path: f})
	}
	return b
}

// AddSource queues in-memory source code for parsing. If language is empty,
// the language is detected from the filename extension.
func (b *CPGBuilder) AddSource(source []byte, filename string, language string) *CPGBuilder {
	b.queued = append(b.queued, queuedSource{
		path:     filename,
		source:   source,
		language: language,
	})
	return b
}

// Build executes the full build pipeline and returns the constructed CPG.
//
// Pipeline phases:
//  1. Parse — parse each queued file and visit the tree
//  2. CFG — construct control flow edges within each function
//  3. Call resolution — link call sites to function definitions
//  4. Function summaries — compute parameter-to-return flow
//  5. Inter-procedural DFG — wire data flow across call boundaries
func (b *CPGBuilder) Build() (*CodePropertyGraph, error) {
	deadline := time.Time{}
	if b.timeout > 0 {
		deadline = time.Now().Add(b.timeout)
	}

	// Phase 1: Parse and visit
	if err := b.phaseParse(deadline); err != nil {
		return nil, err
	}

	// Phase 2: CFG
	if err := b.phaseCFG(deadline); err != nil {
		return nil, err
	}

	// Phase 3: Call resolution
	if err := b.phaseCallResolution(deadline); err != nil {
		return nil, err
	}

	// Phase 4: Function summaries
	summaries := b.computeSummaries()

	// Phase 5: Inter-procedural DFG
	if err := b.phaseInterproceduralDFG(summaries, deadline); err != nil {
		return nil, err
	}

	return b.cpg, nil
}

// ---------- Phase 1: Parse ----------

func (b *CPGBuilder) phaseParse(deadline time.Time) error {
	b.reportProgress("Parse", "starting")

	for _, qs := range b.queued {
		if err := b.checkDeadline(deadline); err != nil {
			return err
		}

		ext := filepath.Ext(qs.path)
		visitor := b.findVisitor(ext, qs.language)
		if visitor == nil {
			continue // no visitor for this extension
		}

		source := qs.source
		if source == nil {
			data, err := os.ReadFile(qs.path)
			if err != nil {
				continue // skip unreadable files
			}
			source = data
		}

		normalizedPath := b.normalizePath(qs.path)

		tree, err := visitor.Parse(source, normalizedPath)
		if err != nil {
			continue // skip files with parse errors
		}

		b.reportProgress("Parse", normalizedPath)
		visitor.Visit(tree, normalizedPath, b)
	}

	return nil
}

// ---------- Phase 2: CFG ----------

func (b *CPGBuilder) phaseCFG(deadline time.Time) error {
	b.reportProgress("CFG", "starting")

	functions := b.cpg.Nodes(model.NodeFunction)
	for _, fn := range functions {
		if err := b.checkDeadline(deadline); err != nil {
			return err
		}
		b.buildCFGForFunction(fn)
	}

	return nil
}

// buildCFGForFunction constructs control flow edges within a single function.
// Children are sorted by source location. Sequential pairs are connected with
// FLOWS_TO. BRANCH and LOOP children get BRANCHES_TO edges to their body
// children. Loops get a back-edge from their last body child.
func (b *CPGBuilder) buildCFGForFunction(fn *model.CpgNode) {
	children := b.cpg.ChildrenOf(fn.ID)
	if len(children) == 0 {
		return
	}

	sortNodesByLocation(children)

	// Connect sequential children with FLOWS_TO
	for i := 0; i < len(children)-1; i++ {
		cur := children[i]
		next := children[i+1]

		// Don't emit FLOWS_TO from a RETURN — control doesn't continue
		if cur.Kind == model.NodeReturn {
			continue
		}

		b.cpg.AddEdge(&model.CpgEdge{
			Source: cur.ID,
			Target: next.ID,
			Kind:   model.EdgeFlowsTo,
		})
	}

	// Handle BRANCH and LOOP children
	for _, child := range children {
		switch child.Kind {
		case model.NodeBranch, model.NodeLoop:
			b.buildCFGForBranchOrLoop(child)
		}
	}
}

// buildCFGForBranchOrLoop adds BRANCHES_TO edges from a BRANCH/LOOP node
// to its body children, and adds a loop back-edge if applicable.
//
// Note: sequential FLOWS_TO edges between body children are NOT added here.
// The Python builder only adds FLOWS_TO at the function level, not inside
// branch/loop bodies. We match that behavior exactly.
func (b *CPGBuilder) buildCFGForBranchOrLoop(node *model.CpgNode) {
	bodyChildren := b.cpg.ChildrenOf(node.ID)
	if len(bodyChildren) == 0 {
		return
	}

	sortNodesByLocation(bodyChildren)

	// BRANCHES_TO from the branch/loop to the first body statement
	b.cpg.AddEdge(&model.CpgEdge{
		Source: node.ID,
		Target: bodyChildren[0].ID,
		Kind:   model.EdgeBranchesTo,
	})

	// Loop back-edge: last body child -> loop node (unless it's a RETURN)
	if node.Kind == model.NodeLoop {
		last := bodyChildren[len(bodyChildren)-1]
		if last.Kind != model.NodeReturn {
			b.cpg.AddEdge(&model.CpgEdge{
				Source: last.ID,
				Target: node.ID,
				Kind:   model.EdgeFlowsTo,
			})
		}
	}
}

// ---------- Phase 3: Call resolution ----------

func (b *CPGBuilder) phaseCallResolution(deadline time.Time) error {
	b.reportProgress("Call resolution", "starting")

	if b.registry == nil {
		return nil
	}

	// Collect all functions and calls
	allFunctions := b.cpg.Nodes(model.NodeFunction)
	allCalls := b.cpg.Nodes(model.NodeCall)

	// Partition calls by file extension so each visitor resolves its own
	callsByExt := make(map[string][]*model.CpgNode)
	for _, call := range allCalls {
		if call.Location == nil {
			continue
		}
		ext := filepath.Ext(call.Location.File)
		callsByExt[ext] = append(callsByExt[ext], call)
	}

	// Aggregate calls by visitor (not extension) so multi-extension visitors
	// (e.g., Python: .py + .pyi) see all their calls in a single batch.
	type visitorCalls struct {
		visitor LanguageVisitor
		calls   []*model.CpgNode
	}
	byVisitor := make(map[string]*visitorCalls)
	for ext, calls := range callsByExt {
		visitor := b.registry.GetVisitor(ext)
		if visitor == nil {
			continue
		}
		vName := visitor.Name()
		vc, ok := byVisitor[vName]
		if !ok {
			vc = &visitorCalls{visitor: visitor}
			byVisitor[vName] = vc
		}
		vc.calls = append(vc.calls, calls...)
	}

	for _, vc := range byVisitor {
		if err := b.checkDeadline(deadline); err != nil {
			return err
		}
		resolutions := vc.visitor.ResolveCalls(b.cpg, allFunctions, vc.calls)
		for _, res := range resolutions {
			b.cpg.AddEdge(&model.CpgEdge{
				Source: res.CallID,
				Target: res.FuncID,
				Kind:   model.EdgeCalls,
			})
		}
	}

	return nil
}

// ---------- Phase 4: Function summaries ----------

// computeSummaries performs intra-procedural BFS from each parameter through
// DATA_FLOWS_TO edges to determine which parameters reach RETURN nodes.
func (b *CPGBuilder) computeSummaries() map[string]*FunctionSummary {
	summaries := make(map[string]*FunctionSummary)

	for _, fn := range b.cpg.Nodes(model.NodeFunction) {
		summary := &FunctionSummary{
			FunctionID:    fn.ID,
			FunctionName:  fn.Name,
			ParamsToSinks: make(map[int][]model.NodeId),
		}

		params := b.cpg.Successors(fn.ID, model.EdgeHasParameter)
		sortNodesByLocation(params)

		// Collect the set of node IDs within this function's scope
		scopeNodes := make(map[string]bool)
		for _, child := range b.cpg.ChildrenOf(fn.ID) {
			scopeNodes[string(child.ID)] = true
		}
		// Also include nodes scoped to branches/loops within the function
		b.collectNestedScope(fn.ID, scopeNodes)

		for pos, param := range params {
			if b.paramReachesReturn(param.ID, scopeNodes) {
				summary.ParamsToReturn = append(summary.ParamsToReturn, pos)
			}
		}

		summaries[string(fn.ID)] = summary
	}

	return summaries
}

// collectNestedScope recursively adds all nodes in nested scopes (branches,
// loops, blocks) to the scope set.
func (b *CPGBuilder) collectNestedScope(scopeID model.NodeId, scopeNodes map[string]bool) {
	for _, child := range b.cpg.ChildrenOf(scopeID) {
		scopeNodes[string(child.ID)] = true
		if child.Kind == model.NodeBranch || child.Kind == model.NodeLoop || child.Kind == model.NodeBlock {
			b.collectNestedScope(child.ID, scopeNodes)
		}
	}
}

// paramReachesReturn does BFS from a parameter node along DATA_FLOWS_TO edges,
// restricted to nodes within the given scope set. Returns true if any RETURN
// node is reachable.
func (b *CPGBuilder) paramReachesReturn(paramID model.NodeId, scopeNodes map[string]bool) bool {
	visited := make(map[string]bool)
	queue := []model.NodeId{paramID}
	visited[string(paramID)] = true

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		for _, succ := range b.cpg.Successors(current, model.EdgeDataFlowsTo) {
			key := string(succ.ID)
			if visited[key] {
				continue
			}
			if !scopeNodes[key] {
				continue
			}
			visited[key] = true

			if succ.Kind == model.NodeReturn {
				return true
			}
			queue = append(queue, succ.ID)
		}
	}
	return false
}

// ---------- Phase 5: Inter-procedural DFG ----------

func (b *CPGBuilder) phaseInterproceduralDFG(summaries map[string]*FunctionSummary, deadline time.Time) error {
	b.reportProgress("Inter-procedural DFG", "starting")

	callEdges := b.cpg.Edges(model.EdgeCalls)
	for _, edge := range callEdges {
		if err := b.checkDeadline(deadline); err != nil {
			return err
		}

		callNode := b.cpg.Node(edge.Source)
		funcNode := b.cpg.Node(edge.Target)
		if callNode == nil || funcNode == nil {
			continue
		}

		// Get function parameters sorted by position
		params := b.cpg.Successors(funcNode.ID, model.EdgeHasParameter)
		sortNodesByPosition(params)

		// Get arguments: DATA_FLOWS_TO predecessors of the call node,
		// sorted by location (these represent the argument expressions).
		args := b.cpg.Predecessors(callNode.ID, model.EdgeDataFlowsTo)
		sortNodesByLocation(args)

		// Wire arg[i] -> param[i]
		limit := len(args)
		if len(params) < limit {
			limit = len(params)
		}
		for i := 0; i < limit; i++ {
			b.cpg.AddEdge(&model.CpgEdge{
				Source: args[i].ID,
				Target: params[i].ID,
				Kind:   model.EdgeDataFlowsTo,
				Attrs:  map[string]interface{}{"interprocedural": true},
			})
		}

		// If summary says params flow to return, wire return sources -> call site
		summary := summaries[string(funcNode.ID)]
		if summary == nil {
			continue
		}
		if len(summary.ParamsToReturn) > 0 {
			// Find RETURN nodes in the function
			for _, child := range b.cpg.ChildrenOf(funcNode.ID) {
				if child.Kind != model.NodeReturn {
					continue
				}
				// Get DATA_FLOWS_TO predecessors of the RETURN (sources of return value)
				retSources := b.cpg.Predecessors(child.ID, model.EdgeDataFlowsTo)
				for _, src := range retSources {
					b.cpg.AddEdge(&model.CpgEdge{
						Source: src.ID,
						Target: callNode.ID,
						Kind:   model.EdgeDataFlowsTo,
						Attrs:  map[string]interface{}{"interprocedural": true},
					})
				}
			}
		}
	}

	return nil
}

// ---------- NodeEmitter implementation ----------

func (b *CPGBuilder) nextID(kind model.NodeKind, loc *model.SourceLocation) model.NodeId {
	b.counter++
	if loc != nil {
		return model.NodeId(fmt.Sprintf("%s:%s:%d:%d:%d", kind, loc.File, loc.Line, loc.Column, b.counter))
	}
	return model.NodeId(fmt.Sprintf("%s::::%d", kind, b.counter))
}

func (b *CPGBuilder) EmitModule(name string, path string, endLoc *model.SourceLocation, sourceText string) model.NodeId {
	loc := &model.SourceLocation{File: path, Line: 1, Column: 0}
	id := b.nextID(model.NodeModule, loc)

	attrs := map[string]interface{}{}
	if sourceText != "" && b.includeSource {
		attrs["source_text"] = sourceText
	}

	node := &model.CpgNode{
		ID:          id,
		Kind:        model.NodeModule,
		Name:        name,
		Location:    loc,
		EndLocation: endLoc,
		Attrs:       attrs,
	}
	b.cpg.AddNode(node)
	return id
}

func (b *CPGBuilder) EmitClass(name string, loc model.SourceLocation, scope model.NodeId, bases []string, endLoc *model.SourceLocation, sourceText string) model.NodeId {
	id := b.nextID(model.NodeClass, &loc)

	attrs := map[string]interface{}{}
	if len(bases) > 0 {
		attrs["bases"] = bases
	}
	if sourceText != "" && b.includeSource {
		attrs["source_text"] = sourceText
	}

	node := &model.CpgNode{
		ID:          id,
		Kind:        model.NodeClass,
		Name:        name,
		Location:    &loc,
		EndLocation: endLoc,
		Scope:       &scope,
		Attrs:       attrs,
	}
	b.cpg.AddNode(node)
	b.cpg.AddEdge(&model.CpgEdge{Source: scope, Target: id, Kind: model.EdgeContains})
	return id
}

func (b *CPGBuilder) EmitFunction(name string, loc model.SourceLocation, scope model.NodeId, isAsync bool, decorators []string, endLoc *model.SourceLocation, sourceText string) model.NodeId {
	id := b.nextID(model.NodeFunction, &loc)

	attrs := map[string]interface{}{
		"is_async": isAsync,
	}
	if len(decorators) > 0 {
		attrs["decorators"] = decorators
	}
	if sourceText != "" && b.includeSource {
		attrs["source_text"] = sourceText
	}

	node := &model.CpgNode{
		ID:          id,
		Kind:        model.NodeFunction,
		Name:        name,
		Location:    &loc,
		EndLocation: endLoc,
		Scope:       &scope,
		Attrs:       attrs,
	}
	b.cpg.AddNode(node)
	b.cpg.AddEdge(&model.CpgEdge{Source: scope, Target: id, Kind: model.EdgeContains})
	return id
}

func (b *CPGBuilder) EmitParameter(name string, loc model.SourceLocation, function model.NodeId, typeAnnotation string, position int, endLoc *model.SourceLocation, sourceText string) model.NodeId {
	id := b.nextID(model.NodeParameter, &loc)

	attrs := map[string]interface{}{
		"type_annotation": interface{}(nil),
		"position":        position,
	}
	if typeAnnotation != "" {
		attrs["type_annotation"] = typeAnnotation
	}
	if sourceText != "" && b.includeSource {
		attrs["source_text"] = sourceText
	}

	node := &model.CpgNode{
		ID:          id,
		Kind:        model.NodeParameter,
		Name:        name,
		Location:    &loc,
		EndLocation: endLoc,
		Scope:       &function,
		Attrs:       attrs,
	}
	b.cpg.AddNode(node)
	b.cpg.AddEdge(&model.CpgEdge{Source: function, Target: id, Kind: model.EdgeHasParameter})
	return id
}

func (b *CPGBuilder) EmitVariable(name string, loc model.SourceLocation, scope model.NodeId, inferredType string, endLoc *model.SourceLocation, sourceText string) model.NodeId {
	id := b.nextID(model.NodeVariable, &loc)

	attrs := map[string]interface{}{}
	if inferredType != "" {
		attrs["inferred_type"] = inferredType
	}
	if sourceText != "" && b.includeSource {
		attrs["source_text"] = sourceText
	}

	node := &model.CpgNode{
		ID:          id,
		Kind:        model.NodeVariable,
		Name:        name,
		Location:    &loc,
		EndLocation: endLoc,
		Scope:       &scope,
		Attrs:       attrs,
	}
	b.cpg.AddNode(node)
	b.cpg.AddEdge(&model.CpgEdge{Source: scope, Target: id, Kind: model.EdgeContains})
	return id
}

func (b *CPGBuilder) EmitCall(targetName string, loc model.SourceLocation, scope model.NodeId, args []string, receiverInferredType string, endLoc *model.SourceLocation, sourceText string) model.NodeId {
	id := b.nextID(model.NodeCall, &loc)

	attrs := map[string]interface{}{
		"args_count": len(args),
	}
	if len(args) > 0 {
		attrs["args"] = args
	}
	if receiverInferredType != "" {
		attrs["receiver_inferred_type"] = receiverInferredType
	}
	if sourceText != "" && b.includeSource {
		attrs["source_text"] = sourceText
	}

	node := &model.CpgNode{
		ID:          id,
		Kind:        model.NodeCall,
		Name:        targetName,
		Location:    &loc,
		EndLocation: endLoc,
		Scope:       &scope,
		Attrs:       attrs,
	}
	b.cpg.AddNode(node)
	b.cpg.AddEdge(&model.CpgEdge{Source: scope, Target: id, Kind: model.EdgeContains})
	return id
}

func (b *CPGBuilder) EmitLiteral(value string, literalType string, loc model.SourceLocation, scope model.NodeId, endLoc *model.SourceLocation, sourceText string) model.NodeId {
	id := b.nextID(model.NodeLiteral, &loc)

	attrs := map[string]interface{}{
		"literal_type": literalType,
		"raw_value":    value,
	}
	if sourceText != "" && b.includeSource {
		attrs["source_text"] = sourceText
	}

	node := &model.CpgNode{
		ID:          id,
		Kind:        model.NodeLiteral,
		Name:        value,
		Location:    &loc,
		EndLocation: endLoc,
		Scope:       &scope,
		Attrs:       attrs,
	}
	b.cpg.AddNode(node)
	b.cpg.AddEdge(&model.CpgEdge{Source: scope, Target: id, Kind: model.EdgeContains})
	return id
}

func (b *CPGBuilder) EmitReturn(loc model.SourceLocation, scope model.NodeId, endLoc *model.SourceLocation, sourceText string) model.NodeId {
	id := b.nextID(model.NodeReturn, &loc)

	attrs := map[string]interface{}{}
	if sourceText != "" && b.includeSource {
		attrs["source_text"] = sourceText
	}

	node := &model.CpgNode{
		ID:          id,
		Kind:        model.NodeReturn,
		Name:        "return",
		Location:    &loc,
		EndLocation: endLoc,
		Scope:       &scope,
		Attrs:       attrs,
	}
	b.cpg.AddNode(node)
	b.cpg.AddEdge(&model.CpgEdge{Source: scope, Target: id, Kind: model.EdgeContains})
	return id
}

func (b *CPGBuilder) EmitImport(module string, names []string, loc model.SourceLocation, scope model.NodeId, isFrom bool, aliases map[string]string, endLoc *model.SourceLocation, sourceText string) model.NodeId {
	id := b.nextID(model.NodeImport, &loc)

	// Build the display name
	var displayName string
	if isFrom {
		displayName = fmt.Sprintf("from %s", module)
	} else {
		displayName = fmt.Sprintf("import %s", module)
	}

	attrs := map[string]interface{}{
		"module":  module,
		"is_from": isFrom,
	}
	if len(names) > 0 {
		attrs["names"] = names
	}
	if len(aliases) > 0 {
		attrs["aliases"] = aliases
	}
	if sourceText != "" && b.includeSource {
		attrs["source_text"] = sourceText
	}

	node := &model.CpgNode{
		ID:          id,
		Kind:        model.NodeImport,
		Name:        displayName,
		Location:    &loc,
		EndLocation: endLoc,
		Scope:       &scope,
		Attrs:       attrs,
	}
	b.cpg.AddNode(node)
	b.cpg.AddEdge(&model.CpgEdge{Source: scope, Target: id, Kind: model.EdgeContains})
	return id
}

func (b *CPGBuilder) EmitBranchNode(branchType string, loc model.SourceLocation, scope model.NodeId, hasElse bool, endLoc *model.SourceLocation, sourceText string) model.NodeId {
	id := b.nextID(model.NodeBranch, &loc)

	attrs := map[string]interface{}{
		"branch_type": branchType,
		"has_else":    hasElse,
	}
	if sourceText != "" && b.includeSource {
		attrs["source_text"] = sourceText
	}

	node := &model.CpgNode{
		ID:          id,
		Kind:        model.NodeBranch,
		Name:        branchType,
		Location:    &loc,
		EndLocation: endLoc,
		Scope:       &scope,
		Attrs:       attrs,
	}
	b.cpg.AddNode(node)
	b.cpg.AddEdge(&model.CpgEdge{Source: scope, Target: id, Kind: model.EdgeContains})
	return id
}

func (b *CPGBuilder) EmitLoopNode(loopType string, loc model.SourceLocation, scope model.NodeId, iteratorVar string, endLoc *model.SourceLocation, sourceText string) model.NodeId {
	id := b.nextID(model.NodeLoop, &loc)

	attrs := map[string]interface{}{
		"loop_type":    loopType,
		"iterator_var": interface{}(nil),
	}
	if iteratorVar != "" {
		attrs["iterator_var"] = iteratorVar
	}
	if sourceText != "" && b.includeSource {
		attrs["source_text"] = sourceText
	}

	node := &model.CpgNode{
		ID:          id,
		Kind:        model.NodeLoop,
		Name:        loopType,
		Location:    &loc,
		EndLocation: endLoc,
		Scope:       &scope,
		Attrs:       attrs,
	}
	b.cpg.AddNode(node)
	b.cpg.AddEdge(&model.CpgEdge{Source: scope, Target: id, Kind: model.EdgeContains})
	return id
}

func (b *CPGBuilder) EmitDataFlow(source, target model.NodeId, attrs map[string]interface{}) {
	if attrs == nil {
		attrs = map[string]interface{}{}
	}
	b.cpg.AddEdge(&model.CpgEdge{
		Source: source,
		Target: target,
		Kind:   model.EdgeDataFlowsTo,
		Attrs:  attrs,
	})
}

func (b *CPGBuilder) EmitDefinition(variable, definedBy model.NodeId) {
	b.cpg.AddEdge(&model.CpgEdge{
		Source: variable,
		Target: definedBy,
		Kind:   model.EdgeDefinedBy,
	})
}

func (b *CPGBuilder) EmitUsage(variable, usedAt model.NodeId) {
	b.cpg.AddEdge(&model.CpgEdge{
		Source: variable,
		Target: usedAt,
		Kind:   model.EdgeUsedBy,
	})
}

func (b *CPGBuilder) EmitControlFlow(from, to model.NodeId) {
	b.cpg.AddEdge(&model.CpgEdge{
		Source: from,
		Target: to,
		Kind:   model.EdgeFlowsTo,
	})
}

func (b *CPGBuilder) EmitBranch(from, trueBranch model.NodeId, falseBranch *model.NodeId) {
	b.cpg.AddEdge(&model.CpgEdge{
		Source: from,
		Target: trueBranch,
		Kind:   model.EdgeBranchesTo,
	})
	if falseBranch != nil {
		b.cpg.AddEdge(&model.CpgEdge{
			Source: from,
			Target: *falseBranch,
			Kind:   model.EdgeBranchesTo,
		})
	}
}

// ---------- Helpers ----------

// normalizePath makes a file path relative to relativeRoot when configured.
// The result always uses forward slashes.
func (b *CPGBuilder) normalizePath(path string) string {
	if b.relativeRoot == "" {
		return filepath.ToSlash(path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	rel, err := filepath.Rel(b.relativeRoot, abs)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(rel)
}

// findVisitor returns the appropriate LanguageVisitor for a file extension
// or explicit language name. Returns nil if no visitor is available.
func (b *CPGBuilder) findVisitor(ext string, language string) LanguageVisitor {
	if b.registry == nil {
		return nil
	}
	if language != "" {
		// Try extension lookup with a synthetic extension
		return b.registry.GetVisitor("." + language)
	}
	return b.registry.GetVisitor(ext)
}

// shouldExclude checks whether a filename or directory name matches any of
// the exclude patterns. Uses filepath.Match for glob-style matching and
// also checks for exact base-name matches.
func shouldExclude(name string, patterns []string) bool {
	for _, pattern := range patterns {
		// Strip leading **/ for gitignore-style patterns
		p := pattern
		if strings.HasPrefix(p, "**/") {
			p = p[3:]
		}

		if name == p {
			return true
		}
		if matched, _ := filepath.Match(p, name); matched {
			return true
		}
	}
	return false
}

// reportProgress calls the progress callback if one is set.
func (b *CPGBuilder) reportProgress(phase, detail string) {
	if b.progress != nil {
		b.progress(phase, detail)
	}
}

// checkDeadline returns an error if the build has exceeded its timeout.
func (b *CPGBuilder) checkDeadline(deadline time.Time) error {
	if deadline.IsZero() {
		return nil
	}
	if time.Now().After(deadline) {
		return fmt.Errorf("build timeout exceeded")
	}
	return nil
}

// sortNodesByLocation sorts nodes by (line, column) in ascending order.
// Nodes without locations are placed at the end.
func sortNodesByLocation(nodes []*model.CpgNode) {
	sort.SliceStable(nodes, func(i, j int) bool {
		li, lj := nodes[i].Location, nodes[j].Location
		if li == nil && lj == nil {
			return false
		}
		if li == nil {
			return false
		}
		if lj == nil {
			return true
		}
		if li.Line != lj.Line {
			return li.Line < lj.Line
		}
		return li.Column < lj.Column
	})
}

// sortNodesByPosition sorts parameter nodes by their "position" attr.
func sortNodesByPosition(nodes []*model.CpgNode) {
	sort.SliceStable(nodes, func(i, j int) bool {
		pi := getPosition(nodes[i])
		pj := getPosition(nodes[j])
		return pi < pj
	})
}

// getPosition extracts the integer position from a node's attrs.
// Returns 0 if the attr is missing or not a number.
func getPosition(node *model.CpgNode) int {
	if node.Attrs == nil {
		return 0
	}
	v, ok := node.Attrs["position"]
	if !ok {
		return 0
	}
	switch p := v.(type) {
	case int:
		return p
	case float64:
		return int(p)
	default:
		return 0
	}
}
