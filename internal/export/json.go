// Package export provides serialization of Code Property Graphs.
package export

import (
	"encoding/json"
	"strings"

	"github.com/rdwj/treeloom-go/internal/graph"
)

// ToJSON serializes a CPG to JSON bytes.
//
// When indent > 0 the output is pretty-printed with that many spaces per
// level, matching Python's json.dumps(data, indent=2, default=str).
// A trailing newline is appended to match Python output.
func ToJSON(cpg *graph.CodePropertyGraph, indent int) ([]byte, error) {
	data := cpg.ToDict()

	var buf []byte
	var err error
	if indent > 0 {
		buf, err = json.MarshalIndent(data, "", strings.Repeat(" ", indent))
	} else {
		buf, err = json.Marshal(data)
	}
	if err != nil {
		return nil, err
	}

	// Python's json.dumps appends a trailing newline when writing to a file
	// via our to_json helper. Append one here for byte-level compatibility.
	buf = append(buf, '\n')
	return buf, nil
}

// FromJSON deserializes a CPG from JSON bytes.
func FromJSON(data []byte) (*graph.CodePropertyGraph, error) {
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	return graph.FromDict(raw)
}
