// SPDX-License-Identifier: GPL-3.0-only
package app

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestComplexGoalRunExecutesStagesAndIndependentAcceptanceReview(t *testing.T) {
	server := &planCheckServer{}
	server.script = func(round int, request string) string {
		switch round {
		case 1:
			if !strings.Contains(request, "执行这个持久目标") {
				t.Errorf("initial request missing persistent-goal instructions")
			}
			return sseChatToolCallChunk("plan_goal", "goal_update", `{"status":"progress","progress":"已拆解为规格分析与实现验证两阶段","acceptance":["需求规格文件包含三项约束","实现文件包含全部约束并通过阶段测试"],"stages":[{"title":"整理需求规格","objective":"把三项用户约束写入需求规格文件","status":"active","evidence":"","verifications":[]},{"title":"实现并验收","objective":"实现满足规格的功能并运行验收检查","status":"pending","evidence":"","verifications":[]}],"verifications":[],"stage_review":{"stage":0,"decision":"","rationale":"","revised_title":"","revised_objective":""},"deliverable_paths":[],"delivery_summary":""}`) + sseChatFinishChunk("tool_calls") + sseDone
		case 2:
			return sseChatToolCallChunk("write_spec", "create", `{"path":"spec.md","content":"Constraint A: preserve the requested behavior.\\nConstraint B: reject invalid inputs.\\nConstraint C: keep the result reproducible.\\n"}`) + sseChatFinishChunk("tool_calls") + sseDone
		case 3:
			return sseChatToolCallChunk("finish_spec", "goal_update", `{"status":"progress","progress":"规格文件已创建并通过内容检查，进入实现阶段","acceptance":["需求规格文件包含三项约束","实现文件包含全部约束并通过阶段测试"],"stages":[{"title":"整理需求规格","objective":"把三项用户约束写入需求规格文件","status":"completed","evidence":"spec.md records constraints A, B, and C","verifications":[{"name":"spec contains all constraints","command":"python -c \"from pathlib import Path; s=Path('spec.md').read_text(); assert all(x in s for x in ('Constraint A', 'Constraint B', 'Constraint C'))\""}]},{"title":"实现并验收","objective":"实现满足规格的功能并运行验收检查","status":"active","evidence":"","verifications":[]}],"verifications":[],"stage_review":{"stage":0,"decision":"","rationale":"","revised_title":"","revised_objective":""},"deliverable_paths":[],"delivery_summary":""}`) + sseChatFinishChunk("tool_calls") + sseDone
		case 4:
			return sseChatToolCallChunk("write_impl", "create", `{"path":"implementation.md","content":"Implemented: preserve requested behavior; reject invalid inputs; ensure reproducible output.\\n"}`) + sseChatFinishChunk("tool_calls") + sseDone
		case 5:
			return sseChatToolCallChunk("complete_goal", "goal_update", `{"status":"completed","progress":"两个阶段均完成，规格与实现文件已核验","acceptance":["需求规格文件包含三项约束","实现文件包含全部约束并通过阶段测试"],"stages":[{"title":"整理需求规格","objective":"把三项用户约束写入需求规格文件","status":"completed","evidence":"spec.md records constraints A, B, and C","verifications":[{"name":"spec contains all constraints","command":"python -c \"from pathlib import Path; s=Path('spec.md').read_text(); assert all(x in s for x in ('Constraint A', 'Constraint B', 'Constraint C'))\""}]},{"title":"实现并验收","objective":"实现满足规格的功能并运行验收检查","status":"completed","evidence":"implementation.md documents all three constraints","verifications":[{"name":"implementation contains constraints","command":"python -c \"from pathlib import Path; s=Path('implementation.md').read_text(); assert all(x in s for x in ('preserve requested behavior', 'reject invalid inputs', 'reproducible output'))\""}]}],"verifications":[{"name":"both deliverables exist","command":"python -c \"from pathlib import Path; assert Path('spec.md').is_file() and Path('implementation.md').is_file()\""}],"stage_review":{"stage":0,"decision":"","rationale":"","revised_title":"","revised_objective":""},"deliverable_paths":["spec.md","implementation.md"],"delivery_summary":"Created the requirements specification and implementation summary, with all three constraints represented."}`) + sseChatFinishChunk("tool_calls") + sseDone
		case 6:
			if !strings.Contains(request, "separate acceptance judge") || !strings.Contains(request, "spec.md records constraints A") || strings.Contains(request, "已拆解为规格分析") || strings.Contains(request, "Progress checkpoint") {
				t.Errorf("reviewer did not receive isolated, evidence-only context: %s", request)
			}
			return sseChatChunk(`{"verdict":"approved","reason":"Both ordered stages have evidence and passing checks; both deliverables are present and the criteria are addressed.","stage":0}`) + sseChatFinishChunk("stop") + sseDone
		default:
			return sseChatChunk("复杂目标已按阶段完成并通过独立验收。") + sseChatFinishChunk("stop") + sseDone
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

	goal, _, err := app.StartGoal(GoalStartRequest{SessionID: "complex-goal-session", Workspace: workspace, Title: "多阶段规格实现", Objective: "整理约束、实现并独立验收", Acceptance: []string{"需求规格文件包含三项约束", "实现文件包含全部约束并通过阶段测试"}, Config: app.config})
	if err != nil {
		t.Fatalf("StartGoal() error = %v", err)
	}
	select {
	case <-recorder.done:
	case <-time.After(20 * time.Second):
		t.Fatal("complex goal run did not finish")
	}
	if got := recorder.endStatus(); got != "run:done" {
		t.Fatalf("run ended with %q, want run:done (errors: %v)", got, recorder.errorEvents())
	}
	if got := server.count(); got < 7 {
		t.Fatalf("provider rounds = %d, want at least 7 for planning, stage work, checkpoints, review, and final report", got)
	}
	for _, item := range []struct{ name, want string }{
		{"spec.md", "Constraint A: preserve the requested behavior."},
		{"spec.md", "Constraint C: keep the result reproducible."},
		{"implementation.md", "reject invalid inputs"},
		{"implementation.md", "ensure reproducible output"},
	} {
		content, err := os.ReadFile(filepath.Join(workspace, item.name))
		if err != nil {
			t.Errorf("expected deliverable %s: %v", item.name, err)
			continue
		}
		if !strings.Contains(string(content), item.want) {
			t.Errorf("deliverable %s missing %q: %q", item.name, item.want, content)
		}
	}
	store, err := app.goalStoreInstance()
	if err != nil {
		t.Fatal(err)
	}
	updated, ok := store.Get(goal.ID)
	if !ok || updated.Status != GoalStatusCompleted || len(updated.Stages) != 2 || updated.Stages[0].Status != "completed" || updated.Stages[1].Status != "completed" || updated.CompletionReview == nil || updated.CompletionReview.Verdict != "approved" || len(updated.FinalVerificationAttempts) != 1 || !updated.FinalVerificationAttempts[0].Passed || len(updated.Deliverables) != 2 {
		t.Fatalf("complex goal did not persist complete stage/review/deliverable state: %+v", updated)
	}
}

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
			return sseChatToolCallChunk("goal_call", "goal_update", `{"status":"completed","progress":"acceptance tests passed","acceptance":["acceptance tests pass"],"stages":[{"title":"Run acceptance tests","objective":"Run the package acceptance tests","status":"completed","evidence":"go test passed","verifications":[{"name":"stage tests","command":"echo stage-ok"}]}],"verifications":[{"name":"acceptance tests","command":"echo verified"}],"stage_review":{"stage":0,"decision":"","rationale":"","revised_title":"","revised_objective":""},"deliverable_paths":[],"delivery_summary":"Acceptance checks completed."}`) +
				sseChatFinishChunk("tool_calls") + sseDone
		case 3:
			if !strings.Contains(request, "separate acceptance judge") || !strings.Contains(request, "acceptance tests pass") || strings.Contains(request, "Progress checkpoint") || strings.Contains(request, `"goal_update"`) {
				t.Errorf("acceptance reviewer did not receive isolated evidence-only context: %s", request)
			}
			return sseChatChunk(`{"verdict":"approved","reason":"The acceptance criterion has a matching passed integration check and stage evidence.","stage":0}`) + sseChatFinishChunk("stop") + sseDone
		case 4:
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
	if got := server.count(); got != 4 {
		t.Fatalf("provider rounds = %d, want 4 including the isolated acceptance reviewer", got)
	}
	store, err := app.goalStoreInstance()
	if err != nil {
		t.Fatal(err)
	}
	updated, ok := store.Get(goal.ID)
	if !ok || updated.Status != GoalStatusCompleted || updated.Progress != "acceptance tests passed" || updated.ActiveRunID != "" || len(updated.Verifications) != 1 || !updated.Verifications[0].Passed || updated.CompletionReview == nil || updated.CompletionReview.Verdict != "approved" || updated.CompletionReview.Model != "test-model" || len(updated.FinalVerificationAttempts) != 1 {
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
