// SPDX-License-Identifier: GPL-3.0-only
package desktop

import (
	"encoding/json"
	"fmt"
	"strings"

	"ally-dev/desktop/uikit"
	backend "ally-dev/internal/app"
	"github.com/egoist/mygo/ui"
)

func parseGoalCommand(text string) (string, bool) {
	text = strings.TrimSpace(text)
	if len(text) < 6 || !strings.EqualFold(text[:5], "/goal") || (text[5] != ' ' && text[5] != '\t' && text[5] != '\n') {
		return "", false
	}
	objective := strings.TrimSpace(text[5:])
	return objective, objective != ""
}

func (a *desktop) startGoal(current *conversation, objective string) {
	objective = strings.TrimSpace(objective)
	if current == nil || objective == "" {
		return
	}
	cfg, found := a.chatConfig(current)
	if !found {
		a.notifyError("所选模型已不存在，请重新选择")
		return
	}
	fresh := a.freshConversation(current.Workspace)
	fresh.Model, fresh.Reasoning = current.Model, current.Reasoning
	a.tabs = append(a.tabs, fresh)
	a.page = "chat"
	a.activateTab(len(a.tabs) - 1)
	a.composerFocus = fresh.ID
	sessionID, workspace := fresh.ID, fresh.Workspace
	fresh.Title = short(objective, 36)
	fresh.Messages = append(fresh.Messages, map[string]any{"role": "user", "content": objective})
	fresh.Busy = true
	fresh.Status = "正在启动目标…"
	a.persist(fresh)
	go func() {
		goal, runID, err := a.core.StartGoal(backend.GoalStartRequest{SessionID: sessionID, Workspace: workspace, Title: objective, Objective: objective, Config: cfg})
		a.win.Update(func() {
			if err != nil {
				fresh.Busy = false
				fresh.Status = err.Error()
				invalidateChatSearch(fresh)
				fresh.Messages = append(fresh.Messages, map[string]any{"role": "assistant", "content": err.Error(), "error": true})
				a.persist(fresh)
				a.notifyError("目标启动失败：" + err.Error())
				return
			}
			if fresh.Busy {
				fresh.RunID = runID
			}
			a.applyGoalUpdate(map[string]any{"goal": goal, "revision": float64(goal.Revision)})
			a.notifySuccess("目标已启动")
		})
	}()
}

func (a *desktop) applyGoalDelete(p map[string]any) {
	sessionID := str(p, "sessionId")
	if sessionID == "" {
		return
	}
	rev := int64(num(p, "revision"))
	if a.goalRev != nil && rev > 0 && rev <= a.goalRev[sessionID] {
		return
	}
	if a.goals != nil {
		delete(a.goals, sessionID)
	}
	if a.goalRev == nil {
		a.goalRev = map[string]int64{}
	}
	a.goalRev[sessionID] = rev
}

func (a *desktop) applyGoalUpdate(p map[string]any) {
	b, err := json.Marshal(p["goal"])
	if err != nil {
		return
	}
	var goal backend.Goal
	if json.Unmarshal(b, &goal) != nil || goal.SessionID == "" {
		return
	}
	rev := int64(num(p, "revision"))
	if a.goalRev != nil && rev > 0 && rev <= a.goalRev[goal.SessionID] {
		return
	}
	if a.goals == nil {
		a.goals = map[string]backend.Goal{}
	}
	if a.goalRev == nil {
		a.goalRev = map[string]int64{}
	}
	a.goals[goal.SessionID] = goal
	a.goalRev[goal.SessionID] = rev
	if goal.Status == backend.GoalStatusCompleted {
		for _, tab := range a.tabs {
			if tab.ID == goal.SessionID && tab.UI != nil {
				tab.UI.GoalPanelVisible = false
				tab.UI.GoalDetailsOpen = false
				break
			}
		}
	}
}

func (a *desktop) deleteGoalRecord(goal backend.Goal) {
	go func() {
		err := a.core.DeleteGoal(goal.ID)
		a.win.Update(func() {
			if err != nil {
				a.notifyError("移除目标记录失败：" + err.Error())
			}
		})
	}()
}

func (a *desktop) continueGoal(goal backend.Goal) {
	var t *conversation
	for _, tab := range a.tabs {
		if tab.ID == goal.SessionID {
			t = tab
			break
		}
	}
	if t == nil {
		a.notifyError("请先打开目标所属会话，再继续目标")
		return
	}
	if t.Busy {
		a.notifyError("当前会话正在运行，请等待或取消后再继续目标")
		return
	}
	cfg, found := a.chatConfig(t)
	if !found {
		a.notifyError("所选模型已不存在，请重新选择")
		return
	}
	t.Messages = append(t.Messages, map[string]any{"role": "user", "content": backend.GoalContinueMessage(goal)})
	invalidateChatSearch(t)
	t.Busy = true
	t.Status = "正在继续目标…"
	a.persist(t)
	go func() {
		_, err := a.core.ContinueGoal(goal.ID, cfg)
		a.win.Update(func() {
			if err != nil {
				t.Busy = false
				t.Status = err.Error()
				invalidateChatSearch(t)
				t.Messages = append(t.Messages, map[string]any{"role": "assistant", "content": err.Error(), "error": true})
				a.persist(t)
				a.notify("继续目标失败：" + err.Error())
			}
		})
	}()
}

func goalStatusLabel(status backend.GoalStatus) string {
	switch status {
	case backend.GoalStatusPending:
		return "待启动"
	case backend.GoalStatusActive:
		return "执行中"
	case backend.GoalStatusPaused:
		return "已暂停"
	case backend.GoalStatusNeedsUser:
		return "等待你补充信息"
	case backend.GoalStatusCompleted:
		return "Agent 报告完成 · 自动检查通过 · 请核验"
	case backend.GoalStatusFailed:
		return "启动失败"
	default:
		return string(status)
	}
}

func goalProgressSummary(goal backend.Goal) string {
	parts := []string{}
	if goal.PreflightRequired && !goal.PreflightReady {
		if goal.ClarificationRequired && !goal.ClarificationAnswered {
			parts = append(parts, "等待你回答澄清问题")
		} else {
			parts = append(parts, "前置理解与计划尚未确认")
		}
	}
	if goal.Status == backend.GoalStatusPaused && goal.StopReason != "" {
		parts = append(parts, "暂停原因："+boundedGoalText(goal.StopReason, 100))
	}
	return strings.Join(parts, " · ")
}

func goalToolActivity(m map[string]any) string {
	name := str(m, "name")
	args, ok := m["args"].(map[string]any)
	if !ok {
		args = parseToolArgs(str(m, "args"))
	}
	label := map[string]string{"create": "创建文件", "edit": "修改文件", "delete": "删除文件", "command": "运行命令", "read": "读取文件", "grep": "搜索代码", "list_files": "查看目录", "subagent": "委派子任务", "goal_update": "更新阶段进度"}[toolKind(name)]
	if label == "" {
		label = name
	}
	detail := toolPurpose(m, args)
	if detail == "" {
		detail = str(args, "path")
	}
	if detail == "" {
		detail = str(args, "purpose")
	}
	if detail == "" {
		detail = "工作区操作"
	}
	return label + " · " + boundedGoalText(detail, 140)
}

func goalDisplayedActivity(st *chatUI, t *conversation) string {
	if st != nil && st.GoalActivity != "" {
		return "正在执行：" + st.GoalActivity
	}
	if st != nil && st.GoalLastActivity != "" {
		return st.GoalLastActivity
	}
	if t != nil {
		for i := len(t.Messages) - 1; i >= 0; i-- {
			m := t.Messages[i]
			if str(m, "role") != "tool_call" {
				continue
			}
			status := str(m, "status")
			if status != "success" && status != "error" && status != "running" {
				continue
			}
			prefix := "最近完成："
			if status == "error" {
				prefix = "最近失败："
			} else if status == "running" {
				prefix = "正在执行："
			}
			activity := prefix + goalToolActivity(m)
			if status == "error" && str(m, "error") != "" {
				activity += " · " + boundedGoalText(str(m, "error"), 140)
			}
			return activity
		}
	}
	return ""
}

func goalStageProgress(goal backend.Goal) (done, total int) {
	total = len(goal.Stages)
	for _, stage := range goal.Stages {
		if stage.Status == "completed" {
			done++
		}
	}
	return done, total
}

func goalCurrentStage(goal backend.Goal) (int, backend.GoalStage, bool) {
	for i, stage := range goal.Stages {
		if stage.Status == "active" {
			return i, stage, true
		}
	}
	return -1, backend.GoalStage{}, false
}

func goalCompletedStageCount(goal backend.Goal) int {
	done, _ := goalStageProgress(goal)
	return done
}

func goalStageProgressLabel(goal backend.Goal) string {
	done, total := goalStageProgress(goal)
	if total > 0 && done == total {
		return fmt.Sprintf("全部 %d 阶段已完成", total)
	}
	if i, _, ok := goalCurrentStage(goal); ok {
		return fmt.Sprintf("当前第 %d/%d 阶段 · 已完成 %d", i+1, total, done)
	}
	for i, stage := range goal.Stages {
		if stage.Status == "pending" {
			return fmt.Sprintf("下一步第 %d/%d 阶段 · 已完成 %d", i+1, total, done)
		}
	}
	return fmt.Sprintf("已完成 %d/%d 阶段", done, total)
}

func goalCurrentStageLabel(goal backend.Goal) string {
	_, total := goalStageProgress(goal)
	if i, stage, ok := goalCurrentStage(goal); ok {
		return fmt.Sprintf("当前第 %d / 共 %d 阶段：%s", i+1, total, stage.Title)
	}
	for i, stage := range goal.Stages {
		if stage.Status == "pending" {
			return fmt.Sprintf("下一阶段：第 %d / 共 %d 阶段：%s", i+1, total, stage.Title)
		}
	}
	return ""
}

func goalStageStatusLabel(stage backend.GoalStage) string {
	if stage.ReviewRequired {
		return "需要复核"
	}
	switch stage.Status {
	case "completed":
		return "已完成"
	case "active":
		return "进行中"
	case "pending":
		return "待开始"
	default:
		return stage.Status
	}
}

func goalStageStatusMarker(stage backend.GoalStage) string {
	if stage.ReviewRequired {
		return "!"
	}
	switch stage.Status {
	case "completed":
		return "✓"
	case "active":
		return "●"
	default:
		return "○"
	}
}

func boundedGoalText(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	runes := []rune(text)
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return text
}

func goalPanelVisible(goal backend.Goal, st *chatUI) bool {
	return goal.ID != "" && (goal.Status != backend.GoalStatusCompleted || st.GoalPanelVisible)
}

func goalSubagentCallCount(t *conversation) int {
	count := 0
	if t == nil {
		return count
	}
	for _, message := range t.Messages {
		if str(message, "role") == "tool_call" && toolKind(str(message, "name")) == "subagent" {
			count++
		}
	}
	return count
}

func (a *desktop) goalPanel(c *ui.Context, t *conversation) {
	goal, ok := a.goals[t.ID]
	if !ok {
		return
	}
	st := t.uiState()
	if !goalPanelVisible(goal, st) {
		return
	}
	panelOpen := st.GoalPanelOpen
	detailsOpen := st.GoalDetailsOpen
	planOpen := st.GoalPlanOpen
	resultsOpen := st.GoalResultsOpen
	ui.Column(c).Key("goal-"+t.ID).FillWidth().Padding(6, 8).Gap(gapTight).Border(1, c.Theme().Border).Radius(radiusCard).Background(c.Theme().Surface).Children(func() {
		summaryPanel := ui.CollapsibleBase(c, &panelOpen)
		summaryPanel.Trigger.FillWidth().Padding(4, 2).Cursor(ui.CursorPointer).Children(func() {
			ui.Column(c).FillWidth().Gap(gapTight).Children(func() {
				ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(gapField).Children(func() {
					ui.Text(c, "目标 · "+goal.Title).FontSize(fsSmall).Bold().Grow(1).MinWidth(0)
					ui.Text(c, goalStatusLabel(goal.Status)).FontSize(fsTiny).TextColor(a.secondaryInk())
					iconGlyph(c, "chevrondown", sizeCaretBlock).Rotate(-90 * (1 - summaryPanel.Progress()))
				})
				if _, total := goalStageProgress(goal); total > 0 {
					ui.Text(c, goalStageProgressLabel(goal)).FontSize(fsSmall).Bold().TextColor(a.secondaryInk())
					done, _ := goalStageProgress(goal)
					bar := ui.Column(c).Key("goal-progress-" + t.ID).FillWidth().MinWidth(0)
					animated := bar.Animate("stage-progress", float32(done)/float32(total), uikit.ProgressDuration)
					bar.Children(func() { ui.Progress(c, float64(animated)).FillWidth() })
				} else if goal.Status == backend.GoalStatusActive || goal.Status == backend.GoalStatusPending {
					message := "总阶段数尚未确定"
					if goal.PreflightRequired && !goal.PreflightReady {
						message = "等待前置计划确认；阶段总数尚未确定"
					} else if goal.PreflightReady {
						message = "计划已确认，阶段列表尚未同步"
					}
					ui.Text(c, message).FontSize(fsTiny).TextColor(a.secondaryInk())
				}
			})
		})
		summaryPanel.Panel(func() {
			ui.Column(c).FillWidth().Gap(gapTight).Children(func() {
				if _, stage, ok := goalCurrentStage(goal); ok {
					ui.Column(c).Key("goal-current-stage-" + t.ID).FillWidth().Gap(gapTight).Children(func() {
						ui.Text(c, goalCurrentStageLabel(goal)).FontSize(fsSmall).Bold().TextColor(c.Theme().Accent)
						if stage.Objective != "" {
							ui.Text(c, "本阶段目标："+boundedGoalText(stage.Objective, 150)).FontSize(fsTiny).TextColor(a.secondaryInk())
						}
					})
				}
				if summary := goalProgressSummary(goal); summary != "" {
					ui.Text(c, summary).FontSize(fsSmall).TextColor(c.Theme().Warning)
				}
				if activity := goalDisplayedActivity(st, t); activity != "" {
					ui.Text(c, activity).FontSize(fsSmall).TextColor(c.Theme().Accent)
				}
				if goal.ClarificationRequired && !goal.ClarificationAnswered && !goal.PreflightReady {
					ui.Text(c, "等待你补充关键决策；答复后将继续执行计划。").FontSize(fsTiny).TextColor(c.Theme().Warning)
				}

				if len(goal.Stages) > 0 {
					plan := ui.CollapsibleBase(c, &planOpen)
					plan.Trigger.FillWidth().Height(28).Padding(0, 4).Cursor(ui.CursorPointer).Children(func() {
						ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(gapField).Children(func() {
							ui.Text(c, fmt.Sprintf("查看阶段计划 · %d 个阶段", len(goal.Stages))).FontSize(fsTiny).Bold()
							ui.Spacer(c)
							iconGlyph(c, "chevrondown", sizeCaretBlock).Rotate(-90 * (1 - plan.Progress()))
						})
					})
					plan.Panel(func() {
						ui.Scroll(c.Key("goal-stage-plan-" + t.ID)).MaxHeight(250).Gap(gapTight).Children(func() {
							for i, stage := range goal.Stages {
								marker, status, markerColor := goalStageStatusMarker(stage), goalStageStatusLabel(stage), a.secondaryInk()
								if stage.Status == "completed" {
									markerColor = c.Theme().Success
								} else if stage.Status == "active" {
									markerColor = c.Theme().Accent
								} else if stage.ReviewRequired {
									markerColor = c.Theme().Warning
								}
								ui.Row(c).Key(fmt.Sprintf("goal-stage-%s-%d", t.ID, i)).FillWidth().AlignItems(ui.Start).Gap(gapInline).Padding(6, 8).Border(1, c.Theme().Border).Radius(radiusSmall).Children(func() {
									ui.Text(c, marker).FontSize(fsSmall).Bold().TextColor(markerColor).Width(22)
									ui.Column(c).Grow(1).MinWidth(0).Gap(gapTight).Children(func() {
										ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(gapInline).Children(func() {
											ui.Text(c, fmt.Sprintf("%d. %s", i+1, stage.Title)).FontSize(fsSmall).Bold()
											ui.Spacer(c)
											ui.Text(c, status).FontSize(fsTiny).TextColor(markerColor)
										})
										if stage.Objective != "" {
											ui.Text(c, boundedGoalText(stage.Objective, 180)).FontSize(fsTiny).TextColor(a.secondaryInk())
										}
									})
								})
							}
						})
					})
				}
				if completed := goalCompletedStageCount(goal); completed > 0 {
					results := ui.CollapsibleBase(c, &resultsOpen)
					results.Trigger.FillWidth().Height(28).Padding(0, 4).Cursor(ui.CursorPointer).Children(func() {
						ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(gapField).Children(func() {
							ui.Text(c, fmt.Sprintf("已完成任务与结论 · %d 步", completed)).FontSize(fsTiny).Bold().TextColor(a.secondaryInk())
							ui.Spacer(c)
							iconGlyph(c, "chevrondown", sizeCaretBlock).Rotate(-90 * (1 - results.Progress()))
						})
					})
					results.Panel(func() {
						ui.Scroll(c.Key("goal-results-" + t.ID)).MaxHeight(180).Gap(gapTight).Children(func() {
							for i, stage := range goal.Stages {
								if stage.Status != "completed" {
									continue
								}
								ui.Column(c).Key(fmt.Sprintf("goal-result-%s-%d", t.ID, i)).FillWidth().Padding(6, 8).Gap(gapTight).Border(1, c.Theme().Border).Radius(radiusSmall).Children(func() {
									ui.Text(c, fmt.Sprintf("第 %d 步 · %s", i+1, stage.Title)).FontSize(fsTiny).Bold().TextColor(c.Theme().Success)
									if stage.Evidence != "" {
										ui.Text(c, "结论："+boundedGoalText(stage.Evidence, 260)).FontSize(fsTiny).TextColor(a.secondaryInk())
									}
									if len(stage.Verifications) > 0 {
										for _, check := range stage.Verifications {
											label := "未通过"
											if check.Passed {
												label = "通过"
											}
											ui.Text(c, "验收："+check.Name+" · "+label).FontSize(fsTiny).TextColor(a.secondaryInk())
										}
									} else {
										ui.Text(c, "阶段检查已通过").FontSize(fsTiny).TextColor(a.secondaryInk())
									}
								})
							}
						})
					})
				}
				parts := ui.CollapsibleBase(c, &detailsOpen)
				parts.Trigger.FillWidth().Height(28).Padding(0, 4).Cursor(ui.CursorPointer).Children(func() {
					ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(gapField).Children(func() {
						ui.Text(c, "验收与运行记录").FontSize(fsTiny).TextColor(a.secondaryInk())
						ui.Spacer(c)
						iconGlyph(c, "chevrondown", sizeCaretBlock).Rotate(-90 * (1 - parts.Progress()))
					})
				})
				parts.Panel(func() {
					ui.Scroll(c.Key("goal-details-scroll-" + t.ID)).MaxHeight(220).Gap(gapInline).Children(func() {
						if goal.PreflightSummary != "" {
							ui.Text(c, "前置理解与计划摘要").FontSize(fsTiny).Bold().TextColor(a.secondaryInk())
							ui.Text(c, goal.PreflightSummary).FontSize(fsTiny).TextColor(a.secondaryInk())
						}
						if goal.Progress != "" {
							ui.Text(c, "Agent 最近汇报（非实时，阶段状态与工具动作见面板上方）").FontSize(fsTiny).Bold().TextColor(a.secondaryInk())
							ui.Text(c, boundedGoalText(goal.Progress, 600)).FontSize(fsTiny).TextColor(a.secondaryInk())
						}
						if goal.PreflightRequired && !goal.PreflightReady && goal.ClarificationRequired && !goal.ClarificationAnswered {
							ui.Text(c, "目标仍被澄清问题阻塞；只有当前目标对应的 ask 得到答复后才能继续。").FontSize(fsTiny).TextColor(a.secondaryInk())
						}
						if calls := goalSubagentCallCount(t); calls == 0 {
							ui.Text(c, "任务子代理：本次未调用，代码实现由主 Agent 完成").FontSize(fsTiny).TextColor(a.secondaryInk())
						} else {
							ui.Text(c, fmt.Sprintf("任务子代理调用：%d 次，详见聊天记录中的委派卡片", calls)).FontSize(fsTiny).TextColor(a.secondaryInk())
						}
						if goal.CompletionReview != nil {
							label := "未通过"
							if goal.CompletionReview.Verdict == "approved" {
								label = "通过"
							}
							ui.Text(c, "独立验收子代理复核（同模型·独立上下文）："+label+" · "+goal.CompletionReview.Reason).FontSize(fsTiny).TextColor(a.secondaryInk())
						}
						if len(goal.Stages) > 0 {
							ui.Text(c, "阶段检查点").FontSize(fsTiny).Bold().TextColor(a.secondaryInk())
							for i, stage := range goal.Stages {
								label := map[string]string{"pending": "待开始", "active": "进行中", "completed": "阶段检查通过"}[stage.Status]
								if stage.ReviewRequired {
									label = "需要复核（副作用已阻止）"
								}
								ui.Text(c, fmt.Sprintf("%d. [%s] %s", i+1, label, stage.Title)).FontSize(fsTiny).TextColor(a.secondaryInk())
								if stage.Objective != "" {
									ui.Text(c, "   阶段目标："+stage.Objective).FontSize(fsTiny).TextColor(a.secondaryInk())
								}
								if stage.Attempts > 0 {
									ui.Text(c, fmt.Sprintf("   验收尝试：%d · 连续失败：%d/%d", stage.Attempts, stage.FailuresSinceReview, backend.GoalStageReviewFailureThreshold)).FontSize(fsTiny).TextColor(a.secondaryInk())
								}
								if stage.Evidence != "" {
									ui.Text(c, "   Agent 阶段说明（待核验）："+stage.Evidence).FontSize(fsTiny).TextColor(a.secondaryInk())
								}
								for _, check := range stage.Verifications {
									checkLabel := "失败"
									if check.Passed {
										checkLabel = "通过"
									}
									ui.Text(c, fmt.Sprintf("   检查 %s：%s（退出码 %d）", check.Name, checkLabel, check.ExitCode)).FontSize(fsTiny).TextColor(a.secondaryInk())
								}
								for _, revision := range stage.ReviewHistory {
									ui.Text(c, fmt.Sprintf("   方案复核：%s · %s", revision.Decision, revision.Rationale)).FontSize(fsTiny).TextColor(a.secondaryInk())
								}
							}
						}
						if goal.Model != nil {
							ui.Text(c, "目标固定模型："+modelLabel(goal.Model.ProviderName, goal.Model.Model)).FontSize(fsTiny).TextColor(a.secondaryInk())
						}
						if goal.DeliverySummary != "" {
							ui.Text(c, "交付摘要").FontSize(fsTiny).Bold().TextColor(a.secondaryInk())
							ui.Text(c, goal.DeliverySummary).FontSize(fsTiny).TextColor(a.secondaryInk())
						}
						if len(goal.Deliverables) > 0 {
							ui.Text(c, "已检查的交付文件").FontSize(fsTiny).Bold().TextColor(a.secondaryInk())
							for _, deliverable := range goal.Deliverables {
								hash := deliverable.SHA256
								if len(hash) > 12 {
									hash = hash[:12]
								}
								ui.Text(c, fmt.Sprintf("%s · %d B · SHA-256 %s…", deliverable.Path, deliverable.Size, hash)).FontSize(fsTiny).TextColor(a.secondaryInk())
							}
						}
						if len(goal.Verifications) > 0 {
							ui.Text(c, "自动验收命令结果").FontSize(fsTiny).Bold().TextColor(a.secondaryInk())
							for _, result := range goal.Verifications {
								label := "失败"
								if result.Passed {
									label = "通过（退出码 0）"
								}
								ui.Text(c, fmt.Sprintf("%s：%s", result.Name, label)).FontSize(fsTiny).TextColor(a.secondaryInk())
							}
						}
						if len(goal.Acceptance) > 0 {
							ui.Text(c, "验收条件").FontSize(fsTiny).Bold().TextColor(a.secondaryInk())
							for i, criterion := range goal.Acceptance {
								ui.Text(c, fmt.Sprintf("%d. %s", i+1, criterion)).FontSize(fsTiny).TextColor(a.secondaryInk())
							}
						}
						if goal.StopReason != "" {
							ui.Text(c, "停止原因："+goal.StopReason).FontSize(fsTiny).TextColor(a.secondaryInk())
						}
						if goal.Status == backend.GoalStatusNeedsUser {
							ui.Text(c, "在聊天输入框补充信息并发送，即可继续目标。 ").FontSize(fsTiny).TextColor(a.secondaryInk())
						}
						if goal.Status == backend.GoalStatusCompleted {
							ui.Text(c, "阶段证据仍由 Agent 报告；自动命令通过只代表退出码为 0，请核验测试覆盖和实际产物。 ").FontSize(fsTiny).TextColor(a.secondaryInk())
						}
						if goal.Status == backend.GoalStatusPaused {
							primary(c, "继续目标").OnClick(func() { a.continueGoal(goal) })
						}
						if goal.Status != backend.GoalStatusActive && goal.Status != backend.GoalStatusPending {
							flat(c, "删除目标记录").OnClick(func() { a.deleteGoalRecord(goal) })
						}
					})
				})
			})
		})
		if panelOpen != st.GoalPanelOpen {
			st.GoalPanelOpen = panelOpen
		}
		if detailsOpen != st.GoalDetailsOpen {
			st.GoalDetailsOpen = detailsOpen
		}
		if planOpen != st.GoalPlanOpen {
			st.GoalPlanOpen = planOpen
		}
		if resultsOpen != st.GoalResultsOpen {
			st.GoalResultsOpen = resultsOpen
		}
	})
}
