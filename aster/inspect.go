// Copyright 2018 henrylee2cn. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package aster

import (
	"bytes"
	"go/ast"
	"go/format"
	"go/token"
	"strings"
)

// LookupImports lookups the import info by package name.
func (f *File) LookupImports(currPkgName string) (imports []*Import, found bool) {
	for _, imp := range f.Imports {
		if imp.Name == currPkgName {
			imports = append(imports, imp)
			found = true
		}
	}
	return
}

// LookupPackages lookups the package object by package name.
// NOTE: Only lookup the parsed module.
func (f *File) LookupPackages(currPkgName string) (pkgs []*Package, found bool) {
	if f.pkg == nil || f.pkg.module == nil {
		return
	}
	imps, found := f.LookupImports(currPkgName)
	if !found {
		return
	}
	mod := f.pkg.module
	for _, imp := range imps {
		if p, ok := mod.Pkgs[imp.Name]; ok {
			pkgs = append(pkgs, p)
			found = true
		}
	}
	return
}

// LookupTypeInFile lookup Type by type name in current file.
func (f *File) LookupTypeInFile(name string) (t TypeNode, found bool) {
	for _, n := range f.Types {
		if name == n.Name() {
			return n, true
		}
	}
	return nil, false
}

// LookupTypeInModule lookup Type by type name in current module.
func (f *File) LookupTypeInModule(name string) (t TypeNode, found bool) {
	t, found = f.LookupTypeInPackage(name)
	if found {
		return
	}
	name = strings.TrimLeft(name, "*")
	// May be in the other module packages?
	a := strings.SplitN(name, ".", 2)
	if len(a) == 1 {
		a = []string{".", name}
	}
	pkgs, ok := f.LookupPackages(a[0])
	if !ok {
		return
	}
	for _, p := range pkgs {
		for _, v := range p.Files {
			t, found = v.LookupTypeInFile(a[1])
			if found {
				return
			}
		}
	}
	return
}

// LookupTypeInPackage lookup Type by type name in current package.
func (f *File) LookupTypeInPackage(name string) (t TypeNode, found bool) {
	if strings.Contains(name, ".") {
		return
	}
	name = strings.TrimLeft(name, "*")
	if f.pkg == nil {
		t, found = f.LookupTypeInFile(name)
		if found {
			return
		}
	} else {
		for _, v := range f.pkg.Files {
			t, found = v.LookupTypeInFile(name)
			if found {
				return
			}
		}
	}
	return
}

func (p *Package) collectNodes() {
	for _, f := range p.Files {
		f.collectNodes(false)
	}
	// Waiting for types ready to do method association
	for _, f := range p.Files {
		f.bindMethods()
	}
}

// Use the method if no other file in the same package,
// otherwise use *Package.collectNodes()
func (f *File) collectNodes(singleParsing bool) {
	f.Funcs = make(map[token.Pos]FuncNode)
	f.collectFuncs()

	f.Types = make(map[token.Pos]TypeNode)
	f.collectTypesOtherThanStruct()
	f.collectStructs()
	f.setStructFields()

	if singleParsing {
		f.bindMethods()
	}
}

func (f *File) collectFuncs() {
	collectFuncs := func(n ast.Node) bool {
		var t *FuncType
		var funcType *ast.FuncType
		switch x := n.(type) {
		case *ast.FuncLit:
			funcType = x.Type
			t = f.newFuncType(nil, nil, x, nil, nil, nil)
		case *ast.FuncDecl:
			funcType = x.Type
			var recv *FuncField
			if recvs := f.expandFuncFields(x.Recv); len(recvs) > 0 {
				recv = recvs[0]
			}
			t = f.newFuncType(
				&x.Name.Name,
				x.Doc,
				&ast.FuncLit{
					Type: x.Type,
					Body: x.Body,
				},
				recv,
				f.expandFuncFields(funcType.Params),
				f.expandFuncFields(funcType.Results),
			)
		default:
			return true
		}
		f.Funcs[t.Pos()] = t
		return true
	}
	ast.Inspect(f.File, collectFuncs)
}

func (f *File) collectTypeSpecs(fn func(*ast.TypeSpec, *ast.CommentGroup)) {
	ast.Inspect(f.File, func(n ast.Node) bool {
		if decl, ok := n.(*ast.GenDecl); ok {
			doc := decl.Doc
			for _, spec := range decl.Specs {
				if td, ok := spec.(*ast.TypeSpec); ok {
					if td.Doc != nil {
						doc = td.Doc
					}
					fn(td, doc)
				}
			}
		}
		return true
	})
}

func (f *File) collectTypesOtherThanStruct() {
	f.collectTypeSpecs(func(node *ast.TypeSpec, doc *ast.CommentGroup) {
		namePtr := &node.Name.Name
		var t TypeNode
		switch x := getElem(node.Type).(type) {
		case *ast.SelectorExpr:
			t = f.newAliasType(namePtr, doc, node.Assign, x)

		case *ast.Ident:
			t = f.newBasicOrAliasType(namePtr, doc, node.Assign, x)

		case *ast.ChanType:
			t = f.newChanType(namePtr, doc, node.Assign, x)

		case *ast.ArrayType:
			t = f.newListType(namePtr, doc, node.Assign, x)

		case *ast.MapType:
			t = f.newMapType(namePtr, doc, node.Assign, x)

		case *ast.InterfaceType:
			t = f.newInterfaceType(namePtr, doc, node.Assign, x)

		default:
			return
		}
		f.Types[t.Pos()] = t
	})
}

// collectStructs collects and maps structType nodes to their positions
func (f *File) collectStructs() {
	collectStructs := func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CompositeLit:
			t, ok := x.Type.(*ast.StructType)
			if !ok {
				return true
			}
			st := f.newStructType(nil, nil, -1, t)
			f.Types[st.Pos()] = st
		case *ast.GenDecl:
			for _, spec := range x.Specs {
				var assign = token.NoPos
				var t ast.Expr
				var structName *string
				var doc = x.Doc
				switch y := spec.(type) {
				case *ast.TypeSpec:
					if y.Type == nil {
						continue
					}
					assign = y.Assign
					structName = &y.Name.Name
					t = y.Type
					if y.Doc != nil {
						doc = y.Doc
					}
				case *ast.ValueSpec:
					assign = -1
					structName = &y.Names[0].Name
					t = y.Type
					if y.Doc != nil {
						doc = y.Doc
					}
				}
				z, ok := t.(*ast.StructType)
				if !ok {
					continue
				}
				st := f.newStructType(structName, doc, assign, z)
				f.Types[st.Pos()] = st
			}
		}
		return true
	}
	ast.Inspect(f.File, collectStructs)
}

func (f *File) setStructFields() {
	for _, t := range f.Types {
		s, ok := t.(*StructType)
		if !ok {
			continue
		}
		s.setFields()
	}
}

func (f *File) bindMethods() {
	for _, m := range f.Funcs {
		recv, found := m.Recv()
		if !found {
			continue
		}
		t, found := f.LookupTypeInPackage(recv.TypeName)
		if !found {
			continue
		}
		t.addMethod(m)
	}
}

// TODO maybe bug
func expandFields(fieldList *ast.FieldList) {
	if fieldList == nil {
		return
	}
	var list = make([]*ast.Field, 0, fieldList.NumFields())
	for _, g := range fieldList.List {
		list = append(list, g)
		if len(g.Names) > 1 {
			for _, name := range g.Names[1:] {
				list = append(list,
					&ast.Field{
						// Doc:     cloneCommentGroup(g.Doc),
						Names: []*ast.Ident{name},
						Type:  g.Type,
						Tag:   cloneBasicLit(g.Tag),
						// Comment: cloneCommentGroup(g.Comment),
					})
			}
			g.Names = g.Names[:1]
			// g.Doc = cloneCommentGroup(g.Doc)
			// g.Comment = cloneCommentGroup(g.Comment)
		}
	}
	fieldList.List = list
}

func (f *File) expandFuncFields(fieldList *ast.FieldList) (fields []*FuncField) {
	if fieldList != nil {
		for _, g := range fieldList.List {
			typeName := f.TryFormat(g.Type)
			m := len(g.Names)
			if m == 0 {
				fields = append(fields, &FuncField{
					TypeName: typeName,
				})
			} else {
				for _, name := range g.Names {
					fields = append(fields, &FuncField{
						Name:     name.Name,
						TypeName: typeName,
					})
				}
			}
		}
	}
	return
}

// Format format the node and returns the string.
func (f *File) Format(node ast.Node) (code string, err error) {
	var dst bytes.Buffer
	err = format.Node(&dst, f.FileSet, node)
	if err != nil {
		return
	}
	return dst.String(), nil
}

// TryFormat format the node and returns the string,
// returns the default string if fail.
func (f *File) TryFormat(node ast.Node, defaultValue ...string) string {
	code, err := f.Format(node)
	if err != nil && len(defaultValue) > 0 {
		return defaultValue[0]
	}
	return code
}

func getElem(e ast.Expr) ast.Expr {
	for {
		s, ok := e.(*ast.StarExpr)
		if ok {
			e = s.X
		} else {
			return e
		}
	}
}

func cloneBasicLit(b *ast.BasicLit) *ast.BasicLit {
	if b == nil {
		return nil
	}
	return &ast.BasicLit{
		Kind:  b.Kind,
		Value: b.Value,
	}
}

func cloneCommentGroup(c *ast.CommentGroup) *ast.CommentGroup {
	if c == nil {
		return nil
	}
	n := new(ast.CommentGroup)
	for _, v := range c.List {
		n.List = append(n.List, &ast.Comment{Text: v.Text})
	}
	return n
}
