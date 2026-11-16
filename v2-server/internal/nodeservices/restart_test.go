package nodeservices

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRestartedServiceRejectsTokensIssuedBeforeTheRestart starts a second service from the same
// options and state directory, as a restarted Cloud process does. A grant lives only in the
// memory of the instance that issued it: nothing under the state directory carries it, and
// the new instance refuses the old token and grant even for the same admitted execution and
// binding, while the old instance still accepts them and the new instance's own token works.
func TestRestartedServiceRejectsTokensIssuedBeforeTheRestart(t *testing.T) {
	before := testService(t, FixtureProvider{})
	beforeClient := serviceClient(t, before, true)
	token, grant := issueToken(t, before, beforeClient, "execution")
	// A grant could be persisted lazily, for example on renewal, so the state directory is
	// checked again after both instances have issued, renewed and refused grants.
	unpersisted := func() {
		t.Helper()
		err := filepath.WalkDir(before.opts.StateDirectory, func(path string, d fs.DirEntry, err error) error {
			if err != nil || !d.Type().IsRegular() {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if strings.Contains(string(data), token) || strings.Contains(string(data), grant) {
				t.Errorf("%s persists the grant", path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if status, data := request(t, beforeClient, "POST", before.URL+"/internal/v1/model-grants/"+grant+"/renew", "", `{"executionId":"execution"}`); status != 200 {
		t.Fatalf("renewal on the instance that issued the grant=%d %s", status, data)
	}
	unpersisted()

	after, err := Start(before.opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := after.Close(); err != nil {
			t.Error(err)
		}
	})
	afterClient := serviceClient(t, after, true)
	if err := after.Permit("execution", "binding"); err != nil {
		t.Fatal(err)
	}
	completion := `{"messages":[{"role":"user","content":"Hi"}]}`
	if status, data := request(t, beforeClient, "POST", before.URL+"/runtime/openai/v1/chat/completions", token, completion); status != 200 {
		t.Fatalf("token on the instance that issued it=%d %s", status, data)
	}
	if status, _ := request(t, afterClient, "POST", after.URL+"/runtime/openai/v1/chat/completions", token, completion); status != 401 {
		t.Fatalf("token issued before the restart=%d", status)
	}
	if status, _ := request(t, afterClient, "POST", after.URL+"/internal/v1/model-grants/"+grant+"/renew", "", `{"executionId":"execution"}`); status != 403 {
		t.Fatalf("renewal of a grant issued before the restart=%d", status)
	}
	if status, _ := request(t, afterClient, "DELETE", after.URL+"/internal/v1/model-grants/"+grant, "", `{"executionId":"execution"}`); status != 403 {
		t.Fatalf("revocation of a grant issued before the restart=%d", status)
	}
	fresh, _ := issueToken(t, after, afterClient, "execution")
	if status, data := request(t, afterClient, "POST", after.URL+"/runtime/openai/v1/chat/completions", fresh, completion); status != 200 {
		t.Fatalf("token issued after the restart=%d %s", status, data)
	}
	unpersisted()
}
