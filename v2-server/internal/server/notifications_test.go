package server_test

import (
	"strings"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
)

func TestNotificationHTTP(t *testing.T) {
	f := newSquadServer(t)
	tid := f.tenant(t)
	f.project(t)
	owner := f.taskClient()
	owner.login()
	storetest.SignIn(t, f.store, "alice", "Alice", store.RoleMember)
	alice := f.taskClient()
	if status, body := alice.do("POST", "/auth/login", map[string]string{"provider": "email", "email": storetest.Email("alice")}); status != 200 {
		t.Fatalf("alice login = %d %v", status, body)
	}
	_, me := alice.do("GET", "/api/v1/me", nil)
	aliceID, _ := me["id"].(string)
	if aliceID == "" {
		t.Fatalf("me = %v", me)
	}
	base := "/api/v2/tenants/" + tid
	inbox := base + "/notifications"

	// Assigning a task to alice puts it in her inbox, unread, and nowhere in the owner's.
	status, task := owner.do("POST", base+"/tasks", map[string]any{"title": "看一下登录页", "projectId": taskProjectID,
		"assignee": map[string]any{"type": "human", "id": aliceID}})
	if status != 201 {
		t.Fatalf("create = %d %v", status, task)
	}
	taskID, _ := task["id"].(string)
	status, got := alice.do("GET", inbox, nil)
	items, _ := got["items"].([]any)
	if status != 200 || got["unread"] != float64(1) || len(items) != 1 {
		t.Fatalf("alice inbox = %d %v", status, got)
	}
	first, _ := items[0].(map[string]any)
	if first["kind"] != store.NotifyAssigned || first["taskId"] != taskID || first["actorName"] != "演示用户" || first["readAt"] != nil {
		t.Fatalf("assigned = %v", first)
	}
	if _, got := owner.do("GET", inbox, nil); got["unread"] != float64(0) {
		t.Fatalf("owner inbox = %v", got)
	}

	// She moves it to 待验收; the creator is told who did it.
	_, task = alice.do("POST", base+"/tasks/"+taskID+"/status", map[string]any{"status": store.TaskRunning, "version": task["version"]})
	if status, body := alice.do("POST", base+"/tasks/"+taskID+"/status", map[string]any{"status": store.TaskReview, "version": task["version"]}); status != 200 {
		t.Fatalf("review = %d %v", status, body)
	}
	_, got = owner.do("GET", inbox+"?limit=1", nil)
	items, _ = got["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["kind"] != store.NotifyReview || items[0].(map[string]any)["actorName"] != "Alice" {
		t.Fatalf("owner after review = %v", got)
	}

	// Pages follow nextCursor to the end; the last page has none.
	if _, got := owner.do("GET", inbox+"?limit=1", nil); got["nextCursor"] != "" {
		t.Fatalf("one notification has a next page: %v", got)
	}
	owner.do("POST", base+"/tasks", map[string]any{"title": "第二条", "projectId": taskProjectID, "assignee": map[string]any{"type": "human", "id": aliceID}})
	_, page := alice.do("GET", inbox+"?limit=1", nil)
	next, _ := page["nextCursor"].(string)
	if next == "" || page["unread"] != float64(2) {
		t.Fatalf("first page = %v", page)
	}
	_, page = alice.do("GET", inbox+"?limit=1&cursor="+next, nil)
	items, _ = page["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["id"] != first["id"] || page["nextCursor"] != "" {
		t.Fatalf("second page = %v", page)
	}

	// Bad limits, cursors and empty read requests are client errors.
	for _, q := range []string{"?limit=0", "?limit=101", "?limit=x"} {
		if status, body := alice.do("GET", inbox+q, nil); status != 400 || body["code"] != "invalid_limit" {
			t.Fatalf("limit %s = %d %v", q, status, body)
		}
	}
	for _, q := range []string{"?cursor=!!", "?cursor=bm90LWEtY3Vyc29y", "?cursor=MjAyNi0xMS0xOVQwMDowMDowMFp8bm9wZQ"} {
		if status, body := alice.do("GET", inbox+q, nil); status != 400 || body["code"] != "invalid_cursor" {
			t.Fatalf("cursor %s = %d %v", q, status, body)
		}
	}
	for _, body := range []map[string]any{{}, {"ids": []string{}}, {"ids": []string{"nope"}}} {
		if status, got := alice.do("POST", inbox+"/read", body); status != 400 || got["code"] != "invalid_notification_ids" {
			t.Fatalf("read %v = %d %v", body, status, got)
		}
	}

	// Someone else cannot mark her notification; she can.
	id, _ := first["id"].(string)
	if status, got := owner.do("POST", inbox+"/read", map[string]any{"ids": []string{id}}); status != 200 || got["unread"] != float64(1) {
		t.Fatalf("owner marking alice's = %d %v", status, got)
	}
	if _, got := alice.do("GET", inbox, nil); got["unread"] != float64(2) {
		t.Fatalf("alice's was marked by someone else: %v", got)
	}
	if status, got := alice.do("POST", inbox+"/read", map[string]any{"ids": []string{strings.ToUpper(id)}}); status != 200 || got["unread"] != float64(1) {
		t.Fatalf("mark read = %d %v", status, got)
	}
	if status, got := owner.do("POST", inbox+"/read", map[string]any{"all": true}); status != 200 || got["unread"] != float64(0) {
		t.Fatalf("mark all = %d %v", status, got)
	}

	// Removed from the team, she can no longer read the inbox at all.
	if _, e := f.db.Exec(f.ctx, `DELETE FROM memberships WHERE tenant_id = $1 AND user_id = $2`, tid, aliceID); e != nil {
		t.Fatal(e)
	}
	if status, body := alice.do("GET", inbox, nil); status != 404 {
		t.Fatalf("removed member inbox = %d %v", status, body)
	}
}
