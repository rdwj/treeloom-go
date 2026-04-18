package model

import (
	"encoding/json"
	"testing"
)

func TestCpgNodeMarshalJSON(t *testing.T) {
	scope := NodeId("function:main.py:1:0:0")
	node := CpgNode{
		ID:   "call:main.py:5:4:0",
		Kind: NodeCall,
		Name: "print",
		Location: &SourceLocation{
			File:   "main.py",
			Line:   5,
			Column: 4,
		},
		EndLocation: &SourceLocation{
			File:   "main.py",
			Line:   5,
			Column: 15,
		},
		Scope: &scope,
		Attrs: map[string]interface{}{
			"args_count": float64(1),
		},
	}

	data, err := json.Marshal(node)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if result["id"] != "call:main.py:5:4:0" {
		t.Errorf("id = %v, want call:main.py:5:4:0", result["id"])
	}
	if result["kind"] != "call" {
		t.Errorf("kind = %v, want call", result["kind"])
	}
	if result["scope"] != "function:main.py:1:0:0" {
		t.Errorf("scope = %v, want function:main.py:1:0:0", result["scope"])
	}

	loc := result["location"].(map[string]interface{})
	if loc["file"] != "main.py" {
		t.Errorf("location.file = %v, want main.py", loc["file"])
	}
	if loc["line"] != float64(5) {
		t.Errorf("location.line = %v, want 5", loc["line"])
	}

	attrs := result["attrs"].(map[string]interface{})
	if attrs["args_count"] != float64(1) {
		t.Errorf("attrs.args_count = %v, want 1", attrs["args_count"])
	}
}

func TestCpgNodeMarshalJSON_NullScope(t *testing.T) {
	node := CpgNode{
		ID:   "module:main.py:0:0:0",
		Kind: NodeModule,
		Name: "main.py",
		Location: &SourceLocation{
			File: "main.py",
			Line: 1,
		},
		// Scope is nil
		// Attrs is nil
	}

	data, err := json.Marshal(node)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	// Scope must be present as null, not omitted
	scopeVal, exists := result["scope"]
	if !exists {
		t.Fatal("scope key missing from JSON; must be present as null")
	}
	if scopeVal != nil {
		t.Errorf("scope = %v, want null", scopeVal)
	}

	// Attrs must be {} not null
	attrs, ok := result["attrs"].(map[string]interface{})
	if !ok {
		t.Fatalf("attrs is not an object, got %T: %v", result["attrs"], result["attrs"])
	}
	if len(attrs) != 0 {
		t.Errorf("attrs = %v, want empty map", attrs)
	}
}

func TestCpgNodeMarshalJSON_NullLocation(t *testing.T) {
	node := CpgNode{
		ID:   "module:synthetic:0:0:0",
		Kind: NodeModule,
		Name: "synthetic",
		// Location is nil
		// EndLocation is nil
	}

	data, err := json.Marshal(node)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if result["location"] != nil {
		t.Errorf("location = %v, want null", result["location"])
	}
	if result["end_location"] != nil {
		t.Errorf("end_location = %v, want null", result["end_location"])
	}
}

func TestCpgNodeRoundTrip(t *testing.T) {
	scope := NodeId("module:main.py:0:0:0")
	original := CpgNode{
		ID:   "function:main.py:3:0:0",
		Kind: NodeFunction,
		Name: "hello",
		Location: &SourceLocation{
			File:   "main.py",
			Line:   3,
			Column: 0,
		},
		EndLocation: &SourceLocation{
			File:   "main.py",
			Line:   5,
			Column: 14,
		},
		Scope: &scope,
		Attrs: map[string]interface{}{
			"is_async": false,
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	var decoded CpgNode
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if decoded.ID != original.ID {
		t.Errorf("ID = %v, want %v", decoded.ID, original.ID)
	}
	if decoded.Kind != original.Kind {
		t.Errorf("Kind = %v, want %v", decoded.Kind, original.Kind)
	}
	if decoded.Name != original.Name {
		t.Errorf("Name = %v, want %v", decoded.Name, original.Name)
	}
	if decoded.Scope == nil || *decoded.Scope != *original.Scope {
		t.Errorf("Scope = %v, want %v", decoded.Scope, original.Scope)
	}
	if decoded.Location == nil || *decoded.Location != *original.Location {
		t.Errorf("Location = %v, want %v", decoded.Location, original.Location)
	}
	if decoded.EndLocation == nil || *decoded.EndLocation != *original.EndLocation {
		t.Errorf("EndLocation = %v, want %v", decoded.EndLocation, original.EndLocation)
	}
}

func TestCpgNodeRoundTrip_NilFields(t *testing.T) {
	original := CpgNode{
		ID:   "module:test.py:0:0:0",
		Kind: NodeModule,
		Name: "test",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	var decoded CpgNode
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if decoded.Location != nil {
		t.Errorf("Location = %v, want nil", decoded.Location)
	}
	if decoded.EndLocation != nil {
		t.Errorf("EndLocation = %v, want nil", decoded.EndLocation)
	}
	if decoded.Scope != nil {
		t.Errorf("Scope = %v, want nil", decoded.Scope)
	}
	if decoded.Attrs == nil {
		t.Fatal("Attrs should be empty map, not nil")
	}
	if len(decoded.Attrs) != 0 {
		t.Errorf("Attrs = %v, want empty", decoded.Attrs)
	}
}

func TestCpgEdgeMarshalJSON(t *testing.T) {
	edge := CpgEdge{
		Source: "function:main.py:1:0:0",
		Target: "call:main.py:5:4:0",
		Kind:   EdgeContains,
	}

	data, err := json.Marshal(edge)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if result["source"] != "function:main.py:1:0:0" {
		t.Errorf("source = %v, want function:main.py:1:0:0", result["source"])
	}
	if result["kind"] != "contains" {
		t.Errorf("kind = %v, want contains", result["kind"])
	}

	// Attrs must be {} not null
	attrs, ok := result["attrs"].(map[string]interface{})
	if !ok {
		t.Fatalf("attrs is not an object, got %T: %v", result["attrs"], result["attrs"])
	}
	if len(attrs) != 0 {
		t.Errorf("attrs = %v, want empty map", attrs)
	}
}

func TestCpgEdgeRoundTrip(t *testing.T) {
	original := CpgEdge{
		Source: "variable:main.py:3:4:0",
		Target: "call:main.py:5:4:0",
		Kind:   EdgeDataFlowsTo,
		Attrs: map[string]interface{}{
			"position": float64(0),
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	var decoded CpgEdge
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if decoded.Source != original.Source {
		t.Errorf("Source = %v, want %v", decoded.Source, original.Source)
	}
	if decoded.Target != original.Target {
		t.Errorf("Target = %v, want %v", decoded.Target, original.Target)
	}
	if decoded.Kind != original.Kind {
		t.Errorf("Kind = %v, want %v", decoded.Kind, original.Kind)
	}
	if decoded.Attrs["position"] != original.Attrs["position"] {
		t.Errorf("Attrs[position] = %v, want %v", decoded.Attrs["position"], original.Attrs["position"])
	}
}

func TestWindowsPathToPosix(t *testing.T) {
	node := CpgNode{
		ID:   "module:src\\main.py:0:0:0",
		Kind: NodeModule,
		Name: "main",
		Location: &SourceLocation{
			File:   "src\\main.py",
			Line:   1,
			Column: 0,
		},
	}

	data, err := json.Marshal(node)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	loc := result["location"].(map[string]interface{})
	if loc["file"] != "src/main.py" {
		t.Errorf("file = %v, want src/main.py (POSIX slashes)", loc["file"])
	}
}

func TestNodeKindValues(t *testing.T) {
	expected := map[NodeKind]string{
		NodeModule:    "module",
		NodeClass:     "class",
		NodeFunction:  "function",
		NodeParameter: "parameter",
		NodeVariable:  "variable",
		NodeCall:      "call",
		NodeLiteral:   "literal",
		NodeReturn:    "return",
		NodeImport:    "import",
		NodeBranch:    "branch",
		NodeLoop:      "loop",
		NodeBlock:     "block",
	}
	for kind, val := range expected {
		if string(kind) != val {
			t.Errorf("NodeKind %v = %q, want %q", kind, string(kind), val)
		}
	}
}

func TestEdgeKindValues(t *testing.T) {
	expected := map[EdgeKind]string{
		EdgeContains:      "contains",
		EdgeHasParameter:  "has_parameter",
		EdgeHasReturnType: "has_return_type",
		EdgeFlowsTo:       "flows_to",
		EdgeBranchesTo:    "branches_to",
		EdgeDataFlowsTo:   "data_flows_to",
		EdgeDefinedBy:     "defined_by",
		EdgeUsedBy:        "used_by",
		EdgeCalls:         "calls",
		EdgeResolveTo:     "resolves_to",
		EdgeImports:       "imports",
	}
	for kind, val := range expected {
		if string(kind) != val {
			t.Errorf("EdgeKind %v = %q, want %q", kind, string(kind), val)
		}
	}
}
