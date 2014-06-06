package compiler

import (
	"bytes"
	"code.google.com/p/go.tools/go/gcimporter"
	"code.google.com/p/go.tools/go/types"
	"encoding/asn1"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/scanner"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var sizes32 = &types.StdSizes{WordSize: 4, MaxAlign: 8}
var reservedKeywords = make(map[string]bool)

func init() {
	for _, keyword := range []string{"abstract", "arguments", "boolean", "break", "byte", "case", "catch", "char", "class", "const", "continue", "debugger", "default", "delete", "do", "double", "else", "enum", "eval", "export", "extends", "false", "final", "finally", "float", "for", "function", "goto", "if", "implements", "import", "in", "instanceof", "int", "interface", "let", "long", "native", "new", "package", "private", "protected", "public", "return", "short", "static", "super", "switch", "synchronized", "this", "throw", "throws", "transient", "true", "try", "typeof", "var", "void", "volatile", "while", "with", "yield"} {
		reservedKeywords[keyword] = true
	}
}

type ErrorList []error

func (err ErrorList) Error() string {
	return err[0].Error()
}

type ImportCError struct{}

func (e *ImportCError) Error() string {
	return `importing "C" is not supported by GopherJS`
}

func Import(path string, mode build.ImportMode, archSuffix string) (*build.Package, error) {
	if path == "C" {
		return nil, &ImportCError{}
	}

	buildContext := &build.Context{
		GOROOT:    build.Default.GOROOT,
		GOPATH:    build.Default.GOPATH,
		GOOS:      build.Default.GOOS,
		GOARCH:    archSuffix,
		Compiler:  "gc",
		BuildTags: []string{"netgo"},
	}
	if path == "runtime" || path == "syscall" {
		buildContext.GOARCH = build.Default.GOARCH
		buildContext.InstallSuffix = archSuffix
	}
	pkg, err := buildContext.Import(path, "", mode)
	if path == "hash/crc32" {
		pkg.GoFiles = []string{"crc32.go", "crc32_generic.go"}
	}
	if pkg.IsCommand() {
		pkg.PkgObj = filepath.Join(pkg.BinDir, filepath.Base(pkg.ImportPath)+".js")
	}
	if _, err := os.Stat(pkg.PkgObj); os.IsNotExist(err) && strings.HasPrefix(pkg.PkgObj, build.Default.GOROOT) {
		// fall back to GOPATH
		gopathPkgObj := build.Default.GOPATH + pkg.PkgObj[len(build.Default.GOROOT):]
		if _, err := os.Stat(gopathPkgObj); err == nil {
			pkg.PkgObj = gopathPkgObj
		}
	}
	return pkg, err
}

func Parse(pkg *build.Package, fileSet *token.FileSet) ([]*ast.File, error) {
	var files []*ast.File
	replacedDeclNames := make(map[string]bool)
	funcName := func(d *ast.FuncDecl) string {
		if d.Recv == nil {
			return d.Name.Name
		}
		recv := d.Recv.List[0].Type
		if star, ok := recv.(*ast.StarExpr); ok {
			recv = star.X
		}
		return recv.(*ast.Ident).Name + "." + d.Name.Name
	}
	isTestPkg := strings.HasSuffix(pkg.ImportPath, "_test")
	importPath := pkg.ImportPath
	if isTestPkg {
		importPath = importPath[:len(importPath)-5]
	}
	if nativesPkg, err := Import("github.com/gopherjs/gopherjs/compiler/natives/"+importPath, 0, "js"); err == nil {
		names := nativesPkg.GoFiles
		if isTestPkg {
			names = nativesPkg.XTestGoFiles
		}
		for _, name := range names {
			file, err := parser.ParseFile(fileSet, filepath.Join(nativesPkg.Dir, name), nil, 0)
			if err != nil {
				panic(err)
			}
			for _, decl := range file.Decls {
				if d, ok := decl.(*ast.FuncDecl); ok {
					replacedDeclNames[funcName(d)] = true
				}
			}
			files = append(files, file)
		}
	}
	delete(replacedDeclNames, "init")

	var errList ErrorList
	for _, name := range pkg.GoFiles {
		if !filepath.IsAbs(name) {
			name = filepath.Join(pkg.Dir, name)
		}
		r, err := os.Open(name)
		if err != nil {
			return nil, err
		}
		file, err := parser.ParseFile(fileSet, name, r, 0)
		r.Close()
		if err != nil {
			if list, isList := err.(scanner.ErrorList); isList {
				for _, entry := range list {
					errList = append(errList, entry)
				}
				continue
			}
			errList = append(errList, err)
			continue
		}

		for _, decl := range file.Decls {
			if d, ok := decl.(*ast.FuncDecl); ok && replacedDeclNames[funcName(d)] {
				d.Name = ast.NewIdent("_")
			}
		}
		files = append(files, applyPatches(file, fileSet, pkg.ImportPath))
	}
	if errList != nil {
		return nil, errList
	}
	return files, nil
}

type Compiler struct {
	typesPackages map[string]*types.Package
}

func New() *Compiler {
	return &Compiler{map[string]*types.Package{"unsafe": types.Unsafe}}
}

func (t *Compiler) NewEmptyTypesPackage(path string) {
	t.typesPackages[path] = types.NewPackage(path, path)
}

func (t *Compiler) WriteProgramCode(pkgs []*Archive, mainPkgPath string, minify bool, w *SourceMapFilter) {
	declsByObject := make(map[string][]*Decl)
	var pendingDecls []*Decl
	for _, pkg := range pkgs {
		for i := range pkg.Declarations {
			d := &pkg.Declarations[i]
			if len(d.DceFilters) == 0 {
				pendingDecls = append(pendingDecls, d)
				continue
			}
			for _, f := range d.DceFilters {
				o := string(pkg.ImportPath) + ":" + string(f)
				declsByObject[o] = append(declsByObject[o], d)
			}
		}
	}

	for len(pendingDecls) != 0 {
		d := pendingDecls[len(pendingDecls)-1]
		pendingDecls = pendingDecls[:len(pendingDecls)-1]
		for _, dep := range d.DceDeps {
			o := string(dep)
			if decls, ok := declsByObject[o]; ok {
				delete(declsByObject, o)
				name := strings.Split(o, ":")[1]
				for _, d := range decls {
					for i, f := range d.DceFilters {
						if string(f) == name {
							d.DceFilters[i] = d.DceFilters[len(d.DceFilters)-1]
							d.DceFilters = d.DceFilters[:len(d.DceFilters)-1]
							break
						}
					}
					if len(d.DceFilters) == 0 {
						pendingDecls = append(pendingDecls, d)
					}
				}
			}
		}
	}

	w.Write([]byte("\"use strict\";\n(function() {\n\n"))
	w.Write(removeWhitespace([]byte(strings.TrimSpace(prelude)), minify))
	w.Write([]byte("\n"))

	// write packages
	for _, pkg := range pkgs {
		t.WritePkgCode(pkg, minify, w)
	}

	// write interfaces
	allTypeNames := []*types.TypeName{types.New("error").(*types.Named).Obj()}
	for _, pkg := range pkgs {
		scope := t.typesPackages[string(pkg.ImportPath)].Scope()
		for _, name := range scope.Names() {
			if typeName, isTypeName := scope.Lookup(name).(*types.TypeName); isTypeName {
				if _, notUsed := declsByObject[string(pkg.ImportPath)+":"+name]; !notUsed {
					allTypeNames = append(allTypeNames, typeName)
				}
			}
		}
	}
	for _, t := range allTypeNames {
		if in, isInterface := t.Type().Underlying().(*types.Interface); isInterface {
			if in.Empty() {
				continue
			}
			implementedBy := make(map[string]bool, 0)
			for _, other := range allTypeNames {
				otherType := other.Type()
				switch otherType.Underlying().(type) {
				case *types.Interface:
					// skip
				case *types.Struct:
					if types.AssignableTo(otherType, in) {
						implementedBy[fmt.Sprintf("$packages[\"%s\"].%s", other.Pkg().Path(), other.Name())] = true
					}
					if types.AssignableTo(types.NewPointer(otherType), in) {
						implementedBy[fmt.Sprintf("$packages[\"%s\"].%s.Ptr", other.Pkg().Path(), other.Name())] = true
					}
				default:
					if types.AssignableTo(otherType, in) {
						implementedBy[fmt.Sprintf("$packages[\"%s\"].%s", other.Pkg().Path(), other.Name())] = true
					}
					if types.AssignableTo(types.NewPointer(otherType), in) {
						implementedBy[fmt.Sprintf("$ptrType($packages[\"%s\"].%s)", other.Pkg().Path(), other.Name())] = true
					}
				}
			}
			list := make([]string, 0, len(implementedBy))
			for ref := range implementedBy {
				list = append(list, ref)
			}
			sort.Strings(list)
			var target string
			switch t.Name() {
			case "error":
				target = "$error"
			default:
				target = fmt.Sprintf("$packages[\"%s\"].%s", t.Pkg().Path(), t.Name())
			}
			w.Write(removeWhitespace([]byte(fmt.Sprintf("%s.implementedBy = [%s];\n", target, strings.Join(list, ", "))), minify))
		}
	}

	for _, pkg := range pkgs {
		w.Write([]byte("$packages[\"" + string(pkg.ImportPath) + "\"].init();\n"))
	}

	w.Write([]byte("$packages[\"" + mainPkgPath + "\"].main(function() {});\n\n})();\n"))
}

func (t *Compiler) WritePkgCode(pkg *Archive, minify bool, w *SourceMapFilter) {
	if w.MappingCallback != nil && pkg.FileSet != nil {
		w.fileSet = token.NewFileSet()
		if err := w.fileSet.Read(json.NewDecoder(bytes.NewReader(pkg.FileSet)).Decode); err != nil {
			panic(err)
		}
	}
	w.Write(removeWhitespace([]byte(fmt.Sprintf("$packages[\"%s\"] = (function() {\n", pkg.ImportPath)), minify))
	vars := []string{"$pkg = {}"}
	for _, imp := range pkg.Imports {
		vars = append(vars, fmt.Sprintf("%s = $packages[\"%s\"]", imp.VarName, imp.Path))
	}
	for _, d := range pkg.Declarations {
		if len(d.DceFilters) == 0 && d.Var != "" {
			vars = append(vars, d.Var)
		}
	}
	if len(vars) != 0 {
		w.Write(removeWhitespace([]byte(fmt.Sprintf("\tvar %s;\n", strings.Join(vars, ", "))), minify))
	}
	for _, d := range pkg.Declarations {
		if len(d.DceFilters) == 0 {
			w.Write(d.BodyCode)
		}
	}
	w.Write(removeWhitespace([]byte("\t$pkg.init = function() {\n"), minify))
	for _, d := range pkg.Declarations {
		if len(d.DceFilters) == 0 {
			w.Write(d.InitCode)
		}
	}
	w.Write(removeWhitespace([]byte("\t};\n\treturn $pkg;\n})();"), minify))
	w.Write([]byte("\n")) // keep this \n even when minified
}

func (t *Compiler) UnmarshalArchive(filename, id string, data []byte) (*Archive, error) {
	var a Archive
	_, err := asn1.Unmarshal(data, &a)
	if err != nil {
		return nil, err
	}

	pkg, err := gcimporter.ImportData(t.typesPackages, filename, id, bytes.NewReader(a.GcData))
	if err != nil {
		return nil, err
	}
	t.typesPackages[pkg.Path()] = pkg

	return &a, nil
}

func (t *Compiler) MarshalArchive(a *Archive) ([]byte, error) {
	return asn1.Marshal(*a)
}

type Archive struct {
	ImportPath   PkgPath
	GcData       []byte
	Dependencies []PkgPath
	Imports      []PkgImport
	Declarations []Decl
	Tests        []string
	FileSet      []byte
}

type PkgPath []byte // make asn1 happy

func (a *Archive) AddDependency(path string) {
	for _, dep := range a.Dependencies {
		if string(dep) == path {
			return
		}
	}
	a.Dependencies = append(a.Dependencies, PkgPath(path))
}

func (a *Archive) AddDependenciesOf(other *Archive) {
	for _, path := range other.Dependencies {
		a.AddDependency(string(path))
	}
}

type PkgImport struct {
	Path    PkgPath
	VarName string
}

type Decl struct {
	Var        string
	BodyCode   []byte
	InitCode   []byte
	DceFilters []DepId
	DceDeps    []DepId
}

type DepId []byte // make asn1 happy

type SourceMapFilter struct {
	Writer          io.Writer
	MappingCallback func(generatedLine, generatedColumn int, fileSet *token.FileSet, originalPos token.Pos)
	line            int
	column          int
	fileSet         *token.FileSet
}

func (f *SourceMapFilter) Write(p []byte) (n int, err error) {
	var n2 int
	for {
		i := bytes.IndexByte(p, '\b')
		w := p
		if i != -1 {
			w = p[:i]
		}

		n2, err = f.Writer.Write(w)
		n += n2
		for {
			i := bytes.IndexByte(w, '\n')
			if i == -1 {
				f.column += len(w)
				break
			}
			f.line++
			f.column = 0
			w = w[i+1:]
		}

		if err != nil || i == -1 {
			return
		}
		if f.MappingCallback != nil {
			f.MappingCallback(f.line+1, f.column, f.fileSet, token.Pos(binary.BigEndian.Uint32(p[i+1:i+5])))
		}
		p = p[i+5:]
		n += 5
	}
}
