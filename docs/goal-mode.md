# Goal mode

## Purpose

`/goal <objective>` starts a goal in a fresh conversation session. Goal state is persisted separately from the transcript; ordinary chat and scheduled tasks retain their existing lifecycle. The goal panel is the UI projection of backend state, not the source of truth.

## Execution contract

1. The first model request receives the user's objective plus a host-provided goal instruction. Before side effects, the agent must persist observable acceptance criteria and at least one ordered stage with a concrete objective; the backend rejects side-effecting tools until both exist and a stage is active. When a key criterion is ambiguous it should use `ask` and wait for the user's answer.
2. `goal_update` persists an ordered plan with a specific objective per stage. A stage cannot be marked complete until its named checks actually run in the workspace command sandbox and exit 0. Results and attempt counts persist per stage; commands and outputs are not stored with the goal. Every update carries the full stage list, and the tool must be alone in its tool batch.
3. After three failed checks for one stage, Ally marks that stage review-required and blocks side-effect tools. The Agent must submit a structured review choosing `revise` (new stage title/objective, with prior plan preserved in review history), `continue` (diagnosed reason to keep the plan), or `needs_user`. This is a bounded replan loop, not an automatic change of user acceptance criteria.
4. Final completion requires all stages to have passing stage checks and evidence, at least one final integration check to exit 0, and a persisted delivery summary. Listed deliverable paths are restricted to workspace-relative files; Ally checks each exists, is not a directory, and records size plus SHA-256. Final and stage checks each run through `runCommandWithConfig`, max 120 seconds per command, no timeout promotion, at most 10 commands per `goal_update`.
5. A normal model final answer does not complete a goal. If the goal remains active, the existing `runChat` loop gets a host-only continuation message, up to three times in one run. A goal run is capped at 200 Agent steps; the persisted goal allows up to 20 user-started/resumed runs.
6. Reaching a run budget, cancellation, provider error, or ordinary run stop pauses an unfinished goal. Sending a follow-up in the goal's session resumes paused/needs-user goals when the run budget permits; the panel button resumes a paused goal explicitly.
7. On application restart, any persisted pending/active goal is changed to paused and is never replayed automatically. This avoids blindly repeating a tool that may already have caused an external side effect.

## Persistence and boundaries

- `internal/app/biz_goal_store.go` owns bounded, atomic JSON persistence (`goals.json` under the app data directory), validation, revisions, and safe in-flight recovery.
- `internal/app/biz_goal.go` owns goal state transitions, bounded command execution for stage/final checks, repeated-failure review, and deliverable hashing.
- `runChat` only supplies the goal-specific request markers, bounded continuation, and run-end transition; tool execution remains in the shared tool executor.
- `goal_update` is injected only into an active goal session. Stateless callers and child agents do not inherit this goal-control tool. Stage/final checks go through `runCommandWithConfig`, retain the original workspace boundary, disable extra roots, honor the session's frozen `command` tool availability, and never promote timed-out checks to background services. Deliverables use the workspace file inspector and store only relative path, size and SHA-256.
- The UI stores goal state from `goal:update` events and loads persisted records at startup. It never performs goal disk I/O during rendering.
- Credentials are not stored with a goal. The goal records its workspace and model identity (provider/model/OAuth account); resuming fails visibly if that identity is no longer configured instead of silently switching models. The user may continue a paused goal from its original session.

## Verification meaning

`completed` now requires Ally-run stage checks, final integration checks, and successful existence/hash inspection for every listed file deliverable. This independently proves only that specified commands exited 0 and the listed files existed with recorded hashes at check time. Stage evidence and semantic acceptance remain Agent-reported; the system cannot know whether checks are adequate or whether an external outcome is correct. The caller should inspect the actual artifacts and decide whether they satisfy the request.
