package executionruntime

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
)

func historyRecord(t *testing.T, seq uint32, line map[string]any) execution.HistoryRecord {
	t.Helper()
	data, err := json.Marshal(line)
	if err != nil {
		t.Fatal(err)
	}
	return execution.HistoryRecord{Sequence: seq, Data: data}
}

func chunk(kind, text string) map[string]any {
	return map[string]any{"type": "update", "update": map[string]any{"sessionUpdate": kind, "content": map[string]string{"type": "text", "text": text}}}
}

func turnEnded() map[string]any {
	return map[string]any{"type": "turnEnded", "stop_reason": "end_turn"}
}

func conversation(t *testing.T) []execution.HistoryRecord {
	return []execution.HistoryRecord{
		historyRecord(t, 0, map[string]any{"type": "meta"}),
		historyRecord(t, 1, chunk("user_message_chunk", "写一份支付重构方案")),
		historyRecord(t, 2, chunk("agent_thought_chunk", "内部推理不交接")),
		historyRecord(t, 3, map[string]any{"type": "update", "update": map[string]any{"sessionUpdate": "tool_call", "title": "Read README.md", "status": "completed", "rawOutput": "工具输出不交接"}}),
		historyRecord(t, 4, chunk("agent_message_chunk", "初稿一")),
		historyRecord(t, 5, turnEnded()),
		historyRecord(t, 7, chunk("agent_message_chunk", "初稿二")),
		historyRecord(t, 6, chunk("user_message_chunk", "再改一版")),
		historyRecord(t, 8, turnEnded()),
	}
}

func TestHandoffListsTurnsWithoutToolDetailOrThoughts(t *testing.T) {
	got := renderHandoff(conversation(t), maxTurnText)
	for _, want := range []string{handoffOpen, "## Turn 1", "User:\n写一份支付重构方案", "Tool call: Read README.md (completed)", "Agent:\n初稿一", "## Turn 2", "User:\n再改一版", "Agent:\n初稿二", handoffClose} {
		if !strings.Contains(got, want) {
			t.Fatalf("handoff lacks %q:\n%s", want, got)
		}
	}
	for _, leak := range []string{"内部推理不交接", "工具输出不交接"} {
		if strings.Contains(got, leak) {
			t.Fatalf("handoff carries %q, which desktop's handoff leaves out:\n%s", leak, got)
		}
	}
	if strings.Index(got, "初稿一") > strings.Index(got, "再改一版") {
		t.Fatal("records were not put back in their recorded order")
	}
}

func TestHandoffDropsTheOldestTurnsToFit(t *testing.T) {
	full := renderHandoff(conversation(t), maxTurnText)
	budget := len(full) - 10
	got := renderHandoff(conversation(t), budget)
	if len(got) > budget || strings.Contains(got, "写一份支付重构方案") || !strings.Contains(got, "再改一版") || !strings.Contains(got, "The 1 earliest turns are left out") {
		t.Fatalf("within %d bytes got %d:\n%s", budget, len(got), got)
	}
	long := []execution.HistoryRecord{historyRecord(t, 1, chunk("user_message_chunk", strings.Repeat("长", 4000))), historyRecord(t, 2, chunk("agent_message_chunk", "结尾"))}
	cut := renderHandoff(long, 2000)
	if len(cut) > 2000 || !strings.HasSuffix(cut, "结尾\n\n"+handoffClose) || !strings.Contains(cut, "[…]") {
		t.Fatalf("a last turn over the limit keeps its end, got %d bytes:\n%s", len(cut), cut)
	}
	if renderHandoff(conversation(t), 10) != "" {
		t.Fatal("a budget smaller than the header must hand over nothing")
	}
}

func TestHandoffOfAnEmptyConversationOnlyExplains(t *testing.T) {
	got := renderHandoff(nil, maxTurnText)
	if !strings.HasPrefix(got, handoffOpen) || !strings.HasSuffix(got, handoffClose) || strings.Contains(got, "## Turn") {
		t.Fatalf("handoff = %q", got)
	}
}
