// Package graph provides the Code Property Graph container and builder.
package graph

import (
	"sort"

	"github.com/rdwj/treeloom-go/internal/model"
)

// CodePropertyGraph is the central container for a Code Property Graph.
//
// It stores AST, control flow, data flow, and call graph edges in a single
// directed multigraph. Nodes and edges are stored in insertion order to
// ensure deterministic serialization.
type CodePropertyGraph struct {
	nodes         map[string]*model.CpgNode
	nodeOrder     []string           // insertion-order node ID strings
	edges         []*model.CpgEdge   // all edges in insertion order
	scopeChildren map[string][]string // scope_id -> [child_node_ids]

	// Consumer annotations, separate from structural CpgNode.Attrs.
	annotations     map[string]map[string]interface{}   // node_id -> key -> value
	edgeAnnotations map[[2]string]map[string]interface{} // [source,target] -> key -> value
}

// NewCPG creates an empty CodePropertyGraph.
func NewCPG() *CodePropertyGraph {
	return &CodePropertyGraph{
		nodes:           make(map[string]*model.CpgNode),
		nodeOrder:       nil,
		edges:           nil,
		scopeChildren:   make(map[string][]string),
		annotations:     make(map[string]map[string]interface{}),
		edgeAnnotations: make(map[[2]string]map[string]interface{}),
	}
}

// AddNode inserts a node into the graph. If a node with the same ID already
// exists, it is silently replaced (but insertion order is preserved from the
// first add).
func (cpg *CodePropertyGraph) AddNode(node *model.CpgNode) {
	key := string(node.ID)
	if _, exists := cpg.nodes[key]; !exists {
		cpg.nodeOrder = append(cpg.nodeOrder, key)
	}
	cpg.nodes[key] = node

	if node.Scope != nil {
		scopeKey := string(*node.Scope)
		cpg.scopeChildren[scopeKey] = append(cpg.scopeChildren[scopeKey], key)
	}
}

// AddEdge appends an edge to the graph.
func (cpg *CodePropertyGraph) AddEdge(edge *model.CpgEdge) {
	cpg.edges = append(cpg.edges, edge)
}

// Node returns the node with the given ID, or nil if not found.
func (cpg *CodePropertyGraph) Node(id model.NodeId) *model.CpgNode {
	return cpg.nodes[string(id)]
}

// Nodes returns all nodes matching the given kind in insertion order.
// If kind is empty, all nodes are returned.
func (cpg *CodePropertyGraph) Nodes(kind model.NodeKind) []*model.CpgNode {
	result := make([]*model.CpgNode, 0)
	for _, key := range cpg.nodeOrder {
		node := cpg.nodes[key]
		if kind == "" || node.Kind == kind {
			result = append(result, node)
		}
	}
	return result
}

// ChildrenOf returns all nodes whose Scope equals the given ID, in insertion order.
func (cpg *CodePropertyGraph) ChildrenOf(id model.NodeId) []*model.CpgNode {
	childKeys := cpg.scopeChildren[string(id)]
	result := make([]*model.CpgNode, 0, len(childKeys))
	for _, key := range childKeys {
		if node, ok := cpg.nodes[key]; ok {
			result = append(result, node)
		}
	}
	return result
}

// Successors returns nodes reachable from id via edges of the given kind.
// If edgeKind is empty, all outgoing edges are followed.
func (cpg *CodePropertyGraph) Successors(id model.NodeId, edgeKind model.EdgeKind) []*model.CpgNode {
	seen := make(map[string]bool)
	var result []*model.CpgNode

	for _, edge := range cpg.edges {
		if edge.Source != id {
			continue
		}
		if edgeKind != "" && edge.Kind != edgeKind {
			continue
		}
		targetKey := string(edge.Target)
		if seen[targetKey] {
			continue
		}
		seen[targetKey] = true
		if node, ok := cpg.nodes[targetKey]; ok {
			result = append(result, node)
		}
	}
	return result
}

// Predecessors returns nodes that have an edge of the given kind pointing to id.
// If edgeKind is empty, all incoming edges are followed.
func (cpg *CodePropertyGraph) Predecessors(id model.NodeId, edgeKind model.EdgeKind) []*model.CpgNode {
	seen := make(map[string]bool)
	var result []*model.CpgNode

	for _, edge := range cpg.edges {
		if edge.Target != id {
			continue
		}
		if edgeKind != "" && edge.Kind != edgeKind {
			continue
		}
		srcKey := string(edge.Source)
		if seen[srcKey] {
			continue
		}
		seen[srcKey] = true
		if node, ok := cpg.nodes[srcKey]; ok {
			result = append(result, node)
		}
	}
	return result
}

// Edges returns all edges matching the given kind.
// If kind is empty, all edges are returned.
func (cpg *CodePropertyGraph) Edges(kind model.EdgeKind) []*model.CpgEdge {
	if kind == "" {
		result := make([]*model.CpgEdge, len(cpg.edges))
		copy(result, cpg.edges)
		return result
	}
	var result []*model.CpgEdge
	for _, edge := range cpg.edges {
		if edge.Kind == kind {
			result = append(result, edge)
		}
	}
	return result
}

// NodeCount returns the total number of nodes.
func (cpg *CodePropertyGraph) NodeCount() int {
	return len(cpg.nodes)
}

// EdgeCount returns the total number of edges.
func (cpg *CodePropertyGraph) EdgeCount() int {
	return len(cpg.edges)
}

// AnnotateNode attaches a consumer annotation to a node (separate from CpgNode.Attrs).
func (cpg *CodePropertyGraph) AnnotateNode(id model.NodeId, key string, value interface{}) {
	idStr := string(id)
	if cpg.annotations[idStr] == nil {
		cpg.annotations[idStr] = make(map[string]interface{})
	}
	cpg.annotations[idStr][key] = value
}

// AnnotateEdge attaches a consumer annotation to an edge.
func (cpg *CodePropertyGraph) AnnotateEdge(source, target model.NodeId, key string, value interface{}) {
	k := [2]string{string(source), string(target)}
	if cpg.edgeAnnotations[k] == nil {
		cpg.edgeAnnotations[k] = make(map[string]interface{})
	}
	cpg.edgeAnnotations[k][key] = value
}

// GetAnnotation retrieves a single annotation value from a node.
func (cpg *CodePropertyGraph) GetAnnotation(id model.NodeId, key string) (interface{}, bool) {
	ann := cpg.annotations[string(id)]
	if ann == nil {
		return nil, false
	}
	v, ok := ann[key]
	return v, ok
}

// Files returns a sorted list of unique file paths from all node locations.
func (cpg *CodePropertyGraph) Files() []string {
	seen := make(map[string]bool)
	for _, key := range cpg.nodeOrder {
		node := cpg.nodes[key]
		if node.Location != nil && node.Location.File != "" {
			seen[node.Location.File] = true
		}
	}

	files := make([]string, 0, len(seen))
	for f := range seen {
		files = append(files, f)
	}
	sort.Strings(files)
	return files
}

// ToDict serializes the CPG to a map structure compatible with Python
// treeloom's JSON format. Nodes and edges appear in insertion order.
func (cpg *CodePropertyGraph) ToDict() map[string]interface{} {
	nodes := make([]interface{}, 0, len(cpg.nodeOrder))
	for _, key := range cpg.nodeOrder {
		node := cpg.nodes[key]
		nodes = append(nodes, nodeToMap(node))
	}

	edges := make([]interface{}, 0, len(cpg.edges))
	for _, edge := range cpg.edges {
		edges = append(edges, edgeToMap(edge))
	}

	// Serialize node annotations.
	anns := make(map[string]interface{}, len(cpg.annotations))
	for id, ann := range cpg.annotations {
		copied := make(map[string]interface{}, len(ann))
		for k, v := range ann {
			copied[k] = v
		}
		anns[id] = copied
	}

	// Serialize edge annotations.
	edgeAnns := make([]interface{}, 0, len(cpg.edgeAnnotations))
	for k, ann := range cpg.edgeAnnotations {
		copied := make(map[string]interface{}, len(ann))
		for ak, av := range ann {
			copied[ak] = av
		}
		edgeAnns = append(edgeAnns, map[string]interface{}{
			"source":      k[0],
			"target":      k[1],
			"annotations": copied,
		})
	}

	return map[string]interface{}{
		"treeloom_version": "0.9.0",
		"nodes":            nodes,
		"edges":            edges,
		"annotations":      anns,
		"edge_annotations": edgeAnns,
	}
}

// nodeToMap converts a CpgNode to the dict structure expected by Python treeloom.
func nodeToMap(n *model.CpgNode) map[string]interface{} {
	m := map[string]interface{}{
		"id":   string(n.ID),
		"kind": string(n.Kind),
		"name": n.Name,
	}

	if n.Location != nil {
		m["location"] = locationToMap(n.Location)
	} else {
		m["location"] = nil
	}

	if n.EndLocation != nil {
		m["end_location"] = locationToMap(n.EndLocation)
	} else {
		m["end_location"] = nil
	}

	if n.Scope != nil {
		m["scope"] = string(*n.Scope)
	} else {
		m["scope"] = nil
	}

	attrs := n.Attrs
	if attrs == nil {
		attrs = map[string]interface{}{}
	}
	m["attrs"] = attrs

	return m
}

// locationToMap converts a SourceLocation to a map with forward-slash paths.
func locationToMap(loc *model.SourceLocation) map[string]interface{} {
	return map[string]interface{}{
		"file":   loc.ToPosix().File,
		"line":   loc.Line,
		"column": loc.Column,
	}
}

// edgeToMap converts a CpgEdge to the dict structure expected by Python treeloom.
func edgeToMap(e *model.CpgEdge) map[string]interface{} {
	attrs := e.Attrs
	if attrs == nil {
		attrs = map[string]interface{}{}
	}
	return map[string]interface{}{
		"source": string(e.Source),
		"target": string(e.Target),
		"kind":   string(e.Kind),
		"attrs":  attrs,
	}
}

// FromDict deserializes a CPG from a map produced by ToDict or parsed from JSON.
func FromDict(data map[string]interface{}) (*CodePropertyGraph, error) {
	cpg := NewCPG()

	if nodesRaw, ok := data["nodes"].([]interface{}); ok {
		for _, nRaw := range nodesRaw {
			nMap, ok := nRaw.(map[string]interface{})
			if !ok {
				continue
			}
			node := mapToNode(nMap)
			cpg.AddNode(node)
		}
	}

	if edgesRaw, ok := data["edges"].([]interface{}); ok {
		for _, eRaw := range edgesRaw {
			eMap, ok := eRaw.(map[string]interface{})
			if !ok {
				continue
			}
			edge := mapToEdge(eMap)
			cpg.AddEdge(&edge)
		}
	}

	if anns, ok := data["annotations"].(map[string]interface{}); ok {
		for id, annRaw := range anns {
			if annMap, ok := annRaw.(map[string]interface{}); ok {
				for k, v := range annMap {
					cpg.AnnotateNode(model.NodeId(id), k, v)
				}
			}
		}
	}

	if edgeAnns, ok := data["edge_annotations"].([]interface{}); ok {
		for _, entryRaw := range edgeAnns {
			entry, ok := entryRaw.(map[string]interface{})
			if !ok {
				continue
			}
			src, _ := entry["source"].(string)
			tgt, _ := entry["target"].(string)
			if anns, ok := entry["annotations"].(map[string]interface{}); ok {
				for k, v := range anns {
					cpg.AnnotateEdge(model.NodeId(src), model.NodeId(tgt), k, v)
				}
			}
		}
	}

	return cpg, nil
}

// mapToNode converts a JSON-decoded map to a CpgNode.
func mapToNode(m map[string]interface{}) *model.CpgNode {
	node := &model.CpgNode{
		ID:   model.NodeId(strVal(m, "id")),
		Kind: model.NodeKind(strVal(m, "kind")),
		Name: strVal(m, "name"),
	}

	if loc := mapToLocation(m["location"]); loc != nil {
		node.Location = loc
	}
	if endLoc := mapToLocation(m["end_location"]); endLoc != nil {
		node.EndLocation = endLoc
	}

	if scopeStr, ok := m["scope"].(string); ok {
		scope := model.NodeId(scopeStr)
		node.Scope = &scope
	}

	if attrs, ok := m["attrs"].(map[string]interface{}); ok {
		node.Attrs = attrs
	} else {
		node.Attrs = map[string]interface{}{}
	}

	return node
}

// mapToEdge converts a JSON-decoded map to a CpgEdge.
func mapToEdge(m map[string]interface{}) model.CpgEdge {
	edge := model.CpgEdge{
		Source: model.NodeId(strVal(m, "source")),
		Target: model.NodeId(strVal(m, "target")),
		Kind:   model.EdgeKind(strVal(m, "kind")),
	}
	if attrs, ok := m["attrs"].(map[string]interface{}); ok {
		edge.Attrs = attrs
	} else {
		edge.Attrs = map[string]interface{}{}
	}
	return edge
}

// mapToLocation converts a JSON-decoded map to a SourceLocation.
func mapToLocation(raw interface{}) *model.SourceLocation {
	m, ok := raw.(map[string]interface{})
	if !ok || m == nil {
		return nil
	}
	loc := &model.SourceLocation{
		File: strVal(m, "file"),
	}
	if v, ok := m["line"].(float64); ok {
		loc.Line = int(v)
	}
	if v, ok := m["column"].(float64); ok {
		loc.Column = int(v)
	}
	return loc
}

// strVal extracts a string value from a map, returning "" if missing or wrong type.
func strVal(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}
