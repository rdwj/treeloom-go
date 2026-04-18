// Package model defines the core data types for the Code Property Graph.
//
// These types mirror the Python treeloom model exactly to ensure JSON
// serialization compatibility between the two implementations.
package model

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

// NodeKind identifies the type of node in the CPG.
type NodeKind string

const (
	NodeModule    NodeKind = "module"
	NodeClass     NodeKind = "class"
	NodeFunction  NodeKind = "function"
	NodeParameter NodeKind = "parameter"
	NodeVariable  NodeKind = "variable"
	NodeCall      NodeKind = "call"
	NodeLiteral   NodeKind = "literal"
	NodeReturn    NodeKind = "return"
	NodeImport    NodeKind = "import"
	NodeBranch    NodeKind = "branch"
	NodeLoop      NodeKind = "loop"
	NodeBlock     NodeKind = "block"
)

// EdgeKind identifies the type of edge in the CPG.
type EdgeKind string

const (
	// AST structure
	EdgeContains      EdgeKind = "contains"
	EdgeHasParameter  EdgeKind = "has_parameter"
	EdgeHasReturnType EdgeKind = "has_return_type"

	// Control flow
	EdgeFlowsTo    EdgeKind = "flows_to"
	EdgeBranchesTo EdgeKind = "branches_to"

	// Data flow
	EdgeDataFlowsTo EdgeKind = "data_flows_to"
	EdgeDefinedBy   EdgeKind = "defined_by"
	EdgeUsedBy      EdgeKind = "used_by"

	// Call graph
	EdgeCalls     EdgeKind = "calls"
	EdgeResolveTo EdgeKind = "resolves_to"

	// Module structure
	EdgeImports EdgeKind = "imports"
)

// NodeId is an opaque node identifier. It serializes as a plain JSON string.
type NodeId string

// SourceLocation identifies a position in a source file.
// Lines are 1-based (matching editor display). Columns are 0-based.
type SourceLocation struct {
	File   string `json:"file"`
	Line   int    `json:"line"`
	Column int    `json:"column"`
}

// ToPosix returns the File path with forward slashes, regardless of OS.
func (sl SourceLocation) ToPosix() SourceLocation {
	sl.File = filepath.ToSlash(sl.File)
	return sl
}

// CpgNode is a node in the Code Property Graph.
type CpgNode struct {
	ID          NodeId                 `json:"id"`
	Kind        NodeKind               `json:"kind"`
	Name        string                 `json:"name"`
	Location    *SourceLocation        `json:"location"`
	EndLocation *SourceLocation        `json:"end_location"`
	Scope       *NodeId                `json:"scope"`
	Attrs       map[string]interface{} `json:"attrs"`
}

// MarshalJSON implements custom JSON marshaling for CpgNode.
//
// Scope serializes as the NodeId string value or null (not omitted).
// Location and EndLocation serialize as objects or null.
// Attrs serializes as {} when empty, never null.
// File paths in locations use forward slashes.
func (n CpgNode) MarshalJSON() ([]byte, error) {
	var scope interface{}
	if n.Scope != nil {
		scope = string(*n.Scope)
	}

	attrs := n.Attrs
	if attrs == nil {
		attrs = map[string]interface{}{}
	}

	loc := posixLocation(n.Location)
	endLoc := posixLocation(n.EndLocation)

	m := map[string]interface{}{
		"id":           string(n.ID),
		"kind":         string(n.Kind),
		"name":         n.Name,
		"location":     loc,
		"end_location": endLoc,
		"scope":        scope,
		"attrs":        attrs,
	}
	return json.Marshal(m)
}

// UnmarshalJSON implements custom JSON unmarshaling for CpgNode.
func (n *CpgNode) UnmarshalJSON(data []byte) error {
	var raw struct {
		ID          string                 `json:"id"`
		Kind        string                 `json:"kind"`
		Name        string                 `json:"name"`
		Location    *SourceLocation        `json:"location"`
		EndLocation *SourceLocation        `json:"end_location"`
		Scope       *string                `json:"scope"`
		Attrs       map[string]interface{} `json:"attrs"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	n.ID = NodeId(raw.ID)
	n.Kind = NodeKind(raw.Kind)
	n.Name = raw.Name
	n.Location = raw.Location
	n.EndLocation = raw.EndLocation

	if raw.Scope != nil {
		scope := NodeId(*raw.Scope)
		n.Scope = &scope
	}

	if raw.Attrs != nil {
		n.Attrs = raw.Attrs
	} else {
		n.Attrs = map[string]interface{}{}
	}

	return nil
}

// CpgEdge is a directed edge in the Code Property Graph.
type CpgEdge struct {
	Source NodeId                 `json:"source"`
	Target NodeId                `json:"target"`
	Kind   EdgeKind              `json:"kind"`
	Attrs  map[string]interface{} `json:"attrs"`
}

// MarshalJSON implements custom JSON marshaling for CpgEdge.
// Attrs serializes as {} when empty, never null.
func (e CpgEdge) MarshalJSON() ([]byte, error) {
	attrs := e.Attrs
	if attrs == nil {
		attrs = map[string]interface{}{}
	}

	m := map[string]interface{}{
		"source": string(e.Source),
		"target": string(e.Target),
		"kind":   string(e.Kind),
		"attrs":  attrs,
	}
	return json.Marshal(m)
}

// UnmarshalJSON implements custom JSON unmarshaling for CpgEdge.
func (e *CpgEdge) UnmarshalJSON(data []byte) error {
	var raw struct {
		Source string                 `json:"source"`
		Target string                 `json:"target"`
		Kind   string                 `json:"kind"`
		Attrs  map[string]interface{} `json:"attrs"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	e.Source = NodeId(raw.Source)
	e.Target = NodeId(raw.Target)
	e.Kind = EdgeKind(raw.Kind)

	if raw.Attrs != nil {
		e.Attrs = raw.Attrs
	} else {
		e.Attrs = map[string]interface{}{}
	}

	return nil
}

// posixLocation converts a SourceLocation pointer for JSON marshaling,
// ensuring the file path uses forward slashes.
func posixLocation(loc *SourceLocation) *SourceLocation {
	if loc == nil {
		return nil
	}
	posix := *loc
	posix.File = strings.ReplaceAll(posix.File, "\\", "/")
	return &posix
}
