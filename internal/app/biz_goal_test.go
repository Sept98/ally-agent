// SPDX-License-Identifier: GPL-3.0-only
package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"
)

func stubGoalCompletionReviewer(app *App, summary string) {
	app.goalCompletionReviewRunner = func(_ context.Context, _ ConfigState, _ string, _ AgentDelegateRequest) (*AgentDelegateResult, error) {
		return &AgentDelegateResult{Status: "completed", Summary: summary}, nil
	}
}

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
		if !strings.Contains(conflicts[0].Error(), "Run other tools first") || !strings.Contains(conflicts[0].Error(), "goal_update alone") {
			t.Fatalf("batch conflict did not tell the model how to recover: %v", conflicts[0])
		}
	}
}

func TestGoalSessionUsesGoalStagesInsteadOfSeparatePlanTool(t *testing.T) {
	app := newAppState(t.TempDir())
	normal := toolFunctionNames(app.buildToolsForSession("normal-session", ConfigState{}))
	if !containsString(normal, "plan") || containsString(normal, "goal_update") {
		t.Fatalf("normal session toolset changed: plan=%v goal_update=%v", containsString(normal, "plan"), containsString(normal, "goal_update"))
	}
	store, err := app.goalStoreInstance()
	if err != nil {
		t.Fatal(err)
	}
	goal := Goal{ID: "goal-plan", SessionID: "goal-session", Workspace: t.TempDir(), Title: "goal plan", Status: GoalStatusActive, MaxRuns: 5}
	if err := store.Create(goal); err != nil {
		t.Fatal(err)
	}
	goalTools := toolFunctionNames(app.buildToolsForSession(goal.SessionID, ConfigState{}))
	if containsString(goalTools, "plan") || !containsString(goalTools, "goal_update") {
		t.Fatalf("goal session should use only the persisted Goal plan: plan=%v goal_update=%v", containsString(goalTools, "plan"), containsString(goalTools, "goal_update"))
	}
}

func TestGoalProgressUpdateEmitsRefreshedStages(t *testing.T) {
	app := newAppState(t.TempDir())
	store, err := app.goalStoreInstance()
	if err != nil {
		t.Fatal(err)
	}
	goal := Goal{ID: "stage-sync", SessionID: "s-stage-sync", Workspace: t.TempDir(), Title: "stage sync", Status: GoalStatusActive, PreflightRequired: true, MaxRuns: 5}
	if err := store.Create(goal); err != nil {
		t.Fatal(err)
	}
	sink := &captureEventSink{}
	app.events = sink
	ready, noClarification := true, false
	stages := []GoalStageUpdate{{Title: "Foundation", Objective: "Create the project structure", Status: "active"}}
	updated, err := app.updateGoalForSession(goal.SessionID, GoalUpdateRequest{
		Status: "progress", Progress: "starting implementation", PreflightSummary: "Requirements are clear; implement in ordered stages.",
		PreflightReady: &ready, ClarificationRequired: &noClarification,
		Acceptance: []string{"the project builds"}, Stages: stages,
	})
	if err != nil {
		t.Fatal(err)
	}
	if sink.count != 1 || sink.name != "goal:update" {
		t.Fatalf("progress update did not emit goal:update: count=%d event=%q", sink.count, sink.name)
	}
	payload, ok := sink.payload.(map[string]any)
	if !ok {
		t.Fatalf("goal update payload has type %T", sink.payload)
	}
	emitted, ok := payload["goal"].(Goal)
	if !ok || emitted.Revision != updated.Revision || emitted.PreflightReady != true || len(emitted.Stages) != 1 || emitted.Stages[0].Status != "active" {
		t.Fatalf("goal:update did not carry the saved preflight and stage state: %#v", payload["goal"])
	}
}

func TestUnresolvedPreflightMustWaitOnAskInsteadOfNeedsUser(t *testing.T) {
	app := newAppState(t.TempDir())
	store, err := app.goalStoreInstance()
	if err != nil {
		t.Fatal(err)
	}
	goal := Goal{ID: "ask-first", SessionID: "s-ask-first", Workspace: t.TempDir(), Title: "ask first", Status: GoalStatusActive, PreflightRequired: true, PreflightSummary: "A decision is needed.", ClarificationRequired: true, Acceptance: []string{"the chosen option is implemented"}, MaxRuns: 5}
	if err := store.Create(goal); err != nil {
		t.Fatal(err)
	}
	_, err = app.updateGoalForSession(goal.SessionID, GoalUpdateRequest{Status: "needs_user", Progress: "waiting"})
	if toolErrorCode(err) != "E_GOAL_CLARIFICATION_REQUIRED" || !strings.Contains(err.Error(), "call ask alone") || !strings.Contains(err.Error(), "status=progress") {
		t.Fatalf("unresolved preflight returned an unhelpful needs_user error: %v", err)
	}
	got, _ := store.Get(goal.ID)
	if got.Status != GoalStatusActive || got.PreflightReady {
		t.Fatalf("invalid needs_user call changed the unresolved goal: %+v", got)
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
	updated, err = app.updateGoalForSession("s1", GoalUpdateRequest{Status: "progress", Progress: "continuing with saved criteria", Stages: []GoalStageUpdate{{Title: "Run tests", Objective: "Run the relevant acceptance tests", Status: "active"}}})
	if err != nil || !stringSlicesEqual(updated.Acceptance, criteria) {
		t.Fatalf("omitting unchanged acceptance did not preserve the saved criteria: %+v, %v", updated, err)
	}
	if _, err := app.updateGoalForSession("s1", GoalUpdateRequest{Status: "progress", Progress: "attempting a scope change", Acceptance: []string{"different criteria"}, Stages: []GoalStageUpdate{{Title: "Run tests", Objective: "Run the relevant acceptance tests", Status: "active"}}}); err == nil || !strings.Contains(err.Error(), `"relevant tests pass"`) {
		t.Fatalf("scope change did not return actionable frozen criteria: %v", err)
	}
	updated, err = app.updateGoalForSession("s1", GoalUpdateRequest{Status: "needs_user", Progress: "need deployment approval"})
	if err != nil || updated.Status != GoalStatusNeedsUser {
		t.Fatalf("needs_user update = %+v, %v", updated, err)
	}
	if _, err := app.updateGoalForSession("s1", GoalUpdateRequest{Status: "completed", Progress: "done", Acceptance: criteria}); err == nil {
		t.Fatal("terminal goal accepted a later model update")
	}
}

func TestGoalCompletionRequiresOrderedStagesAndEvidence(t *testing.T) {
	app := newAppState(t.TempDir())
	stubGoalCompletionReviewer(app, `{"verdict":"approved","reason":"Acceptance evidence is supported.","stage":0}`)
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
	stages[0].Status = "completed"
	stages[0].Evidence = "reviewed relevant files"
	if _, err := app.updateGoalForSession(goal.SessionID, GoalUpdateRequest{Status: "progress", Progress: "done", Acceptance: criteria, Stages: stages}); err == nil || !strings.Contains(err.Error(), "stages[0].verifications") || !strings.Contains(err.Error(), "goal_update runs those commands") {
		t.Fatalf("missing stage checks did not give a corrective instruction: %v", err)
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

func TestGoalStageVerificationRetryUsesLatestAttemptAndKeepsHistory(t *testing.T) {
	app := newAppState(t.TempDir())
	store, err := app.goalStoreInstance()
	if err != nil {
		t.Fatal(err)
	}
	goal := Goal{ID: "retry-stage", SessionID: "s-retry-stage", Workspace: t.TempDir(), Title: "retry stage", Acceptance: []string{"stage passes"}, Status: GoalStatusActive, MaxRuns: 5}
	if err := store.Create(goal); err != nil {
		t.Fatal(err)
	}
	stages := []GoalStageUpdate{{Title: "Check", Objective: "Run the stage check", Status: "active"}}
	if _, err := app.updateGoalForSession(goal.SessionID, GoalUpdateRequest{Status: "progress", Progress: "started", Acceptance: goal.Acceptance, Stages: stages}); err != nil {
		t.Fatal(err)
	}
	stages[0].Status = "completed"
	stages[0].Evidence = "first attempt failed; corrected the check"
	stages[0].Verifications = []GoalVerificationSpec{{Name: "first attempt", Command: "exit 1"}}
	if _, err := app.updateGoalForSession(goal.SessionID, GoalUpdateRequest{Status: "progress", Progress: "first check failed", Acceptance: goal.Acceptance, Stages: stages}); err == nil {
		t.Fatal("expected first verification attempt to fail")
	}
	stages[0].Verifications = []GoalVerificationSpec{{Name: "corrected attempt", Command: "echo stage-passed"}}
	updated, err := app.updateGoalForSession(goal.SessionID, GoalUpdateRequest{Status: "progress", Progress: "corrected check passed", Acceptance: goal.Acceptance, Stages: stages})
	if err != nil {
		t.Fatalf("successful retry was blocked by prior failure: %v", err)
	}
	stage := updated.Stages[0]
	if stage.Status != "completed" || stage.Attempts != 2 || len(stage.VerificationAttempts) != 2 || stage.VerificationAttempts[0].Passed || !stage.VerificationAttempts[1].Passed || !allGoalVerificationsPassed(stage.Verifications) {
		t.Fatalf("verification retry history/latest result incorrect: %+v", stage)
	}
}

func TestGoalCompletionReviewerCanRunRepeatedly(t *testing.T) {
	app := newAppState(t.TempDir())
	calls := 0
	app.goalCompletionReviewRunner = func(_ context.Context, _ ConfigState, _ string, req AgentDelegateRequest) (*AgentDelegateResult, error) {
		calls++
		if req.Name != "" {
			t.Fatalf("reviewer name must be auto-assigned for retries, got %q", req.Name)
		}
		return &AgentDelegateResult{Status: "completed", Summary: `{"verdict":"approved","reason":"Evidence supports acceptance.","stage":0}`}, nil
	}
	goal := Goal{Details: "deliver result", Acceptance: []string{"result exists"}}
	for i := 0; i < 2; i++ {
		if _, err := app.reviewGoalCompletion(context.Background(), ConfigState{}, "session", goal, nil, GoalUpdateRequest{Progress: "checked"}, nil, nil); err != nil {
			t.Fatalf("review attempt %d failed: %v", i+1, err)
		}
	}
	if calls != 2 {
		t.Fatalf("reviewer called %d times", calls)
	}
}

func TestGoalCompletionReviewReopensRequestedStage(t *testing.T) {
	app := newAppState(t.TempDir())
	stubGoalCompletionReviewer(app, `{"verdict":"needs_work","reason":"The acceptance evidence does not show the required output.","stage":1}`)
	store, err := app.goalStoreInstance()
	if err != nil {
		t.Fatal(err)
	}
	goal := Goal{ID: "review-reopen", SessionID: "s-review-reopen", Workspace: t.TempDir(), Title: "review me", Acceptance: []string{"expected output exists"}, Stages: []GoalStage{{Title: "Produce output", Objective: "Create the expected output", Status: "completed", Evidence: "done", Verifications: []GoalVerification{{Name: "stage check", Passed: true, ExitCode: 0}}}}, Status: GoalStatusActive, MaxRuns: 5}
	if err := store.Create(goal); err != nil {
		t.Fatal(err)
	}
	_, err = app.updateGoalForSessionWithConfig(context.Background(), goal.SessionID, ConfigState{Workspace: goal.Workspace}, GoalUpdateRequest{Status: "completed", Progress: "ready", Acceptance: goal.Acceptance, Stages: []GoalStageUpdate{{Title: "Produce output", Objective: "Create the expected output", Status: "completed", Evidence: "done"}}, Verifications: []GoalVerificationSpec{{Name: "integration", Command: "echo verified"}}, DeliverySummary: "done"})
	if err == nil {
		t.Fatal("weak acceptance evidence was approved")
	}
	updated, _ := store.Get(goal.ID)
	if updated.Status != GoalStatusActive || updated.Stages[0].Status != "active" || updated.CompletionReview == nil || updated.CompletionReview.Verdict != "needs_work" || updated.FinalVerificationAttempts[len(updated.FinalVerificationAttempts)-1].Passed != true {
		t.Fatalf("review rejection did not preserve evidence and reopen stage: %+v", updated)
	}
	ctx := context.WithValue(context.Background(), goalRunContextKey{}, goal.ID)
	if err := app.checkGoalToolExecution(ctx, goal.SessionID, "create"); err != nil {
		t.Fatalf("side effects remain blocked after review reopened a stage: %v", err)
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
	stubGoalCompletionReviewer(app, `{"verdict":"approved","reason":"Deliverable and checks are present.","stage":0}`)
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
	if failed.Status != GoalStatusActive || failed.Stages[0].FailuresSinceReview != 0 || len(failed.FinalVerificationAttempts) != 1 || !failed.FinalVerificationAttempts[0].Passed {
		t.Fatalf("deliverable failure was incorrectly attributed to a stage: %+v", failed)
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
	if err == nil || updated.Status != GoalStatusActive || len(updated.Stages) != 1 || updated.Stages[0].Status != "completed" || updated.Stages[0].ReviewRequired || updated.Stages[0].FailuresSinceReview != 0 || len(updated.FinalVerificationAttempts) != 1 || updated.FinalVerificationAttempts[0].Passed {
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
