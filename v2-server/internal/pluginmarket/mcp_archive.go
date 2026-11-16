package pluginmarket

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/pelletier/go-toml/v2"
	"github.com/wanglongan587/cloud/v2-server/internal/strictjson"
)

// MCPSetting is the editable scalar portion of a package declaration. Node remains the
// authority for compiling the transport and resolving its runtime references.
type MCPSetting struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Secret      bool   `json:"secret"`
	Required    bool   `json:"required"`
	Default     any    `json:"default,omitempty"`
}

// MCPDeclaration retains the settings and referenced IDs needed to render configuration.
type MCPDeclaration struct {
	Settings   []MCPSetting `json:"settings"`
	References []string     `json:"references"`
	Transport  string       `json:"transport"`
}

var mcpSettingID = regexp.MustCompile(`^[a-z][A-Za-z0-9]{0,63}$`)

// VerifyMCPArchive verifies identity and reads the immutable declaration from an archive.
// Full transport compilation, command containment and Agent capabilities are checked by Node.
func VerifyMCPArchive(archive []byte, id, version string) (MCPDeclaration, error) {
	var result MCPDeclaration
	z, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil || len(archive) > 256<<20 || len(z.File) > 10000 {
		return result, ErrPackage
	}
	_, identifier, ok := strings.Cut(id, "/")
	if !ok {
		return result, ErrPackage
	}
	var manifest struct{ Identifier, Kind, Version string }
	var config []byte
	seen := map[string]bool{}
	var total uint64
	for _, file := range z.File {
		// Node's StrictRelativePath treats either separator as a path boundary. Keep
		// the verified archive bytes intact, but use that same spelling for lookup
		// and duplicate detection (older official packages were built on Windows).
		name := strings.ReplaceAll(file.Name, "\\", "/")
		if file.FileInfo().IsDir() {
			name = strings.TrimRight(name, "/")
		}
		if name == "" || name == "." || !utf8.ValidString(name) || strings.IndexFunc(name, unicode.IsControl) >= 0 || path.IsAbs(name) || path.Clean(name) != name || name == ".." || strings.HasPrefix(name, "../") || hasMCPDrivePrefix(name) || file.Mode()&os.ModeSymlink != 0 {
			return result, ErrPackage
		}
		if file.FileInfo().IsDir() {
			continue
		}
		if seen[name] {
			return result, ErrPackage
		}
		seen[name] = true
		if file.UncompressedSize64 > 512<<20-total {
			return result, ErrPackage
		}
		total += file.UncompressedSize64
		switch name {
		case "orax.toml":
			raw, err := readZipEntry(file, maxManifestBytes)
			if err != nil || toml.Unmarshal(raw, &manifest) != nil {
				return result, ErrPackage
			}
		case "assets/config.json":
			config, err = readZipEntry(file, 256<<10)
			if err != nil {
				return result, ErrPackage
			}
		}
	}
	if manifest.Identifier != identifier || manifest.Kind != "mcp" || manifest.Version != version || len(config) > 256<<10 || !strictjson.Valid(config) || duplicateMCPKeys(config) {
		return result, ErrPackage
	}
	var raw struct {
		SchemaVersion int `json:"schemaVersion"`
		Settings      map[string]struct {
			Type, Title, Description string
			Required, Secret         bool
			Default                  json.RawMessage
			Order                    *int64
		} `json:"settings"`
		Transport map[string]any `json:"transport"`
	}
	if json.Unmarshal(config, &raw) != nil || raw.SchemaVersion != 1 || len(raw.Settings) > 128 {
		return result, ErrPackage
	}
	result.Transport, _ = raw.Transport["type"].(string)
	if result.Transport != "http" && result.Transport != "stdio" {
		return result, ErrPackage
	}
	// Existing published MCPs can omit the secret flag. Authentication header
	// references are credentials even when the package omits that UI metadata.
	credentials := map[string]bool{}
	if result.Transport == "http" {
		if headers, ok := raw.Transport["headers"].(map[string]any); ok {
			for name, value := range headers {
				if strings.EqualFold(name, "Authorization") || strings.EqualFold(name, "Proxy-Authorization") {
					if reference, ok := value.(map[string]any); ok {
						if id, ok := reference["setting"].(string); ok {
							credentials[id] = true
						}
					}
				}
			}
		}
	}
	result.Settings = []MCPSetting{}
	for id, setting := range raw.Settings {
		if !mcpSettingID.MatchString(id) || strings.TrimSpace(setting.Title) == "" || strings.TrimSpace(setting.Description) == "" {
			return result, ErrPackage
		}
		s := MCPSetting{ID: id, Type: setting.Type, Label: setting.Title, Description: setting.Description, Secret: setting.Secret || credentials[id], Required: setting.Required}
		if s.Type != "string" && s.Type != "number" && s.Type != "boolean" {
			return result, ErrPackage
		}
		if len(setting.Default) > 0 {
			if json.Unmarshal(setting.Default, &s.Default) != nil || !MCPValueMatches(s.Type, s.Default) {
				return result, ErrPackage
			}
		}
		result.Settings = append(result.Settings, s)
	}
	sort.Slice(result.Settings, func(i, j int) bool { return result.Settings[i].ID < result.Settings[j].ID })
	refs := map[string]bool{}
	var visit func(any)
	visit = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if id, ok := x["setting"].(string); ok {
				refs[id] = true
			}
			for _, child := range x {
				visit(child)
			}
		case []any:
			for _, child := range x {
				visit(child)
			}
		}
	}
	visit(raw.Transport)
	result.References = []string{}
	for id := range refs {
		if _, ok := raw.Settings[id]; !ok {
			return result, ErrPackage
		}
		result.References = append(result.References, id)
	}
	sort.Strings(result.References)
	return result, nil
}

func hasMCPDrivePrefix(name string) bool {
	for _, segment := range strings.Split(name, "/") {
		if len(segment) >= 2 && segment[1] == ':' && ((segment[0] >= 'a' && segment[0] <= 'z') || (segment[0] >= 'A' && segment[0] <= 'Z')) {
			return true
		}
	}
	return false
}

// Reject duplicate members before metadata parsing so the form and Node agree on secrecy
// and type. encoding/json would otherwise silently select the last duplicate value.
func duplicateMCPKeys(raw []byte) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	var visit func() bool
	visit = func() bool {
		token, err := d.Token()
		if err != nil {
			return true
		}
		delim, composite := token.(json.Delim)
		if !composite {
			return false
		}
		seen := map[string]bool{}
		for d.More() {
			if delim == '{' {
				key, err := d.Token()
				if err != nil {
					return true
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return true
				}
				seen[name] = true
			}
			if visit() {
				return true
			}
		}
		_, err = d.Token()
		return err != nil
	}
	return visit()
}

// MCPValueMatches checks declared scalar types without accepting null or composite values.
func MCPValueMatches(kind string, value any) bool {
	switch kind {
	case "string":
		_, ok := value.(string)
		return ok
	case "number":
		_, ok := value.(float64)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	}
	return false
}
