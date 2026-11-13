// SPDX-License-Identifier: GPL-3.0-only
package app

import (
	"context"
	"os"
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
	if err := app.checkGoalToolExecution(ctx, goal.SessionID, "create"); toolErrorCode(err) != "E_GOAL_STAGE_REQUIRED" {
		t.Fatalf("side effect before stage checkpoint error = %v", err)
	}
	if err := store.Update(goal.ID, func(g *Goal) error { g.Stages = []GoalStage{{Title: "Implement", Status: "active"}}; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := app.checkGoalToolExecution(ctx, goal.SessionID, "create"); err != nil {
		t.Fatalf("side effect after acceptance and active stage was rejected: %v", err)
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
	updated, err := app.updateGoalForSession("s1", GoalUpdateRequest{Status: "progress", Progress: "tests are running", Acceptance: criteria, Stages: []GoalStageUpdate{{Title: "Run tests", Objective: "Run the relevant acceptance tests", Status: "active"}}})
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

func TestGoalCompletionRequiresOrderedStagesAndEvidence(t *testing.T) {
	app := newAppState(t.TempDir())
	store, err := app.goalStoreInstance()
	if err != nil {
		t.Fatal(err)
	}
	goal := Goal{ID: "stages", SessionID: "s-stages", Workspace: t.TempDir(), Title: "phased goal", Acceptance: []string{"tests pass"}, Status: GoalStatusActive, MaxRuns: 5}
	if err := store.Create(goal); err != nil {
		t.Fatal(err)
	}
	criteria := goal.Acceptance
	stages := []GoalStageUpdate{{Title: "Inspect", Objective: "Inspect relevant behavior", Status: "active"}, {Title: "Implement", Objective: "Implement and test the feature", Status: "pending"}}
	if _, err := app.updateGoalForSession(goal.SessionID, GoalUpdateRequest{Status: "progress", Progress: "started", Acceptance: criteria, Stages: stages}); err != nil {
		t.Fatal(err)
	}
	if _, err := app.updateGoalForSession(goal.SessionID, GoalUpdateRequest{Status: "completed", Progress: "done", Acceptance: criteria, Stages: stages}); err == nil {
		t.Fatal("goal completed with unfinished stages")
	}
	stages = []GoalStageUpdate{{Title: "Inspect", Objective: "Inspect relevant behavior", Status: "completed", Evidence: "reviewed relevant files", Verifications: []GoalVerificationSpec{{Name: "inspect checkpoint", Command: "echo inspected"}}}, {Title: "Implement", Objective: "Implement and test the feature", Status: "active"}}
	if _, err := app.updateGoalForSession(goal.SessionID, GoalUpdateRequest{Status: "progress", Progress: "implementation", Acceptance: criteria, Stages: stages}); err != nil {
		t.Fatalf("ordered stage advancement failed: %v", err)
	}
	stages[1] = GoalStageUpdate{Title: "Implement", Objective: "Implement and test the feature", Status: "completed", Evidence: "focused tests passed", Verifications: []GoalVerificationSpec{{Name: "implementation checkpoint", Command: "echo stage-verified"}}}
	updated, err := app.updateGoalForSession(goal.SessionID, GoalUpdateRequest{Status: "completed", Progress: "done", Acceptance: criteria, Stages: stages, Verifications: []GoalVerificationSpec{{Name: "smoke", Command: "echo verified"}}, DeliverySummary: "Implemented and checked.", StageReview: GoalStageReviewRequest{Stage: 0, Decision: "", Rationale: "", RevisedTitle: "", RevisedObjective: ""}})
	if err != nil || updated.Status != GoalStatusCompleted || len(updated.Verifications) != 1 || !updated.Verifications[0].Passed || len(updated.Deliverables) != 0 || updated.DeliverySummary == "" || !allGoalVerificationsPassed(updated.Stages[0].Verifications) || !allGoalVerificationsPassed(updated.Stages[1].Verifications) {
		t.Fatalf("completion with executed verification failed: %+v, %v", updated, err)
	}
}

func TestGoalRepeatedStageFailuresRequireReviewAndAllowRevision(t *testing.T) {
	app := newAppState(t.TempDir())
	store, err := app.goalStoreInstance()
	if err != nil {
		t.Fatal(err)
	}
	goal := Goal{ID: "review-stage", SessionID: "s-review-stage", Workspace: t.TempDir(), Title: "review stage", Acceptance: []string{"revised stage succeeds"}, Status: GoalStatusActive, MaxRuns: 5}
	if err := store.Create(goal); err != nil {
		t.Fatal(err)
	}
	criteria := goal.Acceptance
	stages := []GoalStageUpdate{{Title: "Original check", Objective: "Pass the intentionally failing check", Status: "active"}}
	if _, err := app.updateGoalForSession(goal.SessionID, GoalUpdateRequest{Status: "progress", Progress: "started", Acceptance: criteria, Stages: stages}); err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= goalStageReviewFailureThreshold; attempt++ {
		stages[0].Status = "completed"
		stages[0].Evidence = "agent says complete"
		stages[0].Verifications = []GoalVerificationSpec{{Name: "expected failure", Command: "exit 1"}}
		_, err := app.updateGoalForSession(goal.SessionID, GoalUpdateRequest{Status: "progress", Progress: "checking stage", Acceptance: criteria, Stages: stages})
		if err == nil {
			t.Fatalf("attempt %d unexpectedly passed", attempt)
		}
		got, _ := store.Get(goal.ID)
		if got.Stages[0].Attempts != attempt || got.Stages[0].FailuresSinceReview != attempt {
			t.Fatalf("attempt not tracked: %+v", got.Stages[0])
		}
	}
	got, _ := store.Get(goal.ID)
	if !got.Stages[0].ReviewRequired {
		t.Fatal("three failures did not require a review")
	}
	ctx := context.WithValue(context.Background(), goalRunContextKey{}, goal.ID)
	if err := app.checkGoalToolExecution(ctx, goal.SessionID, "create"); toolErrorCode(err) != "E_GOAL_STAGE_REVIEW_REQUIRED" {
		t.Fatalf("side effect not blocked pending review: %v", err)
	}
	updated, err := app.updateGoalForSession(goal.SessionID, GoalUpdateRequest{Status: "progress", Progress: "revised after reviewing repeated failures", Acceptance: criteria, StageReview: GoalStageReviewRequest{Stage: 1, Decision: "revise", Rationale: "The assertion target was underspecified; make the observable output explicit.", RevisedTitle: "Observable output check", RevisedObjective: "Compare the generated output against the exact expected value."}})
	if err != nil {
		t.Fatal(err)
	}
	stage := updated.Stages[0]
	if stage.Title != "Observable output check" || stage.Revision != 1 || stage.ReviewRequired || stage.FailuresSinceReview != 0 || len(stage.ReviewHistory) != 1 {
		t.Fatalf("stage revision was not recorded: %+v", stage)
	}
	if err := app.checkGoalToolExecution(ctx, goal.SessionID, "create"); err != nil {
		t.Fatalf("side effects remained blocked after review: %v", err)
	}
}

func TestGoalCompletionVerifiesAndRecordsDeliverablePaths(t *testing.T) {
	app := newAppState(t.TempDir())
	store, err := app.goalStoreInstance()
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	goal := Goal{ID: "deliver", SessionID: "s-deliver", Workspace: workspace, Title: "delivery", Acceptance: []string{"artifact exists"}, Stages: []GoalStage{{Title: "Create artifact", Objective: "Write the output file", Status: "completed", Evidence: "file created", Verifications: []GoalVerification{{Name: "stage check", Passed: true, ExitCode: 0}}}}, Status: GoalStatusActive, MaxRuns: 5}
	if err := store.Create(goal); err != nil {
		t.Fatal(err)
	}
	cfg := ConfigState{Workspace: workspace}
	request := GoalUpdateRequest{Status: "completed", Progress: "ready", Acceptance: goal.Acceptance, Stages: []GoalStageUpdate{{Title: "Create artifact", Objective: "Write the output file", Status: "completed", Evidence: "file created"}}, Verifications: []GoalVerificationSpec{{Name: "integration", Command: "echo verified"}}, DeliverablePaths: []string{"artifact.txt"}, DeliverySummary: "Created artifact.txt."}
	if _, err := app.updateGoalForSessionWithConfig(context.Background(), goal.SessionID, cfg, request); err == nil {
		t.Fatal("missing deliverable was accepted")
	}
	failed, _ := store.Get(goal.ID)
	if failed.Status != GoalStatusActive || failed.Stages[0].FailuresSinceReview != 1 {
		t.Fatalf("missing deliverable failure was not tracked: %+v", failed)
	}
	if err := os.WriteFile(filepath.Join(workspace, "artifact.txt"), []byte("final artifact\n"), 0600); err != nil {
		t.Fatal(err)
	}
	request.Stages[0].Verifications = []GoalVerificationSpec{{Name: "stage tool check", Command: "go version"}}
	updated, err := app.updateGoalForSessionWithConfig(context.Background(), goal.SessionID, cfg, request)
	if err != nil || updated.Status != GoalStatusCompleted || len(updated.Deliverables) != 1 || updated.Deliverables[0].Path != "artifact.txt" || updated.Deliverables[0].Size == 0 || updated.Deliverables[0].SHA256 == "" || updated.DeliverySummary == "" {
		t.Fatalf("deliverable was not verified and recorded: %+v, %v", updated, err)
	}
}

func TestGoalCompletionRequiresSuccessfulVerificationCommand(t *testing.T) {
	app := newAppState(t.TempDir())
	store, err := app.goalStoreInstance()
	if err != nil {
		t.Fatal(err)
	}
	goal := Goal{ID: "verify-fail", SessionID: "s-verify-fail", Workspace: t.TempDir(), Title: "verification gate", Acceptance: []string{"checks pass"}, Stages: []GoalStage{{Title: "Checks", Objective: "Verify all checks", Status: "completed", Evidence: "agent ran checks", Verifications: []GoalVerification{{Name: "stage", ExitCode: 0, Passed: true}}}}, Status: GoalStatusActive, MaxRuns: 5}
	if err := store.Create(goal); err != nil {
		t.Fatal(err)
	}
	updated, err := app.updateGoalForSessionWithConfig(context.Background(), goal.SessionID, ConfigState{Workspace: goal.Workspace}, GoalUpdateRequest{Status: "completed", Progress: "done", Acceptance: goal.Acceptance, Stages: []GoalStageUpdate{{Title: "Checks", Objective: "Verify all checks", Status: "completed", Evidence: "agent ran checks"}}, Verifications: []GoalVerificationSpec{{Name: "must fail", Command: "exit 7"}}, DeliverySummary: "checked"})
	if err == nil || updated.Status != GoalStatusActive || len(updated.Stages) != 1 || updated.Stages[0].ReviewRequired || updated.Stages[0].FailuresSinceReview != 1 || updated.Stages[0].Verifications[len(updated.Stages[0].Verifications)-1].Passed {
		t.Fatalf("failed verification incorrectly completed goal: %+v, %v", updated, err)
	}
}

func TestGoalVerificationHonorsDisabledCommandTool(t *testing.T) {
	app := newAppState(t.TempDir())
	store, err := app.goalStoreInstance()
	if err != nil {
		t.Fatal(err)
	}
	goal := Goal{ID: "disabled-command", SessionID: "s-disabled", Workspace: t.TempDir(), Title: "disabled command", Acceptance: []string{"test passes"}, Stages: []GoalStage{{Title: "Check", Objective: "Run test", Status: "completed", Evidence: "agent report", Verifications: []GoalVerification{{Name: "stage", ExitCode: 0, Passed: true}}}}, Status: GoalStatusActive, MaxRuns: 5}
	if err := store.Create(goal); err != nil {
		t.Fatal(err)
	}
	ctx := withBuiltinToolAvailability(context.Background(), nil)
	if _, err := app.updateGoalForSessionWithConfig(ctx, goal.SessionID, ConfigState{Workspace: goal.Workspace}, GoalUpdateRequest{Status: "completed", Progress: "done", Acceptance: goal.Acceptance, Stages: []GoalStageUpdate{{Title: "Check", Objective: "Run test", Status: "completed", Evidence: "agent report"}}, Verifications: []GoalVerificationSpec{{Name: "check", Command: "echo should-not-run"}}}); toolErrorCode(err) != "E_TOOL_DISABLED" {
		t.Fatalf("disabled command tool was bypassed: %v", err)
	}
	got, _ := store.Get(goal.ID)
	if got.Status != GoalStatusActive || len(got.Verifications) != 0 {
		t.Fatalf("disabled command altered goal: %+v", got)
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
