package pluginmarket

import (
	"archive/zip"
	"bytes"
	"fmt"
	"testing"
)

func TestMCPArchiveRejectsAmbiguousOrMismatchedDeclarations(t *testing.T) {
	const valid = `{"schemaVersion":1,"settings":{"token":{"type":"string","title":"Token","description":"Credential","secret":true}},"transport":{"type":"http","url":"https://example.test/mcp","headers":{"Authorization":{"setting":"token"}}}}`
	for _, tc := range []struct {
		name, config, kind string
		valid              bool
	}{
		{"valid", valid, "mcp", true},
		{"settings-free", `{"schemaVersion":1,"transport":{"type":"stdio","command":"assets/server"}}`, "mcp", true},
		{"wrong kind", valid, "skill", false},
		{"duplicate secrecy", `{"schemaVersion":1,"settings":{"token":{"type":"string","title":"Token","description":"Credential","secret":true,"secret":false}},"transport":{"type":"http"}}`, "mcp", false},
		{"undeclared reference", `{"schemaVersion":1,"transport":{"type":"http","headers":{"Authorization":{"setting":"missing"}}}}`, "mcp", false},
		{"bad schema", `{"schemaVersion":2,"transport":{"type":"http"}}`, "mcp", false},
		{"null default", `{"schemaVersion":1,"settings":{"token":{"type":"string","title":"Token","description":"Credential","default":null}},"transport":{"type":"http"}}`, "mcp", false},
		{"nested type", `{"schemaVersion":1,"settings":{"token":{"type":"object","title":"Token","description":"Credential"}},"transport":{"type":"http"}}`, "mcp", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body bytes.Buffer
			z := zip.NewWriter(&body)
			for name, value := range map[string]string{"orax.toml": "identifier = \"test.mcp\"\nversion = \"1.0.0\"\nkind = \"" + tc.kind + "\"\n", "assets/config.json": tc.config} {
				f, e := z.Create(name)
				if e != nil {
					t.Fatal(e)
				}
				if _, e = f.Write([]byte(value)); e != nil {
					t.Fatal(e)
				}
			}
			if e := z.Close(); e != nil {
				t.Fatal(e)
			}
			declaration, err := VerifyMCPArchive(body.Bytes(), "official/test.mcp", "1.0.0")
			if (err == nil) != tc.valid {
				t.Fatal(declaration, err)
			}
			if _, err = VerifyMCPArchive(body.Bytes(), "official/other", "1.0.0"); err == nil {
				t.Fatal("mismatched identity accepted")
			}
		})
	}
}

func TestMCPArchiveWindowsPathsMatchNode(t *testing.T) {
	const config = `{"schemaVersion":1,"transport":{"type":"http","url":"https://example.test/mcp"}}`
	for _, tc := range []struct {
		name  string
		paths []string
		valid bool
	}{
		{"windows package", []string{`assets\config.json`}, true},
		{"slash package", []string{"assets/config.json"}, true},
		{"mixed duplicate", []string{`assets\config.json`, "assets/config.json"}, false},
		{"windows traversal", []string{`assets\config.json`, `assets\..\escape`}, false},
		{"windows root traversal", []string{`assets\config.json`, `..\escape`}, false},
		{"mixed traversal", []string{`assets\config.json`, `assets/..\escape`}, false},
		{"windows absolute", []string{`assets\config.json`, `\escape`}, false},
		{"windows drive", []string{`assets\config.json`, `C:\escape`}, false},
		{"windows nested drive", []string{`assets\config.json`, `assets\C:escape`}, false},
		{"windows unc", []string{`assets\config.json`, `\\server\escape`}, false},
		{"windows empty component", []string{`assets\config.json`, `assets\\other`}, false},
		{"windows dot component", []string{`assets\config.json`, `assets\.\other`}, false},
		{"traversal directory", []string{`assets\config.json`, `..\outside/`}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var archive bytes.Buffer
			z := zip.NewWriter(&archive)
			f, err := z.Create("orax.toml")
			if err != nil {
				t.Fatal(err)
			}
			_, _ = f.Write([]byte("identifier = \"test.mcp\"\nversion = \"1.0.0\"\nkind = \"mcp\"\n"))
			for _, name := range tc.paths {
				f, err = z.Create(name)
				if err != nil {
					t.Fatal(err)
				}
				if name[len(name)-1] != '/' {
					_, _ = f.Write([]byte(config))
				}
			}
			if err := z.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := VerifyMCPArchive(archive.Bytes(), "official/test.mcp", "1.0.0"); (err == nil) != tc.valid {
				t.Fatal("unexpected archive validation", err)
			}
		})
	}
}

func TestMCPAuthenticationHeaderSettingsAreSecret(t *testing.T) {
	for _, tc := range []struct {
		name, header string
		explicit     bool
		secret       bool
	}{
		{"legacy authorization", "Authorization", false, true},
		{"lowercase authorization", "authorization", false, true},
		{"proxy authorization", "pRoXy-AuThOrIzAtIoN", false, true},
		{"explicit secret", "X-API-Key", true, true},
		{"ordinary setting", "X-Workspace", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var archive bytes.Buffer
			z := zip.NewWriter(&archive)
			for name, value := range map[string]string{
				"orax.toml":          "identifier = \"test.mcp\"\nversion = \"1.0.0\"\nkind = \"mcp\"\n",
				"assets/config.json": fmt.Sprintf(`{"schemaVersion":1,"settings":{"apiKey":{"type":"string","title":"API key","description":"Credential","secret":%t}},"transport":{"type":"http","url":"https://example.test/mcp","headers":{%q:{"setting":"apiKey","prefix":"Bearer "}}}}`, tc.explicit, tc.header),
			} {
				f, err := z.Create(name)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = f.Write([]byte(value))
			}
			if err := z.Close(); err != nil {
				t.Fatal(err)
			}
			declaration, err := VerifyMCPArchive(archive.Bytes(), "official/test.mcp", "1.0.0")
			if err != nil || len(declaration.Settings) != 1 || declaration.Settings[0].Secret != tc.secret {
				t.Fatal("credential classification", declaration, err)
			}
		})
	}
}

func TestMCPSettingsCipherBindsTenantAndPlugin(t *testing.T) {
	t.Setenv("V2_MARKET_CREDENTIAL_KEY", "BwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwc=")
	values := map[string]any{"token": "fixture-secret"}
	sealed, err := SealMCPSettings(values, "tenant-a/official/test")
	if err != nil || bytes.Contains(sealed, []byte("fixture-secret")) {
		t.Fatal("plaintext persisted", err)
	}
	got, err := OpenMCPSettings(sealed, "tenant-a/official/test")
	if err != nil || got["token"] != values["token"] {
		t.Fatal("round trip", err)
	}
	for _, binding := range []string{"tenant-b/official/test", "tenant-a/official/other"} {
		if _, err := OpenMCPSettings(sealed, binding); err == nil {
			t.Fatal("rebound ciphertext", binding)
		}
	}
	sealed[len(sealed)-1] ^= 1
	if _, err = OpenMCPSettings(sealed, "tenant-a/official/test"); err == nil {
		t.Fatal("modified ciphertext accepted")
	}
}
