// SPDX-License-Identifier: GPL-3.0-only
package app

import (
	"context"
	"path/filepath"
	"testing"

	openai "github.com/sashabaranov/go-openai"
)

func TestGoalUpdatesCannotShareBatchWithOtherTools(t *testing.T) {
	call := func(name, args string) openai.ToolCall {
		return openai.ToolCall{Function: openai.FunctionCall{Name: name, Arguments: args}}
	}
	for _, status := range []string{"progress", "completed", "needs_user"} {
		calls := []openai.ToolCall{call("goal_update", `{"status":"`+status+`","progress":"reported"}`), call("read", `{"path":"README.md"}`)}
		conflicts := detectToolBatchConflicts(ConfigState{}, calls)
		if len(conflicts) != 2 || toolErrorCode(conflicts[0]) != "E_GOAL_UPDATE_BATCH_CONFLICT" || toolErrorCode(conflicts[1]) != "E_GOAL_UPDATE_BATCH_CONFLICT" {
			t.Fatalf("terminal goal update batch conflicts = %#v", conflicts)
		}
	}
}

func TestDeleteGoalOnlyRemovesNonRunningMetadata(t *testing.T) {
	app := newAppState(t.TempDir())
	store, err := app.goalStoreInstance()
	if err != nil {
		t.Fatal(err)
	}
	goal := Goal{ID: "running", SessionID: "s1", Workspace: t.TempDir(), Title: "active", Status: GoalStatusActive}
	if err := store.Create(goal); err != nil {
		t.Fatal(err)
	}
	if err := app.DeleteGoal("running"); err == nil {
		t.Fatal("active goal record was deleted")
	}
	if err := store.Update("running", func(g *Goal) error { g.Status = GoalStatusPaused; g.ActiveRunID = ""; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := app.DeleteGoal("running"); err != nil {
		t.Fatalf("paused goal record could not be deleted: %v", err)
	}
	if _, ok := store.Get("running"); ok {
		t.Fatal("deleted goal remains in store")
	}
}

func TestGoalExecutionGateRequiresAcceptanceBeforeSideEffects(t *testing.T) {
	app := newAppState(t.TempDir())
	store, err := app.goalStoreInstance()
	if err != nil {
		t.Fatal(err)
	}
	goal := Goal{ID: "gate", SessionID: "s1", Workspace: t.TempDir(), Title: "goal", Details: "work", Status: GoalStatusActive}
	if err := store.Create(goal); err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), goalRunContextKey{}, goal.ID)
	if err := app.checkGoalToolExecution(ctx, goal.SessionID, "read"); err != nil {
		t.Fatalf("read-only preflight tool blocked: %v", err)
	}
	if err := app.checkGoalToolExecution(ctx, goal.SessionID, "create"); toolErrorCode(err) != "E_GOAL_ACCEPTANCE_REQUIRED" {
		t.Fatalf("side effect before acceptance error = %v", err)
	}
	if err := store.Update(goal.ID, func(g *Goal) error { g.Acceptance = []string{"tests pass"}; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := app.checkGoalToolExecution(ctx, goal.SessionID, "create"); err != nil {
		t.Fatalf("side effect after acceptance was rejected: %v", err)
	}
	if err := store.Update(goal.ID, func(g *Goal) error { g.Status = GoalStatusCompleted; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := app.checkGoalToolExecution(ctx, goal.SessionID, "read"); toolErrorCode(err) != "E_GOAL_TERMINAL" {
		t.Fatalf("tool after terminal goal error = %v", err)
	}
}

func TestGoalRecoveryPausesInflightExecution(t *testing.T) {
	path := filepath.Join(t.TempDir(), "goals.json")
	store, err := NewGoalStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Create(Goal{ID: "g1", SessionID: "s1", Workspace: t.TempDir(), Title: "goal", Details: "work", Status: GoalStatusActive, MaxRuns: 5, ActiveRunID: "run-1"}); err != nil {
		t.Fatal(err)
	}
	recovered, err := NewGoalStore(path)
	if err != nil {
		t.Fatal(err)
	}
	goal, ok := recovered.Get("g1")
	if !ok || goal.Status != GoalStatusPaused || goal.ActiveRunID != "" || goal.StopReason == "" {
		t.Fatalf("in-flight goal was not safely recovered: %+v, %v", goal, ok)
	}
}

func TestGoalProgressNeedsUserAndCompletionTransitions(t *testing.T) {
	app := newAppState(t.TempDir())
	store, err := app.goalStoreInstance()
	if err != nil {
		t.Fatal(err)
	}
	goal := Goal{ID: "g1", SessionID: "s1", Workspace: t.TempDir(), Title: "goal", Details: "work", Status: GoalStatusActive, MaxRuns: 5, ActiveRunID: "r1"}
	if err := store.Create(goal); err != nil {
		t.Fatal(err)
	}
	if _, err := app.updateGoalForSession("s1", GoalUpdateRequest{Status: "progress", Progress: "tests are running"}); err == nil {
		t.Fatal("goal without acceptance criteria accepted progress before defining the contract")
	}
	criteria := []string{"relevant tests pass"}
	updated, err := app.updateGoalForSession("s1", GoalUpdateRequest{Status: "progress", Progress: "tests are running", Acceptance: criteria})
	if err != nil || updated.Progress != "tests are running" || updated.Status != GoalStatusActive || !stringSlicesEqual(updated.Acceptance, criteria) {
		t.Fatalf("progress update = %+v, %v", updated, err)
	}
	updated, err = app.updateGoalForSession("s1", GoalUpdateRequest{Status: "needs_user", Progress: "need deployment approval", Acceptance: criteria})
	if err != nil || updated.Status != GoalStatusNeedsUser {
		t.Fatalf("needs_user update = %+v, %v", updated, err)
	}
	if _, err := app.updateGoalForSession("s1", GoalUpdateRequest{Status: "completed", Progress: "done", Acceptance: criteria}); err == nil {
		t.Fatal("terminal goal accepted a later model update")
	}
}

func TestGoalContinuationIsBoundedAndPauses(t *testing.T) {
	app := newAppState(t.TempDir())
	store, err := app.goalStoreInstance()
	if err != nil {
		t.Fatal(err)
	}
	goal := Goal{ID: "g1", SessionID: "s1", Workspace: t.TempDir(), Title: "goal", Details: "work", Status: GoalStatusActive, MaxRuns: 5}
	if err := store.Create(goal); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxGoalAutoContinuations; i++ {
		if marker, ok := app.goalContinuation("s1", "g1", i); !ok || marker == nil {
			t.Fatalf("continuation %d was not scheduled", i)
		}
	}
	if marker, ok := app.goalContinuation("s1", "g1", maxGoalAutoContinuations); ok || marker != nil {
		t.Fatal("continuation exceeded its per-run budget")
	}
	got, _ := store.Get("g1")
	if got.Status != GoalStatusPaused || got.StopReason == "" {
		t.Fatalf("budget exhaustion did not pause goal: %+v", got)
	}
}

func TestStaleGoalRunCompletionDoesNotOverwriteCurrentRun(t *testing.T) {
	app := newAppState(t.TempDir())
	store, err := app.goalStoreInstance()
	if err != nil {
		t.Fatal(err)
	}
	goal := Goal{ID: "g1", SessionID: "s1", Workspace: t.TempDir(), Title: "goal", Details: "work", Status: GoalStatusActive, MaxRuns: 5, ActiveRunID: "new-run"}
	if err := store.Create(goal); err != nil {
		t.Fatal(err)
	}
	app.finishGoalRun("g1", "old-run", "done", "")
	got, _ := store.Get("g1")
	if got.Status != GoalStatusActive || got.ActiveRunID != "new-run" {
		t.Fatalf("stale completion changed current run: %+v", got)
	}
}
