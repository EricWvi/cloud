package store_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/pluginmarket"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
)

// The names a space sees for an installed skill package are the ones of the package bytes its own
// selection froze: another space installing a newer version, or a failed install, never changes
// them, and a selection installed before names were recorded simply has none.
func TestInstalledPluginSkillNamesFollowTheFrozenPackage(t *testing.T) {
	f := newSquadFixture(t)
	owner := f.login(t, "owner", "owner")
	spaces, e := f.store.TenantSpaces(f.ctx, storetest.Access(t, f.store, f.tenant, owner))
	if e != nil || len(spaces) != 1 {
		t.Fatalf("spaces = %v %v", spaces, e)
	}
	a := spaces[0].ID
	var b string
	if e := f.db.QueryRow(f.ctx, `insert into v2_spaces(tenant_id,name,slug,created_by) values($1,'Other','other',$2) returning id`, f.tenant, owner).Scan(&b); e != nil {
		t.Fatal(e)
	}
	release := func(version, digest string) string {
		t.Helper()
		sum := strings.Repeat(digest, 64)
		if e := f.store.ReplacePluginCatalog(f.ctx, []pluginmarket.Entry{{
			Namespace: "official", Identifier: "pack", Title: "Pack", Kind: "skill", Version: version,
			Description: "Skills", URL: "https://example.org/pack.orax", SHA256: sum,
			MarketplaceVisible: true, SourceURL: "https://example.org/market",
		}}, time.Now()); e != nil {
			t.Fatal(e)
		}
		return sum
	}
	names := func(space string) []string {
		t.Helper()
		got, e := f.store.InstalledPluginSkillNames(f.ctx, f.tenant, space)
		if e != nil {
			t.Fatal(e)
		}
		return got["official/pack"]
	}

	// A selection installed before names were recorded resolves to none (the page falls back).
	first := release("1.0.0", "a")
	if _, e := f.store.InstallSpacePlugin(f.ctx, f.tenant, a, owner, "plugins", "a1", store.PluginInstallInput{Identifier: "official/pack"}); e != nil {
		t.Fatal(e)
	}
	if got := names(a); got != nil {
		t.Fatalf("names before recording = %v, want none", got)
	}
	if e := f.store.UpsertPluginSkills(f.ctx, first, []string{"tdd", "review"}); e != nil {
		t.Fatal(e)
	}
	if got := names(a); !reflect.DeepEqual(got, []string{"review", "tdd"}) {
		t.Fatalf("space A names = %v, want [review tdd]", got)
	}

	// Space B installs 2.0.0, whose archive materializes different names.
	second := release("2.0.0", "c")
	if e := f.store.UpsertPluginSkills(f.ctx, second, []string{"code-review"}); e != nil {
		t.Fatal(e)
	}
	if _, e := f.store.InstallSpacePlugin(f.ctx, f.tenant, b, owner, "plugins", "b1", store.PluginInstallInput{Identifier: "official/pack"}); e != nil {
		t.Fatal(e)
	}
	if got := names(b); !reflect.DeepEqual(got, []string{"code-review"}) {
		t.Fatalf("space B names = %v, want [code-review]", got)
	}
	if got := names(a); !reflect.DeepEqual(got, []string{"review", "tdd"}) {
		t.Fatalf("space A (frozen at 1.0.0) names = %v after B's install, want [review tdd]", got)
	}

	// A failed upgrade in A (stale selection version) records 2.0.0's names but leaves A on 1.0.0.
	if _, e := f.store.InstallSpacePlugin(f.ctx, f.tenant, a, owner, "plugins", "a2", store.PluginInstallInput{Identifier: "official/pack", Version: 99}); e == nil {
		t.Fatal("stale upgrade accepted")
	}
	if got := names(a); !reflect.DeepEqual(got, []string{"review", "tdd"}) {
		t.Fatalf("space A names = %v after a failed upgrade, want [review tdd]", got)
	}
}
