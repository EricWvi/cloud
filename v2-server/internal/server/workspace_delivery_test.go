package server_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/execution/executiontest"
	"github.com/wanglongan587/cloud/v2-server/internal/projectrepos"
	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
	"github.com/wanglongan587/cloud/v2-server/internal/taskrun"
	"github.com/wanglongan587/cloud/v2-server/internal/workspacefs"
)

// deliveryFixture is one tenant with an owner and a member, a seeded project whose mirror is a
// real bare repository, and the HTTP server reading deliveries from that mirror.
type deliveryFixture struct {
	t                     *testing.T
	ctx                   context.Context
	s                     *store.Store
	fs                    *workspacefs.DeliveryFS
	tasks                 *taskrun.Host
	project               store.Project
	tenant, owner, member string
	ownerToken, memberTok string
	base                  string
	ownerHTTP, memberHTTP *http.Client
}

func newDeliveryFixture(t *testing.T) *deliveryFixture {
	t.Helper()
	f := &deliveryFixture{t: t, ctx: context.Background(), s: storetest.Open(t)}
	f.ownerToken = storetest.SignIn(t, f.s, "delivery-owner", "Owner", store.RoleOwner)
	f.memberTok = storetest.SignIn(t, f.s, "delivery-member", "Member", store.RoleMember)
	f.owner, f.tenant = storetest.Login(t, f.s, "delivery-owner")
	member, e := f.s.SessionUser(f.ctx, f.memberTok)
	if e != nil {
		t.Fatal(e)
	}
	f.member = member.ID
	storetest.Access(t, f.s, f.tenant, f.member)
	projects, e := projectrepos.New(f.s, projectrepos.Options{Root: t.TempDir()})
	if e != nil {
		t.Fatal(e)
	}
	if f.project, e = projects.SeedDemo(f.ctx, f.scope()); e != nil {
		t.Fatal(e)
	}
	if f.fs, e = workspacefs.NewDeliveryFS(t.TempDir(), workspacefs.DefaultDeliveryObjectBytes); e != nil {
		t.Fatal(e)
	}
	stub := &executiontest.Stub{}
	f.tasks = taskrun.New(f.s, stub, stub, nil)
	t.Cleanup(f.tasks.Close)
	f.base = f.serve(server.DefaultMaxPatchBytes)
	f.ownerHTTP, f.memberHTTP = f.client(f.base, f.ownerToken), f.client(f.base, f.memberTok)
	return f
}

func (f *deliveryFixture) scope() execution.Scope {
	return execution.Scope{TenantID: f.tenant, UserID: f.owner}
}

// serve starts another server over the same store and mirror with its own patch cap.
func (f *deliveryFixture) serve(maxPatch int) string {
	return f.serveWith(f.fs, maxPatch).URL
}

// serveWith starts a server reading deliveries through reader, for fault and gate injection.
func (f *deliveryFixture) serveWith(reader server.DeliveryReader, maxPatch int) *httptest.Server {
	srv := httptest.NewServer(server.New(f.s, server.Options{SessionTTL: time.Hour, TaskRuns: f.tasks, Deliveries: reader, MaxPatchBytes: maxPatch}))
	f.t.Cleanup(srv.Close)
	return srv
}

func (f *deliveryFixture) client(base, token string) *http.Client {
	jar, _ := cookiejar.New(nil)
	u, _ := url.Parse(base)
	jar.SetCookies(u, []*http.Cookie{{Name: server.SessionCookie, Value: token, Path: "/"}})
	return &http.Client{Jar: jar}
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func (f *deliveryFixture) git(dir string, args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.email=t@example.com", "-c", "user.name=t"}, args...)...)
	cmd.Dir = dir
	out, e := cmd.CombinedOutput()
	if e != nil {
		f.t.Fatalf("git %v: %v: %s", args, e, out)
	}
	return strings.TrimSpace(string(out))
}

func (f *deliveryFixture) write(root, name, content string) {
	f.t.Helper()
	if e := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o755); e != nil {
		f.t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); e != nil {
		f.t.Fatal(e)
	}
}

// deliverySource is one page source whose turns enroll for delivery, with its own checkout.
type deliverySource struct {
	f         *deliveryFixture
	caller    execution.Caller
	prefix    string
	session   string
	workspace string
	root      string
	turns     int
	bind      func(ref execution.Ref)
}

func (f *deliveryFixture) checkout() string {
	f.t.Helper()
	root := filepath.Join(f.t.TempDir(), "checkout")
	f.git("", "clone", "-q", f.project.MirrorPath, root)
	if head := f.git(root, "rev-parse", "HEAD"); head != f.project.Commit {
		f.t.Fatalf("checkout HEAD %s, want project commit %s", head, f.project.Commit)
	}
	return root
}

func (f *deliveryFixture) chatSource(title string) *deliverySource {
	f.t.Helper()
	c, e := f.s.CreateChatConversation(f.ctx, store.NewChatConversation{TenantID: f.tenant, UserID: f.owner, CreateKey: newUUID(), ProjectID: f.project.ID, ExecutorID: "fixture", Title: title})
	if e != nil {
		f.t.Fatal(e)
	}
	src := &deliverySource{f: f, caller: execution.Caller{Kind: execution.CallerChat, ID: c.ID},
		prefix: "/api/v2/tenants/" + f.tenant + "/chat/conversations/" + c.ID + "/workspace"}
	src.bind = func(ref execution.Ref) {
		if e := f.s.RecordChatTurn(f.ctx, c.ID, ref.SessionID, src.caller.AttemptID, ref.TurnID, time.Now()); e != nil {
			f.t.Fatal(e)
		}
	}
	return src
}

func (f *deliveryFixture) taskSource(title string) *deliverySource {
	f.t.Helper()
	task, e := f.s.CreateTask(f.ctx, f.tenant, f.owner, store.TaskInput{Title: title, ProjectID: f.project.ID})
	if e != nil {
		f.t.Fatal(e)
	}
	run, e := f.s.CreateTaskRun(f.ctx, store.NewTaskRun{TenantID: f.tenant, TaskID: task.ID, StartedBy: f.owner, StartKey: newUUID(), ExecutorID: "opencode:fixture"})
	if e != nil {
		f.t.Fatal(e)
	}
	src := &deliverySource{f: f, caller: execution.Caller{Kind: execution.CallerTask, ID: run.ID},
		prefix: "/api/v2/tenants/" + f.tenant + "/tasks/" + task.ID + "/runs/" + run.ID + "/workspace"}
	src.bind = func(ref execution.Ref) {
		if e := f.s.RecordTaskTurn(f.ctx, run.ID, ref.SessionID, ref.TurnID); e != nil {
			f.t.Fatal(e)
		}
	}
	return src
}

// workflowSource is a run with one Agent node attempt; deleting it needs the attempt finished.
func (f *deliveryFixture) workflowSource() (*deliverySource, func()) {
	f.t.Helper()
	wf, _, e := f.s.CreateWorkflow(f.ctx, f.tenant, "交付", `{"nodes":[],"edges":[]}`)
	if e != nil {
		f.t.Fatal(e)
	}
	if _, e := f.s.PublishWorkflow(f.ctx, f.tenant, wf.ID, "v1", func(string) error { return nil }); e != nil {
		f.t.Fatal(e)
	}
	run, e := f.s.CreateWorkflowRun(f.ctx, f.tenant, wf.ID, "", store.WorkflowRunInput{ProjectID: f.project.ID})
	if e != nil {
		f.t.Fatal(e)
	}
	node, e := f.s.CreateWorkflowNodeRun(f.ctx, f.tenant, run.ID, store.WorkflowNodeRunInput{NodeID: "agent", NodeType: "agent"})
	if e != nil {
		f.t.Fatal(e)
	}
	src := &deliverySource{f: f, caller: execution.Caller{Kind: execution.CallerWorkflow, ID: run.ID, NodeID: "agent", AttemptID: node.ID},
		prefix: "/api/v2/tenants/" + f.tenant + "/workflow-runs/" + run.ID + "/workspace", bind: func(execution.Ref) {}}
	remove := func() {
		f.t.Helper()
		output := `{}`
		if _, e := f.s.CompleteWorkflowNodeRun(f.ctx, f.tenant, run.ID, node.ID, store.WorkflowNodeResult{Output: &output}); e != nil {
			f.t.Fatal(e)
		}
		if e := f.s.DeleteWorkflowRun(f.ctx, f.tenant, run.ID); e != nil {
			f.t.Fatal(e)
		}
	}
	return src, remove
}

func (src *deliverySource) source() store.DeliverySource {
	s := store.DeliverySource{Kind: src.caller.Kind, ID: src.caller.ID}
	if src.caller.Kind == execution.CallerTask {
		parts := strings.Split(src.prefix, "/")
		s.TaskID = parts[len(parts)-4]
	}
	return s
}

// start admits the source's next turn, enrolled for delivery, on its checkout.
func (src *deliverySource) start() execution.Ref {
	f := src.f
	f.t.Helper()
	src.turns++
	if src.caller.Kind != execution.CallerWorkflow {
		src.caller.AttemptID = newUUID()
	}
	if src.session == "" {
		src.session, src.workspace, src.root = newUUID(), newUUID(), f.checkout()
	}
	ref := execution.Ref{SessionID: src.session, TurnID: newUUID()}
	turn := execution.Turn{Ref: ref, Caller: src.caller, Prompt: execution.TextPrompt("改"), StartedAt: time.Now()}
	if src.turns == 1 {
		session := execution.Session{ID: src.session, WorkspaceID: src.workspace, Model: execution.Model{ID: "fixture"}, Target: &execution.Target{ProjectID: f.project.ID}}
		if e := f.s.CreateDeliveryExecutionSession(f.ctx, f.scope(), session, turn); e != nil {
			f.t.Fatal(e)
		}
		if e := f.s.RecordWorkspace(f.ctx, src.workspace, store.WorkspaceLocation{Path: src.root, BaseCommit: f.project.Commit}); e != nil {
			f.t.Fatal(e)
		}
	} else if e := f.s.AppendDeliveryExecutionTurn(f.ctx, f.scope(), src.session, turn); e != nil {
		f.t.Fatal(e)
	}
	src.bind(ref)
	return ref
}

// finish ends the turn and delivers its checkout the way the execution runtime does: write the
// ref, then the record. A failed delivery records changes_too_large without touching Git.
func (src *deliverySource) finish(ref execution.Ref, fail bool) store.DeliveryRecord {
	f := src.f
	f.t.Helper()
	if _, e := f.s.StageDeliveryTerminal(f.ctx, f.scope(), ref, execution.Result{Status: execution.StatusCompleted, AnswerComplete: true}); e != nil {
		f.t.Fatal(e)
	}
	work, e := f.s.DeliveryWork(f.ctx, ref)
	if e != nil {
		f.t.Fatal(e)
	}
	record := store.DeliveryRecord{ID: newUUID(), BaseCommit: work.Workspace.BaseCommit}
	if fail {
		record.Status, record.ReasonCode, record.Reason = store.DeliveryFailed, "changes_too_large", "改动过大"
	} else {
		snap, e := f.fs.Deliver(f.ctx, workspacefs.DeliveryRequest{
			Workspace:  workspacefs.Workspace{Root: work.Workspace.Path, BaseCommit: work.Workspace.BaseCommit},
			MirrorPath: work.MirrorPath, ProjectID: work.ProjectID, DeliveryID: record.ID, Message: "Ora delivery: test",
		})
		if e != nil {
			f.t.Fatal(e)
		}
		record.Status, record.DeliveryCommit = store.DeliveryEmpty, snap.Commit
		if snap.Commit != "" {
			record.Status = store.DeliveryDelivered
		}
	}
	winner, e := f.s.CommitTurnDelivery(f.ctx, work, record)
	if e != nil || winner.ID != record.ID {
		f.t.Fatalf("commit delivery: %+v %v", winner, e)
	}
	return winner
}

func (src *deliverySource) turn(fail bool) store.DeliveryRecord {
	src.f.t.Helper()
	return src.finish(src.start(), fail)
}

func (src *deliverySource) get(client *http.Client, path string) workspaceResponse {
	src.f.t.Helper()
	return workspaceCall(src.f.t, client, http.MethodGet, src.f.base, src.prefix+path)
}

// deliveryRefs lists the mirror's whole delivery namespace, name and OID.
func (f *deliveryFixture) deliveryRefs() string {
	f.t.Helper()
	return f.git("", "--git-dir="+f.project.MirrorPath, "for-each-ref", "--format=%(refname) %(objectname)", "refs/ora/deliveries")
}

func state(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	s, ok := body["deliveryState"].(map[string]any)
	if !ok {
		t.Fatalf("no deliveryState in %v", body)
	}
	return s
}

func deliveryID(s map[string]any, key string) any {
	if d, ok := s[key].(map[string]any); ok {
		return d["id"]
	}
	return nil
}

func changedPaths(body map[string]any) []string {
	var out []string
	for _, f := range body["files"].([]any) {
		out = append(out, f.(map[string]any)["path"].(string))
	}
	return out
}

// expectState compares the whole delivery state: mode, inProgress and which delivery fills each
// slot (nil for an absent one).
func expectState(t *testing.T, what string, body map[string]any, mode string, inProgress bool, view, latest, failed any) {
	t.Helper()
	s := state(t, body)
	got := []any{s["mode"], s["inProgress"], deliveryID(s, "viewDelivery"), deliveryID(s, "latestDelivery"), deliveryID(s, "latestFailedDelivery")}
	want := []any{mode, inProgress, view, latest, failed}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: deliveryState (mode, inProgress, view, latest, failed) = %v, want %v", what, got, want)
	}
}

func noStoreHeader(t *testing.T, what string, res workspaceResponse) {
	t.Helper()
	if got := res.header.Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("%s: Cache-Control = %q, want private, no-store", what, got)
	}
}

func TestDeliveryWorkspaceContractAndAuthorization(t *testing.T) {
	f := newDeliveryFixture(t)

	// Legacy: a conversation from before deliveries keeps its live working-directory view.
	legacy := f.chatSource("旧对话")
	root, commit := agentCheckout(t)
	legacyRef := execution.Ref{SessionID: newUUID(), TurnID: newUUID()}
	legacy.caller.AttemptID = "k1"
	if e := f.s.CreateExecutionSession(f.ctx, f.scope(), execution.Session{ID: legacyRef.SessionID, WorkspaceID: "legacy-w", Model: execution.Model{ID: "fixture"}},
		execution.Turn{Ref: legacyRef, Caller: legacy.caller, Prompt: execution.TextPrompt("改")}); e != nil {
		t.Fatal(e)
	}
	legacy.bind(legacyRef)
	if e := f.s.RecordWorkspace(f.ctx, "legacy-w", store.WorkspaceLocation{Path: root, BaseCommit: commit}); e != nil {
		t.Fatal(e)
	}
	res := legacy.get(f.ownerHTTP, "/changes")
	if res.status != 200 || res.body["baseCommit"] != commit || !reflect.DeepEqual(changedPaths(res.body), []string{"README.md", "src/app.go"}) {
		t.Fatalf("legacy changes = %d %v", res.status, res.body)
	}
	expectState(t, "legacy changes", res.body, "working_directory", false, nil, nil, nil)
	noStoreHeader(t, "legacy changes", res)
	if res = legacy.get(f.ownerHTTP, "/diff?path=README.md"); res.status != 200 || !strings.Contains(res.body["patch"].(string), "+agent") {
		t.Fatalf("legacy diff = %d %v", res.status, res.body)
	}
	expectState(t, "legacy diff", res.body, "working_directory", false, nil, nil, nil)
	unknown := newUUID()
	for _, path := range []string{"/diff?path=README.md&deliveryId=" + unknown, "/deliveries/" + unknown + "/patch"} {
		if res := legacy.get(f.ownerHTTP, path); res.status != 404 || res.body["code"] != "delivery_not_found" {
			t.Fatalf("legacy %s = %d %v, want 404 delivery_not_found", path, res.status, res.body)
		}
	}

	// Task run: a delivered turn, then a turn in progress, a failed one and an empty one.
	task := f.taskSource("修复登录页")
	first := task.start()
	f.write(task.root, "README.md", "# Ora demo\n\nfirst turn\n")
	f.write(task.root, "src/new.go", "package src\n")
	// The first turn is enrolled but not delivered: never read the live directory again.
	res = task.get(f.ownerHTTP, "/changes")
	if res.status != 200 || len(res.body["files"].([]any)) != 0 || res.body["baseCommit"] != f.project.Commit {
		t.Fatalf("changes before the first delivery = %d %v", res.status, res.body)
	}
	expectState(t, "before the first delivery", res.body, "delivery", true, nil, nil, nil)
	d1 := task.finish(first, false)
	res = task.get(f.ownerHTTP, "/changes")
	if res.status != 200 || res.body["baseCommit"] != f.project.Commit || !reflect.DeepEqual(changedPaths(res.body), []string{"README.md", "src/new.go"}) {
		t.Fatalf("delivered changes = %d %v", res.status, res.body)
	}
	expectState(t, "delivered", res.body, "delivery", false, d1.ID, d1.ID, nil)
	if view := state(t, res.body)["viewDelivery"].(map[string]any); view["status"] != "delivered" || view["deliveryCommit"] != d1.DeliveryCommit ||
		view["baseCommit"] != f.project.Commit || view["turnPosition"] != float64(1) || view["turnId"] != first.TurnID {
		t.Fatalf("viewDelivery = %v", view)
	}

	second := task.start()
	f.write(task.root, "later.txt", "not delivered yet\n")
	res = task.get(f.ownerHTTP, "/changes")
	if res.status != 200 || !reflect.DeepEqual(changedPaths(res.body), []string{"README.md", "src/new.go"}) {
		t.Fatalf("changes while a turn runs = %d %v", res.status, res.body)
	}
	expectState(t, "turn in progress", res.body, "delivery", true, d1.ID, d1.ID, nil)
	if res = task.get(f.ownerHTTP, "/diff?path=later.txt"); res.status != 200 || res.body["patch"] != "" {
		t.Fatalf("diff of an undelivered file = %d %v", res.status, res.body)
	}
	expectState(t, "diff in progress", res.body, "delivery", true, d1.ID, d1.ID, nil)

	d2 := task.finish(second, true)
	res = task.get(f.ownerHTTP, "/changes")
	if res.status != 200 || !reflect.DeepEqual(changedPaths(res.body), []string{"README.md", "src/new.go"}) {
		t.Fatalf("changes after a failed delivery = %d %v", res.status, res.body)
	}
	expectState(t, "failed latest", res.body, "delivery", false, d1.ID, d2.ID, d2.ID)
	if failed := state(t, res.body)["latestFailedDelivery"].(map[string]any); failed["reasonCode"] != "changes_too_large" || failed["reason"] != "改动过大" || failed["deliveryCommit"] != nil {
		t.Fatalf("latestFailedDelivery = %v", failed)
	}
	// An unpinned diff while the latest outcome failed reads the latest good delivery.
	res = task.get(f.ownerHTTP, "/diff?path=README.md")
	if res.status != 200 || !strings.Contains(res.body["patch"].(string), "+first turn") {
		t.Fatalf("default diff after a failed delivery = %d %v", res.status, res.body)
	}
	expectState(t, "default diff after a failure", res.body, "delivery", false, d1.ID, d2.ID, d2.ID)
	for _, path := range []string{"/deliveries/" + d2.ID + "/patch", "/diff?path=README.md&deliveryId=" + d2.ID} {
		res := task.get(f.ownerHTTP, path)
		if params, _ := res.body["params"].(map[string]any); res.status != 409 || res.body["code"] != "delivery_failed" ||
			params["reasonCode"] != "changes_too_large" || params["reason"] != "改动过大" {
			t.Fatalf("failed delivery %s = %d %v", path, res.status, res.body)
		}
	}

	f.git(task.root, "checkout", "-q", "--", ".")
	f.git(task.root, "clean", "-qfd")
	d3 := task.turn(false)
	if d3.Status != store.DeliveryEmpty {
		t.Fatalf("reverted checkout delivered %+v", d3)
	}
	// Even an empty delivery supersedes the earlier snapshot.
	res = task.get(f.ownerHTTP, "/changes")
	if res.status != 200 || len(res.body["files"].([]any)) != 0 || res.body["baseCommit"] != f.project.Commit {
		t.Fatalf("changes after an empty delivery = %d %v", res.status, res.body)
	}
	expectState(t, "empty latest", res.body, "delivery", false, d3.ID, d3.ID, d2.ID)
	if res = task.get(f.ownerHTTP, "/deliveries/"+d3.ID+"/patch"); res.status != 204 || len(res.raw) != 0 {
		t.Fatalf("empty patch = %d %q", res.status, res.raw)
	}
	noStoreHeader(t, "empty patch", res)
	// A pinned diff reads the named delivery; the rest of the state is the source now.
	res = task.get(f.ownerHTTP, "/diff?path=README.md&deliveryId="+d1.ID)
	if res.status != 200 || !strings.Contains(res.body["patch"].(string), "+first turn") {
		t.Fatalf("pinned diff = %d %v", res.status, res.body)
	}
	expectState(t, "pinned diff", res.body, "delivery", false, d1.ID, d3.ID, d2.ID)
	noStoreHeader(t, "pinned diff", res)
	if res = task.get(f.ownerHTTP, "/diff?path=README.md"); res.status != 200 || res.body["patch"] != "" {
		t.Fatalf("default diff after an empty delivery = %d %v", res.status, res.body)
	}
	// Task runs are team-readable, deliveries included.
	if res = task.get(f.memberHTTP, "/changes"); res.status != 200 {
		t.Fatalf("member reads task changes = %d %v", res.status, res.body)
	}
	expectState(t, "member task changes", res.body, "delivery", false, d3.ID, d3.ID, d2.ID)
	if res = task.get(f.memberHTTP, "/deliveries/"+d1.ID+"/patch"); res.status != 200 || !strings.Contains(string(res.raw), "+first turn") {
		t.Fatalf("member downloads task patch = %d %s", res.status, res.raw)
	}
	for _, bad := range []string{"/deliveries/not-a-uuid/patch", "/diff?path=README.md&deliveryId=not-a-uuid"} {
		if res := task.get(f.ownerHTTP, bad); res.status != 400 || res.body["code"] != "invalid_delivery_id" {
			t.Fatalf("%s = %d %v, want 400 invalid_delivery_id", bad, res.status, res.body)
		}
	}

	// Workflow run: team-readable.
	flow, removeFlow := f.workflowSource()
	ref := flow.start()
	f.write(flow.root, "flow.txt", "workflow\n")
	d5 := flow.finish(ref, false)
	if res = flow.get(f.memberHTTP, "/changes"); res.status != 200 || !reflect.DeepEqual(changedPaths(res.body), []string{"flow.txt"}) {
		t.Fatalf("member reads workflow changes = %d %v", res.status, res.body)
	}
	expectState(t, "workflow changes", res.body, "delivery", false, d5.ID, d5.ID, nil)
	if view := state(t, res.body)["viewDelivery"].(map[string]any); view["nodeId"] != "agent" || view["nodeRunId"] != flow.caller.AttemptID {
		t.Fatalf("workflow viewDelivery = %v", view)
	}
	if res = flow.get(f.memberHTTP, "/deliveries/"+d5.ID+"/patch"); res.status != 200 || !strings.Contains(string(res.raw), "+workflow") {
		t.Fatalf("member downloads workflow patch = %d %s", res.status, res.raw)
	}

	// Chat: owner only; another member gets the denial an unknown conversation gets.
	chat := f.chatSource("新对话")
	ref = chat.start()
	f.write(chat.root, "chat.txt", "chat\n")
	d4 := chat.finish(ref, false)
	if res = chat.get(f.ownerHTTP, "/changes"); res.status != 200 || !reflect.DeepEqual(changedPaths(res.body), []string{"chat.txt"}) {
		t.Fatalf("owner reads chat changes = %d %v", res.status, res.body)
	}
	expectState(t, "chat changes", res.body, "delivery", false, d4.ID, d4.ID, nil)
	missing := "/api/v2/tenants/" + f.tenant + "/chat/conversations/" + newUUID() + "/workspace/changes"
	want := workspaceCall(t, f.memberHTTP, http.MethodGet, f.base, missing)
	if want.status != 404 || want.body["code"] != "not_found" {
		t.Fatalf("unknown conversation = %d %v", want.status, want.body)
	}
	for _, path := range []string{"/changes", "/diff?path=chat.txt", "/diff?path=chat.txt&deliveryId=" + d4.ID, "/deliveries/" + d4.ID + "/patch"} {
		if res := chat.get(f.memberHTTP, path); res.status != want.status || res.body["code"] != want.body["code"] || strings.Contains(string(res.raw), "chat.txt") {
			t.Fatalf("member reads chat %s = %d %s, want the unknown-conversation denial", path, res.status, res.raw)
		}
	}

	// A delivery ID only selects within its own source.
	for _, wrong := range []struct {
		src *deliverySource
		id  string
	}{{chat, d1.ID}, {flow, d1.ID}, {task, d4.ID}, {task, d5.ID}, {flow, d4.ID}} {
		for _, path := range []string{"/deliveries/" + wrong.id + "/patch", "/diff?path=README.md&deliveryId=" + wrong.id} {
			if res := wrong.src.get(f.ownerHTTP, path); res.status != 404 || res.body["code"] != "delivery_not_found" {
				t.Fatalf("%s through %s = %d %v, want 404 delivery_not_found", path, wrong.src.prefix, res.status, res.body)
			}
		}
	}

	// A deleted source's deliveries are unreadable, while its refs stay in the mirror.
	refs := f.deliveryRefs()
	if e := f.s.DeleteChatConversation(f.ctx, f.tenant, f.owner, chat.caller.ID); e != nil {
		t.Fatal(e)
	}
	removeFlow()
	for _, c := range []struct {
		src  *deliverySource
		id   string
		code string
	}{{chat, d4.ID, "not_found"}, {flow, d5.ID, "workflow_run_not_found"}} {
		for _, path := range []string{"/changes", "/diff?path=a&deliveryId=" + c.id, "/deliveries/" + c.id + "/patch"} {
			if res := c.src.get(f.ownerHTTP, path); res.status != 404 || res.body["code"] != c.code {
				t.Fatalf("deleted source %s = %d %v, want 404 %s", path, res.status, res.body, c.code)
			}
		}
		if _, e := f.s.WorkspaceDeliveryByID(f.ctx, f.scope(), c.src.source(), c.id); e == nil {
			t.Fatalf("store still reads delivery %s of a deleted source", c.id)
		}
	}
	if after := f.deliveryRefs(); after != refs {
		t.Fatalf("deleting sources changed delivery refs:\n%s\nwant\n%s", after, refs)
	}
}

func TestDeliveryViewAfterCleanup(t *testing.T) {
	f := newDeliveryFixture(t)
	task := f.taskSource("清理之后")
	ref := task.start()
	f.write(task.root, "README.md", "# Ora demo\n\nkept\n")
	d := task.finish(ref, false)
	if e := os.RemoveAll(task.root); e != nil {
		t.Fatal(e)
	}
	res := task.get(f.memberHTTP, "/changes")
	if res.status != 200 || !reflect.DeepEqual(changedPaths(res.body), []string{"README.md"}) {
		t.Fatalf("changes after cleanup = %d %v", res.status, res.body)
	}
	expectState(t, "after cleanup", res.body, "delivery", false, d.ID, d.ID, nil)
	if res = task.get(f.memberHTTP, "/diff?path=README.md"); res.status != 200 || !strings.Contains(res.body["patch"].(string), "+kept") {
		t.Fatalf("diff after cleanup = %d %v", res.status, res.body)
	}
	if res = task.get(f.memberHTTP, "/deliveries/"+d.ID+"/patch"); res.status != 200 || !strings.Contains(string(res.raw), "+kept") {
		t.Fatalf("patch after cleanup = %d %s", res.status, res.raw)
	}
	for _, path := range []string{"/entries", "/file?path=README.md"} {
		if res := task.get(f.memberHTTP, path); res.status != 410 || res.body["code"] != "workspace_removed" {
			t.Fatalf("%s after cleanup = %d %v, want 410 workspace_removed", path, res.status, res.body)
		}
	}
}

func TestWorkspacePatchDownloadCapRetainsDelivery(t *testing.T) {
	f := newDeliveryFixture(t)
	task := f.taskSource("大改动")
	ref := task.start()
	f.write(task.root, "big.txt", strings.Repeat("改动过大\n", 512))
	d := task.finish(ref, false)
	full := task.get(f.ownerHTTP, "/deliveries/"+d.ID+"/patch")
	if full.status != 200 || len(full.raw) == 0 {
		t.Fatalf("uncapped patch = %d %d bytes", full.status, len(full.raw))
	}
	refs := f.deliveryRefs()
	before, e := f.s.WorkspaceDeliveryByID(f.ctx, f.scope(), task.source(), d.ID)
	if e != nil {
		t.Fatal(e)
	}
	for _, c := range []struct {
		limit  int
		status int
	}{{len(full.raw), 200}, {len(full.raw) - 1, 413}} {
		base := f.serve(c.limit)
		res := workspaceCall(t, f.client(base, f.ownerToken), http.MethodGet, base, task.prefix+"/deliveries/"+d.ID+"/patch")
		if res.status != c.status {
			t.Fatalf("cap %d: status %d, want %d", c.limit, res.status, c.status)
		}
		noStoreHeader(t, fmt.Sprintf("cap %d", c.limit), res)
		if c.status == 200 {
			if string(res.raw) != string(full.raw) {
				t.Fatalf("cap %d: patch differs from the uncapped one", c.limit)
			}
			continue
		}
		params, _ := res.body["params"].(map[string]any)
		if res.body["code"] != "patch_too_large" || params["message"] != "改动过大，无法在线下载" || params["maxBytes"] != float64(c.limit) {
			t.Fatalf("cap %d: %v", c.limit, res.body)
		}
		if strings.Contains(string(res.raw), "diff --git") || res.header.Get("Content-Disposition") != "" {
			t.Fatalf("cap %d: a refused patch leaked partial output: %v %s", c.limit, res.header, res.raw)
		}
	}
	after, e := f.s.WorkspaceDeliveryByID(f.ctx, f.scope(), task.source(), d.ID)
	if e != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("record after a refused download = %+v %v, want %+v", after, e, before)
	}
	if got := f.deliveryRefs(); got != refs || !strings.Contains(got, d.DeliveryCommit) {
		t.Fatalf("delivery refs after a refused download:\n%s\nwant\n%s", got, refs)
	}
}

// The patch from the HTTP route applies with plain git apply on a fresh checkout of the base and
// reproduces the delivery's tree exactly: modified, new, binary, executable and deleted files.
func TestWorkspacePatchAppliesOnBaseOverHTTP(t *testing.T) {
	f := newDeliveryFixture(t)
	task := f.taskSource("应用补丁")
	ref := task.start()
	f.write(task.root, "README.md", "# Ora demo\n\npatched\n")
	f.write(task.root, "docs/guide.md", "new file\n")
	if e := os.WriteFile(filepath.Join(task.root, "logo.bin"), []byte{0, 1, 2, 0xff, 0xfe, 0, 'P', 'N', 'G'}, 0o644); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(task.root, "run.sh"), []byte("#!/bin/sh\necho hi\n"), 0o755); e != nil {
		t.Fatal(e)
	}
	d1 := task.finish(ref, false)
	// A later delivery must not leak into the first one's download.
	f.write(task.root, "second.txt", "second\n")
	if e := os.Remove(filepath.Join(task.root, "README.md")); e != nil {
		t.Fatal(e)
	}
	d2 := task.turn(false)

	for _, d := range []store.DeliveryRecord{d1, d2} {
		res := task.get(f.memberHTTP, "/deliveries/"+d.ID+"/patch")
		if res.status != 200 || res.header.Get("Content-Type") != "application/octet-stream" ||
			res.header.Get("Content-Disposition") != `attachment; filename="delivery-`+d.ID+`.patch"` {
			t.Fatalf("patch %s = %d %v", d.ID, res.status, res.header)
		}
		noStoreHeader(t, "patch", res)
		fresh := filepath.Join(t.TempDir(), "fresh")
		f.git("", "clone", "-q", f.project.MirrorPath, fresh)
		f.git(fresh, "checkout", "-q", "--detach", d.BaseCommit)
		patch := filepath.Join(t.TempDir(), "delivery.patch")
		if e := os.WriteFile(patch, res.raw, 0o600); e != nil {
			t.Fatal(e)
		}
		f.git(fresh, "apply", patch)
		f.git(fresh, "add", "-A")
		got := f.git(fresh, "write-tree")
		want := f.git("", "--git-dir="+f.project.MirrorPath, "rev-parse", d.DeliveryCommit+"^{tree}")
		if got != want {
			t.Fatalf("delivery %s applied to tree %s, want %s\n%s", d.ID, got, want, res.raw)
		}
	}
}

// Every delivery route is read-only: other methods are refused, and neither they nor GET and HEAD
// change the delivery ref namespace or any delivery record.
func TestDeliveryEndpointsAreReadOnly(t *testing.T) {
	f := newDeliveryFixture(t)
	chat, task := f.chatSource("只读"), f.taskSource("只读")
	flow, _ := f.workflowSource()
	sources := []*deliverySource{chat, task, flow}
	ids := map[*deliverySource]string{}
	for _, src := range sources {
		ref := src.start()
		f.write(src.root, "readonly.txt", "x\n")
		ids[src] = src.finish(ref, false).ID
	}
	snapshot := func() (string, []any) {
		var rows []any
		for _, src := range sources {
			view, e := f.s.WorkspaceDeliveryView(f.ctx, f.scope(), src.source())
			if e != nil {
				t.Fatal(e)
			}
			byID, e := f.s.WorkspaceDeliveryByID(f.ctx, f.scope(), src.source(), ids[src])
			if e != nil {
				t.Fatal(e)
			}
			rows = append(rows, view, byID)
		}
		return f.deliveryRefs(), rows
	}
	refs, rows := snapshot()
	if strings.Count(refs, "\n")+1 != len(sources) {
		t.Fatalf("delivery refs before = %q", refs)
	}
	for _, src := range sources {
		for _, path := range []string{"/changes", "/diff?path=readonly.txt&deliveryId=" + ids[src], "/deliveries/" + ids[src] + "/patch"} {
			for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
				req, _ := http.NewRequest(method, f.base+src.prefix+path, strings.NewReader(`{"ref":"refs/ora/deliveries/x","commit":"`+strings.Repeat("0", 40)+`"}`))
				req.Header.Set("Content-Type", "application/json")
				res, e := f.ownerHTTP.Do(req)
				if e != nil {
					t.Fatal(e)
				}
				res.Body.Close()
				// No handler serves these methods; whatever answers must refuse them.
				if res.StatusCode != http.StatusMethodNotAllowed && res.StatusCode != http.StatusNotFound {
					t.Fatalf("%s %s = %d, want it refused", method, src.prefix+path, res.StatusCode)
				}
			}
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				req, _ := http.NewRequest(method, f.base+src.prefix+path, nil)
				res, e := f.ownerHTTP.Do(req)
				if e != nil {
					t.Fatal(e)
				}
				res.Body.Close()
				if res.StatusCode != http.StatusOK {
					t.Fatalf("%s %s = %d", method, src.prefix+path, res.StatusCode)
				}
			}
		}
	}
	if gotRefs, gotRows := snapshot(); gotRefs != refs || !reflect.DeepEqual(gotRows, rows) {
		t.Fatalf("delivery routes changed state:\nrefs %s\nwant %s\nrows %+v\nwant %+v", gotRefs, refs, gotRows, rows)
	}
}

func TestEndedDeliveryWorkDoesNotShowLiveChanges(t *testing.T) {
	f := newDeliveryFixture(t)
	chat := f.chatSource("未落库的交付")
	ref := chat.start()
	f.write(chat.root, "only-live.txt", "live checkout change\n")
	if _, err := f.s.FinishExecutionTurn(f.ctx, f.scope(), ref, execution.Result{
		Status: execution.StatusFailed, FailureCode: execution.FailureDeliveryUnavailable,
	}); err != nil {
		t.Fatal(err)
	}
	res := chat.get(f.ownerHTTP, "/changes")
	if res.status != http.StatusOK {
		t.Fatalf("changes: %d %s", res.status, res.raw)
	}
	state := res.body["deliveryState"].(map[string]any)
	if state["inProgress"] != false || state["mode"] != "delivery" || len(res.body["files"].([]any)) != 0 {
		t.Fatalf("ended work used live checkout or appeared active: %s", res.raw)
	}
}

// A delivery whose ref no longer matches its record is unreadable: every delivery read answers 503
// workspace_unavailable, and no Git diagnostic, mirror path, ref or commit reaches the client.
func TestBrokenDeliveryRefAnswersWorkspaceUnavailable(t *testing.T) {
	f := newDeliveryFixture(t)
	task := f.taskSource("损坏的 ref")
	ref := task.start()
	f.write(task.root, "README.md", "# Ora demo\n\nbroken\n")
	d := task.finish(ref, false)
	name := "refs/ora/deliveries/" + f.project.ID + "/" + d.ID
	f.git("", "--git-dir="+f.project.MirrorPath, "update-ref", name, f.project.Commit)
	for _, path := range []string{"/changes", "/diff?path=README.md", "/diff?path=README.md&deliveryId=" + d.ID, "/deliveries/" + d.ID + "/patch"} {
		res := task.get(f.ownerHTTP, path)
		params, _ := res.body["params"].(map[string]any)
		if res.status != 503 || res.body["code"] != "workspace_unavailable" || params == nil || len(params) != 0 {
			t.Fatalf("%s with a broken ref = %d %s, want 503 workspace_unavailable with empty params", path, res.status, res.raw)
		}
		for _, leak := range []string{f.project.MirrorPath, "refs/ora", d.DeliveryCommit, f.project.Commit, "git", "fatal"} {
			if strings.Contains(string(res.raw), leak) {
				t.Fatalf("%s leaks %q: %s", path, leak, res.raw)
			}
		}
		if res.header.Get("Content-Disposition") != "" {
			t.Fatalf("%s: Content-Disposition on a refused read: %v", path, res.header)
		}
		noStoreHeader(t, path, res)
	}
}

// gatedPatches holds every patch build until released and records how many ran at once.
type gatedPatches struct {
	server.DeliveryReader
	entered chan struct{}
	release chan struct{}
	mu      sync.Mutex
	active  int
	peak    int
}

func (g *gatedPatches) DeliveryPatch(ctx context.Context, snap workspacefs.DeliverySnapshot, maxBytes int) ([]byte, error) {
	g.mu.Lock()
	g.active++
	g.peak = max(g.peak, g.active)
	g.mu.Unlock()
	defer func() { g.mu.Lock(); g.active--; g.mu.Unlock() }()
	g.entered <- struct{}{}
	<-g.release
	return g.DeliveryReader.DeliveryPatch(ctx, snap, maxBytes)
}

// Concurrent downloads build at most four patches at once; the others wait and then succeed, and a
// waiting request whose caller leaves never starts a build.
func TestConcurrentPatchBuildsAreBounded(t *testing.T) {
	f := newDeliveryFixture(t)
	task := f.taskSource("并发下载")
	ref := task.start()
	f.write(task.root, "README.md", "# Ora demo\n\nconcurrent\n")
	d := task.finish(ref, false)
	gate := &gatedPatches{DeliveryReader: f.fs, entered: make(chan struct{}, 16), release: make(chan struct{})}
	srv := f.serveWith(gate, server.DefaultMaxPatchBytes)
	// Registered after the server, so it runs first: a failed assertion must not leave builds
	// parked while the server's cleanup waits for them.
	var released sync.Once
	release := func() { released.Do(func() { close(gate.release) }) }
	t.Cleanup(release)
	base := srv.URL
	client := f.client(base, f.ownerToken)
	url := base + task.prefix + "/deliveries/" + d.ID + "/patch"
	const downloads = 6
	results := make(chan int, downloads)
	for range downloads {
		go func() {
			res, err := client.Get(url)
			if err != nil {
				results <- -1
				return
			}
			res.Body.Close()
			results <- res.StatusCode
		}()
	}
	for range 4 {
		<-gate.entered
	}
	// Four builds hold every slot. A fifth caller that gives up while waiting never starts one.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if res, err := client.Do(req); err == nil {
		res.Body.Close()
		t.Fatalf("a download past the bound answered %d before a slot freed", res.StatusCode)
	}
	select {
	case <-gate.entered:
		t.Fatal("a fifth patch build started while four were running")
	default:
	}
	release()
	for range downloads {
		if status := <-results; status != http.StatusOK {
			t.Fatalf("a waiting download answered %d, want 200", status)
		}
	}
	// Close waits for every handler, the abandoned one included, so the count below is final.
	srv.Close()
	if gate.peak != 4 || len(gate.entered) != downloads-4 {
		t.Fatalf("peak concurrent patch builds = %d and %d more builds after the first four, want 4 and %d", gate.peak, len(gate.entered), downloads-4)
	}
}

// TestStalledPatchReadersDoNotHoldEveryBuildSlot opens as many downloads as there are build slots
// and never reads them. Their patches are larger than the socket buffers, so each handler is
// stuck writing; a further download must still be served because the slot is released once the
// patch is built.
func TestStalledPatchReadersDoNotHoldEveryBuildSlot(t *testing.T) {
	f := newDeliveryFixture(t)
	task := f.taskSource("停滞的下载")
	ref := task.start()
	var big strings.Builder
	for i := 0; big.Len() < 12<<20; i++ {
		fmt.Fprintf(&big, "%08d %s\n", i, newUUID())
	}
	f.write(task.root, "big.txt", big.String())
	d := task.finish(ref, false)
	srv := f.serveWith(f.fs, server.DefaultMaxPatchBytes)
	u, _ := url.Parse(srv.URL)
	path := task.prefix + "/deliveries/" + d.ID + "/patch"
	for range 4 {
		conn, err := net.Dial("tcp", u.Host)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nCookie: %s=%s\r\n\r\n", path, u.Host, server.SessionCookie, f.ownerToken)
	}
	time.Sleep(2 * time.Second) // let the four handlers build their patches and block writing
	client := f.client(srv.URL, f.ownerToken)
	client.Timeout = 20 * time.Second
	res, err := client.Get(srv.URL + path)
	if err != nil {
		t.Fatalf("a download behind four stalled readers did not complete: %v", err)
	}
	defer res.Body.Close()
	n, _ := io.Copy(io.Discard, res.Body)
	if res.StatusCode != http.StatusOK || n == 0 {
		t.Fatalf("download behind stalled readers: status %d, %d bytes", res.StatusCode, n)
	}
}
