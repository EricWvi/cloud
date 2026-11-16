package pluginmarket

import (
	"archive/zip"
	"bytes"
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
