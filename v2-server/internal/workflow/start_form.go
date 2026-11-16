package workflow

import (
	"encoding/json"
	"math"
	"strings"
	"unicode/utf8"
)

const (
	formFieldText     = "text"
	formFieldTextarea = "textarea"
	formFieldNumber   = "number"
	formFieldBoolean  = "boolean"
	formFieldSelect   = "select"

	maxFormFields          = 100
	maxFormOptions         = 100
	maxFormFieldValueBytes = 4000
	maxFormTitleBytes      = 200
	maxFormDescBytes       = 2000
)

// FormOption is one allowed choice for a select form field.
type FormOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// FormField is one input control projected from a Start variable onto Cloud's FormField shape.
type FormField struct {
	Key          string       `json:"key"`
	Label        string       `json:"label"`
	Type         string       `json:"type"`
	Required     bool         `json:"required"`
	Description  string       `json:"description,omitempty"`
	Placeholder  string       `json:"placeholder,omitempty"`
	DefaultValue *any         `json:"defaultValue,omitempty"`
	Options      []FormOption `json:"options,omitempty"`
}

// FormDescriptor is the Form Mode descriptor projected from a published workflow snapshot.
type FormDescriptor struct {
	FormRef     string      `json:"formRef"`
	Title       string      `json:"title,omitempty"`
	Description string      `json:"description,omitempty"`
	Fields      []FormField `json:"fields"`
}

// BuildStartForm projects a published workflow snapshot's root Start node inputVariables onto
// Cloud's FormDescriptor contract. Unrepresentable variables or illegal defaults are omitted so a
// saved workflow never produces an invalid descriptor.
func BuildStartForm(formRef, title, description, graph string) FormDescriptor {
	return FormDescriptor{
		FormRef:     formRef,
		Title:       clipUTF8(title, maxFormTitleBytes),
		Description: clipUTF8(description, maxFormDescBytes),
		Fields:      projectStartFormFields(graph),
	}
}

func projectStartFormFields(graph string) []FormField {
	fields := []FormField{}
	start := rootStartNodeData(graph)
	if start == nil {
		return fields
	}
	declared, _ := start["inputVariables"].([]any)
	seen := map[string]bool{}
	for _, item := range declared {
		if len(fields) == maxFormFields {
			break
		}
		raw, ok := item.(map[string]any)
		if !ok {
			continue
		}
		field, ok := startInputFormField(raw)
		if !ok || seen[field.Key] {
			continue
		}
		seen[field.Key] = true
		fields = append(fields, field)
	}
	return fields
}

// rootStartNodeData returns the data object of the graph's first root-level Start node. Container
// Start nodes (such as Loop entry nodes) are ignored because only the workflow entry collects
// launch inputs.
func rootStartNodeData(graph string) map[string]any {
	dec := json.NewDecoder(strings.NewReader(graph))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil || doc == nil {
		return nil
	}
	nodes, _ := doc["nodes"].([]any)
	for _, raw := range nodes {
		node, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if parent, _ := node["parentId"].(string); strings.TrimSpace(parent) != "" {
			continue
		}
		data, ok := node["data"].(map[string]any)
		if !ok {
			continue
		}
		if container, _ := data["containerId"].(string); strings.TrimSpace(container) != "" {
			continue
		}
		if kind, _ := data["kind"].(string); kind == "start" {
			return data
		}
	}
	return nil
}

func startInputFormField(variable map[string]any) (FormField, bool) {
	key := strings.TrimSpace(stringField(variable, "name"))
	if !validFormToken(key) || strings.Contains(key, ".") {
		return FormField{}, false
	}
	label := strings.TrimSpace(stringField(variable, "displayName"))
	if label == "" {
		label = key
	}
	if len(label) > maxFormTitleBytes {
		return FormField{}, false
	}
	required, _ := variable["required"].(bool)
	fieldType := strings.TrimSpace(stringField(variable, "fieldType"))
	if fieldType == "" {
		fieldType = defaultFieldType(strings.TrimSpace(stringField(variable, "valueType")))
	}
	field := FormField{Key: key, Label: label, Required: required}
	var jsonLike bool
	switch fieldType {
	case "paragraph":
		field.Type = formFieldTextarea
	case "number":
		field.Type = formFieldNumber
	case "checkbox":
		field.Type = formFieldBoolean
	case "select":
		opts := startSelectOptions(variable)
		if len(opts) == 0 {
			field.Type = formFieldText
		} else {
			field.Type = formFieldSelect
			field.Options = opts
		}
	case "file-list", "json":
		field.Type = formFieldTextarea
		jsonLike = true
	default:
		field.Type = formFieldText
	}
	if rawVal, exists := variable["value"]; exists && rawVal != nil {
		if normalized, ok := validFormDefault(field.Type, fieldType, jsonLike, field.Options, rawVal); ok {
			field.DefaultValue = &normalized
		}
	}
	return field, true
}

func startSelectOptions(variable map[string]any) []FormOption {
	declared, _ := variable["options"].([]any)
	out := []FormOption{}
	seen := map[string]bool{}
	for _, item := range declared {
		raw, ok := item.(string)
		if !ok {
			continue
		}
		value := strings.TrimSpace(raw)
		if value == "" || len(value) > maxFormTitleBytes || seen[value] || len(out) == maxFormOptions {
			continue
		}
		seen[value] = true
		out = append(out, FormOption{Value: value, Label: value})
	}
	return out
}

func validFormDefault(controlType, sourceFieldType string, jsonLike bool, options []FormOption, raw any) (any, bool) {
	if jsonLike {
		switch v := raw.(type) {
		case string:
			return v, len(v) <= maxFormFieldValueBytes
		case []any, map[string]any, bool:
			return v, true
		case json.Number:
			f, err := v.Float64()
			return v, err == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
		case float64:
			return v, !math.IsNaN(v) && !math.IsInf(v, 0)
		}
		return nil, false
	}
	switch controlType {
	case formFieldText, formFieldTextarea:
		if s, ok := raw.(string); ok {
			return s, len(s) <= maxFormFieldValueBytes
		}
		if sourceFieldType == "file" {
			if m, ok := raw.(map[string]any); ok && m["kind"] == "workspace_file" {
				if p, ok := m["path"].(string); ok && len(p) <= maxFormFieldValueBytes {
					return p, true
				}
			}
		}
		return nil, false
	case formFieldNumber:
		switch n := raw.(type) {
		case json.Number:
			f, err := n.Float64()
			return n, err == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
		case float64:
			return n, !math.IsNaN(n) && !math.IsInf(n, 0)
		case int64, int:
			return n, true
		}
		return nil, false
	case formFieldBoolean:
		b, ok := raw.(bool)
		return b, ok
	case formFieldSelect:
		s, ok := raw.(string)
		if !ok {
			return nil, false
		}
		for _, opt := range options {
			if opt.Value == s {
				return s, true
			}
		}
		return nil, false
	}
	return nil, false
}

func validFormToken(s string) bool {
	if s == "" || len(s) > maxFormTitleBytes {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.', r == ':', r == '@':
		default:
			return false
		}
	}
	return true
}

func stringField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func clipUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	for maxBytes > 0 && !utf8.RuneStart(s[maxBytes]) {
		maxBytes--
	}
	return s[:maxBytes]
}
