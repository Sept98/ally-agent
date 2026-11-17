// SPDX-License-Identifier: GPL-3.0-only
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

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
	if !goalPreflightToolAllowed(name) {
		for i, stage := range goal.Stages {
			if stage.ReviewRequired {
				return codedToolError("E_GOAL_STAGE_REVIEW_REQUIRED", fmt.Errorf("stage %d failed verification repeatedly; submit a structured stage_review before further side effects", i+1))
			}
		}
		if len(goal.Acceptance) == 0 {
			return codedToolError("E_GOAL_ACCEPTANCE_REQUIRED", errors.New("establish and persist observable acceptance criteria with goal_update before running side-effecting tools"))
		}
		activeStage := false
		for _, stage := range goal.Stages {
			if stage.Status == "active" {
				activeStage = true
				break
			}
		}
		if !activeStage {
			return codedToolError("E_GOAL_STAGE_REQUIRED", errors.New("establish ordered goal stages and mark the current stage active before running side-effecting tools"))
		}
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

type GoalStageUpdate struct {
	Title         string                 `json:"title"`
	Objective     string                 `json:"objective"`
	Status        string                 `json:"status"`
	Evidence      string                 `json:"evidence,omitempty"`
	Verifications []GoalVerificationSpec `json:"verifications"`
}

type GoalStageReviewRequest struct {
	Stage            int    `json:"stage"`
	Decision         string `json:"decision"`
	Rationale        string `json:"rationale"`
	RevisedTitle     string `json:"revised_title,omitempty"`
	RevisedObjective string `json:"revised_objective,omitempty"`
}

type GoalUpdateRequest struct {
	Status           string                 `json:"status"`
	Progress         string                 `json:"progress"`
	Acceptance       []string               `json:"acceptance"`
	Stages           []GoalStageUpdate      `json:"stages"`
	Verifications    []GoalVerificationSpec `json:"verifications"`
	StageReview      GoalStageReviewRequest `json:"stage_review"`
	DeliverablePaths []string               `json:"deliverable_paths"`
	DeliverySummary  string                 `json:"delivery_summary,omitempty"`
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
		b.WriteString("\n\n在产生任何副作用前，先整理可观察、可验证的验收条件和有序阶段，并用 goal_update 的 acceptance、stages 字段持久化；阶段包含 title、status（pending/active/completed）、evidence。每次更新都携带完整 stages；每次只推进一个阶段，完成阶段时记录具体证据。若目标有关键歧义，先用 ask 取得用户确认，不要擅自假设。")
	}
	b.WriteString("\ngoal_update 首次更新时设定 acceptance；此后不要重写或改述验收条件，后续调用省略 acceptance 字段，已保存条件始终生效。若确需改变目标范围，先用 needs_user 向用户询问。每次携带完整 stages；stage 必须含 title、objective、status、evidence、verifications。完成某阶段前提供该阶段 named verification commands；Ally 实际运行后仅在所有命令退出码为0时才把阶段检查点记为完成。失败计数按阶段持久化，连续失败3次会要求先提交 stage_review，选择 revise（需给新 title/objective）、continue（说明复核理由）或 needs_user；复核前后端阻止所有副作用工具。每阶段 attempt、检查记录和方案修订历史会持久保存。最终 completed 必须再次提供集成验收命令、简短 delivery_summary；若有文件产物，列出 workspace-relative deliverable_paths；每项必须是普通文件路径，目录请拆成实际文件分别列出，Ally 会确认文件存在并记录 SHA-256。命令在目标工作区沙箱内运行，每条最多120秒；文件路径不允许逃出工作区。只有阶段检查、最终命令及交付物路径检查全部成功后，Ally 才会调用使用同一模型、但拥有独立上下文和专用判定提示词的验收子 agent 审核证据；模型自述完成、命令退出码为0都不能单独证明语义达标。复核不通过会重新打开其指出的阶段，你必须补齐证据或修复后再提交，不能继续宣称完成；复核调用失败或结果无效时目标保持未完成。无法继续时报告 needs_user。普通回答不代表完成。每个阶段开始时，主 agent 都要先判断是否有边界清晰、互不冲突的只读调查、测试设计或 diff review 值得委派给 subagent；有明显并行收益时才委派，否则由主 agent 执行并简述原因，避免为形式调用子代理。把是否委派及子代理任务简短写入阶段 progress/evidence，主 agent 必须核实子代理报告并亲自运行验收命令，不能把子代理的完成声明当证据。代码修改及其他副作用仍由主 agent 控制。")
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
	return a.updateGoalForSessionWithConfig(context.Background(), sessionID, ConfigState{}, req)
}

func (a *App) updateGoalForSessionWithConfig(ctx context.Context, sessionID string, cfg ConfigState, req GoalUpdateRequest) (Goal, error) {
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
	acceptance, err := normalizeGoalAcceptance(req.Acceptance)
	if err != nil {
		return Goal{}, err
	}
	if len(active.Acceptance) > 0 && len(acceptance) > 0 && !stringSlicesEqual(active.Acceptance, acceptance) {
		frozen, _ := json.Marshal(active.Acceptance)
		return Goal{}, fmt.Errorf("acceptance criteria are fixed for this goal; do not resend or change them. Omit acceptance to keep the saved criteria %s. Use needs_user to ask the user before changing scope", frozen)
	}
	if strings.TrimSpace(req.StageReview.Decision) != "" {
		return a.reviewGoalStage(ctx, store, active, status, progress, req.StageReview)
	}
	for i, stage := range active.Stages {
		if stage.ReviewRequired {
			return Goal{}, codedToolError("E_GOAL_STAGE_REVIEW_REQUIRED", fmt.Errorf("stage %d repeatedly failed verification; submit stage_review (revise, continue, or needs_user) before more work", i+1))
		}
	}

	stageCount := len(req.Stages)
	if stageCount == 0 && status == "needs_user" {
		stageCount = len(active.Stages)
	}
	if stageCount == 0 && status != "needs_user" {
		return Goal{}, errors.New("define an ordered stage plan before progressing or completing the goal")
	}
	if len(active.Stages) > 0 && stageCount != len(active.Stages) {
		return Goal{}, errors.New("goal stage plan is fixed; use stage_review after repeated verification failures to revise it")
	}
	if stageCount > maxGoalStageItems {
		return Goal{}, errors.New("too many goal stages")
	}
	nextStages := make([]GoalStage, stageCount)
	if len(req.Stages) == 0 {
		nextStages = append([]GoalStage(nil), active.Stages...)
	} else {
		commandCount := len(req.Verifications)
		for i := range req.Stages {
			req.Stages[i].Title = strings.TrimSpace(req.Stages[i].Title)
			req.Stages[i].Objective = strings.TrimSpace(req.Stages[i].Objective)
			req.Stages[i].Evidence = strings.TrimSpace(req.Stages[i].Evidence)
			if err := validateGoalVerificationSpecs(req.Stages[i].Verifications); err != nil {
				return Goal{}, fmt.Errorf("stage %d checks: %w", i+1, err)
			}
			commandCount += len(req.Stages[i].Verifications)
			if commandCount > maxGoalVerificationItems {
				return Goal{}, fmt.Errorf("at most %d stage and final verification commands are allowed per update", maxGoalVerificationItems)
			}
			stage := GoalStage{Title: req.Stages[i].Title, Objective: req.Stages[i].Objective, Status: req.Stages[i].Status, Evidence: req.Stages[i].Evidence}
			if stage.Title == "" || len([]rune(stage.Title)) > maxGoalStageTitleRunes || stage.Objective == "" || len([]rune(stage.Objective)) > maxGoalStageEvidenceRunes {
				return Goal{}, fmt.Errorf("stage %d requires a bounded title and objective", i+1)
			}
			if len(active.Stages) > 0 {
				previous := active.Stages[i]
				if previous.Title != stage.Title || previous.Objective != "" && previous.Objective != stage.Objective {
					return Goal{}, errors.New("stage titles and objectives are fixed; repeated failures require stage_review")
				}
				if previous.Status == "completed" && stage.Status != "completed" {
					return Goal{}, fmt.Errorf("completed stage %d cannot be reopened except through stage_review", i+1)
				}
				if previous.Status == "active" && stage.Status == "pending" {
					return Goal{}, fmt.Errorf("active stage %d cannot return to pending", i+1)
				}
				if previous.Objective == "" {
					stage.Objective = req.Stages[i].Objective
				}
				stage.Attempts = previous.Attempts
				stage.FailuresSinceReview = previous.FailuresSinceReview
				stage.ReviewRequired = previous.ReviewRequired
				stage.Revision = previous.Revision
				stage.ReviewHistory = append([]GoalStageRevision(nil), previous.ReviewHistory...)
				stage.Verifications = append([]GoalVerification(nil), previous.Verifications...)
				stage.VerificationAttempts = cloneGoalVerificationAttempts(previous.VerificationAttempts)
			}
			if stage.Status != "pending" && stage.Status != "active" && stage.Status != "completed" {
				return Goal{}, fmt.Errorf("stage %d status must be pending, active, or completed", i+1)
			}
			nextStages[i] = stage
		}
	}
	if len(req.Verifications) > maxGoalVerificationItems {
		return Goal{}, fmt.Errorf("at most %d final verification commands are allowed", maxGoalVerificationItems)
	}
	if err := validateGoalVerificationSpecs(req.Verifications); err != nil {
		return Goal{}, err
	}
	for i := 1; i < len(nextStages); i++ {
		if nextStages[i].Status != "pending" && nextStages[i-1].Status != "completed" {
			return Goal{}, errors.New("goal stages must proceed in order")
		}
	}
	activeStages := 0
	for _, stage := range nextStages {
		if stage.Status == "active" {
			activeStages++
		}
	}
	if activeStages > 1 {
		return Goal{}, errors.New("only one goal stage may be active")
	}
	if status != "needs_user" && len(acceptance) == 0 && len(active.Acceptance) == 0 {
		return Goal{}, errors.New("define observable acceptance criteria before proceeding or completing the goal")
	}

	verifyCfg, configErr := configForGoal(cfg, *active)
	if configErr != nil && (hasNewStageChecks(active.Stages, req.Stages) || status == "completed") {
		return Goal{}, configErr
	}
	verifyCfg.ExtraRoots = nil
	if (hasNewStageChecks(active.Stages, req.Stages) || status == "completed") && builtinToolDisabled(ctx, cfg, "command") {
		return Goal{}, codedToolError("E_TOOL_DISABLED", errors.New("command is disabled in this goal session; enable it before requesting automatic verification"))
	}

	// Every newly completed phase must pass its own checks before it is checkpointed.
	for i := range nextStages {
		previousCompleted := i < len(active.Stages) && active.Stages[i].Status == "completed"
		if nextStages[i].Status != "completed" || previousCompleted {
			continue
		}
		specs := req.Stages[i].Verifications
		if len(specs) == 0 {
			return Goal{}, fmt.Errorf("stage %d needs at least one Ally-run verification command before it can be completed", i+1)
		}
		results, failed, runErr := a.runGoalVerificationCommands(ctx, verifyCfg, specs)
		if runErr != nil {
			return Goal{}, runErr
		}
		nextStages[i].Attempts++
		nextStages[i].Verifications = append([]GoalVerification(nil), results...)
		if len(nextStages[i].VerificationAttempts) >= maxGoalStageVerificationResults {
			return Goal{}, fmt.Errorf("stage %d verification history limit reached", i+1)
		}
		nextStages[i].VerificationAttempts = append(nextStages[i].VerificationAttempts, newGoalVerificationAttempt(nextStages[i].Attempts, results, failed == nil))
		if failed != nil {
			nextStages[i].Status = "active"
			nextStages[i].Evidence = ""
			nextStages[i].FailuresSinceReview++
			nextStages[i].ReviewRequired = nextStages[i].FailuresSinceReview >= goalStageReviewFailureThreshold
			for j := i + 1; j < len(nextStages); j++ {
				nextStages[j].Status = "pending"
				nextStages[j].Evidence = ""
			}
			reason := stageVerificationFailureMessage(i, nextStages[i])
			updated, persistErr := a.persistGoalProgress(store, active.ID, acceptance, nextStages, reason)
			if persistErr != nil {
				return Goal{}, fmt.Errorf("stage verification failed and checkpoint could not be persisted: %w", persistErr)
			}
			a.emitGoalUpdate(updated)
			return updated, fmt.Errorf("stage %d verification %q failed (exit %d); %s", i+1, failed.Name, failed.ExitCode, reason)
		}
	}
	if status == "completed" {
		for i, stage := range nextStages {
			if stage.Status != "completed" || strings.TrimSpace(stage.Evidence) == "" || !allGoalVerificationsPassed(stage.Verifications) {
				return Goal{}, fmt.Errorf("stage %d is not completed with passing checks and evidence", i+1)
			}
		}
		if strings.TrimSpace(req.DeliverySummary) == "" || len([]rune(req.DeliverySummary)) > maxGoalDeliverySummaryRunes {
			return Goal{}, errors.New("provide a bounded final delivery summary")
		}
		if len(req.Verifications) == 0 {
			return Goal{}, errors.New("provide at least one final integration verification command")
		}
		finalResults, failed, runErr := a.runGoalVerificationCommands(ctx, verifyCfg, req.Verifications)
		if runErr != nil {
			return Goal{}, runErr
		}
		if len(active.FinalVerificationAttempts) >= maxGoalStageVerificationResults {
			return Goal{}, errors.New("final verification history limit reached")
		}
		finalAttempt := newGoalVerificationAttempt(len(active.FinalVerificationAttempts)+1, finalResults, failed == nil)
		if failed != nil {
			reason := fmt.Sprintf("final verification %q failed (exit %d); review the integration check and retry", failed.Name, failed.ExitCode)
			if err := store.Update(active.ID, func(g *Goal) error {
				if g.Status != GoalStatusActive {
					return errors.New("goal is no longer active")
				}
				g.FinalVerificationAttempts = append(g.FinalVerificationAttempts, finalAttempt)
				g.Verifications = append([]GoalVerification(nil), finalResults...)
				g.Progress = reason
				g.DeliverySummary = ""
				g.Deliverables = nil
				g.CompletionReview = nil
				return nil
			}); err != nil {
				return Goal{}, fmt.Errorf("final verification failed and checkpoint could not be persisted: %w", err)
			}
			updated, _ := store.Get(active.ID)
			a.emitGoalUpdate(updated)
			return updated, errors.New(reason)
		}
		deliverables, deliveryErr := a.verifyGoalDeliverables(ctx, active.Workspace, req.DeliverablePaths)
		if deliveryErr != nil {
			reason := "最终交付物检查失败：" + deliveryErr.Error()
			if err := store.Update(active.ID, func(g *Goal) error {
				if g.Status != GoalStatusActive {
					return errors.New("goal is no longer active")
				}
				g.FinalVerificationAttempts = append(g.FinalVerificationAttempts, finalAttempt)
				g.Verifications = append([]GoalVerification(nil), finalResults...)
				g.Progress = reason
				g.DeliverySummary = ""
				g.Deliverables = nil
				g.CompletionReview = nil
				return nil
			}); err != nil {
				return Goal{}, fmt.Errorf("deliverable check failed and checkpoint could not be persisted: %w", err)
			}
			updated, _ := store.Get(active.ID)
			a.emitGoalUpdate(updated)
			return updated, errors.New(reason)
		}
		// Persist final state only after phase/integration checks, deliverables,
		// and an isolated completion review all pass.
		review, reviewErr := a.reviewGoalCompletion(ctx, verifyCfg, active.ID, *active, nextStages, req, finalResults, deliverables)
		if reviewErr != nil {
			return Goal{}, reviewErr
		}
		if review.Verdict != "approved" {
			stageIndex := review.Stage - 1
			if stageIndex < 0 || stageIndex >= len(nextStages) {
				stageIndex = len(nextStages) - 1
			}
			for i := stageIndex; i < len(nextStages); i++ {
				if i == stageIndex {
					nextStages[i].Status = "active"
				} else {
					nextStages[i].Status = "pending"
				}
				nextStages[i].Evidence = ""
			}
			reason := review.Reason
			if reason == "" {
				reason = "独立验收复核认为仍需补充证据。"
			}
			if err := store.Update(active.ID, func(g *Goal) error {
				if g.Status != GoalStatusActive {
					return errors.New("goal is no longer active")
				}
				g.Stages = cloneGoal(Goal{Stages: nextStages}).Stages
				g.Verifications = append([]GoalVerification(nil), finalResults...)
				g.FinalVerificationAttempts = append(g.FinalVerificationAttempts, finalAttempt)
				g.Deliverables = append([]GoalDeliverable(nil), deliverables...)
				g.DeliverySummary = ""
				g.Progress = reason
				g.CompletionReview = &review
				return nil
			}); err != nil {
				return Goal{}, err
			}
			updated, _ := store.Get(active.ID)
			a.emitGoalUpdate(updated)
			return updated, fmt.Errorf("独立验收复核未通过，已重新打开阶段 %d：%s", stageIndex+1, reason)
		}
		if err := store.Update(active.ID, func(g *Goal) error {
			if g.Status != GoalStatusActive {
				return errors.New("goal is no longer active")
			}
			g.Acceptance = append([]string(nil), chooseAcceptance(acceptance, g.Acceptance)...)
			g.Stages = cloneGoal(Goal{Stages: nextStages}).Stages
			g.Verifications = append([]GoalVerification(nil), finalResults...)
			g.FinalVerificationAttempts = append(g.FinalVerificationAttempts, finalAttempt)
			g.CompletionReview = &review
			g.Deliverables = append([]GoalDeliverable(nil), deliverables...)
			g.DeliverySummary = strings.TrimSpace(req.DeliverySummary)
			g.Progress = progress
			g.Status = GoalStatusCompleted
			g.StopReason = "阶段检查与验收命令已由 Ally 执行通过；交付物已核验。语义质量仍需用户判断。"
			return nil
		}); err != nil {
			return Goal{}, err
		}
		completed, _ := store.Get(active.ID)
		a.emitGoalUpdate(completed)
		return completed, nil
	}
	updated, persistErr := a.persistGoalProgress(store, active.ID, acceptance, nextStages, progress)
	if persistErr != nil {
		return Goal{}, persistErr
	}
	if status == "needs_user" {
		if err := store.Update(active.ID, func(g *Goal) error {
			if g.Status != GoalStatusActive {
				return errors.New("goal is no longer active")
			}
			g.Status = GoalStatusNeedsUser
			g.StopReason = progress
			return nil
		}); err != nil {
			return Goal{}, err
		}
		updated, _ = store.Get(active.ID)
		a.emitGoalUpdate(updated)
	}
	return updated, nil
}

func chooseAcceptance(incoming, existing []string) []string {
	if len(incoming) > 0 {
		return incoming
	}
	return existing
}

func normalizeGoalAcceptance(items []string) ([]string, error) {
	if len(items) > maxGoalAcceptanceItems {
		return nil, errors.New("too many goal acceptance criteria")
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" || len([]rune(item)) > maxGoalAcceptanceRunes {
			return nil, errors.New("goal acceptance criteria must be non-empty and bounded")
		}
		out = append(out, item)
	}
	return out, nil
}

func validateGoalVerificationSpecs(specs []GoalVerificationSpec) error {
	if len(specs) > maxGoalVerificationItems {
		return fmt.Errorf("at most %d verification commands are allowed", maxGoalVerificationItems)
	}
	for i := range specs {
		specs[i].Name = strings.TrimSpace(specs[i].Name)
		specs[i].Command = strings.TrimSpace(specs[i].Command)
		if specs[i].Name == "" || len([]rune(specs[i].Name)) > maxGoalVerificationNameRunes || specs[i].Command == "" || len([]rune(specs[i].Command)) > maxGoalVerificationCommandRunes {
			return fmt.Errorf("verification %d needs a bounded name and command", i+1)
		}
	}
	return nil
}

func hasNewStageChecks(previous []GoalStage, next []GoalStageUpdate) bool {
	for i, stage := range next {
		if i >= len(previous) || previous[i].Status != "completed" && stage.Status == "completed" && len(stage.Verifications) > 0 {
			return true
		}
	}
	return false
}

func newGoalVerificationAttempt(number int, results []GoalVerification, passed bool) GoalVerificationAttempt {
	return GoalVerificationAttempt{Number: number, Checks: append([]GoalVerification(nil), results...), Passed: passed, At: time.Now().UTC()}
}

func allGoalVerificationsPassed(results []GoalVerification) bool {
	if len(results) == 0 {
		return false
	}
	for _, result := range results {
		if !result.Passed {
			return false
		}
	}
	return true
}

type goalCompletionReviewInput struct {
	Objective       string                        `json:"objective"`
	Acceptance      []string                      `json:"acceptance"`
	Progress        string                        `json:"progress"`
	Stages          []goalCompletionStageEvidence `json:"stages"`
	FinalChecks     []goalCompletionCheckEvidence `json:"final_checks"`
	Deliverables    []GoalDeliverable             `json:"deliverables"`
	DeliverySummary string                        `json:"delivery_summary"`
}

type goalCompletionStageEvidence struct {
	Title     string             `json:"title"`
	Objective string             `json:"objective"`
	Evidence  string             `json:"evidence"`
	Checks    []GoalVerification `json:"checks"`
}

type goalCompletionCheckEvidence struct {
	Name     string `json:"name"`
	Command  string `json:"command"`
	ExitCode int    `json:"exit_code"`
	Passed   bool   `json:"passed"`
}

func (a *App) reviewGoalCompletion(ctx context.Context, cfg ConfigState, sessionID string, goal Goal, stages []GoalStage, req GoalUpdateRequest, finalResults []GoalVerification, deliverables []GoalDeliverable) (GoalCompletionReview, error) {
	input := goalCompletionReviewInput{
		Objective: goal.Details, Acceptance: append([]string(nil), goal.Acceptance...), Progress: req.Progress,
		Deliverables: append([]GoalDeliverable(nil), deliverables...), DeliverySummary: req.DeliverySummary,
	}
	for _, stage := range stages {
		input.Stages = append(input.Stages, goalCompletionStageEvidence{
			Title: stage.Title, Objective: stage.Objective, Evidence: stage.Evidence,
			Checks: append([]GoalVerification(nil), stage.Verifications...),
		})
	}
	for i, spec := range req.Verifications {
		if i >= len(finalResults) {
			break
		}
		input.FinalChecks = append(input.FinalChecks, goalCompletionCheckEvidence{
			Name: spec.Name, Command: spec.Command, ExitCode: finalResults[i].ExitCode, Passed: finalResults[i].Passed,
		})
	}
	data, err := json.Marshal(input)
	if err != nil {
		return GoalCompletionReview{}, err
	}
	task := "You are a separate acceptance judge using a fresh context and the same configured model as the executing agent. Judge only the supplied evidence. Do not trust the executor's completion claim by itself. A zero exit code is not sufficient unless the command meaningfully checks the stated acceptance criteria; commands such as `true` or `echo` are not substantive verification. Every acceptance criterion needs specific supporting evidence, completed ordered stages, passing checks, and coherent deliverables. If evidence is missing, weak, or ambiguous, choose needs_work and identify the one-based stage that should be reopened. Do not propose broad new scope. Return exactly one JSON object, no markdown: {\"verdict\":\"approved\"|\"needs_work\",\"reason\":\"concise evidence-based reason\",\"stage\":1}. Use stage 0 only when approved.\n\nEvidence JSON:\n" + string(data)
	reqDelegate := AgentDelegateRequest{
		Task: task, Role: "independent goal acceptance reviewer",
		Description: "独立复核目标验收证据", CleanContext: true, Model: cfg.Model, MaxSteps: 2, toolsProvided: true,
	}
	var result *AgentDelegateResult
	if a.goalCompletionReviewRunner != nil {
		result, err = a.goalCompletionReviewRunner(ctx, cfg, sessionID, reqDelegate)
	} else {
		result, err = a.executeDelegate(ctx, cfg, sessionID, reqDelegate)
	}
	if err != nil {
		return GoalCompletionReview{}, fmt.Errorf("独立验收复核调用失败：%w", err)
	}
	if result == nil || result.Status != "completed" {
		return GoalCompletionReview{}, errors.New("独立验收复核未能完成")
	}
	var review GoalCompletionReview
	if err := json.Unmarshal([]byte(strings.TrimSpace(result.Summary)), &review); err != nil {
		return GoalCompletionReview{}, errors.New("独立验收复核没有返回有效 JSON；目标保持未完成")
	}
	review.Verdict = strings.TrimSpace(review.Verdict)
	review.Reason = strings.TrimSpace(review.Reason)
	if (review.Verdict != "approved" && review.Verdict != "needs_work") || review.Reason == "" || len([]rune(review.Reason)) > maxGoalStageEvidenceRunes || review.Verdict == "approved" && review.Stage != 0 || review.Verdict == "needs_work" && (review.Stage < 1 || review.Stage > len(stages)) {
		return GoalCompletionReview{}, errors.New("独立验收复核返回了无效判定；目标保持未完成")
	}
	review.Model = cfg.Model
	review.ReviewedAt = time.Now().UTC()
	return review, nil
}

func (a *App) runGoalVerificationCommands(ctx context.Context, cfg ConfigState, specs []GoalVerificationSpec) ([]GoalVerification, *GoalVerification, error) {
	if err := validateGoalVerificationSpecs(specs); err != nil {
		return nil, nil, err
	}
	results := make([]GoalVerification, 0, len(specs))
	for _, spec := range specs {
		if err := ctx.Err(); err != nil {
			return results, nil, err
		}
		result, runErr := a.runCommandWithConfig(ctx, cfg, CommandRequest{Command: strings.TrimSpace(spec.Command), Timeout: 120, noPromote: true})
		verification := GoalVerification{Name: strings.TrimSpace(spec.Name), ExitCode: -1, VerifiedAt: time.Now().UTC()}
		if runErr == nil {
			verification.ExitCode = result.ExitCode
			verification.Passed = !result.TimedOut && result.ExitCode == 0
		}
		results = append(results, verification)
		if !verification.Passed {
			return results, &results[len(results)-1], nil
		}
	}
	return results, nil, nil
}

func (a *App) persistGoalProgress(store *GoalStore, goalID string, acceptance []string, stages []GoalStage, progress string) (Goal, error) {
	if err := store.Update(goalID, func(g *Goal) error {
		if g.Status != GoalStatusActive {
			return errors.New("goal is no longer active")
		}
		if len(g.Acceptance) == 0 {
			g.Acceptance = append([]string(nil), acceptance...)
		}
		g.Stages = cloneGoal(Goal{Stages: stages}).Stages
		g.Verifications = nil
		g.Deliverables = nil
		g.DeliverySummary = ""
		g.CompletionReview = nil
		g.Progress = progress
		return nil
	}); err != nil {
		return Goal{}, err
	}
	updated, ok := store.Get(goalID)
	if !ok {
		return Goal{}, ErrGoalNotFound
	}
	return updated, nil
}

func stageVerificationFailureMessage(index int, stage GoalStage) string {
	if stage.ReviewRequired {
		return fmt.Sprintf("阶段 %d 已连续失败 %d 次，系统已暂停副作用操作；必须提交 stage_review 复核是修改阶段目标、按原目标继续，还是请求用户介入。", index+1, stage.FailuresSinceReview)
	}
	return fmt.Sprintf("阶段 %d 当前失败计数 %d/%d；检查命令和实现后重试。", index+1, stage.FailuresSinceReview, goalStageReviewFailureThreshold)
}

func (a *App) reviewGoalStage(ctx context.Context, store *GoalStore, active *Goal, status, progress string, review GoalStageReviewRequest) (Goal, error) {
	if status != "progress" {
		return Goal{}, errors.New("stage_review must be reported with status progress")
	}
	if review.Stage < 1 || review.Stage > len(active.Stages) {
		return Goal{}, errors.New("stage_review stage index is out of range (use one-based indexing)")
	}
	rationale := strings.TrimSpace(review.Rationale)
	if rationale == "" || len([]rune(rationale)) > maxGoalStageEvidenceRunes {
		return Goal{}, errors.New("stage_review needs a bounded rationale")
	}
	index := review.Stage - 1
	stage := active.Stages[index]
	if !stage.ReviewRequired {
		return Goal{}, errors.New("this stage has not reached the required repeated-failure review threshold")
	}
	decision := strings.TrimSpace(review.Decision)
	if decision != "revise" && decision != "continue" && decision != "needs_user" {
		return Goal{}, errors.New("stage_review decision must be revise, continue, or needs_user")
	}
	if decision == "revise" {
		review.RevisedTitle = strings.TrimSpace(review.RevisedTitle)
		review.RevisedObjective = strings.TrimSpace(review.RevisedObjective)
		if review.RevisedTitle == "" || len([]rune(review.RevisedTitle)) > maxGoalStageTitleRunes || review.RevisedObjective == "" || len([]rune(review.RevisedObjective)) > maxGoalStageEvidenceRunes {
			return Goal{}, errors.New("revising a stage requires a bounded new title and objective")
		}
	}
	if len(stage.ReviewHistory) >= maxGoalStageRevisionHistory {
		return Goal{}, errors.New("stage review history limit reached; request user input")
	}
	if err := store.Update(active.ID, func(g *Goal) error {
		if g.Status != GoalStatusActive || !g.Stages[index].ReviewRequired {
			return errors.New("stage review is stale")
		}
		current := &g.Stages[index]
		revision := GoalStageRevision{Decision: decision, OldTitle: current.Title, OldObjective: current.Objective, Rationale: rationale, At: time.Now().UTC()}
		if decision == "revise" {
			revision.NewTitle, revision.NewObjective = review.RevisedTitle, review.RevisedObjective
			current.Title, current.Objective = review.RevisedTitle, review.RevisedObjective
			current.Revision++
		}
		current.ReviewHistory = append(current.ReviewHistory, revision)
		current.ReviewRequired = false
		current.FailuresSinceReview = 0
		current.Status = "active"
		current.Evidence = ""
		g.Verifications = nil
		g.Deliverables = nil
		g.DeliverySummary = ""
		g.CompletionReview = nil
		g.Progress = progress
		if decision == "needs_user" {
			g.Status = GoalStatusNeedsUser
			g.StopReason = rationale
		}
		return nil
	}); err != nil {
		return Goal{}, err
	}
	updated, _ := store.Get(active.ID)
	a.emitGoalUpdate(updated)
	return updated, nil
}

func (a *App) verifyGoalDeliverables(ctx context.Context, workspace string, paths []string) ([]GoalDeliverable, error) {
	if len(paths) > maxGoalDeliverables {
		return nil, fmt.Errorf("at most %d deliverables are allowed", maxGoalDeliverables)
	}
	seen := make(map[string]bool, len(paths))
	results := make([]GoalDeliverable, 0, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" || len([]rune(path)) > maxGoalDeliveryPathRunes || filepath.IsAbs(path) || filepath.Clean(path) == ".." || strings.HasPrefix(filepath.Clean(path), ".."+string(filepath.Separator)) {
			return nil, errors.New("deliverable paths must be bounded workspace-relative paths")
		}
		clean := filepath.ToSlash(filepath.Clean(path))
		if seen[clean] {
			return nil, fmt.Errorf("duplicate deliverable path %q", clean)
		}
		seen[clean] = true
		info, err := a.InspectWorkspaceFile(ctx, WorkspacePathRequest{Workspace: workspace, Path: clean}, true)
		if err != nil {
			return nil, fmt.Errorf("deliverable %q could not be inspected", clean)
		}
		if info.IsDir || info.SHA256 == "" {
			return nil, fmt.Errorf("deliverable %q is not a regular file; list files inside the directory instead", clean)
		}
		results = append(results, GoalDeliverable{Path: clean, Size: info.Size, SHA256: info.SHA256, VerifiedAt: time.Now().UTC()})
	}
	return results, nil
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
	stages, _ := json.Marshal(g.Stages)
	return &openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleUser,
		Content: fmt.Sprintf("<ally-goal-continuation>\n本轮回答结束，但目标状态仍为 %q。请从检查点继续，不重复已完成工作。验收条件已经固定：后续 goal_update 必须省略 acceptance 字段，不要重新生成、改写或增加标准；如果确需改变范围，先用 needs_user 询问用户。每进入一个新阶段，先由主 agent 判断是否值得把独立只读调查、测试设计或 diff review 委派给 subagent；只在有实际并行收益时调用，否则说明由主 agent 执行的原因。主 agent 核实所有子代理报告，修改和验收仍由主 agent 负责。阶段失败达到阈值时必须先用 goal_update 的 stage_review 选择 revise/continue/needs_user；复核前不能做副作用操作。最终完成必须通过每阶段命令、最终集成命令，并提交 delivery_summary 与实际 deliverable_paths（若产物为文件）。\n当前进度：%s\n已固定验收条件（仅供核对，不要复制到请求中）：%s\n阶段检查点（JSON）：%s\n</ally-goal-continuation>", g.Status, g.Progress, criteria, stages),
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
