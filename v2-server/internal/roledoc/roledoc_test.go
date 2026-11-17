package roledoc_test

import (
	"strings"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/roledoc"
)

func TestParseRequiresFrontMatterNameAndDescription(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		code string
	}{
		{"no front matter", "# 代码审查\n", "role_front_matter_missing"},
		{"missing name", "---\ndescription: 审查\n---\n正文\n", "role_name_missing"},
		{"missing description", "---\nname: 代码审查\n---\n正文\n", "role_description_missing"},
		{"path separator", "---\nname: a/b\ndescription: 审查\n---\n正文\n", "role_name_invalid"},
		{"newline in name", "---\nname: \"a\\nb\"\ndescription: 审查\n---\n正文\n", "role_name_invalid"},
		{"invalid yaml", "---\nname: [\ndescription: 审查\n---\n正文\n", "role_front_matter_invalid"},
		{"too long name", "---\nname: " + strings.Repeat("角", 81) + "\ndescription: 审查\n---\n正文\n", "role_name_invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := roledoc.Parse([]byte(tc.raw))
			if code := roledoc.Code(err); code != tc.code {
				t.Fatalf("code = %q, want %q (err %v)", code, tc.code, err)
			}
		})
	}
}

func TestParseKeepsUnicodeNamesAndBody(t *testing.T) {
	doc, err := roledoc.Parse([]byte("---\nname: 代码审查\ndescription: 检查改动\n---\n\n# 角色\n\n- 逐行检查\n"))
	if err != nil {
		t.Fatal(err)
	}
	if doc.Name != "代码审查" || doc.Description != "检查改动" || doc.Content != "# 角色\n\n- 逐行检查\n" {
		t.Fatalf("doc = %+v", doc)
	}
}

func TestValidateBoundsContentAndAllowsEmptyDescription(t *testing.T) {
	doc, err := roledoc.Validate(roledoc.Document{Name: " 技术写作 ", Content: "# 写作"})
	if err != nil {
		t.Fatal(err)
	}
	if doc.Name != "技术写作" || doc.Description != "" {
		t.Fatalf("doc = %+v", doc)
	}
	if _, err := roledoc.Validate(roledoc.Document{Name: "x", Content: strings.Repeat("a", roledoc.MaxContentChars+1)}); roledoc.Code(err) != "role_content_too_large" {
		t.Fatalf("content bound error = %v", err)
	}
	if _, err := roledoc.Validate(roledoc.Document{Name: "x", Description: strings.Repeat("a", roledoc.MaxDescriptionChars+1)}); roledoc.Code(err) != "role_description_too_large" {
		t.Fatalf("description bound error = %v", err)
	}
}
