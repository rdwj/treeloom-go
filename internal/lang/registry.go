// Package lang provides the language visitor registry and protocol.
package lang

import (
	"github.com/rdwj/treeloom-go/internal/graph"
	"github.com/rdwj/treeloom-go/internal/lang/java"
	"github.com/rdwj/treeloom-go/internal/lang/python"
)

// Registry maps file extensions and language names to LanguageVisitor
// implementations. It is used by the builder to select the right visitor
// for each source file.
//
// Registry implements graph.VisitorRegistry so it can be passed directly
// to the CPGBuilder.
type Registry struct {
	visitors map[string]graph.LanguageVisitor // extension -> visitor
	byName   map[string]graph.LanguageVisitor // language name -> visitor
}

// Compile-time check that Registry implements graph.VisitorRegistry.
var _ graph.VisitorRegistry = (*Registry)(nil)

// NewRegistry creates an empty Registry.
func NewRegistry() *Registry {
	return &Registry{
		visitors: make(map[string]graph.LanguageVisitor),
		byName:   make(map[string]graph.LanguageVisitor),
	}
}

// Register adds a visitor to the registry, keyed by all of its extensions
// and its language name.
func (r *Registry) Register(v graph.LanguageVisitor) {
	r.byName[v.Name()] = v
	for _, ext := range v.Extensions() {
		r.visitors[ext] = v
	}
}

// GetVisitor returns the visitor for a file extension (e.g., ".py"), or nil.
func (r *Registry) GetVisitor(ext string) graph.LanguageVisitor {
	return r.visitors[ext]
}

// GetVisitorByName returns the visitor for a language name (e.g., "python"), or nil.
func (r *Registry) GetVisitorByName(name string) graph.LanguageVisitor {
	return r.byName[name]
}

// SupportedExtensions returns all registered file extensions.
func (r *Registry) SupportedExtensions() []string {
	exts := make([]string, 0, len(r.visitors))
	for ext := range r.visitors {
		exts = append(exts, ext)
	}
	return exts
}

// DefaultRegistry creates a registry with all built-in visitors.
func DefaultRegistry() *Registry {
	r := NewRegistry()
	r.Register(&python.Visitor{})
	r.Register(&java.Visitor{})
	return r
}
