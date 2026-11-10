// SPDX-License-Identifier: GPL-3.0-only
package desktop

import (
	"encoding/json"
	"fmt"
	"strings"

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
		return "Agent 报告完成 · 请核验"
	case backend.GoalStatusFailed:
		return "启动失败"
	default:
		return string(status)
	}
}

func (a *desktop) goalPanel(c *ui.Context, t *conversation) {
	goal, ok := a.goals[t.ID]
	if !ok {
		return
	}
	ui.Column(c).Key("goal-"+t.ID).FillWidth().Padding(10, 12).Gap(gapInline).Border(1, c.Theme().Border).Radius(radiusCard).Background(c.Theme().Surface).Children(func() {
		ui.Row(c).AlignItems(ui.Center).Gap(gapField).Children(func() {
			ui.Text(c, "目标 · "+goal.Title).FontSize(fsSmall).Bold()
			ui.Spacer(c)
			ui.Text(c, goalStatusLabel(goal.Status)).FontSize(fsTiny).TextColor(a.secondaryInk())
		})
		if goal.Progress != "" {
			ui.Text(c, goal.Progress).FontSize(fsSmall).TextColor(a.secondaryInk())
		}
		if goal.Model != nil {
			ui.Text(c, "目标固定模型："+modelLabel(goal.Model.ProviderName, goal.Model.Model)).FontSize(fsTiny).TextColor(a.secondaryInk())
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
			ui.Text(c, "完成状态由 Agent 报告；请依据实际测试和产物核验。 ").FontSize(fsTiny).TextColor(a.secondaryInk())
		}
		if goal.Status == backend.GoalStatusPaused {
			primary(c, "继续目标").OnClick(func() { a.continueGoal(goal) })
		}
		if goal.Status != backend.GoalStatusActive && goal.Status != backend.GoalStatusPending {
			flat(c, "移除记录").OnClick(func() { a.deleteGoalRecord(goal) })
		}
	})
}
