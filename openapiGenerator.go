// Copyright 2019 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this currentFile except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path"
	"strings"
	"unicode"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/ghodss/yaml"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"
	kubemarkers "sigs.k8s.io/controller-tools/pkg/markers"

	"regexp"

	"github.com/solo-io/protoc-gen-openapi/pkg/markers"
	"github.com/solo-io/protoc-gen-openapi/pkg/protomodel"
)

var descriptionExclusionMarkers = []string{"$hide_from_docs", "$hide", "@exclude"}

// Some special types with predefined schemas.
// This is to catch cases where solo apis contain recursive definitions
// Normally these would result in stack-overflow errors when generating the open api schema
// The imperfect solution, is to just generate an empty object for these types
var specialSoloTypes = map[string]openapi3.Schema{
	"core.solo.io.Metadata": {
		Type: openapi3.TypeObject,
	},
	"google.protobuf.ListValue": *openapi3.NewArraySchema().WithItems(openapi3.NewObjectSchema()),
	"google.protobuf.Struct": {
		Type:       openapi3.TypeObject,
		Properties: make(map[string]*openapi3.SchemaRef),
		ExtensionProps: openapi3.ExtensionProps{
			Extensions: map[string]interface{}{
				"x-kubernetes-preserve-unknown-fields": true,
			},
		},
	},
	"google.protobuf.Any": {
		Type:       openapi3.TypeObject,
		Properties: make(map[string]*openapi3.SchemaRef),
		ExtensionProps: openapi3.ExtensionProps{
			Extensions: map[string]interface{}{
				"x-kubernetes-preserve-unknown-fields": true,
			},
		},
	},
	"google.protobuf.Value": {
		Properties: make(map[string]*openapi3.SchemaRef),
		ExtensionProps: openapi3.ExtensionProps{
			Extensions: map[string]interface{}{
				"x-kubernetes-preserve-unknown-fields": true,
			},
		},
	},
	"google.protobuf.BoolValue":   *openapi3.NewBoolSchema().WithNullable(),
	"google.protobuf.StringValue": *openapi3.NewStringSchema().WithNullable(),
	"google.protobuf.DoubleValue": *openapi3.NewFloat64Schema().WithNullable(),
	"google.protobuf.Int32Value":  *openapi3.NewIntegerSchema().WithNullable().WithMin(math.MinInt32).WithMax(math.MaxInt32),
	"google.protobuf.Int64Value":  *openapi3.NewIntegerSchema().WithNullable().WithMin(math.MinInt64).WithMax(math.MaxInt64),
	"google.protobuf.UInt32Value": *openapi3.NewIntegerSchema().WithNullable().WithMin(0).WithMax(math.MaxUint32),
	"google.protobuf.UInt64Value": *openapi3.NewIntegerSchema().WithNullable().WithMin(0).WithMax(math.MaxUint64),
	"google.protobuf.FloatValue":  *openapi3.NewFloat64Schema().WithNullable(),
	"google.protobuf.Duration":    *openapi3.NewStringSchema(),
	"google.protobuf.Empty":       *openapi3.NewObjectSchema().WithMaxProperties(0),
	"google.protobuf.Timestamp":   *openapi3.NewStringSchema().WithFormat("date-time"),
}

type openapiGenerator struct {
	buffer     bytes.Buffer
	model      *protomodel.Model
	perFile    bool
	singleFile bool
	yaml       bool
	useRef     bool

	// transient state as individual files are processed
	currentPackage             *protomodel.PackageDescriptor
	currentFrontMatterProvider *protomodel.FileDescriptor

	messages map[string]*protomodel.MessageDescriptor

	// @solo.io customizations to limit length of generated descriptions
	descriptionConfiguration *DescriptionConfiguration

	// @solo.io customization to support enum validation schemas with int or string values
	// we need to support this since some controllers marshal enums as integers and others as strings
	enumAsIntOrString bool

	// @solo.io customizations to define schemas for certain messages
	customSchemasByMessageName map[string]openapi3.Schema

	// If set to true, OpenAPI schema will include schema to emulate behavior of protobuf oneof fields
	protoOneof bool

	// If set to true, native OpenAPI integer scehmas will be used for integer types instead of Solo wrappers
	// that add Kubernetes extension headers to the schema to treat int as strings.
	intNative bool

	markerRegistry *markers.Registry

	// If set to true, kubebuilder markers and validations such as PreserveUnknownFields, MinItems, default, and all CEL rules will be omitted from the OpenAPI schema.
	// The Type and Required markers will be maintained.
	disableKubeMarkers bool

	// when set, this list of substrings will be used to identify kubebuilder markers to ignore. When multiple are
	// supplied, this will function as a logical OR i.e. any rule which contains a provided substring will be ignored
	ignoredKubeMarkerSubstrings []string

	// If set to true, proto3 optional fields will be properly handled
	supportProto3Optional bool
}

type DescriptionConfiguration struct {
	// Whether or not to include a description in the generated open api schema
	IncludeDescriptionInSchema bool

	// Whether or not the description for properties should be allowed to span multiple lines
	MultilineDescription bool
}

func newOpenAPIGenerator(
	model *protomodel.Model,
	perFile bool,
	singleFile bool,
	yaml bool,
	useRef bool,
	descriptionConfiguration *DescriptionConfiguration,
	enumAsIntOrString bool,
	messagesWithEmptySchema []string,
	protoOneof bool,
	intNative bool,
	disableKubeMarkers bool,
	ignoredKubeMarkers []string,
	supportProto3Optional bool,
) *openapiGenerator {
	mRegistry, err := markers.NewRegistry()
	if err != nil {
		log.Panicf("error initializing marker registry: %v", err)
	}
	return &openapiGenerator{
		model:                       model,
		perFile:                     perFile,
		singleFile:                  singleFile,
		yaml:                        yaml,
		useRef:                      useRef,
		descriptionConfiguration:    descriptionConfiguration,
		enumAsIntOrString:           enumAsIntOrString,
		customSchemasByMessageName:  buildCustomSchemasByMessageName(messagesWithEmptySchema),
		protoOneof:                  protoOneof,
		intNative:                   intNative,
		markerRegistry:              mRegistry,
		disableKubeMarkers:          disableKubeMarkers,
		ignoredKubeMarkerSubstrings: ignoredKubeMarkers,
		supportProto3Optional:       supportProto3Optional,
	}
}

// buildCustomSchemasByMessageName name returns a mapping of message name to a pre-defined openapi schema
// It includes:
//  1. `specialSoloTypes`, a set of pre-defined schemas
//  2. `messagesWithEmptySchema`, a list of messages that are injected at runtime that should contain
//     and empty schema which accepts and preserves all fields
func buildCustomSchemasByMessageName(messagesWithEmptySchema []string) map[string]openapi3.Schema {
	schemasByMessageName := make(map[string]openapi3.Schema)

	// Initialize the hard-coded values
	for name, schema := range specialSoloTypes {
		schemasByMessageName[name] = schema
	}

	// Add the messages that were injected at runtime
	for _, messageName := range messagesWithEmptySchema {
		emptyMessage := openapi3.Schema{
			Type:       openapi3.TypeObject,
			Properties: make(map[string]*openapi3.SchemaRef),
			ExtensionProps: openapi3.ExtensionProps{
				Extensions: map[string]interface{}{
					"x-kubernetes-preserve-unknown-fields": true,
				},
			},
		}
		schemasByMessageName[messageName] = emptyMessage
	}

	return schemasByMessageName
}

func (g *openapiGenerator) generateOutput(filesToGen map[*protomodel.FileDescriptor]bool) (*pluginpb.CodeGeneratorResponse, error) {
	response := pluginpb.CodeGeneratorResponse{}

	// Set supported features to include proto3 optional
	// FEATURE_PROTO3_OPTIONAL is 1 << 5 = 32 (according to protobuf spec)
	supportedFeatures := uint64(pluginpb.CodeGeneratorResponse_FEATURE_PROTO3_OPTIONAL)
	response.SupportedFeatures = &supportedFeatures

	if g.singleFile {
		g.generateSingleFileOutput(filesToGen, &response)
	} else {
		for _, pkg := range g.model.Packages {
			g.currentPackage = pkg

			// anything to output for this package?
			count := 0
			for _, file := range pkg.Files {
				if _, ok := filesToGen[file]; ok {
					count++
				}
			}

			if count > 0 {
				if g.perFile {
					g.generatePerFileOutput(filesToGen, pkg, &response)
				} else {
					g.generatePerPackageOutput(filesToGen, pkg, &response)
				}
			}
		}
	}

	return &response, nil
}

func (g *openapiGenerator) getFileContents(file *protomodel.FileDescriptor,
	messages map[string]*protomodel.MessageDescriptor,
	enums map[string]*protomodel.EnumDescriptor,
	services map[string]*protomodel.ServiceDescriptor,
) {
	for _, m := range file.AllMessages {
		messages[g.relativeName(m)] = m
	}

	for _, e := range file.AllEnums {
		enums[g.relativeName(e)] = e
	}

	for _, s := range file.Services {
		services[g.relativeName(s)] = s
	}
}

func (g *openapiGenerator) generatePerFileOutput(filesToGen map[*protomodel.FileDescriptor]bool, pkg *protomodel.PackageDescriptor,
	response *pluginpb.CodeGeneratorResponse,
) {
	for _, file := range pkg.Files {
		if _, ok := filesToGen[file]; ok {
			g.currentFrontMatterProvider = file
			messages := make(map[string]*protomodel.MessageDescriptor)
			enums := make(map[string]*protomodel.EnumDescriptor)
			services := make(map[string]*protomodel.ServiceDescriptor)

			g.getFileContents(file, messages, enums, services)
			filename := path.Base(file.GetName())
			extension := path.Ext(filename)
			name := filename[0 : len(filename)-len(extension)]

			rf := g.generateFile(name, file, messages, enums, services)
			response.File = append(response.File, &rf)
		}
	}
}

func (g *openapiGenerator) generateSingleFileOutput(filesToGen map[*protomodel.FileDescriptor]bool, response *pluginpb.CodeGeneratorResponse) {
	messages := make(map[string]*protomodel.MessageDescriptor)
	enums := make(map[string]*protomodel.EnumDescriptor)
	services := make(map[string]*protomodel.ServiceDescriptor)

	for file, ok := range filesToGen {
		if ok {
			g.getFileContents(file, messages, enums, services)
		}
	}

	rf := g.generateFile("openapiv3", &protomodel.FileDescriptor{}, messages, enums, services)
	response.File = []*pluginpb.CodeGeneratorResponse_File{&rf}
}

func (g *openapiGenerator) generatePerPackageOutput(filesToGen map[*protomodel.FileDescriptor]bool, pkg *protomodel.PackageDescriptor,
	response *pluginpb.CodeGeneratorResponse,
) {
	// We need to produce a file for this package.

	// Decide which types need to be included in the generated file.
	// This will be all the types in the fileToGen input files, along with any
	// dependent types which are located in packages that don't have
	// a known location on the web.
	messages := make(map[string]*protomodel.MessageDescriptor)
	enums := make(map[string]*protomodel.EnumDescriptor)
	services := make(map[string]*protomodel.ServiceDescriptor)

	g.currentFrontMatterProvider = pkg.FileDesc()

	for _, file := range pkg.Files {
		if _, ok := filesToGen[file]; ok {
			g.getFileContents(file, messages, enums, services)
		}
	}

	rf := g.generateFile(pkg.Name, pkg.FileDesc(), messages, enums, services)
	response.File = append(response.File, &rf)
}

// Generate an OpenAPI spec for a collection of cross-linked files.
func (g *openapiGenerator) generateFile(name string,
	pkg *protomodel.FileDescriptor,
	messages map[string]*protomodel.MessageDescriptor,
	enums map[string]*protomodel.EnumDescriptor,
	services map[string]*protomodel.ServiceDescriptor,
) pluginpb.CodeGeneratorResponse_File {
	g.messages = messages

	allSchemas := make(map[string]*openapi3.SchemaRef)

	// Generate all message schemas
	for _, message := range messages {
		// we generate the top-level messages here and the nested messages are generated
		// inside each top-level message.
		if message.Parent == nil {
			g.generateMessage(message, allSchemas)
		}
	}

	for _, enum := range enums {
		// when there is no parent to the enum.
		if len(enum.QualifiedName()) == 1 {
			g.generateEnum(enum, allSchemas)
		}
	}

	var version string
	var description string
	// only get the API version when generate per package or per file,
	// as we cannot guarantee all protos in the input are the same version.
	if !g.singleFile {
		if g.currentFrontMatterProvider != nil && g.currentFrontMatterProvider.Matter.Description != "" {
			description = g.currentFrontMatterProvider.Matter.Description
		} else if pd := g.generateDescription(g.currentPackage); pd != "" {
			description = pd
		} else {
			description = "OpenAPI Spec for Solo APIs."
		}
		// derive the API version from the package name
		// which is a convention for Istio APIs.
		var p string
		if pkg != nil {
			p = pkg.GetPackage()
		} else {
			p = name
		}
		s := strings.Split(p, ".")
		version = s[len(s)-1]
	} else {
		description = "OpenAPI Spec for Solo APIs."
	}

	c := openapi3.NewComponents()
	c.Schemas = allSchemas

	// Generate paths from services if any
	paths := g.generatePaths(services, allSchemas)

	// add the openapi object required by the spec.
	o := openapi3.T{
		OpenAPI: "3.0.1",
		Info: &openapi3.Info{
			Title:   description,
			Version: version,
		},
		Components: c,
		Paths:      paths,
	}

	g.buffer.Reset()
	var filename *string
	if g.yaml {
		b, err := yaml.Marshal(o)
		if err != nil {
			fmt.Fprintf(os.Stderr, "unable to marshall the output of %v to yaml", name)
		}
		filename = proto.String(name + ".yaml")
		g.buffer.Write(b)
	} else {
		b, err := json.MarshalIndent(o, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "unable to marshall the output of %v to json", name)
		}
		filename = proto.String(name + ".json")
		g.buffer.Write(b)
	}

	return pluginpb.CodeGeneratorResponse_File{
		Name:    filename,
		Content: proto.String(g.buffer.String()),
	}
}

func (g *openapiGenerator) generateMessage(message *protomodel.MessageDescriptor, allSchemas map[string]*openapi3.SchemaRef) {
	if o := g.generateMessageSchema(message); o != nil {
		allSchemas[g.absoluteName(message)] = o.NewRef()
	}
}

func (g *openapiGenerator) generateSoloMessageSchema(message *protomodel.MessageDescriptor, customSchema *openapi3.Schema) *openapi3.Schema {
	o := customSchema
	o.Description = g.generateDescription(message)

	return o
}

func (g *openapiGenerator) generateSoloInt64Schema() *openapi3.Schema {
	schema := openapi3.NewInt64Schema()
	schema.ExtensionProps = openapi3.ExtensionProps{
		Extensions: map[string]interface{}{
			"x-kubernetes-int-or-string": true,
		},
	}

	return schema
}

// isProto3Optional checks if a field is explicitly marked as optional in proto3
func isProto3Optional(field *protomodel.FieldDescriptor) bool {
	// Access the raw proto field
	if field == nil || field.FieldDescriptorProto == nil {
		return false
	}

	proto3Optional := field.GetProto3Optional()
	return proto3Optional
}

func (g *openapiGenerator) generateMessageSchema(message *protomodel.MessageDescriptor) *openapi3.Schema {
	// skip MapEntry message because we handle map using the map's repeated field.
	if message.GetOptions().GetMapEntry() {
		return nil
	}
	o := openapi3.NewObjectSchema()
	o.Description = g.generateDescription(message)
	msgRules := g.validationRules(message)
	g.mustApplyRulesToSchema(msgRules, o, markers.TargetType)

	oneOfFields := make(map[int32][]string)
	var requiredFields []string
	for _, field := range message.Fields {
		repeated := field.IsRepeated()
		fieldName := g.fieldName(field)
		fieldDesc := g.generateDescription(field)
		fieldRules := g.validationRules(field)

		// If the field is a oneof, we need to add the oneof property to the schema
		if field.OneofIndex != nil {
			idx := *field.OneofIndex
			oneOfFields[idx] = append(oneOfFields[idx], fieldName)
		}

		// Check if field should be required
		isRequired := g.markerRegistry.IsRequired(fieldRules)

		// If proto3 optional support is enabled, check if the field is marked as optional
		if g.supportProto3Optional && isRequired && field.FieldDescriptorProto != nil {
			// Skip adding to required fields if it's marked as proto3 optional
			if field.Proto3Optional != nil && *field.Proto3Optional {
				isRequired = false
			}
		}

		if isRequired {
			requiredFields = append(requiredFields, fieldName)
		}

		schemaType := g.markerRegistry.GetSchemaType(fieldRules, markers.TargetField)
		if schemaType != "" {
			tmp := getSoloSchemaForMarkerType(schemaType)
			schema := getSchemaIfRepeated(&tmp, repeated)
			schema.Description = fieldDesc
			g.mustApplyRulesToSchema(fieldRules, schema, markers.TargetField)
			o.WithProperty(fieldName, schema)
			continue
		}

		sr := g.fieldTypeRef(field)
		g.mustApplyRulesToSchema(fieldRules, sr.Value, markers.TargetField)
		o.WithProperty(fieldName, sr.Value)
	}

	if len(requiredFields) > 0 {
		o.Required = requiredFields
	}

	if g.protoOneof {
		// Add protobuf oneof schema for this message
		oneOfs := make([][]*openapi3.Schema, len(oneOfFields))
		for idx := range oneOfFields {
			// oneOfSchemas is a collection (not and required schemas) that should be assigned to the schemas's oneOf field
			oneOfSchemas := newProtoOneOfSchema(oneOfFields[idx]...)
			oneOfs[idx] = append(oneOfs[idx], oneOfSchemas...)
		}

		switch len(oneOfs) {
		case 0:
			// no oneof fields
		case 1:
			o.OneOf = getSchemaRefs(oneOfs[0]...)
		default:
			// Wrap collected OneOf refs with AllOf schema
			for _, schemas := range oneOfs {
				oneOfRef := openapi3.NewOneOfSchema(schemas...)
				o.AllOf = append(o.AllOf, oneOfRef.NewRef())
			}
		}
	}

	return o
}

func getSoloSchemaForMarkerType(t markers.Type) openapi3.Schema {
	switch t {
	case markers.TypeObject:
		return specialSoloTypes["google.protobuf.Struct"]
	case markers.TypeValue:
		return specialSoloTypes["google.protobuf.Value"]
	default:
		log.Panicf("unexpected schema type %v", t)
		return openapi3.Schema{}
	}
}

func getSchemaRefs(schemas ...*openapi3.Schema) openapi3.SchemaRefs {
	var refs openapi3.SchemaRefs
	for _, schema := range schemas {
		refs = append(refs, schema.NewRef())
	}
	return refs
}

func getSchemaIfRepeated(schema *openapi3.Schema, repeated bool) *openapi3.Schema {
	if repeated {
		schema = openapi3.NewArraySchema().WithItems(schema)
	}
	return schema
}

// newProtoOneOfSchema returns a schema that can be used to represent a collection of fields
// that must be encoded as a oneOf in OpenAPI.
// For e.g., if the fields x and y are a part of a proto oneof, then they can be represented as
// follows, such that only one of x or y is required and specifying neither is also acceptable.
//
//	{
//		"not": {
//			"anyOf": [
//				{
//					"required": [
//						"x"
//					]
//				},
//				{
//					"required": [
//						"y"
//					]
//				}
//			]
//		}
//	},
//	{
//		"required": [
//			"x"
//		]
//	},
//	{
//		"required": [
//			"y"
//		]
//	}
func newProtoOneOfSchema(fields ...string) []*openapi3.Schema {
	fieldSchemas := make([]*openapi3.Schema, len(fields))
	for i, field := range fields {
		schema := openapi3.NewSchema()
		schema.Required = []string{field}
		fieldSchemas[i] = schema
	}
	// convert fieldSchema to a oneOf schema
	anyOfSchema := openapi3.NewAnyOfSchema(fieldSchemas...)
	notAnyOfSchema := openapi3.NewSchema()
	notAnyOfSchema.Not = anyOfSchema.NewRef()

	allOneOfSchemas := make([]*openapi3.Schema, len(fieldSchemas)+1)
	allOneOfSchemas[0] = notAnyOfSchema
	for i, fieldSchema := range fieldSchemas {
		allOneOfSchemas[i+1] = fieldSchema
	}
	return allOneOfSchemas
}

func (g *openapiGenerator) generateEnum(enum *protomodel.EnumDescriptor, allSchemas map[string]*openapi3.SchemaRef) {
	o := g.generateEnumSchema(enum)
	allSchemas[g.absoluteName(enum)] = o.NewRef()
}

func (g *openapiGenerator) generateEnumSchema(enum *protomodel.EnumDescriptor) *openapi3.Schema {
	/**
	  The out of the box solution created an enum like:
	  	enum:
	  	- - option_a
	  	  - option_b
	  	  - option_c

	  Instead, what we want is:
	  	enum:
	  	- option_a
	  	- option_b
	  	- option_c
	*/
	o := openapi3.NewStringSchema()
	o.Description = g.generateDescription(enum)

	// If the schema should be int or string, mark it as such
	if g.enumAsIntOrString {
		o.ExtensionProps = openapi3.ExtensionProps{
			Extensions: map[string]interface{}{
				"x-kubernetes-int-or-string": true,
			},
		}
		return o
	}

	// otherwise, return define the expected string values
	values := enum.GetValue()
	for _, v := range values {
		o.Enum = append(o.Enum, v.GetName())
	}
	o.Type = "string"

	return o
}

func (g *openapiGenerator) absoluteName(desc protomodel.CoreDesc) string {
	typeName := protomodel.DottedName(desc)
	return desc.PackageDesc().Name + "." + typeName
}

// converts the first section of the leading comment or the description of the proto
// to a single line of description.
func (g *openapiGenerator) generateDescription(desc protomodel.CoreDesc) string {
	if g.descriptionConfiguration.MultilineDescription {
		return g.generateMultiLineDescription(desc)
	}

	if !g.descriptionConfiguration.IncludeDescriptionInSchema {
		return ""
	}

	c := strings.TrimSpace(desc.Location().GetLeadingComments())
	t := strings.Split(c, "\n\n")[0]
	// omit the comment that starts with `$`.
	if strings.HasPrefix(t, "$") {
		return ""
	}

	return strings.Join(strings.Fields(t), " ")
}

func (g *openapiGenerator) generateMultiLineDescription(desc protomodel.CoreDesc) string {
	if !g.descriptionConfiguration.IncludeDescriptionInSchema {
		return ""
	}
	comments, _ := g.parseComments(desc)
	return comments
}

func (g *openapiGenerator) mustApplyRulesToSchema(
	rules []string,
	o *openapi3.Schema,
	target kubemarkers.TargetType,
) {
	if g.disableKubeMarkers {
		return
	}
	g.markerRegistry.MustApplyRulesToSchema(rules, o, target)
}

func (g *openapiGenerator) validationRules(desc protomodel.CoreDesc) []string {
	_, validationRules := g.parseComments(desc)
	return validationRules
}

func (g *openapiGenerator) parseComments(desc protomodel.CoreDesc) (comments string, validationRules []string) {
	c := strings.TrimSpace(desc.Location().GetLeadingComments())
	blocks := strings.Split(c, "\n\n")

	var ignoredKubeMarkersRegexp *regexp.Regexp
	if len(g.ignoredKubeMarkerSubstrings) > 0 {
		ignoredKubeMarkersRegexp = regexp.MustCompile(
			fmt.Sprintf("(?:%s)", strings.Join(g.ignoredKubeMarkerSubstrings, "|")),
		)
	}

	var sb strings.Builder
	for i, block := range blocks {
		if shouldNotRenderDesc(strings.TrimSpace(block)) {
			continue
		}
		if i > 0 {
			sb.WriteString("\n\n")
		}
		var blockSb strings.Builder
		lines := strings.Split(block, "\n")
		for i, line := range lines {
			if i > 0 {
				blockSb.WriteString("\n")
			}
			l := strings.TrimSpace(line)
			if shouldNotRenderDesc(l) {
				continue
			}

			if strings.HasPrefix(l, markers.Kubebuilder) {
				if isIgnoredKubeMarker(ignoredKubeMarkersRegexp, l) {
					continue
				}

				validationRules = append(validationRules, l)
				continue
			}
			if len(line) > 0 && line[0] == ' ' {
				line = line[1:]
			}
			blockSb.WriteString(strings.TrimRight(line, " "))
		}

		block = blockSb.String()
		sb.WriteString(block)
	}

	comments = strings.TrimSpace(sb.String())
	return
}

func shouldNotRenderDesc(desc string) bool {
	desc = strings.TrimSpace(desc)
	for _, marker := range descriptionExclusionMarkers {
		if strings.HasPrefix(desc, marker) {
			return true
		}
	}
	return false
}

func (g *openapiGenerator) fieldType(field *protomodel.FieldDescriptor) *openapi3.Schema {
	var schema *openapi3.Schema
	var isMap bool
	switch *field.Type {
	case descriptorpb.FieldDescriptorProto_TYPE_FLOAT, descriptorpb.FieldDescriptorProto_TYPE_DOUBLE:
		schema = openapi3.NewFloat64Schema()

	case descriptorpb.FieldDescriptorProto_TYPE_INT32, descriptorpb.FieldDescriptorProto_TYPE_SINT32, descriptorpb.FieldDescriptorProto_TYPE_SFIXED32:
		schema = openapi3.NewInt32Schema()

	case descriptorpb.FieldDescriptorProto_TYPE_INT64, descriptorpb.FieldDescriptorProto_TYPE_SINT64,
		descriptorpb.FieldDescriptorProto_TYPE_SFIXED64, descriptorpb.FieldDescriptorProto_TYPE_FIXED64:
		if g.intNative {
			schema = openapi3.NewInt64Schema()
		} else {
			schema = g.generateSoloInt64Schema()
		}

	case descriptorpb.FieldDescriptorProto_TYPE_FIXED32:
		schema = openapi3.NewInt32Schema()

	case descriptorpb.FieldDescriptorProto_TYPE_UINT32:
		schema = openapi3.NewIntegerSchema().WithMin(0).WithMax(math.MaxUint32)

	case descriptorpb.FieldDescriptorProto_TYPE_UINT64:
		if g.intNative {
			// we don't set the max here beacause it is too large to be represented without scientific notation
			// in YAML format
			schema = openapi3.NewIntegerSchema().WithMin(0).WithFormat("uint64")
		} else {
			schema = g.generateSoloInt64Schema()
		}

	case descriptorpb.FieldDescriptorProto_TYPE_BOOL:
		schema = openapi3.NewBoolSchema()

	case descriptorpb.FieldDescriptorProto_TYPE_STRING:
		schema = openapi3.NewStringSchema()

	case descriptorpb.FieldDescriptorProto_TYPE_MESSAGE:
		msg := field.FieldType.(*protomodel.MessageDescriptor)
		if soloSchema, ok := g.customSchemasByMessageName[g.absoluteName(msg)]; ok {
			// Allow for defining special Solo types
			schema = g.generateSoloMessageSchema(msg, &soloSchema)
		} else if msg.GetOptions().GetMapEntry() {
			isMap = true
			sr := g.fieldTypeRef(msg.Fields[1])
			if g.useRef && sr.Ref != "" {
				schema = openapi3.NewObjectSchema()
				// in `$ref`, the value of the schema is not in the output.
				sr.Value = nil
				schema.AdditionalProperties = sr
			} else {
				schema = openapi3.NewObjectSchema().WithAdditionalProperties(sr.Value)
			}
		} else {
			schema = g.generateMessageSchema(msg)
		}

	case descriptorpb.FieldDescriptorProto_TYPE_BYTES:
		schema = openapi3.NewBytesSchema()

	case descriptorpb.FieldDescriptorProto_TYPE_ENUM:
		enum := field.FieldType.(*protomodel.EnumDescriptor)
		schema = g.generateEnumSchema(enum)
	}

	if field.IsRepeated() && !isMap {
		schema = openapi3.NewArraySchema().WithItems(schema)
	}

	if schema != nil {
		schema.Description = g.generateDescription(field)
	}

	return schema
}

// fieldTypeRef generates the `$ref` in addition to the schema for a field.
func (g *openapiGenerator) fieldTypeRef(field *protomodel.FieldDescriptor) *openapi3.SchemaRef {
	s := g.fieldType(field)
	var ref string
	if *field.Type == descriptorpb.FieldDescriptorProto_TYPE_MESSAGE {
		msg := field.FieldType.(*protomodel.MessageDescriptor)
		// only generate `$ref` for top level messages.
		if _, ok := g.messages[g.relativeName(field.FieldType)]; ok && msg.Parent == nil {
			ref = fmt.Sprintf("#/components/schemas/%v", g.absoluteName(field.FieldType))
		}
	}
	return openapi3.NewSchemaRef(ref, s)
}

func (g *openapiGenerator) fieldName(field *protomodel.FieldDescriptor) string {
	return field.GetJsonName()
}

func (g *openapiGenerator) relativeName(desc protomodel.CoreDesc) string {
	typeName := protomodel.DottedName(desc)
	if desc.PackageDesc() == g.currentPackage {
		return typeName
	}

	return desc.PackageDesc().Name + "." + typeName
}

func isIgnoredKubeMarker(regexp *regexp.Regexp, l string) bool {
	if regexp == nil {
		return false
	}

	return regexp.MatchString(l)
}

// generatePaths creates OpenAPI paths from service descriptors with HTTP annotations
func (g *openapiGenerator) generatePaths(services map[string]*protomodel.ServiceDescriptor, schemas map[string]*openapi3.SchemaRef) openapi3.Paths {
	paths := make(openapi3.Paths)

	for _, service := range services {
		for _, method := range service.Methods {
			// Find HTTP rules in method options
			options := method.GetOptions()
			if options == nil {
				continue
			}

			// HTTP rules from method options
			var httpPath string
			var httpMethod string
			var httpBody string

			// First check if the method has a comment that contains http path and method
			// Looking for pattern like: @http.get "/path" or similar annotations
			comments, _ := g.parseComments(method)

			// Extract HTTP details from options
			// The HTTP rule can be:
			// - Google API HTTP annotations (extension 72295728)
			// - Comments with HTTP path and method

			// Manual inspection of method options for HTTP rules (google.api.http)
			// Extract field number 72295728 which is the http option in google.api.annotations

			// For proto MethodOptions, HttpRule is in extension with tag 72295728
			// We need to inspect options for this extension field
			for _, uninterpreted := range options.GetUninterpretedOption() {
				if uninterpreted.GetName() != nil && len(uninterpreted.GetName()) > 0 {
					for _, namePart := range uninterpreted.GetName() {
						// Check if this is the google.api.http extension
						if namePart.GetNamePart() == "google.api.http" || namePart.GetNamePart() == "(google.api.http)" {
							// This could contain the HTTP rule
							// Try to extract path and method
							aggregate := uninterpreted.GetAggregateValue()
							if strings.Contains(aggregate, "get:") {
								httpMethod = "GET"
								parts := strings.Split(aggregate, "get:")
								if len(parts) > 1 {
									httpPath = strings.TrimSpace(parts[1])
									httpPath = strings.Trim(httpPath, "\"")
								}
							} else if strings.Contains(aggregate, "post:") {
								httpMethod = "POST"
								parts := strings.Split(aggregate, "post:")
								if len(parts) > 1 {
									httpPath = strings.TrimSpace(parts[1])
									httpPath = strings.Trim(httpPath, "\"")
								}
							} else if strings.Contains(aggregate, "put:") {
								httpMethod = "PUT"
								parts := strings.Split(aggregate, "put:")
								if len(parts) > 1 {
									httpPath = strings.TrimSpace(parts[1])
									httpPath = strings.Trim(httpPath, "\"")
								}
							} else if strings.Contains(aggregate, "delete:") {
								httpMethod = "DELETE"
								parts := strings.Split(aggregate, "delete:")
								if len(parts) > 1 {
									httpPath = strings.TrimSpace(parts[1])
									httpPath = strings.Trim(httpPath, "\"")
								}
							} else if strings.Contains(aggregate, "patch:") {
								httpMethod = "PATCH"
								parts := strings.Split(aggregate, "patch:")
								if len(parts) > 1 {
									httpPath = strings.TrimSpace(parts[1])
									httpPath = strings.Trim(httpPath, "\"")
								}
							}

							// Extract body field if present
							if strings.Contains(aggregate, "body:") {
								parts := strings.Split(aggregate, "body:")
								if len(parts) > 1 {
									httpBody = strings.TrimSpace(parts[1])
									httpBody = strings.Trim(httpBody, "\"")
								}
							}
						}
					}
				}
			}

			// Look in comments for HTTP annotations if not found in options
			// Format: @http.get("/path") or @http.post("/path", body="*")
			if httpMethod == "" && httpPath == "" {
				comment := comments
				if strings.Contains(comment, "@http.get") {
					httpMethod = "GET"
					startIdx := strings.Index(comment, "@http.get") + len("@http.get")
					pathStartOffset := strings.Index(comment[startIdx:], "\"")
					if pathStartOffset >= 0 {
						pathStart := startIdx + pathStartOffset + 1
						pathEndOffset := strings.Index(comment[pathStart:], "\"")
						if pathEndOffset >= 0 {
							pathEnd := pathStart + pathEndOffset
							httpPath = comment[pathStart:pathEnd]
						}
					}
				} else if strings.Contains(comment, "@http.post") {
					httpMethod = "POST"
					startIdx := strings.Index(comment, "@http.post") + len("@http.post")
					pathStartOffset := strings.Index(comment[startIdx:], "\"")
					if pathStartOffset >= 0 {
						pathStart := startIdx + pathStartOffset + 1
						pathEndOffset := strings.Index(comment[pathStart:], "\"")
						if pathEndOffset >= 0 {
							pathEnd := pathStart + pathEndOffset
							httpPath = comment[pathStart:pathEnd]
						}
					}

					if strings.Contains(comment, "body=") {
						bodyStart := strings.Index(comment, "body=") + len("body=")
						bodyValStartOffset := strings.Index(comment[bodyStart:], "\"")
						if bodyValStartOffset >= 0 {
							bodyValStart := bodyStart + bodyValStartOffset + 1
							bodyValEndOffset := strings.Index(comment[bodyValStart:], "\"")
							if bodyValEndOffset >= 0 {
								bodyValEnd := bodyValStart + bodyValEndOffset
								httpBody = comment[bodyValStart:bodyValEnd]
							}
						}
					}
				}
				// Similarly for PUT, DELETE, PATCH if needed
			}

			// If still no HTTP method/path, try to infer from method name
			if httpMethod == "" || httpPath == "" {
				rpcName := method.GetName()
				if strings.HasPrefix(rpcName, "Get") {
					httpMethod = "GET"
					resource := strings.TrimPrefix(rpcName, "Get")
					httpPath = "/api/v1/" + camelToSnake(resource)
				} else if strings.HasPrefix(rpcName, "List") {
					httpMethod = "GET"
					resource := strings.TrimPrefix(rpcName, "List")
					httpPath = "/api/v1/" + camelToSnake(resource) + "s"
				} else if strings.HasPrefix(rpcName, "Create") {
					httpMethod = "POST"
					resource := strings.TrimPrefix(rpcName, "Create")
					httpPath = "/api/v1/" + camelToSnake(resource)
					httpBody = "*"
				} else if strings.HasPrefix(rpcName, "Update") {
					httpMethod = "PUT"
					resource := strings.TrimPrefix(rpcName, "Update")
					httpPath = "/api/v1/" + camelToSnake(resource)
					httpBody = "*"
				} else if strings.HasPrefix(rpcName, "Delete") {
					httpMethod = "DELETE"
					resource := strings.TrimPrefix(rpcName, "Delete")
					httpPath = "/api/v1/" + camelToSnake(resource)
				} else if strings.HasPrefix(rpcName, "Patch") {
					httpMethod = "PATCH"
					resource := strings.TrimPrefix(rpcName, "Patch")
					httpPath = "/api/v1/" + camelToSnake(resource)
					httpBody = "*"
				} else {
					// Default to POST for other methods
					httpMethod = "POST"
					httpPath = "/api/v1/" + camelToSnake(rpcName)
					httpBody = "*"
				}
			}

			// Prepare the OpenAPI operation
			operation := &openapi3.Operation{
				Summary:     method.GetName(),
				Description: g.generateDescription(method),
				OperationID: fmt.Sprintf("%s_%s", service.GetName(), method.GetName()),
				Tags:        []string{service.GetName()},
			}

			// Add request body if needed
			if httpBody != "" {
				inputMsg := method.Input
				if inputMsg != nil {
					ref := &openapi3.SchemaRef{
						Ref: fmt.Sprintf("#/components/schemas/%s", g.absoluteName(inputMsg)),
					}
					requestBody := &openapi3.RequestBody{
						Required: true,
						Content: openapi3.Content{
							"application/json": &openapi3.MediaType{
								Schema: ref,
							},
						},
					}
					operation.RequestBody = &openapi3.RequestBodyRef{
						Value: requestBody,
					}
				}
			}

			// Add response
			outputMsg := method.Output
			if outputMsg != nil {
				successResponse := &openapi3.Response{
					Description: &[]string{"Success"}[0],
				}
				if schema, ok := schemas[g.absoluteName(outputMsg)]; ok {
					successResponse.Content = openapi3.Content{
						"application/json": &openapi3.MediaType{
							Schema: schema,
						},
					}
				}
				operation.Responses = openapi3.Responses{
					"200": &openapi3.ResponseRef{
						Value: successResponse,
					},
				}
			}

			// Add the operation to the path
			pathItem := paths[httpPath]
			if pathItem == nil {
				pathItem = &openapi3.PathItem{}
				paths[httpPath] = pathItem
			}

			// Set the operation on the path item
			switch httpMethod {
			case "GET":
				pathItem.Get = operation
			case "POST":
				pathItem.Post = operation
			case "PUT":
				pathItem.Put = operation
			case "DELETE":
				pathItem.Delete = operation
			case "PATCH":
				pathItem.Patch = operation
			}
		}
	}

	return paths
}

// camelToSnake converts a camelCase string to snake_case
func camelToSnake(s string) string {
	var result strings.Builder
	for i, r := range s {
		if i > 0 && 'A' <= r && r <= 'Z' {
			result.WriteRune('_')
		}
		result.WriteRune(unicode.ToLower(r))
	}
	return result.String()
}
