# Goal mode

## Purpose

`/goal <objective>` starts a goal in a fresh conversation session. Goal state is persisted separately from the transcript; ordinary chat and scheduled tasks retain their existing lifecycle. The goal panel is the UI projection of backend state, not the source of truth.

## Execution contract

1. The first model request receives the user's objective plus a host-provided goal instruction. Before side effects, the agent must establish observable acceptance criteria with `goal_update`; the backend rejects side-effecting tools until criteria are persisted. When a key criterion is ambiguous it should use `ask` and wait for the user's answer.
2. `goal_update` records progress and transitions the goal to `needs_user` or `completed`. Acceptance criteria become fixed once established. Every `goal_update` must be the only tool call in its batch so acceptance persistence cannot race with side effects. A terminal goal state blocks later tool calls in that run.
3. A normal model final answer does not complete a goal. If the goal remains active, the existing `runChat` loop gets a host-only continuation message, up to three times in one run. A goal run is also capped at 200 Agent steps; the persisted goal allows up to 20 user-started/resumed runs.
4. Reaching a run budget, cancellation, provider error, or ordinary run stop pauses an unfinished goal. Sending a follow-up in the goal's session resumes paused/needs-user goals when the run budget permits; the panel button resumes a paused goal explicitly.
5. On application restart, any persisted pending/active goal is changed to paused and is never replayed automatically. This avoids blindly repeating a tool that may already have caused an external side effect.

## Persistence and boundaries

- `internal/app/biz_goal_store.go` owns bounded, atomic JSON persistence (`goals.json` under the app data directory), validation, revisions, and safe in-flight recovery.
- `internal/app/biz_goal.go` owns goal state transitions and the small lifecycle API.
- `runChat` only supplies the goal-specific request markers, bounded continuation, and run-end transition; tool execution remains in the shared tool executor.
- `goal_update` is injected only into an active goal session. Stateless callers and child agents do not inherit this goal-control tool.
- The UI stores goal state from `goal:update` events and loads persisted records at startup. It never performs goal disk I/O during rendering.
- Credentials are not stored with a goal. The goal records its workspace and model identity (provider/model/OAuth account); resuming fails visibly if that identity is no longer configured instead of silently switching models. The user may continue a paused goal from its original session.

## Verification meaning

`completed` means the Agent reported that its listed acceptance criteria were met; it is **not** a cryptographic or independent proof. The panel labels this as an Agent report and asks the user to inspect the actual tests/artifacts. Goal mode improves continuity and makes stopping conditions explicit, but cannot guarantee correctness for every model, external service, or acceptance condition. The caller must verify the reported command outputs, tests, and deliverables before treating the task as accepted.
