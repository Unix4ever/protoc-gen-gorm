package plugin

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"unicode"
	"unicode/utf8"

	"github.com/infobloxopen/protoc-gen-gorm/internal/version"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/runtime/protoimpl"
	"google.golang.org/protobuf/types/pluginpb"
)

// SupportedFeatures reports the set of supported protobuf language features.
var SupportedFeatures = uint64(pluginpb.CodeGeneratorResponse_FEATURE_PROTO3_OPTIONAL)

// GenerateVersionMarkers specifies whether to generate version markers.
var GenerateVersionMarkers = true

// Standard library dependencies.
const (
	base64Package  = protogen.GoImportPath("encoding/base64")
	mathPackage    = protogen.GoImportPath("math")
	reflectPackage = protogen.GoImportPath("reflect")
	sortPackage    = protogen.GoImportPath("sort")
	stringsPackage = protogen.GoImportPath("strings")
	syncPackage    = protogen.GoImportPath("sync")
	timePackage    = protogen.GoImportPath("time")
	utf8Package    = protogen.GoImportPath("unicode/utf8")
)

// Protobuf library dependencies.
//
// These are declared as an interface type so that they can be more easily
// patched to support unique build environments that impose restrictions
// on the dependencies of generated source code.
var (
	protoPackage         goImportPath = protogen.GoImportPath("google.golang.org/protobuf/proto")
	protoifacePackage    goImportPath = protogen.GoImportPath("google.golang.org/protobuf/runtime/protoiface")
	protoimplPackage     goImportPath = protogen.GoImportPath("google.golang.org/protobuf/runtime/protoimpl")
	protojsonPackage     goImportPath = protogen.GoImportPath("google.golang.org/protobuf/encoding/protojson")
	protoreflectPackage  goImportPath = protogen.GoImportPath("google.golang.org/protobuf/reflect/protoreflect")
	protoregistryPackage goImportPath = protogen.GoImportPath("google.golang.org/protobuf/reflect/protoregistry")
)

type goImportPath interface {
	String() string
	Ident(string) protogen.GoIdent
}

type Params struct {
	Engine                string
	Enums, Gateway, Quiet bool
}

func GenerateFile(gen *protogen.Plugin, file *protogen.File, params Params) *protogen.GeneratedFile {
	filename := file.GeneratedFilenamePrefix + ".gorm.pb.go"
	g := gen.NewGeneratedFile(filename, file.GoImportPath)
	f := newFileInfo(file)

	genGeneratedHeader(gen, g, f)

	// op := &OrmPlugin{}
	// response := command.GeneratePlugin(command.Read(), op, ".nope.pb.go")
	// op.CleanFiles(response)
	// g.P(op.String())

	g.P("package ", file.GoPackageName)
	g.P()

	// Emit a static check that enforces a minimum version of the proto package.
	if GenerateVersionMarkers {
		g.P("const (")
		g.P("// Verify that this generated code is sufficiently up-to-date.")
		g.P("_ = ", protoimplPackage.Ident("EnforceVersion"), "(", protoimpl.GenVersion, " - ", protoimplPackage.Ident("MinVersion"), ")")
		g.P("// Verify that runtime/protoimpl is sufficiently up-to-date.")
		g.P("_ = ", protoimplPackage.Ident("EnforceVersion"), "(", protoimplPackage.Ident("MaxVersion"), " - ", protoimpl.GenVersion, ")")
		g.P(")")
		g.P()
	}

	for i, imps := 0, f.Desc.Imports(); i < imps.Len(); i++ {
		genImport(gen, g, f, imps.Get(i))
	}

	return g

}

// genStandaloneComments prints all leading comments for a FileDescriptorProto
// location identified by the field number n.
func genStandaloneComments(g *protogen.GeneratedFile, f *fileInfo, n int32) {
	loc := f.Desc.SourceLocations().ByPath(protoreflect.SourcePath{n})
	for _, s := range loc.LeadingDetachedComments {
		g.P(protogen.Comments(s))
		g.P()
	}
	if s := loc.LeadingComments; s != "" {
		g.P(protogen.Comments(s))
		g.P()
	}
}

func genGeneratedHeader(gen *protogen.Plugin, g *protogen.GeneratedFile, f *fileInfo) {
	g.P("// Code generated by protoc-gen-gorm. DO NOT EDIT.")

	if GenerateVersionMarkers {
		g.P("// versions:")
		protocGenGormVersion := version.String()
		protocVersion := "(unknown)"
		if v := gen.Request.GetCompilerVersion(); v != nil {
			protocVersion = fmt.Sprintf("v%v.%v.%v", v.GetMajor(), v.GetMinor(), v.GetPatch())
			if s := v.GetSuffix(); s != "" {
				protocVersion += "-" + s
			}
		}
		g.P("// \tprotoc-gen-gorm ", protocGenGormVersion)
		g.P("// \tprotoc          ", protocVersion)
	}

	if f.Proto.GetOptions().GetDeprecated() {
		g.P("// ", f.Desc.Path(), " is a deprecated file.")
	} else {
		g.P("// source: ", f.Desc.Path())
	}
	g.P()
}

func genImport(gen *protogen.Plugin, g *protogen.GeneratedFile, f *fileInfo, imp protoreflect.FileImport) {
	impFile, ok := gen.FilesByPath[imp.Path()]
	if !ok {
		return
	}
	if impFile.GoImportPath == f.GoImportPath {
		// Don't generate imports or aliases for types in the same Go package.
		return
	}
	// Generate imports for all non-weak dependencies, even if they are not
	// referenced, because other code and tools depend on having the
	// full transitive closure of protocol buffer types in the binary.
	if !imp.IsWeak {
		g.Import(impFile.GoImportPath)
	}
	if !imp.IsPublic {
		return
	}

	// Generate public imports by generating the imported file, parsing it,
	// and extracting every symbol that should receive a forwarding declaration.
	impGen := GenerateFile(gen, impFile)
	impGen.Skip()
	b, err := impGen.Content()
	if err != nil {
		gen.Error(err)
		return
	}
	fset := token.NewFileSet()
	astFile, err := parser.ParseFile(fset, "", b, parser.ParseComments)
	if err != nil {
		gen.Error(err)
		return
	}
	genForward := func(tok token.Token, name string, expr ast.Expr) {
		// Don't import unexported symbols.
		r, _ := utf8.DecodeRuneInString(name)
		if !unicode.IsUpper(r) {
			return
		}
		// Don't import the FileDescriptor.
		if name == impFile.GoDescriptorIdent.GoName {
			return
		}
		// Don't import decls referencing a symbol defined in another package.
		// i.e., don't import decls which are themselves public imports:
		//
		//	type T = somepackage.T
		if _, ok := expr.(*ast.SelectorExpr); ok {
			return
		}
		g.P(tok, " ", name, " = ", impFile.GoImportPath.Ident(name))
	}
	g.P("// Symbols defined in public import of ", imp.Path(), ".")
	g.P()
	for _, decl := range astFile.Decls {
		switch decl := decl.(type) {
		case *ast.GenDecl:
			for _, spec := range decl.Specs {
				switch spec := spec.(type) {
				case *ast.TypeSpec:
					genForward(decl.Tok, spec.Name.Name, spec.Type)
				case *ast.ValueSpec:
					for i, name := range spec.Names {
						var expr ast.Expr
						if i < len(spec.Values) {
							expr = spec.Values[i]
						}
						genForward(decl.Tok, name.Name, expr)
					}
				case *ast.ImportSpec:
				default:
					panic(fmt.Sprintf("can't generate forward for spec type %T", spec))
				}
			}
		}
	}
	g.P()
}
