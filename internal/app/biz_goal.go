// SPDX-License-Identifier: GPL-3.0-only
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	openai "github.com/sashabaranov/go-openai"
)

const (
	maxGoalAutoContinuations = 3
	maxGoalStepsPerRun       = 200
)

type goalRunContextKey struct{}

func goalPreflightToolAllowed(name string) bool {
	switch normalizeToolName(name) {
	case "read", "grep", "list_files", "web_fetch", "calculate", "plan", "ask", "wait", "skill", "screenshot", "goal_update":
		return true
	default:
		return false
	}
}

func (a *App) checkGoalToolExecution(ctx context.Context, sessionID, name string) error {
	goalID, _ := ctx.Value(goalRunContextKey{}).(string)
	if goalID == "" {
		return nil
	}
	store, err := a.goalStoreInstance()
	if err != nil {
		return codedToolError("E_GOAL_STATE", err)
	}
	goal, ok := store.Get(goalID)
	if !ok || goal.SessionID != sessionID {
		return codedToolError("E_GOAL_STATE", errors.New("goal no longer belongs to this session"))
	}
	if goal.Status != GoalStatusActive {
		return codedToolError("E_GOAL_TERMINAL", fmt.Errorf("goal is %q; no more tools may run in this goal turn", goal.Status))
	}
	if len(goal.Acceptance) == 0 && !goalPreflightToolAllowed(name) {
		return codedToolError("E_GOAL_ACCEPTANCE_REQUIRED", errors.New("establish and persist observable acceptance criteria with goal_update before running side-effecting tools"))
	}
	return nil
}

// GoalStartRequest starts a user-authored objective in an otherwise fresh session.
type GoalStartRequest struct {
	SessionID  string
	Workspace  string
	Title      string
	Objective  string
	Acceptance []string
	Config     ConfigState
}

type GoalUpdateRequest struct {
	Status     string   `json:"status"`
	Progress   string   `json:"progress"`
	Acceptance []string `json:"acceptance"`
}

func (a *App) goalStoreInstance() (*GoalStore, error) {
	a.goalStoreMu.Lock()
	defer a.goalStoreMu.Unlock()
	if a.goalStore != nil {
		return a.goalStore, nil
	}
	store, err := NewGoalStore(filepath.Join(a.dataDir, "goals.json"))
	if err != nil {
		return nil, err
	}
	a.goalStore = store
	return store, nil
}

func (a *App) ListGoals() ([]Goal, error) {
	store, err := a.goalStoreInstance()
	if err != nil {
		return nil, err
	}
	return store.List(), nil
}

// DeleteGoal removes only the goal record; it never changes workspace files or chat history.
func (a *App) DeleteGoal(goalID string) error {
	store, err := a.goalStoreInstance()
	if err != nil {
		return err
	}
	goal, ok := store.Get(strings.TrimSpace(goalID))
	if !ok {
		return ErrGoalNotFound
	}
	if goal.Status == GoalStatusActive || goal.Status == GoalStatusPending || goal.ActiveRunID != "" {
		return errors.New("pause or finish the goal before removing its record")
	}
	if err := store.Remove(goal.ID); err != nil {
		return err
	}
	a.emit("goal:deleted", map[string]any{"goalId": goal.ID, "sessionId": goal.SessionID, "revision": goal.Revision + 1})
	return nil
}

// StartGoal creates a bounded goal record and starts its first ordinary chat run.
// Goal state is persisted separately from conversation history.
func (a *App) StartGoal(req GoalStartRequest) (Goal, string, error) {
	if err := a.ensureInitialized(); err != nil {
		return Goal{}, "", err
	}
	sessionID := strings.TrimSpace(req.SessionID)
	workspace := strings.TrimSpace(req.Workspace)
	objective := strings.TrimSpace(req.Objective)
	title := strings.TrimSpace(req.Title)
	if sessionID == "" || workspace == "" || objective == "" {
		return Goal{}, "", errors.New("session, workspace, and objective are required")
	}
	if title == "" {
		title = objective
	}
	if len([]rune(title)) > 120 {
		title = string([]rune(title)[:120])
	}
	req.Config.Workspace = workspace
	cfg := a.effectiveConfig(req.Config)
	if !toolListHasName(builtinToolsForConfig(cfg), "goal_update") {
		return Goal{}, "", errors.New("goal_update is disabled; enable it before starting a goal")
	}
	// A goal must not silently take over a conversation whose request tools were
	// already frozen without the goal protocol. The desktop starts goals in a new tab.
	if len(a.loadSessionHistoryCopy(sessionID)) != 0 {
		return Goal{}, "", errors.New("start a goal in a fresh session so its goal controls are available")
	}
	store, err := a.goalStoreInstance()
	if err != nil {
		return Goal{}, "", err
	}
	var model *ModelIdentity
	if cfg.LastUsedModel != nil {
		identity := *cfg.LastUsedModel
		model = &identity
	}
	g := Goal{ID: newID(), SessionID: sessionID, Workspace: workspace, Title: title, Details: objective, Acceptance: append([]string(nil), req.Acceptance...), Model: model, Status: GoalStatusPending, MaxRuns: maxGoalRunBudget}
	if err := store.Create(g); err != nil {
		return Goal{}, "", err
	}
	runID, err := a.StartChat(ChatRequest{SessionID: sessionID, Message: objective, Config: req.Config, GoalID: g.ID})
	if err != nil {
		_ = store.Update(g.ID, func(current *Goal) error {
			current.Status = GoalStatusFailed
			current.StopReason = err.Error()
			return nil
		})
		if failed, ok := store.Get(g.ID); ok {
			a.emitGoalUpdate(failed)
		}
		return Goal{}, "", err
	}
	updated, _ := store.Get(g.ID)
	a.emitGoalUpdate(updated)
	return updated, runID, nil
}

func goalStartMessage(g Goal) string {
	var b strings.Builder
	b.WriteString("执行这个持久目标，直到达到验收条件；不要只给方案或中途总结。\n\n目标：\n")
	b.WriteString(g.Details)
	if len(g.Acceptance) > 0 {
		b.WriteString("\n\n验收条件：\n")
		for i, item := range g.Acceptance {
			fmt.Fprintf(&b, "%d. %s\n", i+1, item)
		}
	} else {
		b.WriteString("\n\n在产生任何副作用前，先整理可观察、可验证的验收条件，并用 goal_update 的 acceptance 字段持久化；若目标有关键歧义，先用 ask 取得用户确认，不要擅自假设。")
	}
	b.WriteString("\n每次 goal_update 都携带当前完整 acceptance 列表。完成每个实质步骤后报告进度；只有逐项验证全部验收条件后才报告 completed；无法继续时报告 needs_user 并说明阻塞。普通轮次结束不代表目标完成。对于涉及代码的长任务，先独立调查并制定验收步骤；把互不冲突的只读调查、测试设计或最终 diff review 委派给子 agent，避免多个 agent 并发修改相同文件。主 agent 必须核实子 agent 的结论，并亲自运行验收命令；不能把子 agent 的完成声明当成验证证据。")
	return b.String()
}

func (a *App) attachGoalRun(goalID, sessionID, runID string) error {
	store, err := a.goalStoreInstance()
	if err != nil {
		return err
	}
	err = store.Update(goalID, func(g *Goal) error {
		if g.SessionID != sessionID {
			return errors.New("goal belongs to another session")
		}
		if g.Status != GoalStatusPending && g.Status != GoalStatusActive && g.Status != GoalStatusPaused && g.Status != GoalStatusNeedsUser {
			return fmt.Errorf("goal is not runnable in status %q", g.Status)
		}
		if g.ActiveRunID != "" {
			return errors.New("goal already has an active run")
		}
		if g.RunCount >= g.MaxRuns {
			return errors.New("goal run budget exhausted")
		}
		g.Status = GoalStatusActive
		g.ActiveRunID = runID
		g.RunCount++
		g.StopReason = ""
		return nil
	})
	if err == nil {
		if g, ok := store.Get(goalID); ok {
			a.emitGoalUpdate(g)
		}
	}
	return err
}

func (a *App) emitGoalUpdate(g Goal) {
	a.emit("goal:update", map[string]any{"goal": cloneGoal(g), "revision": g.Revision})
}

func toolListHasName(tools []openai.Tool, name string) bool {
	for _, tool := range tools {
		if tool.Function != nil && normalizeToolName(tool.Function.Name) == normalizeToolName(name) {
			return true
		}
	}
	return false
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (a *App) updateGoalForSession(sessionID string, req GoalUpdateRequest) (Goal, error) {
	store, err := a.goalStoreInstance()
	if err != nil {
		return Goal{}, err
	}
	var active *Goal
	for _, candidate := range store.List() {
		if candidate.SessionID == sessionID && candidate.Status == GoalStatusActive {
			if active != nil {
				return Goal{}, errors.New("multiple active goals found for this session")
			}
			g := candidate
			active = &g
		}
	}
	if active == nil {
		return Goal{}, errors.New("no active goal for this session")
	}
	status := strings.TrimSpace(req.Status)
	if status != "progress" && status != "needs_user" && status != "completed" {
		return Goal{}, errors.New("status must be progress, needs_user, or completed")
	}
	progress := strings.TrimSpace(req.Progress)
	if progress == "" {
		return Goal{}, errors.New("progress must not be empty")
	}
	acceptance := make([]string, 0, len(req.Acceptance))
	for _, item := range req.Acceptance {
		item = strings.TrimSpace(item)
		if item != "" {
			acceptance = append(acceptance, item)
		}
	}
	err = store.Update(active.ID, func(g *Goal) error {
		if g.Status != GoalStatusActive {
			return errors.New("goal is no longer active")
		}
		if len(g.Acceptance) > 0 && len(acceptance) > 0 && !stringSlicesEqual(g.Acceptance, acceptance) {
			return errors.New("acceptance criteria are fixed for this goal; ask the user before changing scope")
		}
		if len(g.Acceptance) == 0 && len(acceptance) > 0 {
			g.Acceptance = acceptance
		}
		if status != "needs_user" && len(g.Acceptance) == 0 {
			return errors.New("define observable acceptance criteria before proceeding or completing the goal")
		}
		g.Progress = progress
		switch status {
		case "progress":
		case "needs_user":
			g.Status = GoalStatusNeedsUser
			g.StopReason = progress
		case "completed":
			g.Status = GoalStatusCompleted
			g.StopReason = "验收条件已由 Agent 报告完成；请检查实际验证结果。"
		}
		return nil
	})
	if err != nil {
		return Goal{}, err
	}
	updated, _ := store.Get(active.ID)
	a.emitGoalUpdate(updated)
	return updated, nil
}

func (a *App) activeGoalForSession(sessionID string) bool {
	store, err := a.goalStoreInstance()
	if err != nil {
		return false
	}
	for _, g := range store.List() {
		if g.SessionID == sessionID && g.Status == GoalStatusActive {
			return true
		}
	}
	return false
}

func (a *App) resumableGoalForSession(sessionID string) string {
	store, err := a.goalStoreInstance()
	if err != nil {
		return ""
	}
	for _, g := range store.List() {
		if g.SessionID == sessionID && (g.Status == GoalStatusPaused || g.Status == GoalStatusNeedsUser) && g.RunCount < g.MaxRuns {
			return g.ID
		}
	}
	return ""
}

func (a *App) goalContinuation(sessionID, goalID string, count int) (*openai.ChatCompletionMessage, bool) {
	store, err := a.goalStoreInstance()
	if err != nil {
		return nil, false
	}
	g, ok := store.Get(goalID)
	if !ok || g.SessionID != sessionID || g.Status != GoalStatusActive {
		return nil, false
	}
	if count >= maxGoalAutoContinuations {
		_ = store.Update(goalID, func(current *Goal) error {
			if current.Status == GoalStatusActive {
				current.Status = GoalStatusPaused
				current.StopReason = "本次自动续跑次数已达上限；检查进展后可继续。"
			}
			return nil
		})
		updated, _ := store.Get(goalID)
		a.emitGoalUpdate(updated)
		return nil, false
	}
	criteria, _ := json.Marshal(g.Acceptance)
	return &openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleUser,
		Content: fmt.Sprintf("<ally-goal-continuation>\n本轮回答结束，但目标状态仍为 %q。请从已有进度继续执行，不要重复已完成工作；先检查实际状态，再完成未满足的验收条件。若需要用户决策，用 goal_update 报告 needs_user；只有完成并验证全部验收条件后才报告 completed。\n当前进度：%s\n验收条件：%s\n</ally-goal-continuation>", g.Status, g.Progress, criteria),
	}, true
}

func (a *App) finishGoalRun(goalID, runID, runStatus, runError string) {
	if goalID == "" {
		return
	}
	store, err := a.goalStoreInstance()
	if err != nil {
		a.logAppError("goal state could not be loaded after run", "goalId", goalID, "error", err.Error())
		return
	}
	if err := store.Update(goalID, func(g *Goal) error {
		if g.ActiveRunID != "" && g.ActiveRunID != runID {
			return nil // stale completion from an earlier run
		}
		g.ActiveRunID = ""
		if g.Status == GoalStatusCompleted || g.Status == GoalStatusNeedsUser || g.Status == GoalStatusFailed {
			return nil
		}
		if g.Status == GoalStatusActive {
			g.Status = GoalStatusPaused
			if runError != "" {
				g.StopReason = runError
			} else if runStatus == "done" {
				g.StopReason = "本次 run 已结束，目标尚未明确完成；可继续目标。"
			} else {
				g.StopReason = "run 已停止；目标已暂停，可检查检查点后继续。"
			}
		}
		return nil
	}); err != nil {
		a.logAppError("goal state could not be persisted after run", "goalId", goalID, "error", err.Error())
		return
	}
	if updated, ok := store.Get(goalID); ok {
		a.emitGoalUpdate(updated)
	}
}

// GoalContinueMessage is the visible user message paired with a resumed goal run.
func GoalContinueMessage(g Goal) string { return "继续目标：" + g.Title }

func configForGoal(cfg ConfigState, goal Goal) (ConfigState, error) {
	if goal.Model != nil {
		resolved, ok := modelConfigForTask(cfg, goal.Model)
		if !ok {
			return ConfigState{}, errors.New("goal model is no longer configured; select or restore the original model before continuing")
		}
		cfg = resolved
	}
	if strings.TrimSpace(goal.Workspace) == "" {
		return ConfigState{}, errors.New("goal workspace is missing; cannot resume safely")
	}
	cfg.Workspace = goal.Workspace
	return cfg, nil
}

// ContinueGoal resumes a paused goal in its original session.
func (a *App) ContinueGoal(goalID string, cfg ConfigState) (string, error) {
	store, err := a.goalStoreInstance()
	if err != nil {
		return "", err
	}
	g, ok := store.Get(strings.TrimSpace(goalID))
	if !ok {
		return "", ErrGoalNotFound
	}
	if g.Status != GoalStatusPaused {
		return "", fmt.Errorf("goal cannot be continued from status %q", g.Status)
	}
	if g.RunCount >= g.MaxRuns {
		return "", errors.New("goal run budget exhausted")
	}
	cfg, err = configForGoal(cfg, g)
	if err != nil {
		return "", err
	}
	if err := store.Update(g.ID, func(current *Goal) error {
		if current.Status != GoalStatusPaused {
			return errors.New("goal state changed; refresh and retry")
		}
		current.Status = GoalStatusActive
		current.StopReason = ""
		return nil
	}); err != nil {
		return "", err
	}
	message := GoalContinueMessage(g)
	runID, err := a.StartChat(ChatRequest{SessionID: g.SessionID, Message: message, Config: cfg, GoalID: g.ID})
	if err != nil {
		_ = store.Update(g.ID, func(current *Goal) error {
			current.Status = GoalStatusPaused
			current.StopReason = err.Error()
			return nil
		})
		return "", err
	}
	return runID, nil
}
