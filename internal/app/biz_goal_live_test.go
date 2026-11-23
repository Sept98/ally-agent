//go:build goal_live

// SPDX-License-Identifier: GPL-3.0-only
package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestGoalLiveWorkflow is an opt-in integration test. It uses the already
// configured model through Ally's config loader, while keeping goal state and
// generated files in temporary directories. It never logs credentials.
func TestGoalLiveWorkflow(t *testing.T) {
	configApp := NewApp()
	base, err := configApp.GetConfig()
	if err != nil {
		t.Fatalf("load configured models through Ally: %v", err)
	}
	if len(base.Models) == 0 {
		t.Skip("no configured chat model is available")
	}

	selected := -1
	for i, model := range base.Models {
		if modelHasUsableCredential(model) && strings.Contains(strings.ToLower(model.ProviderName+" "+model.DisplayName+" "+model.Model), "longcat") {
			selected = i
			break
		}
	}
	if selected < 0 && base.LastUsedModel != nil {
		selected = modelIndexByIdentity(base.Models, *base.LastUsedModel)
	}
	if selected < 0 {
		for i, model := range base.Models {
			if modelHasUsableCredential(model) {
				selected = i
				break
			}
		}
	}
	if selected < 0 {
		t.Skip("no configured model has an API key or OAuth account")
	}
	model := base.Models[selected]
	identity := identityOfModel(model)
	cfg := configApp.configForModelEntry(model)
	cfg.LastUsedModel = &identity
	cfg.CompactThresholdMode = CompactThresholdModeTokens
	cfg.CompactThresholdTokens = 30_000
	cfg.AutoCompactMethod = autoCompactMethodSummary

	workspace := t.TempDir()
	cfg.Workspace = workspace
	app := newAppState(t.TempDir())
	app.initialized = true
	app.config = cloneConfig(base)
	app.stats = nil
	recorder := &compactRecorder{done: make(chan struct{})}
	app.events = recorder

	objective := `在当前空工作区实现一个仅依赖 Go 标准库的 CSV 成绩汇总 CLI，并交付 go.mod、main.go、main_test.go、README.md。输入 CSV 必须有 name,score 表头；校验列数、非空姓名、数值分数且范围为 0..100；成功时输出含 count、average、highest 的 JSON；错误输入必须返回非零退出码并给出可读错误。测试应覆盖正常汇总、边界分数和无效输入。先设定可观察验收条件和阶段，然后实际创建文件、运行 go test，并用样例和坏输入执行 CLI 验证；不得只给方案或伪造测试证据。`
	goal, runID, err := app.StartGoal(GoalStartRequest{
		SessionID: "goal-live-" + newID(), Workspace: workspace, Title: "真实模型多阶段 CSV CLI 验收",
		Objective: objective, Config: cfg,
	})
	if err != nil {
		t.Fatalf("start live goal run: %v", err)
	}
	select {
	case <-recorder.done:
	case <-time.After(9 * time.Minute):
		_ = app.CancelRun(runID)
		t.Fatal("live goal run exceeded 9 minutes; cancelled to bound model usage")
	}

	store, err := app.goalStoreInstance()
	if err != nil {
		t.Fatalf("read isolated goal state: %v", err)
	}
	result, ok := store.Get(goal.ID)
	if !ok {
		t.Fatal("live goal state was not persisted")
	}
	if result.Status != GoalStatusCompleted {
		t.Fatalf("live model did not complete the goal (status=%s, reason=%s, progress=%s)", result.Status, result.StopReason, result.Progress)
	}
	for _, name := range []string{"go.mod", "main.go", "main_test.go", "README.md"} {
		if _, err := os.Stat(filepath.Join(workspace, name)); err != nil {
			t.Errorf("expected deliverable %s: %v", name, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	commandResult, err := app.runCommandWithConfig(ctx, cfg, CommandRequest{Command: "go test ./...", Timeout: 30, noPromote: true})
	if err != nil || commandResult.ExitCode != 0 || commandResult.TimedOut {
		t.Fatalf("independent final go test failed (err=%v, exit=%d, timedOut=%t)", err, commandResult.ExitCode, commandResult.TimedOut)
	}
	compactions := len(recorder.compactedEvents())
	if compactions == 0 {
		t.Fatal("live goal completed without exercising automatic context compaction")
	}
	t.Logf("live goal completed with configured model %q/%q; stages=%d, final checks=%d, compactions=%d", model.ProviderName, model.Model, len(result.Stages), len(result.Verifications), compactions)
}

func modelHasUsableCredential(model ModelConfig) bool {
	if model.AuthType != "" && model.OAuthAccountID != "" {
		return true
	}
	if strings.TrimSpace(model.APIKey) != "" {
		return true
	}
	for _, key := range model.APIKeys {
		if strings.TrimSpace(key) != "" {
			return true
		}
	}
	return false
}
