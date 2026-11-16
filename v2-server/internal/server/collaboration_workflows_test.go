package server_test

import (
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/workflow"
)

func TestCollaborationWorkflowTargetsAndFormsContract(t *testing.T) {
	f := newSquadServer(t)
	f.taskRunServer(t)
	tid := f.tenant(t)

	// Create a second tenant directly so it is distinct from the demo tenant shared by SignIn.
	var foreignTid string
	if err := f.db.QueryRow(f.ctx, `INSERT INTO tenants (name) VALUES ('别的工作区') RETURNING id::text`).Scan(&foreignTid); err != nil {
		t.Fatal(err)
	}
	if foreignTid == tid {
		t.Fatalf("expected distinct foreign tenant, got %s", foreignTid)
	}

	v1Graph := `{
		"nodes": [{
			"id": "start",
			"type": "workflow",
			"data": {
				"kind": "start",
				"title": "开始",
				"inputVariables": [
					{"name": "branch", "displayName": "目标分支", "valueType": "string", "fieldType": "text-input", "required": true, "value": "main"},
					{"name": "strict", "displayName": "严格模式", "valueType": "boolean", "fieldType": "checkbox", "required": false, "value": false},
					{"name": "severity", "displayName": "级别", "valueType": "string", "fieldType": "select", "options": ["low", "high"], "value": "high"}
				]
			}
		}],
		"edges": []
	}`
	v2Graph := `{
		"nodes": [{
			"id": "start",
			"type": "workflow",
			"data": {
				"kind": "start",
				"title": "开始",
				"inputVariables": [
					{"name": "scope", "displayName": "检查范围", "valueType": "string", "fieldType": "paragraph", "required": true}
				]
			}
		}],
		"edges": []
	}`
	emptyGraph := `{"nodes":[{"id":"start","type":"workflow","data":{"kind":"start","title":"开始"}}],"edges":[]}`

	published, _, err := f.store.CreateWorkflow(f.ctx, tid, "代码评审", v1Graph)
	if err != nil {
		t.Fatal(err)
	}
	v1Snap, err := f.store.PublishWorkflow(f.ctx, tid, published.ID, "v1", workflow.ValidateWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	// Editing the draft without publishing must not alter the published start form.
	if _, err := f.store.UpdateWorkflowDraft(f.ctx, tid, published.ID, v2Graph); err != nil {
		t.Fatal(err)
	}

	noVars, _, err := f.store.CreateWorkflow(f.ctx, tid, "空表单流程", emptyGraph)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.PublishWorkflow(f.ctx, tid, noVars.ID, "v1", workflow.ValidateWorkflow); err != nil {
		t.Fatal(err)
	}

	unpublished, _, err := f.store.CreateWorkflow(f.ctx, tid, "未发布草稿", v1Graph)
	if err != nil {
		t.Fatal(err)
	}

	deleted, _, err := f.store.CreateWorkflow(f.ctx, tid, "已删除流程", v1Graph)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.PublishWorkflow(f.ctx, tid, deleted.ID, "v1", workflow.ValidateWorkflow); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DeleteWorkflow(f.ctx, tid, deleted.ID); err != nil {
		t.Fatal(err)
	}

	foreignWf, _, err := f.store.CreateWorkflow(f.ctx, foreignTid, "跨租户流程", v1Graph)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.PublishWorkflow(f.ctx, foreignTid, foreignWf.ID, "v1", workflow.ValidateWorkflow); err != nil {
		t.Fatal(err)
	}

	// newContractClient validates every response against api/openapi.json.
	contract := newContractClient(t, f.srv.URL)
	contract.login()

	status, targets := contract.do("GET", "/api/v1/tenants/"+tid+"/collaboration/targets", nil)
	if status != 200 {
		t.Fatalf("targets = %d %v", status, targets)
	}
	workflows := map[string]map[string]any{}
	for _, item := range targets["items"].([]any) {
		target := item.(map[string]any)
		if target["type"] == "workflow" {
			workflows[target["id"].(string)] = target
		}
	}
	if len(workflows) != 2 {
		t.Fatalf("workflow targets = %v, want 2 published live workflows", workflows)
	}
	if _, ok := workflows[unpublished.ID]; ok {
		t.Fatalf("unpublished workflow leaked into targets: %v", workflows)
	}
	if _, ok := workflows[deleted.ID]; ok {
		t.Fatalf("deleted workflow leaked into targets: %v", workflows)
	}
	if _, ok := workflows[foreignWf.ID]; ok {
		t.Fatalf("foreign tenant workflow leaked into targets: %v", workflows)
	}

	wfTarget := workflows[published.ID]
	desc := wfTarget["interactionDescriptor"].(map[string]any)
	if wfTarget["displayName"] != "代码评审" || desc["mode"] != "form" || desc["requiresTask"] != false || desc["formRef"] != published.ID {
		t.Fatalf("workflow target = %v", wfTarget)
	}

	// Reading the form returns the v1 published snapshot fields, not the unpublished draft.
	status, form := contract.do("GET", "/api/v1/tenants/"+tid+"/collaboration/forms/"+desc["formRef"].(string), nil)
	if status != 200 || form["formRef"] != published.ID || form["title"] != "代码评审" {
		t.Fatalf("form = %d %v", status, form)
	}
	fields := form["fields"].([]any)
	if len(fields) != 3 {
		t.Fatalf("fields = %v, want 3 from v1", fields)
	}
	branch := fields[0].(map[string]any)
	strict := fields[1].(map[string]any)
	severity := fields[2].(map[string]any)
	if branch["key"] != "branch" || branch["label"] != "目标分支" || branch["type"] != "text" || branch["required"] != true || branch["defaultValue"] != "main" {
		t.Fatalf("branch field = %v", branch)
	}
	if strict["key"] != "strict" || strict["type"] != "boolean" || strict["defaultValue"] != false {
		t.Fatalf("strict field = %v", strict)
	}
	if severity["key"] != "severity" || severity["type"] != "select" || severity["defaultValue"] != "high" || len(severity["options"].([]any)) != 2 {
		t.Fatalf("severity field = %v", severity)
	}

	// Empty Start variables produce an empty array (never null) conforming to FormDescriptor.
	status, emptyForm := contract.do("GET", "/api/v1/tenants/"+tid+"/collaboration/forms/"+noVars.ID, nil)
	if status != 200 || len(emptyForm["fields"].([]any)) != 0 {
		t.Fatalf("empty form = %d %v", status, emptyForm)
	}

	// Publishing v2 updates the form; reactivating v1 switches it back.
	if _, err := f.store.PublishWorkflow(f.ctx, tid, published.ID, "v2", workflow.ValidateWorkflow); err != nil {
		t.Fatal(err)
	}
	status, formV2 := contract.do("GET", "/api/v1/tenants/"+tid+"/collaboration/forms/"+published.ID, nil)
	if status != 200 || len(formV2["fields"].([]any)) != 1 || formV2["fields"].([]any)[0].(map[string]any)["key"] != "scope" || formV2["fields"].([]any)[0].(map[string]any)["type"] != "textarea" {
		t.Fatalf("v2 form = %d %v", status, formV2)
	}
	if _, err := f.store.ActivateWorkflow(f.ctx, tid, published.ID, v1Snap.ID); err != nil {
		t.Fatal(err)
	}
	status, formV1Again := contract.do("GET", "/api/v1/tenants/"+tid+"/collaboration/forms/"+published.ID, nil)
	if status != 200 || len(formV1Again["fields"].([]any)) != 3 {
		t.Fatalf("reactivated v1 form = %d %v", status, formV1Again)
	}

	// Unpublished, deleted, foreign, missing, and non-UUID refs return 404 conforming to Error.
	for _, missingRef := range []string{
		unpublished.ID,
		deleted.ID,
		foreignWf.ID,
		"00000000-0000-4000-8000-000000000099",
		"not-a-uuid",
	} {
		status, errBody := contract.do("GET", "/api/v1/tenants/"+tid+"/collaboration/forms/"+missingRef, nil)
		if status != 404 || errBody["code"] != "form_descriptor_not_found" {
			t.Fatalf("missing form %s = %d %v", missingRef, status, errBody)
		}
	}
}
