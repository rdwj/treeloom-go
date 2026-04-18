// Package python implements the Python language visitor for treeloom's CPG builder.
//
// It walks Python tree-sitter parse trees and emits CPG nodes/edges using
// the graph.NodeEmitter interface. This is a port of the Python treeloom
// reference implementation (treeloom/lang/builtin/python.py).
package python

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/python"

	"github.com/rdwj/treeloom-go/internal/graph"
	"github.com/rdwj/treeloom-go/internal/model"
)

// literalTypes maps tree-sitter node types to treeloom literal type names.
var literalTypes = map[string]string{
	"integer":             "int",
	"float":               "float",
	"string":              "str",
	"true":                "bool",
	"false":               "bool",
	"none":                "none",
	"concatenated_string": "str",
}

// Visitor implements graph.LanguageVisitor for Python source files.
type Visitor struct{}

// Compile-time check that Visitor implements graph.LanguageVisitor.
var _ graph.LanguageVisitor = (*Visitor)(nil)

func (v *Visitor) Name() string           { return "python" }
func (v *Visitor) Extensions() []string   { return []string{".py", ".pyi"} }

// Parse parses Python source bytes and returns the tree-sitter parse tree.
// Returns an error if parsing produces a tree with errors.
func (v *Visitor) Parse(source []byte, filename string) (interface{}, error) {
	parser := sitter.NewParser()
	parser.SetLanguage(python.GetLanguage())
	tree, err := parser.ParseCtx(context.Background(), nil, source)
	if err != nil {
		return nil, err
	}
	if tree.RootNode().HasError() {
		return nil, fmt.Errorf("parse tree has errors for %s", filename)
	}
	return &parsedTree{tree: tree, source: source}, nil
}

// parsedTree bundles the tree-sitter tree with the source bytes so Visit
// can access both without requiring the caller to pass source separately.
type parsedTree struct {
	tree   *sitter.Tree
	source []byte
}

// Visit walks the parse tree and emits CPG nodes/edges via the emitter.
func (v *Visitor) Visit(tree interface{}, filePath string, emitter graph.NodeEmitter) {
	pt, ok := tree.(*parsedTree)
	if !ok {
		return
	}

	root := pt.tree.RootNode()
	source := pt.source

	baseName := filepath.Base(filePath)
	moduleName := strings.TrimSuffix(baseName, filepath.Ext(baseName))

	moduleEnd := endLocation(root, filePath)
	moduleID := emitter.EmitModule(moduleName, filePath, moduleEnd, "")

	ctx := &visitContext{
		emitter:         emitter,
		filePath:        filePath,
		source:          source,
		scopeStack:      []model.NodeId{moduleID},
		definedVars:     newScopeStack(),
		varTypes:        make(map[string]string),
		funcReturnTypes: make(map[string]string),
	}

	for i := 0; i < int(root.NamedChildCount()); i++ {
		child := root.NamedChild(i)
		visitNode(child, ctx)
	}
}

// ResolveCalls links CALL nodes to FUNCTION definitions using type-based MRO
// resolution, name-based resolution, and import-following as fallbacks.
func (v *Visitor) ResolveCalls(
	cpg *graph.CodePropertyGraph,
	functionNodes, callNodes []*model.CpgNode,
) []graph.CallResolution {
	// Build function name -> candidates index
	functions := make(map[string][]*model.CpgNode)
	for _, fn := range functionNodes {
		functions[fn.Name] = append(functions[fn.Name], fn)
	}

	// Build class hierarchy and method index for type-based resolution
	classNodes := make(map[string]*model.CpgNode)
	for _, n := range cpg.Nodes(model.NodeClass) {
		classNodes[n.Name] = n
	}

	methodIndex := make(map[[2]string]*model.CpgNode)
	for _, fn := range functionNodes {
		scope := cpg.Node(scopeOf(fn))
		if scope != nil && scope.Kind == model.NodeClass {
			methodIndex[[2]string{scope.Name, fn.Name}] = fn
		}
	}

	// Build import map: local_name -> (module_name, original_name)
	importMap := make(map[string][2]string)
	for _, impNode := range cpg.Nodes(model.NodeImport) {
		isFrom, _ := impNode.Attrs["is_from"].(bool)
		if !isFrom {
			continue
		}
		moduleName, _ := impNode.Attrs["module"].(string)
		names := attrStringSlice(impNode.Attrs, "names")
		aliases := attrStringMap(impNode.Attrs, "aliases")
		for _, impName := range names {
			local := impName
			if a, ok := aliases[impName]; ok {
				local = a
			}
			importMap[local] = [2]string{moduleName, impName}
		}
	}

	var resolved []graph.CallResolution
	for _, callNode := range callNodes {
		target := callNode.Name
		var fn *model.CpgNode

		// Try type-based MRO resolution for method calls
		receiverType, _ := callNode.Attrs["receiver_inferred_type"].(string)
		if receiverType != "" && strings.Contains(target, ".") {
			methodName := target[strings.LastIndex(target, ".")+1:]
			fn = resolveMethodViaMRO(receiverType, methodName, methodIndex, classNodes)
		}

		// Fall back to name-based resolution
		if fn == nil {
			fn = resolveSingleCall(callNode, target, functions, cpg)
		}

		// Try short name (strip qualifier)
		if fn == nil && strings.Contains(target, ".") {
			shortName := target[strings.LastIndex(target, ".")+1:]
			fn = resolveSingleCall(callNode, shortName, functions, cpg)
		}

		// Try import-following
		if fn == nil {
			if impInfo, ok := importMap[target]; ok {
				impModule, impName := impInfo[0], impInfo[1]
				candidates := functions[impName]
				for _, candidate := range candidates {
					scope := cpg.Node(scopeOf(candidate))
					if scope != nil && scope.Kind == model.NodeModule {
						modParts := strings.Split(impModule, ".")
						lastPart := modParts[len(modParts)-1]
						if scope.Name == impModule || scope.Name == lastPart {
							fn = candidate
							break
						}
					}
				}
			}
		}

		if fn != nil {
			resolved = append(resolved, graph.CallResolution{
				CallID: callNode.ID,
				FuncID: fn.ID,
			})
		}
	}

	return resolved
}

// --- Visit dispatch ---------------------------------------------------------

func visitNode(node *sitter.Node, ctx *visitContext) {
	switch node.Type() {
	case "class_definition":
		visitClassDefinition(node, ctx)
	case "decorated_definition":
		visitDecoratedDefinition(node, ctx)
	case "function_definition":
		visitFunctionDefinition(node, ctx)
	case "expression_statement":
		visitExpressionStatement(node, ctx)
	case "return_statement":
		visitReturnStatement(node, ctx)
	case "import_statement":
		visitImportStatement(node, ctx)
	case "import_from_statement":
		visitImportFromStatement(node, ctx)
	case "if_statement":
		visitIfStatement(node, ctx)
	case "for_statement":
		visitForStatement(node, ctx)
	case "while_statement":
		visitWhileStatement(node, ctx)
	default:
		// Recurse into children for unrecognized node types
		for i := 0; i < int(node.NamedChildCount()); i++ {
			visitNode(node.NamedChild(i), ctx)
		}
	}
}

// --- Visit context ----------------------------------------------------------

type visitContext struct {
	emitter           graph.NodeEmitter
	filePath          string
	source            []byte
	scopeStack        []model.NodeId
	definedVars       *scopeStack
	pendingDecorators []string
	varTypes          map[string]string
	classStack        []string
	funcReturnTypes   map[string]string
}

func (ctx *visitContext) currentScope() model.NodeId {
	return ctx.scopeStack[len(ctx.scopeStack)-1]
}

func (ctx *visitContext) pushScope(id model.NodeId) {
	ctx.scopeStack = append(ctx.scopeStack, id)
}

func (ctx *visitContext) popScope() {
	if len(ctx.scopeStack) > 1 {
		ctx.scopeStack = ctx.scopeStack[:len(ctx.scopeStack)-1]
	}
}

// --- Scope stack (LEGB-style variable lookup) -------------------------------

type scopeStack struct {
	stack []map[string]model.NodeId
}

func newScopeStack() *scopeStack {
	return &scopeStack{
		stack: []map[string]model.NodeId{{}},
	}
}

func (s *scopeStack) push() {
	s.stack = append(s.stack, make(map[string]model.NodeId))
}

func (s *scopeStack) pop() {
	if len(s.stack) > 1 {
		s.stack = s.stack[:len(s.stack)-1]
	}
}

func (s *scopeStack) define(name string, id model.NodeId) {
	s.stack[len(s.stack)-1][name] = id
}

func (s *scopeStack) lookup(name string) (model.NodeId, bool) {
	for i := len(s.stack) - 1; i >= 0; i-- {
		if id, ok := s.stack[i][name]; ok {
			return id, true
		}
	}
	return "", false
}

// --- Node visitors ----------------------------------------------------------

func visitClassDefinition(node *sitter.Node, ctx *visitContext) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	className := nodeText(nameNode, ctx.source)
	loc := location(node, ctx.filePath)
	scope := ctx.currentScope()

	// Extract base classes from superclasses argument_list
	var bases []string
	superclasses := node.ChildByFieldName("superclasses")
	if superclasses != nil {
		for i := 0; i < int(superclasses.NamedChildCount()); i++ {
			child := superclasses.NamedChild(i)
			if child.Type() == "identifier" || child.Type() == "attribute" {
				bases = append(bases, nodeText(child, ctx.source))
			}
		}
	}

	classID := ctx.emitter.EmitClass(
		className, loc, scope, bases,
		endLocation(node, ctx.filePath),
		nodeText(node, ctx.source),
	)

	ctx.pushScope(classID)
	ctx.classStack = append(ctx.classStack, className)
	ctx.definedVars.push()

	body := node.ChildByFieldName("body")
	if body != nil {
		for i := 0; i < int(body.NamedChildCount()); i++ {
			visitNode(body.NamedChild(i), ctx)
		}
	}

	ctx.definedVars.pop()
	ctx.classStack = ctx.classStack[:len(ctx.classStack)-1]
	ctx.popScope()
}

func visitDecoratedDefinition(node *sitter.Node, ctx *visitContext) {
	var decoratorNames []string

	for i := 0; i < int(node.NamedChildCount()); i++ {
		child := node.NamedChild(i)
		if child.Type() == "decorator" {
			for j := 0; j < int(child.NamedChildCount()); j++ {
				decChild := child.NamedChild(j)
				switch decChild.Type() {
				case "call":
					name := extractCallName(decChild, ctx.source)
					decoratorNames = append(decoratorNames, name)
					visitCallExpression(decChild, ctx)
				case "identifier", "attribute":
					decoratorNames = append(decoratorNames, nodeText(decChild, ctx.source))
				}
			}
		} else if child.Type() == "function_definition" || child.Type() == "class_definition" {
			ctx.pendingDecorators = decoratorNames
			visitNode(child, ctx)
			ctx.pendingDecorators = nil
		}
	}
}

func visitFunctionDefinition(node *sitter.Node, ctx *visitContext) {
	// Check for async
	isAsync := false
	for i := 0; i < int(node.ChildCount()); i++ {
		if node.Child(i).Type() == "async" {
			isAsync = true
			break
		}
	}

	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	funcName := nodeText(nameNode, ctx.source)
	loc := location(node, ctx.filePath)
	scope := ctx.currentScope()

	var decorators []string
	if len(ctx.pendingDecorators) > 0 {
		decorators = ctx.pendingDecorators
		ctx.pendingDecorators = nil
	}

	funcID := ctx.emitter.EmitFunction(
		funcName, loc, scope, isAsync, decorators,
		endLocation(node, ctx.filePath),
		nodeText(node, ctx.source),
	)

	// Extract return type annotation
	returnTypeNode := node.ChildByFieldName("return_type")
	if returnTypeNode == nil {
		// Try manual search for type after ->
		for i := 0; i < int(node.ChildCount()); i++ {
			child := node.Child(i)
			if child.Type() == "type" {
				prev := child.PrevSibling()
				if prev != nil && prev.Type() == "->" {
					returnTypeNode = child
					break
				}
			}
		}
	}
	if returnTypeNode != nil {
		if retType := extractTypeText(returnTypeNode, ctx.source); retType != "" {
			ctx.funcReturnTypes[funcName] = retType
		}
	}

	ctx.pushScope(funcID)
	ctx.definedVars.push()

	// Emit parameters
	paramsNode := node.ChildByFieldName("parameters")
	if paramsNode != nil {
		position := 0
		for i := 0; i < int(paramsNode.NamedChildCount()); i++ {
			child := paramsNode.NamedChild(i)
			paramName, typeAnn, ok := extractSingleParamName(child, ctx.source)
			if !ok {
				continue
			}
			paramLoc := location(child, ctx.filePath)
			paramID := ctx.emitter.EmitParameter(
				paramName, paramLoc, funcID, typeAnn, position,
				endLocation(child, ctx.filePath),
				"",
			)
			ctx.definedVars.define(paramName, paramID)
			if typeAnn != "" {
				ctx.varTypes[paramName] = typeAnn
			}
			position++
		}
	}

	body := node.ChildByFieldName("body")
	if body != nil {
		for i := 0; i < int(body.NamedChildCount()); i++ {
			visitNode(body.NamedChild(i), ctx)
		}
	}

	ctx.definedVars.pop()
	ctx.popScope()
}

func visitExpressionStatement(node *sitter.Node, ctx *visitContext) {
	for i := 0; i < int(node.NamedChildCount()); i++ {
		child := node.NamedChild(i)
		switch child.Type() {
		case "assignment", "augmented_assignment":
			visitAssignment(child, ctx)
		default:
			visitExpression(child, ctx)
		}
	}
}

func visitAssignment(node *sitter.Node, ctx *visitContext) {
	left := node.ChildByFieldName("left")
	right := node.ChildByFieldName("right")
	if left == nil {
		return
	}

	scope := ctx.currentScope()
	varName := nodeText(left, ctx.source)
	loc := location(left, ctx.filePath)

	// Extract type annotation from annotated assignment
	var annotationType string
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == "type" {
			annotationType = extractTypeText(child, ctx.source)
			if annotationType != "" {
				ctx.varTypes[varName] = annotationType
			}
			break
		}
	}

	// Infer type from constructor call on RHS
	inferredType := annotationType
	if annotationType == "" && left.Type() == "identifier" && right != nil && right.Type() == "call" {
		callName := extractCallName(right, ctx.source)
		if callName != "" {
			short := callName
			if strings.Contains(callName, ".") {
				short = callName[strings.LastIndex(callName, ".")+1:]
			}
			retType, ok := ctx.funcReturnTypes[short]
			if ok {
				inferredType = retType
				ctx.varTypes[varName] = retType
			} else {
				inferredType = short
				ctx.varTypes[varName] = short
			}
		}
	}

	varID := ctx.emitter.EmitVariable(
		varName, loc, scope, inferredType,
		endLocation(left, ctx.filePath),
		"",
	)
	ctx.definedVars.define(varName, varID)

	if right != nil {
		rhsID := visitExpression(right, ctx)
		if rhsID != nil {
			ctx.emitter.EmitDefinition(varID, *rhsID)
			ctx.emitter.EmitDataFlow(*rhsID, varID, nil)
		}
	}
}

func visitReturnStatement(node *sitter.Node, ctx *visitContext) {
	loc := location(node, ctx.filePath)
	scope := ctx.currentScope()
	retID := ctx.emitter.EmitReturn(
		loc, scope,
		endLocation(node, ctx.filePath),
		"",
	)

	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == "return" {
			continue
		}
		if !child.IsNamed() {
			continue
		}
		exprID := visitExpression(child, ctx)
		if exprID != nil {
			ctx.emitter.EmitDataFlow(*exprID, retID, nil)
			if child.Type() == "identifier" {
				ctx.emitter.EmitUsage(*exprID, retID)
			}
		}
	}
}

func visitImportStatement(node *sitter.Node, ctx *visitContext) {
	loc := location(node, ctx.filePath)
	scope := ctx.currentScope()

	var names []string
	aliases := make(map[string]string)

	for i := 0; i < int(node.NamedChildCount()); i++ {
		child := node.NamedChild(i)
		switch child.Type() {
		case "dotted_name":
			names = append(names, nodeText(child, ctx.source))
		case "aliased_import":
			nameChild := child.ChildByFieldName("name")
			aliasChild := child.ChildByFieldName("alias")
			if nameChild != nil {
				orig := nodeText(nameChild, ctx.source)
				names = append(names, orig)
				if aliasChild != nil {
					aliases[orig] = nodeText(aliasChild, ctx.source)
				}
			}
		}
	}

	moduleName := ""
	if len(names) > 0 {
		moduleName = names[0]
	}

	var aliasesArg map[string]string
	if len(aliases) > 0 {
		aliasesArg = aliases
	}

	ctx.emitter.EmitImport(
		moduleName, names, loc, scope, false, aliasesArg,
		endLocation(node, ctx.filePath),
		"",
	)
}

func visitImportFromStatement(node *sitter.Node, ctx *visitContext) {
	loc := location(node, ctx.filePath)
	scope := ctx.currentScope()

	var moduleName string
	var importedNames []string
	aliases := make(map[string]string)
	sawImport := false

	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		switch child.Type() {
		case "from":
			continue
		case "import":
			sawImport = true
			continue
		case "dotted_name":
			text := nodeText(child, ctx.source)
			if !sawImport {
				moduleName = text
			} else {
				importedNames = append(importedNames, text)
			}
		case "aliased_import":
			nameChild := child.ChildByFieldName("name")
			aliasChild := child.ChildByFieldName("alias")
			if nameChild != nil {
				orig := nodeText(nameChild, ctx.source)
				importedNames = append(importedNames, orig)
				if aliasChild != nil {
					aliases[orig] = nodeText(aliasChild, ctx.source)
				}
			}
		}
	}

	var aliasesArg map[string]string
	if len(aliases) > 0 {
		aliasesArg = aliases
	}

	ctx.emitter.EmitImport(
		moduleName, importedNames, loc, scope, true, aliasesArg,
		endLocation(node, ctx.filePath),
		"",
	)
}

func visitIfStatement(node *sitter.Node, ctx *visitContext) {
	loc := location(node, ctx.filePath)
	scope := ctx.currentScope()

	hasElse := false
	for i := 0; i < int(node.NamedChildCount()); i++ {
		if node.NamedChild(i).Type() == "else_clause" {
			hasElse = true
			break
		}
	}

	branchID := ctx.emitter.EmitBranchNode(
		"if", loc, scope, hasElse,
		endLocation(node, ctx.filePath),
		"",
	)

	// Visit the condition
	condition := node.ChildByFieldName("condition")
	if condition != nil {
		visitExpression(condition, ctx)
	}

	// Visit if body
	consequence := node.ChildByFieldName("consequence")
	if consequence != nil {
		ctx.pushScope(branchID)
		for i := 0; i < int(consequence.NamedChildCount()); i++ {
			visitNode(consequence.NamedChild(i), ctx)
		}
		ctx.popScope()
	}

	// Visit elif/else clauses
	for i := 0; i < int(node.NamedChildCount()); i++ {
		child := node.NamedChild(i)
		switch child.Type() {
		case "elif_clause":
			visitElifClause(child, ctx, branchID)
		case "else_clause":
			body := child.ChildByFieldName("body")
			if body != nil {
				ctx.pushScope(branchID)
				for j := 0; j < int(body.NamedChildCount()); j++ {
					visitNode(body.NamedChild(j), ctx)
				}
				ctx.popScope()
			}
		}
	}
}

func visitElifClause(node *sitter.Node, ctx *visitContext, parentBranch model.NodeId) {
	loc := location(node, ctx.filePath)
	elifID := ctx.emitter.EmitBranchNode(
		"elif", loc, parentBranch, false,
		endLocation(node, ctx.filePath),
		"",
	)

	condition := node.ChildByFieldName("condition")
	if condition != nil {
		visitExpression(condition, ctx)
	}

	consequence := node.ChildByFieldName("consequence")
	if consequence != nil {
		ctx.pushScope(elifID)
		for i := 0; i < int(consequence.NamedChildCount()); i++ {
			visitNode(consequence.NamedChild(i), ctx)
		}
		ctx.popScope()
	}
}

func visitForStatement(node *sitter.Node, ctx *visitContext) {
	loc := location(node, ctx.filePath)
	scope := ctx.currentScope()

	var iteratorVar string
	left := node.ChildByFieldName("left")
	if left != nil && left.Type() == "identifier" {
		iteratorVar = nodeText(left, ctx.source)
	}

	loopID := ctx.emitter.EmitLoopNode(
		"for", loc, scope, iteratorVar,
		endLocation(node, ctx.filePath),
		"",
	)

	// Emit the iterator variable
	if left != nil && left.Type() == "identifier" && iteratorVar != "" {
		varLoc := location(left, ctx.filePath)
		varID := ctx.emitter.EmitVariable(
			iteratorVar, varLoc, loopID, "",
			endLocation(left, ctx.filePath),
			"",
		)
		ctx.definedVars.define(iteratorVar, varID)
	}

	// Visit iterable expression
	right := node.ChildByFieldName("right")
	if right != nil {
		visitExpression(right, ctx)
	}

	body := node.ChildByFieldName("body")
	if body != nil {
		ctx.pushScope(loopID)
		for i := 0; i < int(body.NamedChildCount()); i++ {
			visitNode(body.NamedChild(i), ctx)
		}
		ctx.popScope()
	}
}

func visitWhileStatement(node *sitter.Node, ctx *visitContext) {
	loc := location(node, ctx.filePath)
	scope := ctx.currentScope()

	loopID := ctx.emitter.EmitLoopNode(
		"while", loc, scope, "",
		endLocation(node, ctx.filePath),
		"",
	)

	condition := node.ChildByFieldName("condition")
	if condition != nil {
		visitExpression(condition, ctx)
	}

	body := node.ChildByFieldName("body")
	if body != nil {
		ctx.pushScope(loopID)
		for i := 0; i < int(body.NamedChildCount()); i++ {
			visitNode(body.NamedChild(i), ctx)
		}
		ctx.popScope()
	}
}

// --- Expression visitor (returns *model.NodeId of the emitted node) ---------

func visitExpression(node *sitter.Node, ctx *visitContext) *model.NodeId {
	switch node.Type() {
	case "call":
		return visitCallExpression(node, ctx)

	case "integer", "float", "string", "true", "false", "none", "concatenated_string":
		return visitLiteral(node, ctx)

	case "identifier":
		varName := nodeText(node, ctx.source)
		if id, ok := ctx.definedVars.lookup(varName); ok {
			return &id
		}
		return nil

	case "attribute":
		return visitAttribute(node, ctx)

	case "subscript":
		return visitSubscript(node, ctx)

	case "binary_operator":
		return visitBinaryOperator(node, ctx)

	case "comparison_operator":
		for i := 0; i < int(node.NamedChildCount()); i++ {
			visitExpression(node.NamedChild(i), ctx)
		}
		return nil

	case "parenthesized_expression":
		for i := 0; i < int(node.NamedChildCount()); i++ {
			return visitExpression(node.NamedChild(i), ctx)
		}
		return nil

	case "keyword_argument":
		valNode := node.ChildByFieldName("value")
		if valNode != nil {
			return visitExpression(valNode, ctx)
		}
		return nil

	case "dictionary_splat":
		for i := 0; i < int(node.NamedChildCount()); i++ {
			child := node.NamedChild(i)
			if child.Type() == "identifier" {
				varName := nodeText(child, ctx.source)
				if id, ok := ctx.definedVars.lookup(varName); ok {
					return &id
				}
			}
		}
		return nil

	case "list_comprehension", "set_comprehension", "generator_expression":
		for i := 0; i < int(node.NamedChildCount()); i++ {
			child := node.NamedChild(i)
			if child.Type() == "for_in_clause" {
				iterable := child.ChildByFieldName("right")
				if iterable != nil {
					visitExpression(iterable, ctx)
				}
			} else if child.Type() != "for_in_clause" && child.Type() != "if_clause" {
				visitExpression(child, ctx)
			}
		}
		return nil

	case "dictionary_comprehension":
		for i := 0; i < int(node.NamedChildCount()); i++ {
			child := node.NamedChild(i)
			if child.Type() == "for_in_clause" {
				iterable := child.ChildByFieldName("right")
				if iterable != nil {
					visitExpression(iterable, ctx)
				}
			} else if child.IsNamed() {
				visitExpression(child, ctx)
			}
		}
		return nil
	}

	// Default: recurse into named children
	for i := 0; i < int(node.NamedChildCount()); i++ {
		visitExpression(node.NamedChild(i), ctx)
	}
	return nil
}

func visitLiteral(node *sitter.Node, ctx *visitContext) *model.NodeId {
	loc := location(node, ctx.filePath)
	value := nodeText(node, ctx.source)
	litType := literalTypes[node.Type()]

	// Check for f-string interpolation
	if node.Type() == "string" || node.Type() == "concatenated_string" {
		interpIDs := collectInterpolationIDs(node, ctx)
		if len(interpIDs) > 0 {
			fstrID := ctx.emitter.EmitCall(
				"f-string", loc, ctx.currentScope(), nil, "",
				endLocation(node, ctx.filePath),
				"",
			)
			for _, iid := range interpIDs {
				ctx.emitter.EmitDataFlow(iid, fstrID, nil)
			}
			return &fstrID
		}
	}

	id := ctx.emitter.EmitLiteral(
		value, litType, loc, ctx.currentScope(),
		endLocation(node, ctx.filePath),
		"",
	)
	return &id
}

func visitAttribute(node *sitter.Node, ctx *visitContext) *model.NodeId {
	attrText := nodeText(node, ctx.source)
	loc := location(node, ctx.filePath)

	// Field sensitivity: reuse existing definition
	if existingID, ok := ctx.definedVars.lookup(attrText); ok {
		return &existingID
	}

	attrID := ctx.emitter.EmitVariable(
		attrText, loc, ctx.currentScope(), "",
		endLocation(node, ctx.filePath),
		"",
	)
	ctx.definedVars.define(attrText, attrID)

	// Wire data from the receiver object
	attrNameNode := node.ChildByFieldName("attribute")
	var fieldName string
	if attrNameNode != nil {
		fieldName = nodeText(attrNameNode, ctx.source)
	}

	objNode := node.ChildByFieldName("object")
	if objNode != nil {
		switch objNode.Type() {
		case "identifier":
			objName := nodeText(objNode, ctx.source)
			if objDefID, ok := ctx.definedVars.lookup(objName); ok {
				attrs := makeFieldAttrs(fieldName)
				ctx.emitter.EmitDataFlow(objDefID, attrID, attrs)
			}
		case "attribute", "subscript":
			receiverID := visitExpression(objNode, ctx)
			if receiverID != nil {
				attrs := makeFieldAttrs(fieldName)
				ctx.emitter.EmitDataFlow(*receiverID, attrID, attrs)
			}
		}
	}
	return &attrID
}

func visitSubscript(node *sitter.Node, ctx *visitContext) *model.NodeId {
	subText := nodeText(node, ctx.source)
	loc := location(node, ctx.filePath)

	subID := ctx.emitter.EmitVariable(
		subText, loc, ctx.currentScope(), "",
		endLocation(node, ctx.filePath),
		"",
	)

	valNode := node.ChildByFieldName("value")
	if valNode != nil {
		switch valNode.Type() {
		case "identifier":
			valName := nodeText(valNode, ctx.source)
			if valDefID, ok := ctx.definedVars.lookup(valName); ok {
				ctx.emitter.EmitDataFlow(valDefID, subID, nil)
			}
		case "attribute", "subscript":
			valID := visitExpression(valNode, ctx)
			if valID != nil {
				ctx.emitter.EmitDataFlow(*valID, subID, nil)
			}
		}
	}

	// Visit subscript key for nested calls/refs
	keyNode := node.ChildByFieldName("subscript")
	if keyNode != nil {
		visitExpression(keyNode, ctx)
	}

	return &subID
}

func visitBinaryOperator(node *sitter.Node, ctx *visitContext) *model.NodeId {
	left := node.ChildByFieldName("left")
	right := node.ChildByFieldName("right")
	operator := node.ChildByFieldName("operator")

	// Detect %-formatting
	isPercentFmt := false
	if operator != nil {
		isPercentFmt = nodeText(operator, ctx.source) == "%"
	} else {
		// Fallback: scan unnamed children for %
		for i := 0; i < int(node.ChildCount()); i++ {
			child := node.Child(i)
			if !child.IsNamed() && child.Type() == "%" {
				isPercentFmt = true
				break
			}
		}
	}

	if isPercentFmt {
		var leftID *model.NodeId
		if left != nil {
			leftID = visitExpression(left, ctx)
		}
		loc := location(node, ctx.filePath)
		fmtID := ctx.emitter.EmitCall(
			"%", loc, ctx.currentScope(), nil, "",
			endLocation(node, ctx.filePath),
			"",
		)
		if leftID != nil {
			ctx.emitter.EmitDataFlow(*leftID, fmtID, nil)
		}
		if right != nil {
			rhsIDs := collectExpressionIDs(right, ctx)
			for _, rid := range rhsIDs {
				ctx.emitter.EmitDataFlow(rid, fmtID, nil)
			}
		}
		return &fmtID
	}

	// Non-% binary operator
	var leftID *model.NodeId
	if left != nil {
		leftID = visitExpression(left, ctx)
	}
	if right != nil {
		visitExpression(right, ctx)
	}
	return leftID
}

func visitCallExpression(node *sitter.Node, ctx *visitContext) *model.NodeId {
	funcNode := node.ChildByFieldName("function")
	if funcNode == nil {
		return nil
	}

	var targetName string
	var receiverCallID *model.NodeId

	if funcNode.Type() == "attribute" {
		objNode := funcNode.ChildByFieldName("object")
		attrNode := funcNode.ChildByFieldName("attribute")

		if objNode != nil && objNode.Type() == "call" {
			// Case 1: chained call (e.g. foo().bar())
			receiverCallID = visitExpression(objNode, ctx)
			methodName := nodeText(funcNode, ctx.source)
			if attrNode != nil {
				methodName = nodeText(attrNode, ctx.source)
			}
			innerFuncName := extractCallName(objNode, ctx.source)
			if innerFuncName != "" {
				targetName = innerFuncName + "." + methodName
			} else {
				targetName = methodName
			}
		} else if objNode != nil && objNode.Type() == "attribute" {
			// Case 2: chained attribute (e.g. request.form.get())
			receiverCallID = visitExpression(objNode, ctx)
			targetName = nodeText(funcNode, ctx.source)
		} else {
			targetName = nodeText(funcNode, ctx.source)
		}
	} else {
		targetName = nodeText(funcNode, ctx.source)
	}

	// Look up receiver's inferred type for method calls
	var receiverType string
	if funcNode.Type() == "attribute" {
		obj := funcNode.ChildByFieldName("object")
		if obj != nil && obj.Type() == "identifier" {
			receiverName := nodeText(obj, ctx.source)
			if t, ok := ctx.varTypes[receiverName]; ok {
				receiverType = t
			} else if receiverName == "self" || receiverName == "cls" {
				if len(ctx.classStack) > 0 {
					receiverType = ctx.classStack[len(ctx.classStack)-1]
				}
			}
		}
	}

	loc := location(node, ctx.filePath)
	scope := ctx.currentScope()

	// Collect arguments
	argsNode := node.ChildByFieldName("arguments")
	var argTexts []string
	var argIDs []*model.NodeId
	var argIsVar []bool
	if argsNode != nil {
		for i := 0; i < int(argsNode.NamedChildCount()); i++ {
			child := argsNode.NamedChild(i)
			argTexts = append(argTexts, nodeText(child, ctx.source))
			argIDs = append(argIDs, visitExpression(child, ctx))
			argIsVar = append(argIsVar, child.Type() == "identifier")
		}
	}

	callID := ctx.emitter.EmitCall(
		targetName, loc, scope, argTexts, receiverType,
		endLocation(node, ctx.filePath),
		"",
	)

	// Wire data flow from chained receiver
	if receiverCallID != nil {
		ctx.emitter.EmitDataFlow(*receiverCallID, callID, nil)
	}

	// Wire DATA_FLOWS_TO and USED_BY from arguments to the call
	for i, argID := range argIDs {
		if argID != nil {
			ctx.emitter.EmitDataFlow(*argID, callID, nil)
			if i < len(argIsVar) && argIsVar[i] {
				ctx.emitter.EmitUsage(*argID, callID)
			}
		}
	}

	return &callID
}

// --- Helper functions -------------------------------------------------------

func location(node *sitter.Node, filePath string) model.SourceLocation {
	return model.SourceLocation{
		File:   filePath,
		Line:   int(node.StartPoint().Row) + 1,
		Column: int(node.StartPoint().Column),
	}
}

func endLocation(node *sitter.Node, filePath string) *model.SourceLocation {
	loc := model.SourceLocation{
		File:   filePath,
		Line:   int(node.EndPoint().Row) + 1,
		Column: int(node.EndPoint().Column),
	}
	return &loc
}

func nodeText(node *sitter.Node, source []byte) string {
	return node.Content(source)
}

func extractCallName(callNode *sitter.Node, source []byte) string {
	funcPart := callNode.ChildByFieldName("function")
	if funcPart == nil {
		return ""
	}
	return nodeText(funcPart, source)
}

// extractTypeText extracts the base type name from a tree-sitter type node.
// Handles simple (int), generic (list[str] -> list), and qualified (mod.Type -> Type).
func extractTypeText(typeNode *sitter.Node, source []byte) string {
	raw := nodeText(typeNode, source)
	base := stripGenerics(raw)
	if strings.Contains(base, ".") {
		base = base[strings.LastIndex(base, ".")+1:]
	}
	if base == "" {
		return ""
	}
	return base
}

func stripGenerics(typeStr string) string {
	idx := strings.Index(typeStr, "[")
	if idx != -1 {
		return typeStr[:idx]
	}
	return typeStr
}

// extractSingleParamName extracts a parameter name and optional type annotation
// from a tree-sitter parameter child node. Returns ("", "", false) for
// non-parameter nodes (punctuation, self, cls).
func extractSingleParamName(child *sitter.Node, source []byte) (name string, typeAnn string, ok bool) {
	switch child.Type() {
	case "identifier":
		n := nodeText(child, source)
		if n == "self" || n == "cls" {
			return "", "", false
		}
		return n, "", true

	case "default_parameter":
		nameNode := child.ChildByFieldName("name")
		if nameNode != nil {
			n := nodeText(nameNode, source)
			if n == "self" || n == "cls" {
				return "", "", false
			}
			return n, "", true
		}

	case "typed_parameter":
		var paramName string
		var paramType string
		for i := 0; i < int(child.NamedChildCount()); i++ {
			sub := child.NamedChild(i)
			if sub.Type() == "identifier" && paramName == "" {
				n := nodeText(sub, source)
				if n == "self" || n == "cls" {
					return "", "", false
				}
				paramName = n
			} else if sub.Type() == "type" {
				paramType = extractTypeText(sub, source)
			}
		}
		if paramName != "" {
			return paramName, paramType, true
		}

	case "typed_default_parameter":
		nameNode := child.ChildByFieldName("name")
		if nameNode == nil {
			return "", "", false
		}
		n := nodeText(nameNode, source)
		if n == "self" || n == "cls" {
			return "", "", false
		}
		var paramType string
		for i := 0; i < int(child.NamedChildCount()); i++ {
			sub := child.NamedChild(i)
			if sub.Type() == "type" {
				paramType = extractTypeText(sub, source)
				break
			}
		}
		return n, paramType, true

	case "list_splat_pattern":
		for i := 0; i < int(child.NamedChildCount()); i++ {
			sub := child.NamedChild(i)
			if sub.Type() == "identifier" {
				return "*" + nodeText(sub, source), "", true
			}
		}

	case "dictionary_splat_pattern":
		for i := 0; i < int(child.NamedChildCount()); i++ {
			sub := child.NamedChild(i)
			if sub.Type() == "identifier" {
				return "**" + nodeText(sub, source), "", true
			}
		}
	}

	return "", "", false
}

func collectInterpolationIDs(node *sitter.Node, ctx *visitContext) []model.NodeId {
	var ids []model.NodeId
	for i := 0; i < int(node.NamedChildCount()); i++ {
		child := node.NamedChild(i)
		if child.Type() == "interpolation" {
			for j := 0; j < int(child.NamedChildCount()); j++ {
				inner := child.NamedChild(j)
				exprID := visitExpression(inner, ctx)
				if exprID != nil {
					ids = append(ids, *exprID)
				}
			}
		} else {
			// Recurse into concatenated_string parts
			ids = append(ids, collectInterpolationIDs(child, ctx)...)
		}
	}
	return ids
}

func collectExpressionIDs(node *sitter.Node, ctx *visitContext) []model.NodeId {
	if node.Type() == "tuple" || node.Type() == "list" {
		var ids []model.NodeId
		for i := 0; i < int(node.NamedChildCount()); i++ {
			child := node.NamedChild(i)
			eid := visitExpression(child, ctx)
			if eid != nil {
				ids = append(ids, *eid)
			}
		}
		return ids
	}
	eid := visitExpression(node, ctx)
	if eid != nil {
		return []model.NodeId{*eid}
	}
	return nil
}

func makeFieldAttrs(fieldName string) map[string]interface{} {
	if fieldName == "" {
		return nil
	}
	return map[string]interface{}{"field_name": fieldName}
}

// --- Call resolution helpers ------------------------------------------------

func scopeOf(node *model.CpgNode) model.NodeId {
	if node.Scope != nil {
		return *node.Scope
	}
	return ""
}

func resolveMethodViaMRO(
	className, methodName string,
	methodIndex map[[2]string]*model.CpgNode,
	classNodes map[string]*model.CpgNode,
) *model.CpgNode {
	visited := make(map[string]bool)
	queue := []string{className}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if visited[current] {
			continue
		}
		visited[current] = true

		if fn, ok := methodIndex[[2]string{current, methodName}]; ok {
			return fn
		}

		node := classNodes[current]
		if node != nil {
			bases := attrStringSlice(node.Attrs, "bases")
			queue = append(queue, bases...)
		}
	}
	return nil
}

func resolveSingleCall(
	callNode *model.CpgNode,
	name string,
	functions map[string][]*model.CpgNode,
	cpg *graph.CodePropertyGraph,
) *model.CpgNode {
	candidates := functions[name]
	if len(candidates) == 0 {
		return nil
	}
	if len(candidates) == 1 {
		return candidates[0]
	}

	// Multiple candidates -- disambiguate via scope
	callTarget := callNode.Name
	if strings.Contains(callTarget, ".") {
		qualifier := callTarget[:strings.LastIndex(callTarget, ".")]
		for _, fn := range candidates {
			scope := cpg.Node(scopeOf(fn))
			if scope != nil && scope.Name == qualifier {
				return fn
			}
		}
		// Walk up the scope chain
		for _, fn := range candidates {
			scope := cpg.Node(scopeOf(fn))
			for scope != nil {
				if scope.Name == qualifier {
					return fn
				}
				scope = cpg.Node(scopeOf(scope))
			}
		}
	}

	return candidates[0]
}

// attrStringSlice extracts a []string from a node's attrs map.
// Handles both []string and []interface{} (from JSON deserialization).
func attrStringSlice(attrs map[string]interface{}, key string) []string {
	raw, ok := attrs[key]
	if !ok {
		return nil
	}
	switch v := raw.(type) {
	case []string:
		return v
	case []interface{}:
		result := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				result = append(result, s)
			}
		}
		return result
	}
	return nil
}

// attrStringMap extracts a map[string]string from a node's attrs map.
// Handles both map[string]string and map[string]interface{} (from JSON deserialization).
func attrStringMap(attrs map[string]interface{}, key string) map[string]string {
	raw, ok := attrs[key]
	if !ok {
		return nil
	}
	switch v := raw.(type) {
	case map[string]string:
		return v
	case map[string]interface{}:
		result := make(map[string]string, len(v))
		for k, val := range v {
			if s, ok := val.(string); ok {
				result[k] = s
			}
		}
		return result
	}
	return nil
}
