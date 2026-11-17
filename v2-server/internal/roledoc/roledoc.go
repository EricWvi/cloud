// Package roledoc validates reusable Agent role documents and their Markdown wire format.
package roledoc

import (
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const (
	// MaxFileBytes bounds one imported role Markdown file before parsing.
	MaxFileBytes = 1 << 20
	// MaxNameChars is the editor's title limit and the length of a graph's role reference.
	MaxNameChars = 80
	// MaxDescriptionChars is the editor's one-line description limit.
	MaxDescriptionChars = 500
	// MaxContentChars bounds the role definition an Agent prompt receives.
	MaxContentChars = 100000
)

// Error is a stable, user-actionable validation failure.
type Error struct {
	Code string
}

func (e *Error) Error() string { return e.Code }

func fail(code string) error { return &Error{Code: code} }

var frontMatter = regexp.MustCompile(`(?m)^---\r?$`)

// Document is the validated data stored for one role.
type Document struct {
	Name        string
	Description string
	Content     string
}

// Parse reads one role Markdown file: YAML front matter carrying name and description, then the
// role definition as the body. Desktop imports roles in this shape.
func Parse(raw []byte) (Document, error) {
	if len(raw) > MaxFileBytes {
		return Document{}, fail("role_file_too_large")
	}
	if !utf8.Valid(raw) {
		return Document{}, fail("role_invalid_encoding")
	}
	text := strings.TrimPrefix(string(raw), "\ufeff")
	matches := frontMatter.FindAllStringIndex(text, -1)
	if len(matches) < 2 || matches[0][0] != 0 {
		return Document{}, fail("role_front_matter_missing")
	}
	var manifest struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	if e := yaml.Unmarshal([]byte(text[matches[0][1]:matches[1][0]]), &manifest); e != nil {
		return Document{}, fail("role_front_matter_invalid")
	}
	body := strings.TrimLeft(text[matches[1][1]:], "\r\n")
	doc, e := Validate(Document{Name: manifest.Name, Description: manifest.Description, Content: body})
	if e != nil {
		return Document{}, e
	}
	if doc.Description == "" {
		return Document{}, fail("role_description_missing")
	}
	return doc, nil
}

// Validate applies the same name, description and content rules to edits and imports. A role may
// have no description when it is typed in the editor; only an import file must declare one.
func Validate(d Document) (Document, error) {
	d.Name = strings.TrimSpace(d.Name)
	d.Description = strings.TrimSpace(d.Description)
	if err := ValidateName(d.Name); err != nil {
		return Document{}, err
	}
	if err := ValidateDescription(d.Description); err != nil {
		return Document{}, err
	}
	if utf8.RuneCountInString(d.Content) > MaxContentChars {
		return Document{}, fail("role_content_too_large")
	}
	if !utf8.ValidString(d.Content) || strings.IndexByte(d.Content, 0) >= 0 {
		return Document{}, fail("role_content_invalid")
	}
	return d, nil
}

// ValidateName keeps a role name usable as a graph reference and as a prompt label: non-empty,
// Unicode-friendly, and free of the separators that would make a name read as a path or wrap a
// line in the prompt.
func ValidateName(name string) error {
	if name == "" {
		return fail("role_name_missing")
	}
	if utf8.RuneCountInString(name) > MaxNameChars ||
		strings.ContainsAny(name, "/\\\r\n") ||
		strings.IndexByte(name, 0) >= 0 {
		return fail("role_name_invalid")
	}
	return nil
}

// ValidateDescription bounds the optional one-line description.
func ValidateDescription(description string) error {
	if utf8.RuneCountInString(description) > MaxDescriptionChars {
		return fail("role_description_too_large")
	}
	if strings.IndexByte(description, 0) >= 0 {
		return fail("role_description_invalid")
	}
	return nil
}

// Code returns the stable error code for an Error and "" for any other error.
func Code(err error) string {
	var validation *Error
	if errors.As(err, &validation) {
		return validation.Code
	}
	return ""
}
