package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"regexp"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

// Usage: go run openapi_to_proto.go <input-openapi.yaml> <output.proto>
func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "Usage: openapi_to_proto <input-openapi.yaml> <output.proto>")
		os.Exit(1)
	}
	inPath := os.Args[1]
	outPath := os.Args[2]

	data, err := ioutil.ReadFile(inPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read input file: %v\n", err)
		os.Exit(2)
	}

	loader := openapi3.NewLoader()
	loader.IsExternalRefsAllowed = true
	doc, err := loader.LoadFromData(data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to parse OpenAPI: %v\n", err)
		os.Exit(3)
	}
	err = doc.Validate(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "OpenAPI validation errors: %v\n", err)
		os.Exit(4)
	}

	proto := generateProto(doc)
	if err := ioutil.WriteFile(outPath, []byte(proto), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to write proto file: %v\n", err)
		os.Exit(5)
	}
	fmt.Println("Wrote proto to", outPath)
}

// generateProto builds .proto text from OpenAPI document
func generateProto(doc *openapi3.T) string {
	var b strings.Builder
	// Header
	b.WriteString("syntax = \"proto3\";\n\n")
	b.WriteString("package generated;\n")
	b.WriteString("import \"google/api/annotations.proto\";\n")
	b.WriteString("import \"google/protobuf/struct.proto\";\n")
	b.WriteString("import \"google/protobuf/empty.proto\";\n\n")

	// Schemas: enums and messages
	for name, schemaRef := range doc.Components.Schemas {
		schema := schemaRef.Value
		// top-level enum
		if len(schema.Enum) > 0 {
			enumName := capitalize(name)
			b.WriteString("enum " + enumName + " {\n")
			for i, v := range schema.Enum {
				constName := normalizeEnum(fmt.Sprint(v))
				b.WriteString(fmt.Sprintf("  %s = %d;\n", constName, i))
			}
			b.WriteString("}\n\n")
		}
		// message for object schemas
		if len(schema.Properties) > 0 {
			msgName := capitalize(name)
			b.WriteString("message " + msgName + " {\n")
			// inline enums for fields
			for fld, fldRef := range schema.Properties {
				if len(fldRef.Value.Enum) > 0 {
					inline := capitalize(fld) + "Enum"
					b.WriteString("  enum " + inline + " {\n")
					for i, v := range fldRef.Value.Enum {
						cn := normalizeEnum(fmt.Sprint(v))
						b.WriteString(fmt.Sprintf("    %s = %d;\n", cn, i))
					}
					b.WriteString("  }\n")
				}
			}
			// required lookup
			required := make(map[string]bool)
			for _, r := range schema.Required {
				required[r] = true
			}
			// fields
			idx := 1
			for fld, fldRef := range schema.Properties {
				opt := ""
				if !required[fld] {
					opt = "optional "
				}
				t := mapType(fld, fldRef)
				b.WriteString(fmt.Sprintf("  %s%s %s = %d;\n", opt, t, fld, idx))
				idx++
			}
			b.WriteString("}\n\n")
		}
	}

	// Service
	b.WriteString("service ApiService {\n")
	// iterate paths with Map()
	for path, pathItem := range doc.Paths.Map() {
		for method, op := range pathItem.Operations() {
			rpc := op.OperationID
			if rpc == "" {
				rpc = capitalize(strings.ToLower(method)) + formatPath(path)
			}
			reqType := "google.protobuf.Empty"
			if op.RequestBody != nil && op.RequestBody.Value != nil {
				for _, media := range op.RequestBody.Value.Content {
					if media.Schema != nil {
						reqType = resolveType(media.Schema)
						break
					}
				}
			}

			// determine response type
			respType := "google.protobuf.Empty"
			for code, respRef := range op.Responses.Map() {
				if strings.HasPrefix(code, "2") || code == "default" {
					for _, media := range respRef.Value.Content {
						if media.Schema != nil {
							respType = resolveType(media.Schema)
							break
						}
					}
					break
				}
			}
			// RPC
			b.WriteString(fmt.Sprintf("  rpc %s(%s) returns (%s) {\n", rpc, reqType, respType))
			b.WriteString("    option (google.api.http) = {\n")
			b.WriteString(fmt.Sprintf("      %s: \"%s\"\n", strings.ToLower(method), path))
			if method == "POST" || method == "PUT" || method == "PATCH" {
				b.WriteString("      body: \"*\"\n")
			}
			b.WriteString("    };\n  }\n")
		}
	}
	b.WriteString("}\n")
	return b.String()
}

func mapType(field string, ref *openapi3.SchemaRef) string {
	if ref.Ref != "" {
		parts := strings.Split(ref.Ref, "/")
		return capitalize(parts[len(parts)-1])
	}
	s := ref.Value
	if len(s.Enum) > 0 {
		return capitalize(field) + "Enum"
	}
	tp := ""
	if s.Type != nil && len(*s.Type) > 0 {
		tp = (*s.Type)[0]
	}
	switch tp {
	case "integer":
		return "int32"
	case "number":
		return "double"
	case "boolean":
		return "bool"
	case "string":
		return "string"
	case "array":
		if s.Items != nil {
			return "repeated " + mapType(field, s.Items)
		}
	case "object":
		return "map<string, string>"
	}
	return "string"
}

func resolveType(ref *openapi3.SchemaRef) string {
	if ref.Ref != "" {
		parts := strings.Split(ref.Ref, "/")
		return capitalize(parts[len(parts)-1])
	}
	return "google.protobuf.Empty"
}

func normalizeEnum(v string) string {
	r := regexp.MustCompile("[^A-Za-z0-9]")
	return r.ReplaceAllString(strings.ToUpper(v), "_")
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func formatPath(path string) string {
	r := regexp.MustCompile(`[{}\\/\\-]`)
	clean := r.ReplaceAllString(path, "_")
	r2 := regexp.MustCompile(`_+`)
	return r2.ReplaceAllString(clean, "_")
}
