// SPDX-License-Identifier: GPL-3.0-only
package app

import (
	"context"
	"path/filepath"
	"testing"
)

func TestGoalPreflightBlocksSideEffectsUntilRequiredClarificationIsAnswered(t *testing.T) {
	app := newAppState(t.TempDir())
	workspace := t.TempDir()
	goal := Goal{
		ID: "preflight-goal", SessionID: "preflight-session", Workspace: workspace,
		Title: "Preflight", Details: "Implement the requested behavior", Status: GoalStatusActive,
		PreflightRequired: true, MaxRuns: 5,
	}
	store, err := app.goalStoreInstance()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Create(goal); err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), goalRunContextKey{}, goal.ID)
	if err := app.checkGoalToolExecution(ctx, goal.SessionID, "create"); toolErrorCode(err) != "E_GOAL_PREFLIGHT_REQUIRED" {
		t.Fatalf("side effect before preflight = %v, want E_GOAL_PREFLIGHT_REQUIRED", err)
	}
	if err := app.checkGoalToolExecution(ctx, goal.SessionID, "ask"); err != nil {
		t.Fatalf("ask must remain available during preflight: %v", err)
	}

	if _, err := app.updateGoalForSessionWithConfig(ctx, goal.SessionID, ConfigState{Workspace: workspace}, GoalUpdateRequest{
		Status: "progress", Progress: "planning", Stages: []GoalStageUpdate{{Title: "Implement", Objective: "Implement requested behavior", Status: "active"}},
	}); err == nil {
		t.Fatal("first goal update without preflight fields was accepted")
	}

	notReady, clarificationRequired := false, true
	preflight := GoalUpdateRequest{
		Status: "progress", Progress: "先拆分任务并澄清阻塞决策",
		PreflightSummary: "需要先确定输出格式；拆分为实现与验收两个阶段。",
		PreflightReady:   &notReady, ClarificationRequired: &clarificationRequired,
		Acceptance: []string{"输出满足确认后的格式"},
		Stages:     []GoalStageUpdate{{Title: "实现", Objective: "按确认后的格式实现", Status: "active", Verifications: []GoalVerificationSpec{}}},
	}
	updated, err := app.updateGoalForSessionWithConfig(ctx, goal.SessionID, ConfigState{Workspace: workspace}, preflight)
	if err != nil {
		t.Fatalf("persist preflight requiring clarification: %v", err)
	}
	if updated.PreflightReady || !updated.ClarificationRequired || updated.PreflightSummary == "" {
		t.Fatalf("preflight state = %+v, want blocked pending clarification", updated)
	}
	if err := app.checkGoalToolExecution(ctx, goal.SessionID, "create"); toolErrorCode(err) != "E_GOAL_PREFLIGHT_REQUIRED" {
		t.Fatalf("side effect while clarification is pending = %v, want E_GOAL_PREFLIGHT_REQUIRED", err)
	}

	ready := true
	preflight.PreflightSummary = "用户确认输出为 JSON；按此实现并验收。"
	preflight.PreflightReady = &ready
	preflight.ClarificationRequired = &notReady
	preflight.Acceptance = nil
	if _, err := app.updateGoalForSessionWithConfig(ctx, goal.SessionID, ConfigState{Workspace: workspace}, preflight); toolErrorCode(err) != "E_GOAL_CLARIFICATION_REQUIRED" {
		t.Fatalf("ready before ask answer = %v, want E_GOAL_CLARIFICATION_REQUIRED", err)
	}
	if err := app.bindGoalClarificationAsk(ctx, goal.SessionID, "expected-ask"); err != nil {
		t.Fatalf("bind clarification ask: %v", err)
	}
	if err := app.markGoalClarificationAnswered(ctx, goal.SessionID, "unrelated-ask"); err != nil {
		t.Fatalf("ignore unrelated ask response: %v", err)
	}
	stillBlocked, _ := store.Get(goal.ID)
	if stillBlocked.ClarificationAnswered {
		t.Fatal("unrelated ask response satisfied the goal clarification")
	}
	if err := app.markGoalClarificationAnswered(ctx, goal.SessionID, "expected-ask"); err != nil {
		t.Fatalf("record answered ask: %v", err)
	}
	updated, err = app.updateGoalForSessionWithConfig(ctx, goal.SessionID, ConfigState{Workspace: workspace}, preflight)
	if err != nil {
		t.Fatalf("mark preflight ready after answer: %v", err)
	}
	if !updated.PreflightReady || !updated.ClarificationAnswered || updated.ClarificationRequired {
		t.Fatalf("resolved preflight state = %+v", updated)
	}
	if err := app.checkGoalToolExecution(ctx, goal.SessionID, "create"); err != nil {
		t.Fatalf("side effect after answered clarification and ready plan = %v", err)
	}
	reloaded, err := NewGoalStore(filepath.Join(app.dataDir, "goals.json"))
	if err != nil {
		t.Fatalf("reload goal state: %v", err)
	}
	persisted, ok := reloaded.Get(goal.ID)
	if !ok || !persisted.PreflightReady || !persisted.ClarificationAnswered || persisted.ClarificationRequired || persisted.PreflightSummary != preflight.PreflightSummary {
		t.Fatalf("reloaded preflight state = %+v", persisted)
	}
}
