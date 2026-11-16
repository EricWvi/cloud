package pluginmarket

import (
	"archive/zip"
	"errors"
	"io"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// SkillNamesInArchive lists the skill names a skill package materializes at runtime: its
// `assets/<name>/SKILL.md` directories, exactly what the agent runtime projects into a checkout.
// The package must already be validated (VerifySkillArchive); a malformed zip is ErrPackage.
func SkillNamesInArchive(path, id, version string) ([]string, error) {
	z, err := zip.OpenReader(path)
	if err != nil {
		return nil, ErrPackage
	}
	defer z.Close()
	_, identifier, ok := strings.Cut(id, "/")
	if !ok {
		return nil, ErrPackage
	}
	var manifest struct{ Kind, Identifier, Version string }
	count := 0
	for _, f := range z.File {
		if f.Name != "orax.toml" {
			continue
		}
		count++
		if count > 1 || f.UncompressedSize64 > maxManifestBytes || !f.Mode().IsRegular() {
			return nil, ErrPackage
		}
		r, e := f.Open()
		if e != nil {
			return nil, ErrPackage
		}
		data, e := io.ReadAll(io.LimitReader(r, maxManifestBytes+1))
		r.Close()
		if e != nil || len(data) > maxManifestBytes || toml.Unmarshal(data, &manifest) != nil {
			return nil, ErrPackage
		}
	}
	if count != 1 || manifest.Kind != "skill" || manifest.Identifier != identifier || manifest.Version != version {
		return nil, ErrPackage
	}
	names := map[string]bool{}
	for _, f := range z.File {
		rest, ok := strings.CutPrefix(f.Name, "assets/")
		if !ok || !strings.HasSuffix(rest, "/SKILL.md") {
			continue
		}
		parts := strings.Split(rest, "/")
		if len(parts) != 2 || parts[0] == "" {
			continue
		}
		if !f.Mode().IsRegular() {
			continue
		}
		names[parts[0]] = true
	}
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil, errors.New("archive has no discoverable skills")
	}
	return out, nil
}
