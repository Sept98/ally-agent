// SPDX-License-Identifier: GPL-3.0-only
package desktop

import (
	"strings"
	"testing"

	backend "ally-dev/internal/app"
)

func TestParseGoalCommand(t *testing.T) {
	for _, tc := range []struct {
		text, want string
		ok         bool
	}{
		{"/goal build a widget", "build a widget", true},
		{" /GOAL\t目标\n", "目标", true},
		{"/goal", "", false},
		{"/goals thing", "", false},
		{"ordinary /goal thing", "", false},
	} {
		got, ok := parseGoalCommand(tc.text)
		if got != tc.want || ok != tc.ok {
			t.Errorf("parseGoalCommand(%q) = %q, %v; want %q, %v", tc.text, got, ok, tc.want, tc.ok)
		}
	}
}

func TestGoalProgressSummaryShowsStageAndBoundsProgress(t *testing.T) {
	goal := backend.Goal{Progress: strings.Repeat("进展", 100), Stages: []backend.GoalStage{{Title: "调查", Status: "completed"}, {Title: "实现", Status: "active"}, {Title: "验收", Status: "pending"}}}
	got := goalProgressSummary(goal)
	if !strings.Contains(got, "阶段 1/3 完成") || !strings.Contains(got, "当前 第2步：实现") || len([]rune(got)) > 180 {
		t.Fatalf("goal progress summary is missing stage info or unbounded: %q", got)
	}
}

func TestGoalSubagentCallCountUsesTranscriptToolCalls(t *testing.T) {
	tab := &conversation{Messages: []map[string]any{
		{"role": "tool_call", "name": "read"},
		{"role": "tool_call", "name": "subagent"},
		{"role": "subagent", "subId": "agent-1"},
	}}
	if got := goalSubagentCallCount(tab); got != 1 {
		t.Fatalf("subagent call count = %d, want 1", got)
	}
	if got := goalSubagentCallCount(&conversation{}); got != 0 {
		t.Fatalf("subagent call count without calls = %d, want 0", got)
	}
}

func TestCompletedGoalPanelIsHiddenUntilOpened(t *testing.T) {
	goal := backend.Goal{ID: "g", Status: backend.GoalStatusCompleted}
	state := newChatUI()
	if goalPanelVisible(goal, &state) {
		t.Fatal("completed goal should not occupy the bottom of the chat by default")
	}
	state.GoalPanelVisible = true
	if !goalPanelVisible(goal, &state) {
		t.Fatal("completed goal results should remain accessible when requested")
	}
	goal.Status = backend.GoalStatusActive
	state.GoalPanelVisible = false
	if !goalPanelVisible(goal, &state) {
		t.Fatal("active goal progress must remain visible")
	}
}

func TestGoalActivityTracksToolEvents(t *testing.T) {
	tab := &conversation{ID: "s"}
	a := &desktop{goals: map[string]backend.Goal{"s": {ID: "g", SessionID: "s", Status: backend.GoalStatusActive}}}
	a.applyToolEvent(tab, "tool:start", map[string]any{"toolCallId": "c1", "name": "command", "args": map[string]any{"purpose": "运行阶段测试"}})
	if got := tab.uiState().GoalActivity; !strings.Contains(got, "command") || !strings.Contains(got, "运行阶段测试") {
		t.Fatalf("goal activity did not reflect started tool: %q", got)
	}
	a.applyToolEvent(tab, "tool:result", map[string]any{"toolCallId": "c1", "name": "command", "result": map[string]any{"ok": true}})
	if got := tab.uiState().GoalActivity; got != "" {
		t.Fatalf("goal activity remained after tool result: %q", got)
	}
}

func TestCompletedGoalUpdateClosesBottomPanel(t *testing.T) {
	state := newChatUI()
	state.GoalPanelVisible, state.GoalDetailsOpen = true, true
	tab := &conversation{ID: "s", UI: &state}
	a := &desktop{tabs: []*conversation{tab}}
	a.applyGoalUpdate(map[string]any{"goal": map[string]any{"id": "g", "session_id": "s", "title": "done", "status": "completed"}, "revision": float64(2)})
	if tab.UI.GoalPanelVisible || tab.UI.GoalDetailsOpen {
		t.Fatal("completed goal update left the bottom result panel open")
	}
}

func TestGoalUpdateStoredWithoutOpenTab(t *testing.T) {
	a := &desktop{}
	a.applyGoalUpdate(map[string]any{"goal": map[string]any{"id": "g", "session_id": "background", "title": "task", "status": "paused", "progress": "half", "stop_reason": "waiting"}, "revision": float64(3)})
	g, ok := a.goals["background"]
	if !ok || string(g.Status) != "paused" || g.Progress != "half" || g.StopReason != "waiting" {
		t.Fatalf("goal update not retained: %+v, %v", g, ok)
	}
	a.applyGoalDelete(map[string]any{"sessionId": "background", "revision": float64(4)})
	a.applyGoalUpdate(map[string]any{"goal": map[string]any{"id": "g", "session_id": "background", "title": "stale", "status": "active"}, "revision": float64(3)})
	if _, ok := a.goals["background"]; ok {
		t.Fatal("stale update resurrected a deleted goal")
	}
}
