// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 tangssst <tangssst@qq.com>
//
// 会话删除是不可恢复操作（前端快照 + 后端模型上下文一起删除）。这条测试直接执行
// App.vue 里的实际代码，钉住“先确认、按 id 解析、确认后复查运行态”三条行为：
//   1. deleteSession 只弹确认，不立刻删除；
//   2. 确认回调按 id 找到目标（确认框打开期间列表被后台事件刷新也不会删错）；
//   3. 确认期间会话开跑（或已被别处删掉）时不删、不报错。
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const source = readFileSync(new URL('../App.vue', import.meta.url), 'utf8');
const start = source.indexOf('// 会话删除会连同全部对话与模型上下文一起从磁盘移除');
const end = source.indexOf('\nfunction addWelcome', start);
assert.ok(start >= 0 && end > start, 'App.vue 里找不到会话删除这一段（改名了？）');

function harness({ sessions = [], activeId = '' } = {}) {
  const state = {
    sessions: { value: sessions },
    activeSessionId: { value: activeId },
    activeWorkspaceId: { value: 'tab-1' },
    workspaceTabs: { value: [] },
    promptText: { value: '' },
    expandedArchiveSessions: { value: new Set() },
    sessionsSelectedIndex: { value: 0 },
    sessionPromptTexts: {},
    plansBySession: {},
    planRevisionsBySession: {},
    planPanelCollapsedBySession: {},
    displayMessagesCacheBySession: new Map(),
  };
  const calls = { dialogs: [], warnings: [], deleted: [], saves: 0 };
  const busySessions = new Set();
  const bindings = {
    sessions: state.sessions,
    activeSessionId: state.activeSessionId,
    activeWorkspaceId: state.activeWorkspaceId,
    workspaceTabs: state.workspaceTabs,
    promptText: state.promptText,
    expandedArchiveSessions: state.expandedArchiveSessions,
    sessionsSelectedIndex: state.sessionsSelectedIndex,
    sessionPromptTexts: state.sessionPromptTexts,
    plansBySession: state.plansBySession,
    planRevisionsBySession: state.planRevisionsBySession,
    planPanelCollapsedBySession: state.planPanelCollapsedBySession,
    displayMessagesCacheBySession: state.displayMessagesCacheBySession,
    currentWorkspaceSessions: {
      get value() { return state.sessions.value; },
    },
    sessionBusy: (session) => busySessions.has(session?.id),
    sessionDisplayTitle: (session) => session?.title || '',
    // 只回键名和参数，断言的还是调用点传了什么，而不是某个语言的文案。
    t: (key, params) => (params ? `${key}:${JSON.stringify(params)}` : key),
    message: { warning: (text) => calls.warnings.push(text) },
    dialog: { warning: (options) => calls.dialogs.push(options) },
    releaseSessionAttachments: () => {},
    createReplacementSession: (title) => ({ id: `replacement-${title}`, messages: [] }),
    repointTabSession: () => {},
    updateWelcomeModelRows: () => {},
    applySessionWorkspace: () => {},
    loadPlan: () => {},
    scrollMessagesToBottom: () => {},
    dropContextDriftNotices: () => {},
    deletePendingAttachments: () => {},
    enqueueSessionWrite: (write) => Promise.resolve(write()),
    DeleteSession: async (id) => { calls.deleted.push(id); },
    saveSessions: () => { calls.saves += 1; },
  };
  const deleteSession = new Function(...Object.keys(bindings), `${source.slice(start, end)}
    return { deleteSession, performDeleteSession };`)(...Object.values(bindings));
  return { ...state, ...deleteSession, calls, busySessions };
}

function session(id, title = id) {
  return { id, title, workspace: 'F:/a', messages: [], updatedAt: 1 };
}

test('删除会话先弹确认，确认前不删、不落盘', () => {
  const h = harness({ sessions: [session('s1', 'First'), session('s2')], activeId: 's2' });
  h.deleteSession(0);

  assert.equal(h.calls.dialogs.length, 1);
  const confirm = h.calls.dialogs[0];
  assert.equal(confirm.title, 'app.sessions.deleteConfirmTitle');
  assert.equal(confirm.content, 'app.sessions.deleteConfirmContent:{"title":"First"}');
  assert.equal(confirm.positiveText, 'common.delete');
  assert.equal(confirm.negativeText, 'common.cancel');
  assert.equal(typeof confirm.onPositiveClick, 'function');

  // 确认之前列表原样、也没有发删除请求。
  assert.deepEqual(h.sessions.value.map((item) => item.id), ['s1', 's2']);
  assert.deepEqual(h.calls.deleted, []);
  assert.equal(h.calls.saves, 0);
});

test('确认后按 id 删除目标会话并同步后端', async () => {
  const h = harness({ sessions: [session('s1', 'First'), session('s2')], activeId: 's1' });
  h.deleteSession(0);
  await h.calls.dialogs[0].onPositiveClick();

  assert.deepEqual(h.sessions.value.map((item) => item.id), ['s2']);
  assert.deepEqual(h.calls.deleted, ['s1']);
  // 删的是活动会话：接手的是列表里剩下的那条，而不是复用被删的下标。
  assert.equal(h.activeSessionId.value, 's2');
  assert.equal(h.promptText.value, '');
  assert.equal(h.calls.saves, 1);
});

test('确认期间列表变化时按 id 解析，不会删错下标对应的会话', async () => {
  const h = harness({ sessions: [session('s1', 'First'), session('s2'), session('s3')], activeId: 's3' });
  h.deleteSession(0);
  // 确认框还开着，后台刷新把另一条会话插到了列表最前面。
  h.sessions.value.unshift(session('s0', 'Inserted'));
  await h.calls.dialogs[0].onPositiveClick();

  assert.deepEqual(h.calls.deleted, ['s1']);
  assert.deepEqual(h.sessions.value.map((item) => item.id), ['s0', 's2', 's3']);
});

test('确认期间会话已开跑或已被删除时不执行删除', async () => {
  const running = harness({ sessions: [session('s1'), session('s2')] });
  running.deleteSession(0);
  running.busySessions.add('s1');
  await running.calls.dialogs[0].onPositiveClick();
  assert.deepEqual(running.calls.deleted, []);
  assert.deepEqual(running.calls.warnings, ['app.sessions.runningDeleteBlocked']);
  assert.equal(running.sessions.value.length, 2);

  const gone = harness({ sessions: [session('s1'), session('s2')] });
  gone.deleteSession(0);
  gone.sessions.value = gone.sessions.value.filter((item) => item.id !== 's1');
  await gone.calls.dialogs[0].onPositiveClick();
  assert.deepEqual(gone.calls.deleted, []);
  assert.equal(gone.calls.warnings.length, 0);
});
