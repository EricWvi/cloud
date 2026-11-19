package server_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/routers/legacy"
)

func TestTaskCommentWorkflowDeletedRunHidesOutputAndError(t *testing.T) {
	f := newSquadServer(t)
	commentWorkflowServer(t, f)
	c := f.taskClient()
	c.login()
	tid := f.tenant(t)
	f.project(t)
	wf, _, err := f.store.CreateWorkflow(f.ctx, tid, "Deleted output", commentWorkflowGraph)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.PublishWorkflow(f.ctx, tid, wf.ID, "v1", nil); err != nil {
		t.Fatal(err)
	}
	base := "/api/v2/tenants/" + tid
	_, task := c.do("POST", base+"/tasks", map[string]any{"title": "DeletedOutput", "projectId": taskProjectID})
	taskPath := base + "/tasks/" + task["id"].(string)
	payload := map[string]any{"body": "@Deleted output", "workflow": map[string]any{"workflowId": wf.ID, "values": map[string]any{"branch": "private-input", "tags": []any{}, "meta": map[string]any{}}}}
	key := map[string]string{"Idempotency-Key": "deleted-output"}
	status, created := c.doHeaders("POST", taskPath+"/comments", payload, key)
	if status != 201 {
		t.Fatalf("create = %d %v", status, created)
	}
	runID := created["workflowRun"].(map[string]any)["id"].(string)
	waitForCommentWorkflowStatus(t, c, taskPath+"/timeline", runID, "succeeded")
	// Seed both fields so the regression checks error as well as successful output.
	if _, err := f.db.Exec(f.ctx, `UPDATE v2_workflow_runs SET error = 'private error' WHERE id = $1`, runID); err != nil {
		t.Fatal(err)
	}
	wfDoc, err := openapi3.NewLoader().LoadFromFile("../../task-workflows.openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	wfRoutes, err := legacy.NewRouter(wfDoc)
	if err != nil {
		t.Fatal(err)
	}
	taskRoutes := c.routes
	c.routes = wfRoutes
	if code, wfList := c.do("GET", taskPath+"/workflow-runs", nil); code != 200 {
		t.Fatalf("workflow-runs before delete = %d %v", code, wfList)
	} else if items := wfList["items"].([]any); len(items) != 1 || items[0].(map[string]any)["failure"] != "private error" || items[0].(map[string]any)["deleted"] != false {
		t.Fatalf("workflow-runs before delete = %v", items)
	}
	if err := f.store.DeleteWorkflowRun(f.ctx, tid, runID); err != nil {
		t.Fatal(err)
	}
	if code, wfList := c.do("GET", taskPath+"/workflow-runs", nil); code != 200 {
		t.Fatalf("workflow-runs after delete = %d %v", code, wfList)
	} else if items := wfList["items"].([]any); len(items) != 1 || items[0].(map[string]any)["deleted"] != true || items[0].(map[string]any)["failure"] != "" {
		t.Fatalf("deleted workflow-runs leaked failure: %v", items)
	}
	c.routes = taskRoutes
	for _, read := range []string{"timeline", "replay"} {
		var comment map[string]any
		if read == "timeline" {
			code, timeline := c.do("GET", taskPath+"/timeline", nil)
			if code != 200 || len(timeline["comments"].([]any)) != 1 {
				t.Fatalf("deleted timeline = %d %v", code, timeline)
			}
			comment = timeline["comments"].([]any)[0].(map[string]any)
		} else {
			code, replay := c.doHeaders("POST", taskPath+"/comments", payload, key)
			if code != 201 {
				t.Fatalf("deleted replay = %d %v", code, replay)
			}
			comment = replay
		}
		run := comment["workflowRun"].(map[string]any)
		if run["id"] != runID || run["deleted"] != true || run["output"] != nil || run["error"] != nil {
			t.Fatalf("%s exposed deleted run content: %v", read, run)
		}
	}
	// This endpoint belongs to the workflow contract, not the task client's router.
	response, err := c.http.Get(c.base + base + "/workflow-runs/" + runID)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNotFound || body["code"] != "workflow_run_not_found" {
		t.Fatalf("deleted run detail = %d %v", response.StatusCode, body)
	}
}
