package fleet

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
)

// HostSchemaFileName is the published file name of the host document schema.
const HostSchemaFileName = "host.schema.json"

// SchemaDownloadBase is where releases publish the schema: the versioned
// asset lives at <base>/download/<tag>/host.schema.json and the latest
// release at <base>/latest/download/host.schema.json.
const SchemaDownloadBase = "https://github.com/valthaide/tide/releases"

// SchemaID returns the canonical $id of the host schema for a Tide release
// version (with or without a leading "v"). Empty/dev builds use "dev".
func SchemaID(version string) string {
	v := normalizeReleaseVersion(version)
	return SchemaDownloadBase + "/download/" + v + "/" + HostSchemaFileName
}

// LatestSchemaURL is the unversioned address; it redirects to the last
// release's schema.
func LatestSchemaURL() string {
	return SchemaDownloadBase + "/latest/download/" + HostSchemaFileName
}

func normalizeReleaseVersion(version string) string {
	v := strings.TrimSpace(version)
	if v == "" || v == "dev" {
		return "dev"
	}
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	return v
}

// HostSchemaJSON generates the JSON Schema (draft 2020-12) for fleet host
// documents from the same structs ParseHostDocument accepts. The schema is
// never written or edited by hand: regenerate the committed copy with
//
//	tide schema host --version vX.Y.Z > schemas/host.schema.json
//
// Field descriptions come from the `desc` struct tags in fleet.go, field
// names and types from the `yaml` tags and Go types, so the schema cannot
// drift from the parser without the committed-schema test noticing.
func HostSchemaJSON(version string) ([]byte, error) {
	gen := &schemaGen{defs: map[string]any{}, done: map[reflect.Type]bool{}}
	root := gen.schemaFor(reflect.TypeOf(HostDocument{}), "schema")
	if ref, ok := root["$ref"].(string); ok {
		// Inline the root struct so the document schema carries title/$id
		// next to its properties instead of being a bare reference.
		name := strings.TrimPrefix(ref, "#/$defs/")
		root = gen.defs[name].(map[string]any)
		delete(gen.defs, name)
	}
	doc := map[string]any{
		"$schema":     "https://json-schema.org/draft/2020-12/schema",
		"$id":         SchemaID(version),
		"title":       "Tide fleet host document",
		"description": "Applications that should run on one host, declared in hosts/<fqdn>.yaml of a fleet repository. Generated from the Tide fleet parser structs; do not edit by hand.",
		"type":        "object",
		"x-tide": map[string]any{
			"tide_version":         normalizeReleaseVersion(version),
			"fleet_format_version": SupportedSchemaVersion,
			"latest_schema":        LatestSchemaURL(),
		},
	}
	for k, v := range root {
		doc[k] = v
	}
	if len(gen.defs) > 0 {
		doc["$defs"] = gen.defs
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

type schemaGen struct {
	defs map[string]any
	done map[reflect.Type]bool
}

// schemaFor maps a Go type to a JSON Schema fragment. fieldName is the yaml
// name of the field the type came from ("" for the root), used for the few
// fields whose schema is narrower than their Go type.
func (g *schemaGen) schemaFor(t reflect.Type, fieldName string) map[string]any {
	if fieldName == "schema" && t.Kind() == reflect.Int {
		return map[string]any{
			"type": "integer",
			"enum": []any{SupportedSchemaVersion},
		}
	}
	switch t.Kind() {
	case reflect.Pointer:
		inner := g.schemaFor(t.Elem(), fieldName)
		out := map[string]any{}
		for k, v := range inner {
			out[k] = v
		}
		if typ, ok := out["type"].(string); ok {
			out["type"] = []any{typ, "null"}
		}
		return out
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int64, reflect.Int32:
		return map[string]any{"type": "integer"}
	case reflect.Float64, reflect.Float32:
		return map[string]any{"type": "number"}
	case reflect.Slice, reflect.Array:
		return map[string]any{"type": "array", "items": g.schemaFor(t.Elem(), "")}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": g.schemaFor(t.Elem(), "")}
	case reflect.Struct:
		name := t.Name()
		if !g.done[t] {
			g.done[t] = true // set before recursing so self-references terminate
			g.defs[name] = g.structSchema(t)
		}
		return map[string]any{"$ref": "#/$defs/" + name}
	default:
		return map[string]any{}
	}
}

func (g *schemaGen) structSchema(t reflect.Type) map[string]any {
	props := map[string]any{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		yamlTag := f.Tag.Get("yaml")
		name, _, _ := strings.Cut(yamlTag, ",")
		if name == "-" || f.PkgPath != "" {
			continue
		}
		if name == "" {
			name = strings.ToLower(f.Name)
		}
		prop := g.schemaFor(f.Type, name)
		if desc := strings.TrimSpace(f.Tag.Get("desc")); desc != "" {
			prop = withDescription(prop, desc)
		}
		props[name] = prop
	}
	return map[string]any{
		"type":                 "object",
		"properties":           props,
		"additionalProperties": false,
	}
}

// withDescription attaches a description, resolving a $ref into an allOf so
// the property keeps both its type and its description.
func withDescription(prop map[string]any, desc string) map[string]any {
	if ref, ok := prop["$ref"]; ok {
		return map[string]any{"allOf": []any{map[string]any{"$ref": ref}}, "description": desc}
	}
	prop["description"] = desc
	return prop
}
