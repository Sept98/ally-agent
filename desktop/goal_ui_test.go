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

func TestGoalProgressSummaryShowsCurrentStageAndBoundsProgress(t *testing.T) {
	goal := backend.Goal{Progress: strings.Repeat("进展", 100), Stages: []backend.GoalStage{{Title: "调查", Status: "completed"}, {Title: "实现", Status: "active"}, {Title: "验收", Status: "pending"}}}
	got := goalProgressSummary(goal)
	if strings.Contains(got, "实现") || strings.Contains(got, "进展") {
		t.Fatalf("summary must not masquerade stale model progress as live state: %q", got)
	}
	if done, total := goalStageProgress(goal); done != 1 || total != 3 {
		t.Fatalf("stage progress = %d/%d, want 1/3", done, total)
	}
	if label := goalStageProgressLabel(goal); label != "当前第 2/3 阶段 · 已完成 1" {
		t.Fatalf("progress label = %q", label)
	}
	if i, stage, ok := goalCurrentStage(goal); !ok || i != 1 || stage.Title != "实现" {
		t.Fatalf("current stage = %d, %+v, %v", i, stage, ok)
	}
	if label := goalCurrentStageLabel(goal); label != "当前第 2 / 共 3 阶段：实现" {
		t.Fatalf("current stage label = %q", label)
	}
	if got := goalCompletedStageCount(goal); got != 1 {
		t.Fatalf("completed stage count = %d, want 1", got)
	}
	if got := goalCurrentStageLabel(backend.Goal{Stages: []backend.GoalStage{{Title: "预备", Status: "pending"}}}); got != "下一阶段：第 1 / 共 1 阶段：预备" {
		t.Fatalf("upcoming-stage label = %q", got)
	}
	if got := goalStageProgressLabel(backend.Goal{Stages: []backend.GoalStage{{Status: "completed"}, {Status: "completed"}}}); got != "全部 2 阶段已完成" {
		t.Fatalf("completed-goal label = %q", got)
	}
}

func TestGoalStageStatusPresentation(t *testing.T) {
	for _, tc := range []struct {
		stage  backend.GoalStage
		label  string
		marker string
	}{
		{backend.GoalStage{Status: "completed"}, "已完成", "✓"},
		{backend.GoalStage{Status: "active"}, "进行中", "●"},
		{backend.GoalStage{Status: "pending"}, "待开始", "○"},
		{backend.GoalStage{Status: "active", ReviewRequired: true}, "需要复核", "!"},
	} {
		if got := goalStageStatusLabel(tc.stage); got != tc.label {
			t.Errorf("goalStageStatusLabel(%+v) = %q, want %q", tc.stage, got, tc.label)
		}
		if got := goalStageStatusMarker(tc.stage); got != tc.marker {
			t.Errorf("goalStageStatusMarker(%+v) = %q, want %q", tc.stage, got, tc.marker)
		}
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
	if got := tab.uiState().GoalActivity; !strings.Contains(got, "运行命令") || !strings.Contains(got, "运行阶段测试") {
		t.Fatalf("goal activity did not reflect started tool: %q", got)
	}
	a.applyToolEvent(tab, "tool:result", map[string]any{"toolCallId": "c1", "name": "command", "result": map[string]any{"ok": true}})
	if got := tab.uiState().GoalActivity; got != "" {
		t.Fatalf("active goal activity remained after tool result: %q", got)
	}
	if got := tab.uiState().GoalLastActivity; !strings.Contains(got, "刚完成：") || !strings.Contains(got, "运行阶段测试") {
		t.Fatalf("completed tool activity was not retained: %q", got)
	}
	if got := goalDisplayedActivity(tab.uiState(), tab); !strings.Contains(got, "刚完成：") {
		t.Fatalf("displayed activity missing last completed tool: %q", got)
	}
}

func TestGoalDisplayedActivityRecoversFromTranscript(t *testing.T) {
	tab := &conversation{Messages: []map[string]any{{"role": "tool_call", "name": "create", "status": "success", "args": `{"path":"internal/sim/world.go"}`}}}
	got := goalDisplayedActivity(&chatUI{}, tab)
	if !strings.Contains(got, "最近完成") || !strings.Contains(got, "internal/sim/world.go") {
		t.Fatalf("transcript activity = %q", got)
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
