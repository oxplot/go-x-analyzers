package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

const defaultPassesRoot = "golang.org/x/tools/go/analysis/passes"

type goPackage struct {
	ImportPath string
	Name       string
	Dir        string
	Error      *goListError
	GoFiles    []string
}

type goListError struct {
	Err string
}

type analyzerPackage struct {
	ImportPath  string
	PackageName string
	HasAnalyzer bool
	HasSuite    bool
}

func main() {
	passesRoot := flag.String("passes", defaultPassesRoot, "root import path to scan for analyzer packages")
	outputPath := flag.String("out", "main.go", "generated Go source file")
	excludePath := flag.String("exclude", "excluded.txt", "file containing analyzer names to exclude, one per line")
	flag.Parse()

	analyzers, err := listAnalyzerPackages(*passesRoot)
	if err != nil {
		fatalf("%v", err)
	}
	if len(analyzers) == 0 {
		fatalf("no analyzer packages found under %s", *passesRoot)
	}

	excludedNames, err := readExcludedNames(*excludePath)
	if err != nil {
		fatalf("%v", err)
	}
	analyzers = excludeAnalyzerPackages(analyzers, excludedNames)

	source, err := renderMain(analyzers)
	if err != nil {
		fatalf("%v", err)
	}
	if err := os.WriteFile(*outputPath, source, 0o644); err != nil {
		fatalf("write %s: %v", *outputPath, err)
	}
}

func readExcludedNames(path string) (map[string]bool, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	excluded := map[string]bool{}
	for line := range strings.SplitSeq(string(content), "\n") {
		name := strings.TrimSpace(line)
		if name == "" || strings.HasPrefix(name, "#") {
			continue
		}
		excluded[name] = true
	}

	return excluded, nil
}

func excludeAnalyzerPackages(analyzers []analyzerPackage, excluded map[string]bool) []analyzerPackage {
	if len(excluded) == 0 {
		return analyzers
	}
	filtered := analyzers[:0]
	for _, analyzer := range analyzers {
		if !excluded[analyzer.PackageName] {
			filtered = append(filtered, analyzer)
		}
	}
	return filtered
}

func listAnalyzerPackages(passesRoot string) ([]analyzerPackage, error) {
	out, err := goList(passesRoot + "/...")
	if err != nil {
		return nil, err
	}

	var analyzers []analyzerPackage
	seenNames := map[string]string{}
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var pkg goPackage
		if err := dec.Decode(&pkg); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("decode go list output: %w", err)
		}

		if pkg.Error != nil {
			return nil, fmt.Errorf("go list %s: %s", pkg.ImportPath, pkg.Error.Err)
		}
		if !isDirectPassPackage(pkg.ImportPath, passesRoot) || pkg.Name == "main" {
			continue
		}

		exports, err := packageAnalyzerExports(pkg.Dir, pkg.GoFiles)
		if err != nil {
			return nil, fmt.Errorf("inspect %s: %w", pkg.ImportPath, err)
		}
		if !exports.HasAnalyzer && !exports.HasSuite {
			continue
		}

		if previousPath, exists := seenNames[pkg.Name]; exists {
			return nil, fmt.Errorf("package name %q is used by both %s and %s", pkg.Name, previousPath, pkg.ImportPath)
		}
		seenNames[pkg.Name] = pkg.ImportPath

		analyzers = append(analyzers, analyzerPackage{
			ImportPath:  pkg.ImportPath,
			PackageName: pkg.Name,
			HasAnalyzer: exports.HasAnalyzer,
			HasSuite:    exports.HasSuite,
		})
	}

	sort.Slice(analyzers, func(i, j int) bool {
		return analyzers[i].ImportPath < analyzers[j].ImportPath
	})
	return analyzers, nil
}

func goList(pattern string) ([]byte, error) {
	cmd := exec.Command("go", "list", "-json", pattern)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err == nil {
		return out, nil
	}
	if stderr.Len() > 0 {
		return nil, fmt.Errorf("go list %s: %v\n%s", pattern, err, strings.TrimSpace(stderr.String()))
	}
	return nil, fmt.Errorf("go list %s: %w", pattern, err)
}

func isDirectPassPackage(importPath, passesRoot string) bool {
	rel, ok := strings.CutPrefix(importPath, passesRoot+"/")
	return ok && rel != "" && !strings.Contains(rel, "/")
}

type analyzerExports struct {
	HasAnalyzer bool
	HasSuite    bool
}

func packageAnalyzerExports(dir string, goFiles []string) (analyzerExports, error) {
	var exports analyzerExports
	fset := token.NewFileSet()
	for _, fileName := range goFiles {
		file, err := parser.ParseFile(fset, filepath.Join(dir, fileName), nil, parser.SkipObjectResolution)
		if err != nil {
			return analyzerExports{}, err
		}
		exports.HasAnalyzer = exports.HasAnalyzer || fileDeclaresVar(file, "Analyzer")
		exports.HasSuite = exports.HasSuite || fileDeclaresVar(file, "Suite")
	}
	return exports, nil
}

func fileDeclaresVar(file *ast.File, varName string) bool {
	for _, decl := range file.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok || genDecl.Tok != token.VAR {
			continue
		}

		for _, spec := range genDecl.Specs {
			valueSpec, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, name := range valueSpec.Names {
				if name.Name == varName {
					return true
				}
			}
		}
	}
	return false
}

func renderMain(analyzers []analyzerPackage) ([]byte, error) {
	var b bytes.Buffer
	hasSuite := includesSuite(analyzers)

	fmt.Fprintln(&b, "// Code generated by go generate; DO NOT EDIT.")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "package main")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "import (")
	if hasSuite {
		fmt.Fprintln(&b, "\t\"golang.org/x/tools/go/analysis\"")
	}
	fmt.Fprintln(&b, "\t\"golang.org/x/tools/go/analysis/multichecker\"")
	fmt.Fprintln(&b)
	for _, analyzer := range analyzers {
		importAlias := ""
		if path.Base(analyzer.ImportPath) != analyzer.PackageName {
			importAlias = analyzer.PackageName + " "
		}
		fmt.Fprintf(&b, "\t%s%q\n", importAlias, analyzer.ImportPath)
	}
	fmt.Fprintln(&b, ")")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "func main() {")
	if hasSuite {
		fmt.Fprintln(&b, "\tanalyzers := []*analysis.Analyzer{")
		for _, analyzer := range analyzers {
			if analyzer.HasAnalyzer {
				fmt.Fprintf(&b, "\t\t%s.Analyzer,\n", analyzer.PackageName)
			}
		}
		fmt.Fprintln(&b, "\t}")
		for _, analyzer := range analyzers {
			if analyzer.HasSuite {
				fmt.Fprintf(&b, "\tanalyzers = append(analyzers, %s.Suite...)\n", analyzer.PackageName)
			}
		}
		fmt.Fprintln(&b, "\tmultichecker.Main(analyzers...)")
	} else {
		fmt.Fprintln(&b, "\tmultichecker.Main(")
		for _, analyzer := range analyzers {
			if analyzer.HasAnalyzer {
				fmt.Fprintf(&b, "\t\t%s.Analyzer,\n", analyzer.PackageName)
			}
		}
		fmt.Fprintln(&b, "\t)")
	}
	fmt.Fprintln(&b, "}")

	source, err := format.Source(b.Bytes())
	if err != nil {
		return nil, fmt.Errorf("format generated source: %w", err)
	}
	return source, nil
}

func includesSuite(analyzers []analyzerPackage) bool {
	for _, analyzer := range analyzers {
		if analyzer.HasSuite {
			return true
		}
	}
	return false
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "genpasses: "+format+"\n", args...)
	os.Exit(1)
}
