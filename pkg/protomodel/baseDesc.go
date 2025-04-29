// Copyright 2018 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package protomodel

import (
	"strings"

	"google.golang.org/protobuf/types/descriptorpb"
)

// CoreDesc is an interface abstracting the abilities shared by all descriptors
type CoreDesc interface {
	PackageDesc() *PackageDescriptor
	FileDesc() *FileDescriptor
	QualifiedName() []string
	IsHidden() bool
	Class() string
	Location() LocationDescriptor
}

// The common data for every descriptor in the model. This implements the coreDesc interface.
type baseDesc struct {
	loc    *descriptorpb.SourceCodeInfo_Location
	hidden bool
	cl     string
	file   *FileDescriptor
	name   []string
}

func newBaseDesc(file *FileDescriptor, path pathVector, qualifiedName []string) baseDesc {
	loc := file.find(path)
	cl := ""
	com := ""

	if loc != nil {
		var newCom string
		com = loc.GetLeadingComments()
		if com != "" {
			cl, newCom = getClass(com)
			if cl != "" {
				clone := *loc
				clone.LeadingComments = &newCom
				loc = &clone
			}
		} else {
			com = loc.GetTrailingComments()
			if com != "" {
				cl, newCom = getClass(com)
				if cl != "" {
					clone := *loc
					clone.TrailingComments = &newCom
					loc = &clone
				}
			}
		}
	}

	return baseDesc{
		file:   file,
		loc:    loc,
		hidden: strings.Contains(com, "$hide_from_docs") || strings.Contains(com, "[#not-implemented-hide:]"),
		cl:     cl,
		name:   qualifiedName,
	}
}

const class = "$class: "

func getClass(com string) (cl string, newCom string) {
	start := strings.Index(com, class)
	if start < 0 {
		return
	}

	name := start + len(class)
	end := strings.IndexAny(com[name:], " \t\n") + start + len(class)

	if end < 0 {
		newCom = com[:start]
		cl = com[name:]
	} else {
		newCom = com[:start] + com[end:]
		cl = com[name:end]
	}

	return
}

func (bd baseDesc) PackageDesc() *PackageDescriptor {
	return bd.file.Parent
}

func (bd baseDesc) FileDesc() *FileDescriptor {
	return bd.file
}

func (bd baseDesc) QualifiedName() []string {
	return bd.name
}

func (bd baseDesc) IsHidden() bool {
	return bd.hidden
}

func (bd baseDesc) Class() string {
	return bd.cl
}

func (bd baseDesc) Location() LocationDescriptor {
	return newLocationDescriptor(bd.loc, bd.file)
}

// // FieldDescriptor describes a field within a message
// type FieldDescriptor struct {
// 	*descriptorpb.FieldDescriptorProto
// 	Location   LocationDescriptor
// 	parent     CoreDesc
// 	FieldType  CoreDesc
// 	OneofIndex *int32
// }

// // Parent implements CoreDesc.Parent
// func (f *FieldDescriptor) Parent() CoreDesc {
// 	return f.parent
// }

// // PackageDesc implements CoreDesc.PackageDesc
// func (f *FieldDescriptor) PackageDesc() *PackageDescriptor {
// 	return f.parent.PackageDesc()
// }

// // IsRepeated returns whether the field is a repeated field
// func (f *FieldDescriptor) IsRepeated() bool {
// 	return f.Label != nil && *f.Label == descriptorpb.FieldDescriptorProto_LABEL_REPEATED
// }

// // FileDesc implements CoreDesc.FileDesc
// func (f *FieldDescriptor) FileDesc() *FileDescriptor {
// 	return f.parent.FileDesc()
// }

// // IsHidden implements CoreDesc.IsHidden
// func (f *FieldDescriptor) IsHidden() bool {
// 	// We don't have hidden field descriptors, so return false
// 	return false
// }

// // Class implements CoreDesc.Class
// func (f *FieldDescriptor) Class() string {
// 	// Field descriptors don't have a class, so return empty string
// 	return ""
// }

// // QualifiedName implements CoreDesc.QualifiedName
// func (f *FieldDescriptor) QualifiedName() []string {
// 	// Field descriptors don't have a qualified name, so return empty slice
// 	return []string{}
// }
