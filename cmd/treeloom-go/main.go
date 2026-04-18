// Command treeloom-go builds Code Property Graphs from source files.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/rdwj/treeloom-go/internal/export"
	"github.com/rdwj/treeloom-go/internal/graph"
	"github.com/rdwj/treeloom-go/internal/lang"
)

const version = "0.9.0"

func main() {
	root := &cobra.Command{
		Use:   "treeloom-go",
		Short: "Language-agnostic Code Property Graph builder",
	}

	root.AddCommand(buildCmd())
	root.AddCommand(versionCmd())

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println("treeloom-go", version)
		},
	}
}

func buildCmd() *cobra.Command {
	var (
		output        string
		excludes      []string
		quiet         bool
		progress      bool
		languages     []string
		timeout       float64
		includeSource bool
		relativeRoot  string
	)

	cmd := &cobra.Command{
		Use:   "build <path>",
		Short: "Build a CPG from source files",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBuild(args[0], buildOpts{
				output:        output,
				excludes:      excludes,
				quiet:         quiet,
				progress:      progress,
				languages:     languages,
				timeout:       timeout,
				includeSource: includeSource,
				relativeRoot:  relativeRoot,
			})
		},
	}

	f := cmd.Flags()
	f.StringVarP(&output, "output", "o", "cpg.json", "Output JSON file")
	f.StringArrayVar(&excludes, "exclude", nil, "Exclusion glob pattern (repeatable)")
	f.BoolVarP(&quiet, "quiet", "q", false, "Suppress summary output")
	f.BoolVar(&progress, "progress", false, "Print each file as parsed to stderr")
	f.StringArrayVar(&languages, "language", nil, "Only process files for this language (repeatable)")
	f.Float64Var(&timeout, "timeout", 0, "Abort if build exceeds seconds")
	f.BoolVar(&includeSource, "include-source", false, "Include source text in CPG nodes")
	f.StringVar(&relativeRoot, "relative-root", "", "Store paths relative to this directory")

	return cmd
}

type buildOpts struct {
	output        string
	excludes      []string
	quiet         bool
	progress      bool
	languages     []string
	timeout       float64
	includeSource bool
	relativeRoot  string
}

func runBuild(target string, opts buildOpts) error {
	// Resolve the target path.
	path, err := filepath.Abs(target)
	if err != nil {
		return fmt.Errorf("resolving path: %w", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("path does not exist: %s", path)
	}

	// Determine relative_root: explicit flag wins, else directory itself
	// (if dir) or parent (if file).
	relRoot := opts.relativeRoot
	if relRoot == "" {
		if info.IsDir() {
			relRoot = path
		} else {
			relRoot = filepath.Dir(path)
		}
	} else {
		relRoot, err = filepath.Abs(relRoot)
		if err != nil {
			return fmt.Errorf("resolving relative-root: %w", err)
		}
	}

	registry := lang.DefaultRegistry()

	// Validate --language flags against registry.
	var langExts map[string]bool
	if len(opts.languages) > 0 {
		langExts = make(map[string]bool)
		for _, name := range opts.languages {
			v := registry.GetVisitorByName(strings.ToLower(name))
			if v == nil {
				supported := registry.SupportedExtensions()
				return fmt.Errorf("unknown language: %q (supported extensions: %v)", name, supported)
			}
			for _, ext := range v.Extensions() {
				langExts[ext] = true
			}
		}
	}

	// Collect source files.
	var files []string
	if info.IsDir() {
		files, err = collectFiles(path, registry, opts.excludes, langExts)
		if err != nil {
			return fmt.Errorf("scanning directory: %w", err)
		}
	} else {
		files = []string{path}
	}

	if opts.progress {
		fmt.Fprintf(os.Stderr, "Found %d files to parse\n", len(files))
	}

	// Build the CPG using the builder pipeline.
	builderOpts := []graph.BuilderOption{
		graph.WithRegistry(registry),
		graph.WithRelativeRoot(relRoot),
		graph.WithIncludeSource(opts.includeSource),
	}
	if opts.progress {
		builderOpts = append(builderOpts, graph.WithProgress(func(phase, detail string) {
			fmt.Fprintf(os.Stderr, "[%s] %s\n", phase, detail)
		}))
	}
	if opts.timeout > 0 {
		builderOpts = append(builderOpts, graph.WithTimeout(
			time.Duration(opts.timeout*float64(time.Second)),
		))
	}

	builder := graph.NewBuilder(builderOpts...)
	for _, f := range files {
		builder.AddFile(f)
	}

	cpg, err := builder.Build()
	if err != nil {
		return fmt.Errorf("building CPG: %w", err)
	}

	// Serialize to JSON (2-space indent, matching Python output).
	jsonData, err := export.ToJSON(cpg, 2)
	if err != nil {
		return fmt.Errorf("serializing CPG: %w", err)
	}

	if err := os.WriteFile(opts.output, jsonData, 0644); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}

	if !opts.quiet {
		fmt.Fprintf(os.Stderr, "Built CPG: %d nodes, %d edges, %d files -> %s\n",
			cpg.NodeCount(), cpg.EdgeCount(), len(cpg.Files()), opts.output)
	}

	return nil
}

// defaultExcludes are the glob patterns excluded by default, matching
// Python treeloom's _DEFAULT_EXCLUDES.
var defaultExcludes = []string{
	"__pycache__",
	"node_modules",
	".git",
	"venv",
	".venv",
}

// collectFiles walks a directory and returns files whose extension has a
// registered visitor, excluding paths matching the exclude patterns.
func collectFiles(
	root string,
	registry *lang.Registry,
	extraExcludes []string,
	langExts map[string]bool,
) ([]string, error) {
	supported := make(map[string]bool)
	for _, ext := range registry.SupportedExtensions() {
		supported[ext] = true
	}

	allExcludes := make([]string, 0, len(defaultExcludes)+len(extraExcludes))
	allExcludes = append(allExcludes, defaultExcludes...)
	allExcludes = append(allExcludes, extraExcludes...)

	var files []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}

		name := info.Name()

		// Skip excluded directories.
		if info.IsDir() {
			for _, pat := range allExcludes {
				// Strip **/ prefix for directory name matching.
				clean := strings.TrimPrefix(pat, "**/")
				if matched, _ := filepath.Match(clean, name); matched {
					return filepath.SkipDir
				}
			}
			return nil
		}

		// Check extension is supported.
		ext := filepath.Ext(name)
		if !supported[ext] {
			return nil
		}

		// Check language filter.
		if langExts != nil && !langExts[ext] {
			return nil
		}

		// Check file-level exclude patterns.
		rel, _ := filepath.Rel(root, path)
		for _, pat := range allExcludes {
			if matched, _ := filepath.Match(pat, rel); matched {
				return nil
			}
			if matched, _ := filepath.Match(pat, name); matched {
				return nil
			}
		}

		files = append(files, path)
		return nil
	})

	return files, err
}
