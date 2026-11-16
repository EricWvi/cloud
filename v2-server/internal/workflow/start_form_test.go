package workflow

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildStartFormMapsControlsOptionsAndDefaults(t *testing.T) {
	graph := `{
		"nodes": [
			{
				"id": "loop-1",
				"data": {"kind": "loop"}
			},
			{
				"id": "loop-start",
				"parentId": "loop-1",
				"data": {
					"kind": "start",
					"containerId": "loop-1",
					"inputVariables": [
						{"name": "inner", "valueType": "string"}
					]
				}
			},
			{
				"id": "start",
				"data": {
					"kind": "start",
					"inputVariables": [
						{"name": "repo", "displayName": "仓库路径", "valueType": "string", "fieldType": "text-input", "required": true, "value": "ora-space/cloud"},
						{"name": "notes", "displayName": "补充说明", "valueType": "string", "fieldType": "paragraph"},
						{"name": "retries", "valueType": "integer", "value": 0},
						{"name": "dry_run", "displayName": "演练", "valueType": "boolean", "value": false},
						{"name": "mode", "valueType": "string", "fieldType": "select", "options": [" fast ", "deep", "fast"], "value": "deep"},
						{"name": "empty_select", "valueType": "string", "fieldType": "select", "options": ["  "]},
						{"name": "spec", "valueType": "file", "value": {"kind": "workspace_file", "path": "docs/spec.md"}},
						{"name": "targets", "valueType": "array[string]", "value": ["a", "b"]}
					]
				}
			}
		],
		"edges": []
	}`
	d := BuildStartForm("11111111-1111-4111-8111-111111111111", "代码评审", "已发布版本 v1", graph)
	if d.FormRef != "11111111-1111-4111-8111-111111111111" || d.Title != "代码评审" || d.Description != "已发布版本 v1" {
		t.Fatalf("metadata = %+v", d)
	}
	if len(d.Fields) != 8 {
		t.Fatalf("fields len = %d, want 8: %+v", len(d.Fields), d.Fields)
	}
	byKey := map[string]FormField{}
	for _, f := range d.Fields {
		byKey[f.Key] = f
	}
	if _, leaked := byKey["inner"]; leaked {
		t.Fatalf("container Start variable leaked: %+v", byKey["inner"])
	}
	if f := byKey["repo"]; f.Type != "text" || f.Label != "仓库路径" || !f.Required || f.DefaultValue == nil || *f.DefaultValue != "ora-space/cloud" {
		t.Fatalf("repo = %+v", f)
	}
	if f := byKey["notes"]; f.Type != "textarea" || f.Label != "补充说明" || f.Required {
		t.Fatalf("notes = %+v", f)
	}
	if f := byKey["retries"]; f.Type != "number" || f.DefaultValue == nil {
		t.Fatalf("retries = %+v", f)
	}
	if f := byKey["dry_run"]; f.Type != "boolean" || f.DefaultValue == nil || *f.DefaultValue != false {
		t.Fatalf("dry_run = %+v", f)
	}
	if f := byKey["mode"]; f.Type != "select" || len(f.Options) != 2 || f.Options[0].Value != "fast" || f.DefaultValue == nil || *f.DefaultValue != "deep" {
		t.Fatalf("mode = %+v", f)
	}
	if f := byKey["empty_select"]; f.Type != "text" || len(f.Options) != 0 {
		t.Fatalf("empty_select = %+v", f)
	}
	if f := byKey["spec"]; f.Type != "text" || f.DefaultValue == nil || *f.DefaultValue != "docs/spec.md" {
		t.Fatalf("spec = %+v", f)
	}
	if f := byKey["targets"]; f.Type != "textarea" || f.DefaultValue == nil {
		t.Fatalf("targets = %+v", f)
	}

	// Ensure zero defaults (false and 0) survive JSON serialization.
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	items := decoded["fields"].([]any)
	foundZero, foundFalse := false, false
	for _, item := range items {
		m := item.(map[string]any)
		if m["key"] == "retries" && m["defaultValue"] == float64(0) {
			foundZero = true
		}
		if m["key"] == "dry_run" && m["defaultValue"] == false {
			foundFalse = true
		}
	}
	if !foundZero || !foundFalse {
		t.Fatalf("zero defaults lost in JSON: %s", raw)
	}
}

func TestBuildStartFormDropsMalformedAndCapsLengths(t *testing.T) {
	graph := `{
		"nodes": [{
			"id": "start",
			"data": {
				"kind": "start",
				"inputVariables": [
					{"name": "", "valueType": "string"},
					{"name": "has space", "valueType": "string"},
					{"name": "中文", "valueType": "string"},
					{"name": "bad.dot", "valueType": "string"},
					{"name": "ok", "displayName": "` + strings.Repeat("字", 70) + `", "valueType": "string"},
					{"name": "kept", "valueType": "number", "value": "not-a-number"},
					{"name": "kept", "valueType": "string"},
					{"name": "sel", "valueType": "string", "fieldType": "select", "options": ["a"], "value": "stale"}
				]
			}
		}],
		"edges": []
	}`
	d := BuildStartForm("ref", strings.Repeat("好", 100), strings.Repeat("好", 1000), graph)
	if len(d.Title) > 200 || len(d.Description) > 2000 {
		t.Fatalf("title/description not clipped: %d / %d", len(d.Title), len(d.Description))
	}
	if len(d.Fields) != 2 {
		t.Fatalf("fields = %+v, want [kept, sel]", d.Fields)
	}
	if d.Fields[0].Key != "kept" || d.Fields[0].DefaultValue != nil {
		t.Fatalf("kept = %+v", d.Fields[0])
	}
	if d.Fields[1].Key != "sel" || d.Fields[1].DefaultValue != nil {
		t.Fatalf("sel = %+v", d.Fields[1])
	}

	empty := BuildStartForm("ref", "Empty", "", "not-json")
	if empty.Fields == nil || len(empty.Fields) != 0 {
		t.Fatalf("empty fields = %#v", empty.Fields)
	}
}
