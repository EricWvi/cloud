package pluginmarket

import (
	"testing"
)

func TestSkillNamesInArchive(t *testing.T) {
	manifest := `resolver = 1
identifier = "code-review-and-quality"
namespace = "top-skills"
kind = "skill"
version = "1.0.0"
description = "A skill."
`
	file := workflowArchive(t, manifest, map[string]string{
		"assets/code-review-and-quality/SKILL.md":  "---\nname: code-review-and-quality\n---\n\nReview code.\n",
		"assets/code-review-and-quality/README.md": "ignored",
		"assets/other/SKILL.md":                    "---\nname: other\n---\n\nAnother.\n",
		"assets/not-a-skill/file.txt":              "ignored",
	})
	names, err := SkillNamesInArchive(file, "top-skills/code-review-and-quality", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "code-review-and-quality" || names[1] != "other" {
		t.Fatalf("names = %v", names)
	}
}

func TestSkillNamesInArchiveRejectsInvalid(t *testing.T) {
	manifest := `resolver = 1
identifier = "code-review-and-quality"
namespace = "top-skills"
kind = "skill"
version = "1.0.0"
description = "A skill."
`
	for _, test := range []struct {
		name     string
		manifest string
		files    map[string]string
		id       string
	}{
		{
			name:     "no skill assets",
			manifest: manifest,
			files:    map[string]string{"assets/readme.md": "no skills here"},
		},
		{
			name:     "nested skill path",
			manifest: manifest,
			files:    map[string]string{"assets/nested/one/SKILL.md": "---\nname: one\n---\n"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			file := workflowArchive(t, test.manifest, test.files)
			id := test.id
			if id == "" {
				id = "top-skills/code-review-and-quality"
			}
			if _, err := SkillNamesInArchive(file, id, "1.0.0"); err == nil {
				t.Fatal("archive was accepted")
			}
		})
	}
}
