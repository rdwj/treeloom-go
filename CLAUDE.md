# treeloom-go

Go reimplementation of the treeloom CPG `build` command for performance-critical workloads. Produces JSON output compatible with Python treeloom's downstream tools (greploom, ctk assemble, sanicode).

See rdwj/treeloom#100 for context.

## Quick start

```bash
go build -o treeloom-go ./cmd/treeloom-go
./treeloom-go build <source_path> -o cpg.json --relative-root <root>
```

## Project structure

```
cmd/treeloom-go/main.go         # CLI (cobra)
internal/
  model/model.go                # NodeId, NodeKind, EdgeKind, CpgNode, CpgEdge, SourceLocation
  graph/cpg.go                  # CodePropertyGraph container
  graph/builder.go              # CPGBuilder (5-phase pipeline, NodeEmitter impl)
  lang/registry.go              # Language visitor registry
  lang/python/visitor.go        # Python tree-sitter visitor
  lang/java/visitor.go          # Java tree-sitter visitor
  export/json.go                # JSON serialization
```

## Architecture

The builder executes a 5-phase pipeline matching the Python implementation exactly:

1. **Parse** -- tree-sitter parse + visitor walk (emits AST nodes, intra-procedural DFG)
2. **CFG** -- control flow edges within each function
3. **Call resolution** -- link call sites to function definitions
4. **Function summaries** -- BFS from params through DATA_FLOWS_TO
5. **Inter-procedural DFG** -- wire data flow across call boundaries

## Key constraint

Output JSON must be structurally compatible with Python treeloom. Node/edge counts, kinds, names, locations, and attrs must match for the same input. Node IDs differ (different counters) but the graph structure is identical.

## Dependencies

- `github.com/smacker/go-tree-sitter` -- tree-sitter Go bindings (CGo)
- `github.com/spf13/cobra` -- CLI framework

## Testing

```bash
go test ./...
```

Fixture-based comparison against Python treeloom:
```bash
treeloom build tests/fixtures/python/simple_function.py -o /tmp/py.json --relative-root tests/fixtures/python/
treeloom-go build tests/fixtures/python/simple_function.py -o /tmp/go.json --relative-root tests/fixtures/python/
# Compare node/edge counts, kinds, attrs
```

## Languages supported

- Python (.py, .pyi)
- Java (.java)
