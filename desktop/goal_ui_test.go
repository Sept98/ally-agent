// SPDX-License-Identifier: GPL-3.0-only
package desktop

import "testing"

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
