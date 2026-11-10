// SPDX-License-Identifier: GPL-3.0-only
package app

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGoalRunContinuesUntilGoalUpdateCompletes(t *testing.T) {
	server := &planCheckServer{}
	server.script = func(round int, request string) string {
		switch round {
		case 1:
			if !strings.Contains(request, "执行这个持久目标") || !strings.Contains(request, "acceptance tests pass") {
				t.Errorf("initial request missing goal instructions and acceptance criteria")
			}
			return sseChatChunk("Progress checkpoint") + sseChatFinishChunk("stop") + sseDone
		case 2:
			if !strings.Contains(request, "ally-goal-continuation") {
				t.Errorf("normal model stop did not trigger bounded continuation")
			}
			return sseChatToolCallChunk("goal_call", "goal_update", `{"status":"completed","progress":"acceptance tests passed","acceptance":["acceptance tests pass"]}`) +
				sseChatFinishChunk("tool_calls") + sseDone
		case 3:
			return sseChatChunk("验收条件已逐项核对。") + sseChatFinishChunk("stop") + sseDone
		default:
			t.Errorf("unexpected extra model request %d", round)
			return sseChatChunk("unexpected") + sseChatFinishChunk("stop") + sseDone
		}
	}
	httpServer := httptest.NewServer(server.handler())
	defer httpServer.Close()
	workspace := t.TempDir()
	app := newAppState(filepath.Join(t.TempDir(), "data"))
	app.initialized = true
	app.config = ConfigState{APIFormat: apiFormatOpenAIChat, BaseURL: httpServer.URL, APIKeys: []string{"test-key"}, Model: "test-model", Workspace: workspace}
	recorder := &compactRecorder{done: make(chan struct{})}
	app.events = recorder
	app.stats = nil

	goal, _, err := app.StartGoal(GoalStartRequest{
		SessionID: "goal-run-session",
		Workspace: workspace,
		Title:     "bounded goal",
		Objective: "complete a test goal",
		Acceptance: []string{
			"acceptance tests pass",
		},
		Config: app.config,
	})
	if err != nil {
		t.Fatalf("StartGoal() error = %v", err)
	}
	select {
	case <-recorder.done:
	case <-time.After(20 * time.Second):
		t.Fatal("goal run did not finish")
	}
	if got := recorder.endStatus(); got != "run:done" {
		t.Fatalf("run ended with %q (errors: %v)", got, recorder.errorEvents())
	}
	if got := server.count(); got != 3 {
		t.Fatalf("provider rounds = %d, want 3", got)
	}
	store, err := app.goalStoreInstance()
	if err != nil {
		t.Fatal(err)
	}
	updated, ok := store.Get(goal.ID)
	if !ok || updated.Status != GoalStatusCompleted || updated.Progress != "acceptance tests passed" || updated.ActiveRunID != "" {
		t.Fatalf("goal did not reach reported-complete state: %+v, %v", updated, ok)
	}
	app.mu.Lock()
	history := app.histories[goal.SessionID]
	app.mu.Unlock()
	for _, message := range history {
		if strings.Contains(message.Content, "ally-goal-continuation") || strings.Contains(message.Content, "执行这个持久目标") {
			t.Fatalf("ephemeral goal instructions were persisted in history: %q", message.Content)
		}
	}
}
