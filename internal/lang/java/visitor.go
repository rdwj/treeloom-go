// Package java implements the Java language visitor for treeloom CPG construction.
//
// It walks Java tree-sitter parse trees and emits CPG nodes/edges via the
// NodeEmitter interface, producing AST structure, intra-procedural data flow,
// and call nodes that the builder later resolves.
package java

import (
	"context"
	"fmt"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/java"

	"github.com/rdwj/treeloom-go/internal/graph"
	"github.com/rdwj/treeloom-go/internal/model"
)

// literalTypes maps tree-sitter node types to treeloom literal type labels.
var literalTypes = map[string]string{
	"decimal_integer_literal":        "int",
	"hex_integer_literal":            "int",
	"octal_integer_literal":          "int",
	"binary_integer_literal":         "int",
	"decimal_floating_point_literal": "float",
	"hex_floating_point_literal":     "float",
	"string_literal":                 "str",
	"character_literal":              "str",
	"true":                           "bool",
	"false":                          "bool",
	"null_literal":                   "none",
}

// classTypeNodes are tree-sitter node types that represent class/interface type references.
var classTypeNodes = map[string]bool{
	"type_identifier":        true,
	"generic_type":           true,
	"scoped_type_identifier": true,
}

// nodeHandlers maps tree-sitter node types to visit methods.
var nodeHandlers map[string]func(*Visitor, *sitter.Node, *visitContext)

func init() {
	nodeHandlers = map[string]func(*Visitor, *sitter.Node, *visitContext){
		"class_declaration":            (*Visitor).visitClassLike,
		"interface_declaration":        (*Visitor).visitClassLike,
		"enum_declaration":             (*Visitor).visitClassLike,
		"record_declaration":           (*Visitor).visitRecordDeclaration,
		"method_declaration":           (*Visitor).visitMethodDeclaration,
		"constructor_declaration":      (*Visitor).visitConstructorDeclaration,
		"local_variable_declaration":   (*Visitor).visitLocalVariableDeclaration,
		"field_declaration":            (*Visitor).visitFieldDeclaration,
		"expression_statement":         (*Visitor).visitExpressionStatement,
		"return_statement":             (*Visitor).visitReturnStatement,
		"import_declaration":           (*Visitor).visitImportDeclaration,
		"if_statement":                 (*Visitor).visitIfStatement,
		"for_statement":                (*Visitor).visitForStatement,
		"enhanced_for_statement":       (*Visitor).visitEnhancedForStatement,
		"while_statement":              (*Visitor).visitWhileStatement,
		"do_statement":                 (*Visitor).visitDoStatement,
		"switch_expression":            (*Visitor).visitSwitchExpression,
		"try_statement":                (*Visitor).visitTryStatement,
		"try_with_resources_statement": (*Visitor).visitTryWithResourcesStatement,
		"throw_statement":              (*Visitor).visitThrowStatement,
		"static_initializer":           (*Visitor).visitStaticInitializer,
		"synchronized_statement":       (*Visitor).visitSynchronizedStatement,
	}
}

// scopeStack implements LEGB-style variable scope chaining.
// Variable definitions write to the innermost scope; lookups walk from inner
// to outer.
type scopeStack struct {
	stack []map[string]model.NodeId
}

func newScopeStack() *scopeStack {
	return &scopeStack{stack: []map[string]model.NodeId{{}}}
}

func (s *scopeStack) push() {
	s.stack = append(s.stack, map[string]model.NodeId{})
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

// visitContext holds mutable state carried through the tree walk.
type visitContext struct {
	emitter     graph.NodeEmitter
	filePath    string
	source      []byte
	scopeStack  []model.NodeId
	definedVars *scopeStack
	varTypes    map[string]string
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

// Visitor implements graph.LanguageVisitor for Java source files.
type Visitor struct{}

// Name returns "java".
func (v *Visitor) Name() string { return "java" }

// Extensions returns the file extensions handled by this visitor.
func (v *Visitor) Extensions() []string { return []string{".java"} }

// parsedTree bundles the tree-sitter tree with the source bytes so Visit
// can access both without requiring the caller to pass source separately.
type parsedTree struct {
	tree   *sitter.Tree
	source []byte
}

// Parse parses Java source bytes and returns the tree-sitter Tree bundled
// with the source bytes.
func (v *Visitor) Parse(source []byte, filename string) (interface{}, error) {
	parser := sitter.NewParser()
	parser.SetLanguage(java.GetLanguage())
	tree, err := parser.ParseCtx(context.Background(), nil, source)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", filename, err)
	}
	root := tree.RootNode()
	if root.HasError() {
		return nil, fmt.Errorf("parse errors in %s", filename)
	}
	return &parsedTree{tree: tree, source: source}, nil
}

// Visit walks a parsed Java tree and emits CPG nodes/edges via the emitter.
func (v *Visitor) Visit(tree interface{}, filePath string, emitter graph.NodeEmitter) {
	pt, ok := tree.(*parsedTree)
	if !ok {
		return
	}
	root := pt.tree.RootNode()
	source := pt.source

	moduleEnd := endLocation(root, filePath)
	moduleID := emitter.EmitModule(
		fileBaseName(filePath),
		filePath,
		moduleEnd,
		"",
	)

	ctx := &visitContext{
		emitter:     emitter,
		filePath:    filePath,
		source:      source,
		scopeStack:  []model.NodeId{moduleID},
		definedVars: newScopeStack(),
		varTypes:    make(map[string]string),
	}

	childCount := int(root.ChildCount())
	for i := 0; i < childCount; i++ {
		child := root.Child(i)
		if child != nil {
			v.visitNode(child, ctx)
		}
	}
}

// ResolveCalls links CALL nodes to FUNCTION definitions using type-based MRO
// resolution and name-based fallback.
func (v *Visitor) ResolveCalls(
	cpg *graph.CodePropertyGraph,
	functionNodes, callNodes []*model.CpgNode,
) []graph.CallResolution {
	// Build functions dict: name -> list of candidates
	functions := make(map[string][]*model.CpgNode)
	for _, fn := range functionNodes {
		functions[fn.Name] = append(functions[fn.Name], fn)
	}

	// Build class hierarchy and method index
	classNodes := make(map[string]*model.CpgNode)
	for _, n := range cpg.Nodes(model.NodeClass) {
		classNodes[n.Name] = n
	}

	// Method index: (className, methodName) -> function node
	methodIndex := make(map[struct{ className, methodName string }]*model.CpgNode)
	for _, fn := range functionNodes {
		scope := scopeOf(cpg, fn)
		if scope != nil && scope.Kind == model.NodeClass {
			methodIndex[struct{ className, methodName string }{scope.Name, fn.Name}] = fn
		}
	}

	// Build import map: localName -> (module, originalName)
	type importInfo struct {
		module, name string
	}
	importMap := make(map[string]importInfo)
	for _, imp := range cpg.Nodes(model.NodeImport) {
		isFrom, _ := imp.Attrs["is_from"].(bool)
		if !isFrom {
			continue
		}
		module, _ := imp.Attrs["module"].(string)
		names, _ := imp.Attrs["names"]
		if namesList, ok := names.([]interface{}); ok {
			for _, n := range namesList {
				if s, ok := n.(string); ok {
					importMap[s] = importInfo{module, s}
				}
			}
		}
		if namesList, ok := names.([]string); ok {
			for _, s := range namesList {
				importMap[s] = importInfo{module, s}
			}
		}
	}

	var resolved []graph.CallResolution
	for _, callNode := range callNodes {
		target := callNode.Name
		var fn *model.CpgNode

		// Try type-based resolution for method calls
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
			if info, ok := importMap[target]; ok {
				candidates := functions[info.name]
				for _, candidate := range candidates {
					scope := scopeOf(cpg, candidate)
					if scope != nil && scope.Kind == model.NodeModule {
						parts := strings.Split(info.module, ".")
						if scope.Name == info.module || (len(parts) > 0 && scope.Name == parts[len(parts)-1]) {
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

// resolveMethodViaMRO walks the class hierarchy (left-to-right BFS) to find
// a matching method.
func resolveMethodViaMRO(
	className, methodName string,
	methodIndex map[struct{ className, methodName string }]*model.CpgNode,
	classNodes map[string]*model.CpgNode,
) *model.CpgNode {
	type methodKey = struct{ className, methodName string }
	visited := make(map[string]bool)
	queue := []string{className}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if visited[current] {
			continue
		}
		visited[current] = true

		if fn, ok := methodIndex[methodKey{current, methodName}]; ok {
			return fn
		}

		node := classNodes[current]
		if node == nil {
			continue
		}
		if bases, ok := node.Attrs["bases"]; ok {
			switch b := bases.(type) {
			case []string:
				queue = append(queue, b...)
			case []interface{}:
				for _, item := range b {
					if s, ok := item.(string); ok {
						queue = append(queue, s)
					}
				}
			}
		}
	}
	return nil
}

// resolveSingleCall picks the best function candidate for a given call name.
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
	// Disambiguate by scope for qualified calls
	if strings.Contains(callNode.Name, ".") {
		qualifier := callNode.Name[:strings.LastIndex(callNode.Name, ".")]
		for _, fn := range candidates {
			scope := scopeOf(cpg, fn)
			if scope != nil && scope.Name == qualifier {
				return fn
			}
		}
	}
	return candidates[0]
}

// scopeOf returns the scope node for a given node, or nil.
func scopeOf(cpg *graph.CodePropertyGraph, node *model.CpgNode) *model.CpgNode {
	if node.Scope == nil {
		return nil
	}
	return cpg.Node(*node.Scope)
}

// ---------- Visit dispatch ----------

func (v *Visitor) visitNode(node *sitter.Node, ctx *visitContext) {
	handler, ok := nodeHandlers[node.Type()]
	if ok {
		handler(v, node, ctx)
	} else {
		childCount := int(node.ChildCount())
		for i := 0; i < childCount; i++ {
			child := node.Child(i)
			if child != nil {
				v.visitNode(child, ctx)
			}
		}
	}
}

// ---------- Declaration handlers ----------

func (v *Visitor) visitClassLike(node *sitter.Node, ctx *visitContext) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, ctx.source)

	// Extract base classes from extends/implements clauses
	var bases []string
	childCount := int(node.ChildCount())
	for i := 0; i < childCount; i++ {
		child := node.Child(i)
		if child == nil {
			continue
		}
		switch child.Type() {
		case "superclass", "super_interfaces":
			subCount := int(child.ChildCount())
			for j := 0; j < subCount; j++ {
				sub := child.Child(j)
				if sub == nil {
					continue
				}
				if classTypeNodes[sub.Type()] {
					bases = append(bases, nodeText(sub, ctx.source))
				} else if sub.Type() == "type_list" {
					tlCount := int(sub.ChildCount())
					for k := 0; k < tlCount; k++ {
						t := sub.Child(k)
						if t != nil && classTypeNodes[t.Type()] {
							bases = append(bases, nodeText(t, ctx.source))
						}
					}
				}
			}
		case "extends_interfaces":
			subCount := int(child.ChildCount())
			for j := 0; j < subCount; j++ {
				sub := child.Child(j)
				if sub != nil && sub.Type() == "type_list" {
					tlCount := int(sub.ChildCount())
					for k := 0; k < tlCount; k++ {
						t := sub.Child(k)
						if t != nil && classTypeNodes[t.Type()] {
							bases = append(bases, nodeText(t, ctx.source))
						}
					}
				}
			}
		}
	}

	classID := ctx.emitter.EmitClass(
		name,
		location(node, ctx.filePath),
		ctx.currentScope(),
		bases,
		endLocation(node, ctx.filePath),
		nodeText(node, ctx.source),
	)
	ctx.pushScope(classID)
	ctx.definedVars.push()

	body := node.ChildByFieldName("body")
	if body != nil {
		bodyCount := int(body.ChildCount())
		for i := 0; i < bodyCount; i++ {
			child := body.Child(i)
			if child != nil {
				v.visitNode(child, ctx)
			}
		}
	}

	ctx.definedVars.pop()
	ctx.popScope()
}

func (v *Visitor) visitRecordDeclaration(node *sitter.Node, ctx *visitContext) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, ctx.source)

	classID := ctx.emitter.EmitClass(
		name,
		location(node, ctx.filePath),
		ctx.currentScope(),
		nil,
		endLocation(node, ctx.filePath),
		nodeText(node, ctx.source),
	)
	ctx.pushScope(classID)
	ctx.definedVars.push()

	// Emit record components as parameters scoped to the class
	paramsNode := node.ChildByFieldName("parameters")
	if paramsNode != nil {
		v.emitTypedParams(paramsNode, classID, ctx)
	}

	body := node.ChildByFieldName("body")
	if body != nil {
		bodyCount := int(body.ChildCount())
		for i := 0; i < bodyCount; i++ {
			child := body.Child(i)
			if child != nil {
				v.visitNode(child, ctx)
			}
		}
	}

	ctx.definedVars.pop()
	ctx.popScope()
}

func (v *Visitor) visitMethodDeclaration(node *sitter.Node, ctx *visitContext) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, ctx.source)

	funcID := ctx.emitter.EmitFunction(
		name,
		location(node, ctx.filePath),
		ctx.currentScope(),
		false,
		nil,
		endLocation(node, ctx.filePath),
		nodeText(node, ctx.source),
	)
	ctx.pushScope(funcID)
	ctx.definedVars.push()

	paramsNode := node.ChildByFieldName("parameters")
	if paramsNode != nil {
		v.emitTypedParams(paramsNode, funcID, ctx)
	}

	body := node.ChildByFieldName("body")
	if body != nil {
		bodyCount := int(body.ChildCount())
		for i := 0; i < bodyCount; i++ {
			child := body.Child(i)
			if child != nil {
				v.visitNode(child, ctx)
			}
		}
	}

	ctx.definedVars.pop()
	ctx.popScope()
}

func (v *Visitor) visitConstructorDeclaration(node *sitter.Node, ctx *visitContext) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, ctx.source)

	funcID := ctx.emitter.EmitFunction(
		name,
		location(node, ctx.filePath),
		ctx.currentScope(),
		false,
		nil,
		endLocation(node, ctx.filePath),
		nodeText(node, ctx.source),
	)
	ctx.pushScope(funcID)
	ctx.definedVars.push()

	paramsNode := node.ChildByFieldName("parameters")
	if paramsNode != nil {
		v.emitTypedParams(paramsNode, funcID, ctx)
	}

	body := node.ChildByFieldName("body")
	if body != nil {
		bodyCount := int(body.ChildCount())
		for i := 0; i < bodyCount; i++ {
			child := body.Child(i)
			if child != nil {
				v.visitNode(child, ctx)
			}
		}
	}

	ctx.definedVars.pop()
	ctx.popScope()
}

func (v *Visitor) emitTypedParams(paramsNode *sitter.Node, funcID model.NodeId, ctx *visitContext) {
	pos := 0
	childCount := int(paramsNode.ChildCount())
	for i := 0; i < childCount; i++ {
		child := paramsNode.Child(i)
		if child == nil {
			continue
		}

		switch child.Type() {
		case "formal_parameter":
			nameNode := child.ChildByFieldName("name")
			typeNode := child.ChildByFieldName("type")
			if nameNode == nil {
				continue
			}
			paramName := nodeText(nameNode, ctx.source)
			var typeAnn string
			if typeNode != nil {
				typeAnn = nodeText(typeNode, ctx.source)
			}

			paramID := ctx.emitter.EmitParameter(
				paramName,
				location(nameNode, ctx.filePath),
				funcID,
				typeAnn,
				pos,
				endLocation(child, ctx.filePath),
				"",
			)
			ctx.definedVars.define(paramName, paramID)
			pos++

		case "spread_parameter":
			// Structure: type_identifier, "...", variable_declarator(name=identifier)
			var decl *sitter.Node
			var typeText string
			scCount := int(child.ChildCount())
			for j := 0; j < scCount; j++ {
				sc := child.Child(j)
				if sc == nil {
					continue
				}
				if classTypeNodes[sc.Type()] {
					typeText = nodeText(sc, ctx.source)
				} else if sc.Type() == "variable_declarator" {
					decl = sc
				}
			}
			if decl != nil {
				vnameNode := decl.ChildByFieldName("name")
				if vnameNode != nil {
					paramName := nodeText(vnameNode, ctx.source)
					var typeAnn string
					if typeText != "" {
						typeAnn = typeText + "..."
					}
					paramID := ctx.emitter.EmitParameter(
						paramName,
						location(vnameNode, ctx.filePath),
						funcID,
						typeAnn,
						pos,
						endLocation(child, ctx.filePath),
						"",
					)
					ctx.definedVars.define(paramName, paramID)
				}
			}
			pos++
		}
	}
}

// ---------- Variable and assignment handlers ----------

func (v *Visitor) visitLocalVariableDeclaration(node *sitter.Node, ctx *visitContext) {
	var typeAnn string
	childCount := int(node.ChildCount())
	for i := 0; i < childCount; i++ {
		child := node.Child(i)
		if child == nil {
			continue
		}
		if child.Type() == "variable_declarator" || child.Type() == ";" {
			continue
		}
		if child.IsNamed() {
			typeAnn = nodeText(child, ctx.source)
			break
		}
	}
	for i := 0; i < childCount; i++ {
		child := node.Child(i)
		if child != nil && child.Type() == "variable_declarator" {
			v.visitVariableDeclarator(child, ctx, typeAnn)
		}
	}
}

func (v *Visitor) visitFieldDeclaration(node *sitter.Node, ctx *visitContext) {
	var typeAnn string
	childCount := int(node.ChildCount())
	for i := 0; i < childCount; i++ {
		child := node.Child(i)
		if child == nil {
			continue
		}
		if child.Type() == "variable_declarator" || child.Type() == ";" {
			continue
		}
		if child.Type() == "modifiers" {
			continue
		}
		if child.IsNamed() {
			typeAnn = nodeText(child, ctx.source)
			break
		}
	}
	for i := 0; i < childCount; i++ {
		child := node.Child(i)
		if child != nil && child.Type() == "variable_declarator" {
			v.visitVariableDeclarator(child, ctx, typeAnn)
		}
	}
}

func (v *Visitor) visitVariableDeclarator(node *sitter.Node, ctx *visitContext, typeAnn string) *model.NodeId {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return nil
	}
	varName := nodeText(nameNode, ctx.source)
	loc := location(nameNode, ctx.filePath)

	// Infer type from declared type or constructor call on RHS
	inferredType := typeAnn
	valueNode := node.ChildByFieldName("value")
	if valueNode != nil && valueNode.Type() == "object_creation_expression" {
		valCount := int(valueNode.ChildCount())
		for i := 0; i < valCount; i++ {
			child := valueNode.Child(i)
			if child != nil && classTypeNodes[child.Type()] {
				inferredType = nodeText(child, ctx.source)
				break
			}
		}
	}

	if inferredType != "" {
		// Strip generics for type tracking: "List<String>" -> "List"
		baseType := inferredType
		if idx := strings.Index(baseType, "<"); idx >= 0 {
			baseType = baseType[:idx]
		}
		if idx := strings.Index(baseType, "["); idx >= 0 {
			baseType = baseType[:idx]
		}
		ctx.varTypes[varName] = baseType
	}

	varID := ctx.emitter.EmitVariable(
		varName,
		loc,
		ctx.currentScope(),
		inferredType,
		endLocation(nameNode, ctx.filePath),
		"",
	)
	ctx.definedVars.define(varName, varID)

	if valueNode != nil {
		rhsID := v.visitExpression(valueNode, ctx)
		if rhsID != nil {
			ctx.emitter.EmitDefinition(varID, *rhsID)
			ctx.emitter.EmitDataFlow(*rhsID, varID, nil)
		}
	}

	return &varID
}

func (v *Visitor) visitAssignmentExpression(node *sitter.Node, ctx *visitContext) {
	left := node.ChildByFieldName("left")
	right := node.ChildByFieldName("right")
	if left == nil {
		return
	}
	varName := nodeText(left, ctx.source)
	loc := location(left, ctx.filePath)

	varID := ctx.emitter.EmitVariable(
		varName,
		loc,
		ctx.currentScope(),
		"",
		endLocation(left, ctx.filePath),
		"",
	)
	ctx.definedVars.define(varName, varID)

	if right != nil {
		rhsID := v.visitExpression(right, ctx)
		if rhsID != nil {
			ctx.emitter.EmitDefinition(varID, *rhsID)
			ctx.emitter.EmitDataFlow(*rhsID, varID, nil)
		}
	}
}

// ---------- Statement handlers ----------

func (v *Visitor) visitExpressionStatement(node *sitter.Node, ctx *visitContext) {
	childCount := int(node.ChildCount())
	for i := 0; i < childCount; i++ {
		child := node.Child(i)
		if child == nil {
			continue
		}
		if child.Type() == "assignment_expression" {
			v.visitAssignmentExpression(child, ctx)
		} else if child.Type() != ";" {
			v.visitExpression(child, ctx)
		}
	}
}

func (v *Visitor) visitReturnStatement(node *sitter.Node, ctx *visitContext) {
	retID := ctx.emitter.EmitReturn(
		location(node, ctx.filePath),
		ctx.currentScope(),
		endLocation(node, ctx.filePath),
		"",
	)

	childCount := int(node.ChildCount())
	for i := 0; i < childCount; i++ {
		child := node.Child(i)
		if child == nil {
			continue
		}
		if child.Type() == "return" || child.Type() == ";" {
			continue
		}
		exprID := v.visitExpression(child, ctx)
		if exprID != nil {
			ctx.emitter.EmitDataFlow(*exprID, retID, nil)
			if child.Type() == "identifier" {
				ctx.emitter.EmitUsage(*exprID, retID)
			}
		}
	}
}

func (v *Visitor) visitImportDeclaration(node *sitter.Node, ctx *visitContext) {
	var fullName string
	isWildcard := false
	childCount := int(node.ChildCount())
	for i := 0; i < childCount; i++ {
		child := node.Child(i)
		if child == nil {
			continue
		}
		switch child.Type() {
		case "import", ";", "static":
			continue
		case "scoped_identifier", "identifier":
			fullName = nodeText(child, ctx.source)
		case "asterisk":
			isWildcard = true
		}
	}
	if fullName == "" {
		return
	}

	var moduleName, importedName string
	if idx := strings.LastIndex(fullName, "."); idx >= 0 {
		moduleName = fullName[:idx]
		if isWildcard {
			importedName = "*"
		} else {
			importedName = fullName[idx+1:]
		}
	} else {
		moduleName = fullName
		importedName = fullName
	}

	ctx.emitter.EmitImport(
		moduleName,
		[]string{importedName},
		location(node, ctx.filePath),
		ctx.currentScope(),
		true, // Java imports are always "from" style
		nil,
		endLocation(node, ctx.filePath),
		"",
	)
}

func (v *Visitor) visitIfStatement(node *sitter.Node, ctx *visitContext) {
	hasElse := node.ChildByFieldName("alternative") != nil
	branchID := ctx.emitter.EmitBranchNode(
		"if",
		location(node, ctx.filePath),
		ctx.currentScope(),
		hasElse,
		endLocation(node, ctx.filePath),
		"",
	)

	condition := node.ChildByFieldName("condition")
	if condition != nil {
		v.visitExpression(condition, ctx)
	}

	consequence := node.ChildByFieldName("consequence")
	if consequence != nil {
		ctx.pushScope(branchID)
		consCount := int(consequence.ChildCount())
		for i := 0; i < consCount; i++ {
			child := consequence.Child(i)
			if child != nil {
				v.visitNode(child, ctx)
			}
		}
		ctx.popScope()
	}

	alternative := node.ChildByFieldName("alternative")
	if alternative != nil {
		ctx.pushScope(branchID)
		if alternative.Type() == "if_statement" {
			v.visitIfStatement(alternative, ctx)
		} else {
			altCount := int(alternative.ChildCount())
			for i := 0; i < altCount; i++ {
				child := alternative.Child(i)
				if child != nil {
					v.visitNode(child, ctx)
				}
			}
		}
		ctx.popScope()
	}
}

func (v *Visitor) visitForStatement(node *sitter.Node, ctx *visitContext) {
	loopID := ctx.emitter.EmitLoopNode(
		"for",
		location(node, ctx.filePath),
		ctx.currentScope(),
		"",
		endLocation(node, ctx.filePath),
		"",
	)
	ctx.pushScope(loopID)

	initNode := node.ChildByFieldName("init")
	if initNode != nil {
		v.visitNode(initNode, ctx)
	}

	condition := node.ChildByFieldName("condition")
	if condition != nil {
		v.visitExpression(condition, ctx)
	}

	update := node.ChildByFieldName("update")
	if update != nil {
		v.visitExpression(update, ctx)
	}

	body := node.ChildByFieldName("body")
	if body != nil {
		bodyCount := int(body.ChildCount())
		for i := 0; i < bodyCount; i++ {
			child := body.Child(i)
			if child != nil {
				v.visitNode(child, ctx)
			}
		}
	}

	ctx.popScope()
}

func (v *Visitor) visitEnhancedForStatement(node *sitter.Node, ctx *visitContext) {
	nameNode := node.ChildByFieldName("name")
	var iteratorVar string
	if nameNode != nil {
		iteratorVar = nodeText(nameNode, ctx.source)
	}

	loopID := ctx.emitter.EmitLoopNode(
		"for",
		location(node, ctx.filePath),
		ctx.currentScope(),
		iteratorVar,
		endLocation(node, ctx.filePath),
		"",
	)

	if iteratorVar != "" && nameNode != nil {
		varID := ctx.emitter.EmitVariable(
			iteratorVar,
			location(nameNode, ctx.filePath),
			loopID,
			"",
			endLocation(nameNode, ctx.filePath),
			"",
		)
		ctx.definedVars.define(iteratorVar, varID)
	}

	body := node.ChildByFieldName("body")
	if body != nil {
		ctx.pushScope(loopID)
		bodyCount := int(body.ChildCount())
		for i := 0; i < bodyCount; i++ {
			child := body.Child(i)
			if child != nil {
				v.visitNode(child, ctx)
			}
		}
		ctx.popScope()
	}
}

func (v *Visitor) visitWhileStatement(node *sitter.Node, ctx *visitContext) {
	loopID := ctx.emitter.EmitLoopNode(
		"while",
		location(node, ctx.filePath),
		ctx.currentScope(),
		"",
		endLocation(node, ctx.filePath),
		"",
	)

	condition := node.ChildByFieldName("condition")
	if condition != nil {
		v.visitExpression(condition, ctx)
	}

	body := node.ChildByFieldName("body")
	if body != nil {
		ctx.pushScope(loopID)
		bodyCount := int(body.ChildCount())
		for i := 0; i < bodyCount; i++ {
			child := body.Child(i)
			if child != nil {
				v.visitNode(child, ctx)
			}
		}
		ctx.popScope()
	}
}

func (v *Visitor) visitDoStatement(node *sitter.Node, ctx *visitContext) {
	loopID := ctx.emitter.EmitLoopNode(
		"do_while",
		location(node, ctx.filePath),
		ctx.currentScope(),
		"",
		endLocation(node, ctx.filePath),
		"",
	)

	body := node.ChildByFieldName("body")
	if body != nil {
		ctx.pushScope(loopID)
		bodyCount := int(body.ChildCount())
		for i := 0; i < bodyCount; i++ {
			child := body.Child(i)
			if child != nil {
				v.visitNode(child, ctx)
			}
		}
		ctx.popScope()
	}

	condition := node.ChildByFieldName("condition")
	if condition != nil {
		v.visitExpression(condition, ctx)
	}
}

func (v *Visitor) visitSwitchExpression(node *sitter.Node, ctx *visitContext) {
	condition := node.ChildByFieldName("condition")
	if condition != nil {
		v.visitExpression(condition, ctx)
	}

	branchID := ctx.emitter.EmitBranchNode(
		"switch",
		location(node, ctx.filePath),
		ctx.currentScope(),
		false,
		endLocation(node, ctx.filePath),
		"",
	)

	body := node.ChildByFieldName("body")
	if body != nil {
		ctx.pushScope(branchID)
		bodyCount := int(body.ChildCount())
		for i := 0; i < bodyCount; i++ {
			child := body.Child(i)
			if child == nil {
				continue
			}
			switch child.Type() {
			case "switch_block_statement_group":
				stmtCount := int(child.ChildCount())
				for j := 0; j < stmtCount; j++ {
					stmt := child.Child(j)
					if stmt != nil && stmt.Type() != "switch_label" && stmt.Type() != ":" {
						v.visitNode(stmt, ctx)
					}
				}
			case "switch_rule":
				stmtCount := int(child.ChildCount())
				for j := 0; j < stmtCount; j++ {
					stmt := child.Child(j)
					if stmt != nil && stmt.Type() != "switch_label" && stmt.Type() != "->" {
						v.visitNode(stmt, ctx)
					}
				}
			default:
				v.visitNode(child, ctx)
			}
		}
		ctx.popScope()
	}
}

func (v *Visitor) visitTryStatement(node *sitter.Node, ctx *visitContext) {
	childCount := int(node.ChildCount())
	for i := 0; i < childCount; i++ {
		child := node.Child(i)
		if child == nil {
			continue
		}
		switch child.Type() {
		case "block":
			// Try body
			blockCount := int(child.ChildCount())
			for j := 0; j < blockCount; j++ {
				stmt := child.Child(j)
				if stmt != nil {
					v.visitNode(stmt, ctx)
				}
			}
		case "catch_clause":
			v.visitCatchClause(child, ctx)
		case "finally_clause":
			fcCount := int(child.ChildCount())
			for j := 0; j < fcCount; j++ {
				stmt := child.Child(j)
				if stmt != nil && stmt.Type() == "block" {
					bCount := int(stmt.ChildCount())
					for k := 0; k < bCount; k++ {
						s := stmt.Child(k)
						if s != nil {
							v.visitNode(s, ctx)
						}
					}
				}
			}
		}
	}
}

func (v *Visitor) visitTryWithResourcesStatement(node *sitter.Node, ctx *visitContext) {
	childCount := int(node.ChildCount())
	for i := 0; i < childCount; i++ {
		child := node.Child(i)
		if child == nil {
			continue
		}
		switch child.Type() {
		case "resource_specification":
			resCount := int(child.ChildCount())
			for j := 0; j < resCount; j++ {
				resource := child.Child(j)
				if resource != nil && resource.Type() == "resource" {
					v.visitResource(resource, ctx)
				}
			}
		case "block":
			blockCount := int(child.ChildCount())
			for j := 0; j < blockCount; j++ {
				stmt := child.Child(j)
				if stmt != nil {
					v.visitNode(stmt, ctx)
				}
			}
		case "catch_clause":
			v.visitCatchClause(child, ctx)
		case "finally_clause":
			fcCount := int(child.ChildCount())
			for j := 0; j < fcCount; j++ {
				stmt := child.Child(j)
				if stmt != nil && stmt.Type() == "block" {
					bCount := int(stmt.ChildCount())
					for k := 0; k < bCount; k++ {
						s := stmt.Child(k)
						if s != nil {
							v.visitNode(s, ctx)
						}
					}
				}
			}
		}
	}
}

func (v *Visitor) visitCatchClause(node *sitter.Node, ctx *visitContext) {
	childCount := int(node.ChildCount())
	for i := 0; i < childCount; i++ {
		child := node.Child(i)
		if child == nil {
			continue
		}
		switch child.Type() {
		case "catch_formal_parameter":
			nameNode := child.ChildByFieldName("name")
			if nameNode != nil {
				varName := nodeText(nameNode, ctx.source)
				varID := ctx.emitter.EmitVariable(
					varName,
					location(nameNode, ctx.filePath),
					ctx.currentScope(),
					"",
					endLocation(nameNode, ctx.filePath),
					"",
				)
				ctx.definedVars.define(varName, varID)
			}
		case "block":
			blockCount := int(child.ChildCount())
			for j := 0; j < blockCount; j++ {
				stmt := child.Child(j)
				if stmt != nil {
					v.visitNode(stmt, ctx)
				}
			}
		}
	}
}

func (v *Visitor) visitResource(node *sitter.Node, ctx *visitContext) {
	nameNode := node.ChildByFieldName("name")
	valueNode := node.ChildByFieldName("value")
	if nameNode == nil {
		return
	}
	varName := nodeText(nameNode, ctx.source)
	varID := ctx.emitter.EmitVariable(
		varName,
		location(nameNode, ctx.filePath),
		ctx.currentScope(),
		"",
		endLocation(nameNode, ctx.filePath),
		"",
	)
	ctx.definedVars.define(varName, varID)

	if valueNode != nil {
		rhsID := v.visitExpression(valueNode, ctx)
		if rhsID != nil {
			ctx.emitter.EmitDefinition(varID, *rhsID)
			ctx.emitter.EmitDataFlow(*rhsID, varID, nil)
		}
	}
}

func (v *Visitor) visitThrowStatement(node *sitter.Node, ctx *visitContext) {
	childCount := int(node.ChildCount())
	for i := 0; i < childCount; i++ {
		child := node.Child(i)
		if child == nil {
			continue
		}
		if child.Type() == "throw" || child.Type() == ";" {
			continue
		}
		if child.IsNamed() {
			v.visitExpression(child, ctx)
		}
	}
}

func (v *Visitor) visitStaticInitializer(node *sitter.Node, ctx *visitContext) {
	childCount := int(node.ChildCount())
	for i := 0; i < childCount; i++ {
		child := node.Child(i)
		if child != nil && child.Type() == "block" {
			blockCount := int(child.ChildCount())
			for j := 0; j < blockCount; j++ {
				stmt := child.Child(j)
				if stmt != nil {
					v.visitNode(stmt, ctx)
				}
			}
		}
	}
}

func (v *Visitor) visitSynchronizedStatement(node *sitter.Node, ctx *visitContext) {
	childCount := int(node.ChildCount())
	for i := 0; i < childCount; i++ {
		child := node.Child(i)
		if child == nil {
			continue
		}
		switch child.Type() {
		case "parenthesized_expression":
			v.visitExpression(child, ctx)
		case "block":
			blockCount := int(child.ChildCount())
			for j := 0; j < blockCount; j++ {
				stmt := child.Child(j)
				if stmt != nil {
					v.visitNode(stmt, ctx)
				}
			}
		}
	}
}

// ---------- Expression visitor ----------

func (v *Visitor) visitExpression(node *sitter.Node, ctx *visitContext) *model.NodeId {
	if node == nil {
		return nil
	}

	switch node.Type() {
	case "method_invocation":
		return v.visitMethodCall(node, ctx)

	case "object_creation_expression":
		return v.visitObjectCreation(node, ctx)

	case "identifier":
		name := nodeText(node, ctx.source)
		if id, ok := ctx.definedVars.lookup(name); ok {
			return &id
		}
		return nil

	case "assignment_expression":
		v.visitAssignmentExpression(node, ctx)
		return nil

	case "binary_expression":
		return v.visitBinaryExpression(node, ctx)

	case "parenthesized_expression":
		childCount := int(node.ChildCount())
		for i := 0; i < childCount; i++ {
			child := node.Child(i)
			if child != nil && child.IsNamed() {
				return v.visitExpression(child, ctx)
			}
		}
		return nil

	case "lambda_expression":
		return v.visitLambda(node, ctx)

	case "array_creation_expression", "array_initializer":
		return v.visitArrayExpression(node, ctx)

	case "cast_expression":
		childCount := int(node.ChildCount())
		for i := 0; i < childCount; i++ {
			child := node.Child(i)
			if child == nil || !child.IsNamed() {
				continue
			}
			switch child.Type() {
			case "type_identifier", "generic_type", "integral_type",
				"floating_point_type", "boolean_type", "void_type":
				continue
			default:
				return v.visitExpression(child, ctx)
			}
		}
		return nil

	case "update_expression":
		childCount := int(node.ChildCount())
		for i := 0; i < childCount; i++ {
			child := node.Child(i)
			if child != nil && child.Type() == "identifier" {
				name := nodeText(child, ctx.source)
				if id, ok := ctx.definedVars.lookup(name); ok {
					return &id
				}
			}
		}
		return nil

	case "array_access":
		namedCount := int(node.NamedChildCount())
		if namedCount > 0 {
			first := node.NamedChild(0)
			return v.visitExpression(first, ctx)
		}
		return nil

	case "ternary_expression":
		return v.visitTernaryExpression(node, ctx)

	case "method_reference":
		return v.visitMethodReference(node, ctx)

	case "field_access":
		return v.visitFieldAccess(node, ctx)

	case "unary_expression":
		operand := node.ChildByFieldName("operand")
		if operand != nil {
			return v.visitExpression(operand, ctx)
		}
		return nil

	case "instanceof_expression":
		left := node.ChildByFieldName("left")
		if left != nil {
			return v.visitExpression(left, ctx)
		}
		return nil

	default:
		// Check if it's a literal type
		if litType, ok := literalTypes[node.Type()]; ok {
			id := ctx.emitter.EmitLiteral(
				nodeText(node, ctx.source),
				litType,
				location(node, ctx.filePath),
				ctx.currentScope(),
				endLocation(node, ctx.filePath),
				"",
			)
			return &id
		}

		// Default: recurse into named children
		childCount := int(node.ChildCount())
		for i := 0; i < childCount; i++ {
			child := node.Child(i)
			if child != nil && child.IsNamed() {
				v.visitExpression(child, ctx)
			}
		}
		return nil
	}
}

func (v *Visitor) visitBinaryExpression(node *sitter.Node, ctx *visitContext) *model.NodeId {
	left := node.ChildByFieldName("left")
	right := node.ChildByFieldName("right")

	// Find operator (the unnamed child between operands)
	var op string
	childCount := int(node.ChildCount())
	for i := 0; i < childCount; i++ {
		child := node.Child(i)
		if child != nil && !child.IsNamed() {
			op = child.Type()
			break
		}
	}

	var leftID, rightID *model.NodeId
	if left != nil {
		leftID = v.visitExpression(left, ctx)
	}
	if right != nil {
		rightID = v.visitExpression(right, ctx)
	}

	if op == "+" {
		concatID := ctx.emitter.EmitCall(
			"<string_concat>",
			location(node, ctx.filePath),
			ctx.currentScope(),
			nil,
			"",
			endLocation(node, ctx.filePath),
			"",
		)
		if leftID != nil {
			ctx.emitter.EmitDataFlow(*leftID, concatID, nil)
		}
		if rightID != nil {
			ctx.emitter.EmitDataFlow(*rightID, concatID, nil)
		}
		return &concatID
	}

	return nil
}

func (v *Visitor) visitTernaryExpression(node *sitter.Node, ctx *visitContext) *model.NodeId {
	condition := node.ChildByFieldName("condition")
	consequence := node.ChildByFieldName("consequence")
	alternative := node.ChildByFieldName("alternative")

	if condition != nil {
		v.visitExpression(condition, ctx)
	}

	var consID, altID *model.NodeId
	if consequence != nil {
		consID = v.visitExpression(consequence, ctx)
	}
	if alternative != nil {
		altID = v.visitExpression(alternative, ctx)
	}

	if consID != nil || altID != nil {
		mergeID := ctx.emitter.EmitCall(
			"<ternary>",
			location(node, ctx.filePath),
			ctx.currentScope(),
			nil,
			"",
			endLocation(node, ctx.filePath),
			"",
		)
		if consID != nil {
			ctx.emitter.EmitDataFlow(*consID, mergeID, nil)
		}
		if altID != nil {
			ctx.emitter.EmitDataFlow(*altID, mergeID, nil)
		}
		return &mergeID
	}
	return nil
}

func (v *Visitor) visitMethodCall(node *sitter.Node, ctx *visitContext) *model.NodeId {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return nil
	}
	methodName := nodeText(nameNode, ctx.source)

	objNode := node.ChildByFieldName("object")
	targetName := methodName
	if objNode != nil {
		targetName = nodeText(objNode, ctx.source) + "." + methodName
	}

	// Infer receiver type for type-based resolution
	var receiverType string
	if objNode != nil {
		objText := nodeText(objNode, ctx.source)
		if t, ok := ctx.varTypes[objText]; ok {
			receiverType = t
		}
	}

	argsNode := node.ChildByFieldName("arguments")
	argTexts, argIDs, argIsVar := v.collectArgs(argsNode, ctx)

	callID := ctx.emitter.EmitCall(
		targetName,
		location(node, ctx.filePath),
		ctx.currentScope(),
		argTexts,
		receiverType,
		endLocation(node, ctx.filePath),
		"",
	)
	v.wireArgs(argIDs, argIsVar, callID, ctx)

	// Wire receiver object into call for taint propagation
	if objNode != nil {
		receiverID := v.visitExpression(objNode, ctx)
		if receiverID != nil {
			ctx.emitter.EmitDataFlow(*receiverID, callID, nil)
		}
	}

	return &callID
}

func (v *Visitor) visitObjectCreation(node *sitter.Node, ctx *visitContext) *model.NodeId {
	typeName := "Object"
	childCount := int(node.ChildCount())
	for i := 0; i < childCount; i++ {
		child := node.Child(i)
		if child != nil && classTypeNodes[child.Type()] {
			typeName = nodeText(child, ctx.source)
			break
		}
	}

	argsNode := node.ChildByFieldName("arguments")
	argTexts, argIDs, argIsVar := v.collectArgs(argsNode, ctx)

	callID := ctx.emitter.EmitCall(
		"new "+typeName,
		location(node, ctx.filePath),
		ctx.currentScope(),
		argTexts,
		"",
		endLocation(node, ctx.filePath),
		"",
	)
	v.wireArgs(argIDs, argIsVar, callID, ctx)
	return &callID
}

func (v *Visitor) visitMethodReference(node *sitter.Node, ctx *visitContext) *model.NodeId {
	var parts []string
	childCount := int(node.ChildCount())
	for i := 0; i < childCount; i++ {
		child := node.Child(i)
		if child == nil {
			continue
		}
		if child.Type() == "::" {
			continue
		}
		if child.IsNamed() {
			parts = append(parts, nodeText(child, ctx.source))
		}
	}
	targetName := nodeText(node, ctx.source)
	if len(parts) > 0 {
		targetName = strings.Join(parts, ".")
	}

	callID := ctx.emitter.EmitCall(
		targetName,
		location(node, ctx.filePath),
		ctx.currentScope(),
		nil,
		"",
		endLocation(node, ctx.filePath),
		"",
	)
	return &callID
}

func (v *Visitor) visitFieldAccess(node *sitter.Node, ctx *visitContext) *model.NodeId {
	objNode := node.ChildByFieldName("object")
	fieldNode := node.ChildByFieldName("field")
	if fieldNode == nil {
		return nil
	}

	var objText string
	if objNode != nil {
		objText = nodeText(objNode, ctx.source)
	}
	fieldText := nodeText(fieldNode, ctx.source)
	var dottedName string
	if objText != "" {
		dottedName = objText + "." + fieldText
	} else {
		dottedName = fieldText
	}

	if existing, ok := ctx.definedVars.lookup(dottedName); ok {
		return &existing
	}

	varID := ctx.emitter.EmitVariable(
		dottedName,
		location(node, ctx.filePath),
		ctx.currentScope(),
		"",
		endLocation(node, ctx.filePath),
		"",
	)
	ctx.definedVars.define(dottedName, varID)

	// Wire DFG from the object if it's in scope
	if objNode != nil {
		if objID, ok := ctx.definedVars.lookup(nodeText(objNode, ctx.source)); ok {
			ctx.emitter.EmitDataFlow(objID, varID, nil)
		}
	}

	return &varID
}

func (v *Visitor) visitLambda(node *sitter.Node, ctx *visitContext) *model.NodeId {
	loc := location(node, ctx.filePath)
	lambdaName := fmt.Sprintf("lambda$%d$%d", loc.Line, loc.Column)

	funcID := ctx.emitter.EmitFunction(
		lambdaName,
		loc,
		ctx.currentScope(),
		false,
		nil,
		endLocation(node, ctx.filePath),
		nodeText(node, ctx.source),
	)
	ctx.pushScope(funcID)
	ctx.definedVars.push()

	// Emit parameters
	childCount := int(node.ChildCount())
	for i := 0; i < childCount; i++ {
		child := node.Child(i)
		if child == nil {
			continue
		}
		if child.Type() == "->" {
			break
		}
		switch child.Type() {
		case "formal_parameters":
			v.emitTypedParams(child, funcID, ctx)
		case "inferred_parameters":
			pos := 0
			ipCount := int(child.ChildCount())
			for j := 0; j < ipCount; j++ {
				paramChild := child.Child(j)
				if paramChild != nil && paramChild.Type() == "identifier" {
					pname := nodeText(paramChild, ctx.source)
					pid := ctx.emitter.EmitParameter(
						pname,
						location(paramChild, ctx.filePath),
						funcID,
						"",
						pos,
						endLocation(paramChild, ctx.filePath),
						"",
					)
					ctx.definedVars.define(pname, pid)
					pos++
				}
			}
		case "identifier":
			// Single unparenthesized param: x -> ...
			pname := nodeText(child, ctx.source)
			pid := ctx.emitter.EmitParameter(
				pname,
				location(child, ctx.filePath),
				funcID,
				"",
				0,
				endLocation(child, ctx.filePath),
				"",
			)
			ctx.definedVars.define(pname, pid)
		}
	}

	// Visit body (after the -> token)
	var result *model.NodeId
	afterArrow := false
	for i := 0; i < childCount; i++ {
		child := node.Child(i)
		if child == nil {
			continue
		}
		if child.Type() == "->" {
			afterArrow = true
			continue
		}
		if afterArrow {
			if child.Type() == "block" {
				blockCount := int(child.ChildCount())
				for j := 0; j < blockCount; j++ {
					stmt := child.Child(j)
					if stmt != nil {
						v.visitNode(stmt, ctx)
					}
				}
			} else if child.IsNamed() {
				result = v.visitExpression(child, ctx)
			}
		}
	}

	ctx.definedVars.pop()
	ctx.popScope()
	return result
}

func (v *Visitor) visitArrayExpression(node *sitter.Node, ctx *visitContext) *model.NodeId {
	var lastID *model.NodeId
	target := node

	if node.Type() == "array_creation_expression" {
		childCount := int(node.ChildCount())
		for i := 0; i < childCount; i++ {
			child := node.Child(i)
			if child != nil && child.Type() == "array_initializer" {
				target = child
				break
			}
		}
	}

	targetCount := int(target.ChildCount())
	for i := 0; i < targetCount; i++ {
		child := target.Child(i)
		if child != nil && child.IsNamed() {
			result := v.visitExpression(child, ctx)
			if result != nil {
				lastID = result
			}
		}
	}
	return lastID
}

// ---------- Argument helpers ----------

func (v *Visitor) collectArgs(argsNode *sitter.Node, ctx *visitContext) ([]string, []*model.NodeId, []bool) {
	var argTexts []string
	var argIDs []*model.NodeId
	var argIsVar []bool

	if argsNode != nil {
		childCount := int(argsNode.ChildCount())
		for i := 0; i < childCount; i++ {
			child := argsNode.Child(i)
			if child == nil || !child.IsNamed() {
				continue
			}
			argTexts = append(argTexts, nodeText(child, ctx.source))
			exprID := v.visitExpression(child, ctx)
			argIDs = append(argIDs, exprID)
			argIsVar = append(argIsVar, child.Type() == "identifier")
		}
	}

	return argTexts, argIDs, argIsVar
}

func (v *Visitor) wireArgs(argIDs []*model.NodeId, argIsVar []bool, callID model.NodeId, ctx *visitContext) {
	for i, argID := range argIDs {
		if argID != nil {
			ctx.emitter.EmitDataFlow(*argID, callID, nil)
			if i < len(argIsVar) && argIsVar[i] {
				ctx.emitter.EmitUsage(*argID, callID)
			}
		}
	}
}

// ---------- Helpers ----------

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

// fileBaseName extracts a stem-like name from a file path for module names.
// "com/example/Foo.java" -> "Foo"
func fileBaseName(filePath string) string {
	// Find last path separator
	name := filePath
	if idx := strings.LastIndex(name, "/"); idx >= 0 {
		name = name[idx+1:]
	}
	if idx := strings.LastIndex(name, "\\"); idx >= 0 {
		name = name[idx+1:]
	}
	// Strip extension
	if idx := strings.LastIndex(name, "."); idx >= 0 {
		name = name[:idx]
	}
	return name
}
