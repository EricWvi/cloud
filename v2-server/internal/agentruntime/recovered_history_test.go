//go:build linux

package agentruntime

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
)

func userChunk(t *testing.T, seq uint32, text string, preview bool) execution.HistoryRecord {
	t.Helper()
	data, err := json.Marshal(map[string]any{"type": "update", "update": map[string]any{"sessionUpdate": "user_message_chunk", "content": map[string]string{"type": "text", "text": text}}})
	if err != nil {
		t.Fatal(err)
	}
	return execution.HistoryRecord{Sequence: seq, Data: data, Preview: preview}
}

func TestRecoveredHistoryFollowsTheEarlierConversationWithoutTheHandoff(t *testing.T) {
	const handoff = "<ora_previous_conversation>…</ora_previous_conversation>"
	earlier := []execution.HistoryRecord{userChunk(t, 1, "第一轮", false), userChunk(t, 4, "第二轮", false)}
	h := newRecoveredHistory(&execution.Recovery{Handoff: handoff, History: earlier})

	fresh := &execution.HistorySnapshot{Records: []execution.HistoryRecord{userChunk(t, 1, handoff, false), userChunk(t, 2, "重启后的消息", false), userChunk(t, 3, "进行中", true)}}
	got := h.join(fresh, true)
	want := []execution.HistoryRecord{earlier[0], earlier[1], userChunk(t, 7, "重启后的消息", false), userChunk(t, 8, "进行中", true)}
	if !reflect.DeepEqual(got.Records, want) {
		t.Fatalf("joined snapshot = %+v, want %+v", got.Records, want)
	}

	// A preview holds only pending records; the page places them among the records it has.
	preview := h.join(&execution.HistorySnapshot{Records: []execution.HistoryRecord{userChunk(t, 3, "进行中", true)}}, false)
	if len(preview.Records) != 1 || preview.Records[0].Sequence != 8 {
		t.Fatalf("joined preview = %+v", preview.Records)
	}

	// The user typing the same words is kept: only the exact handoff block is left out.
	typed := h.join(&execution.HistorySnapshot{Records: []execution.HistoryRecord{userChunk(t, 5, "…", false)}}, false)
	if len(typed.Records) != 1 {
		t.Fatalf("a different user block was left out: %+v", typed.Records)
	}
	var none *recoveredHistory
	if same := none.join(fresh, true); same != fresh {
		t.Fatal("a session that was not recovered must publish its snapshot unchanged")
	}
}
