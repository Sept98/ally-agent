// SPDX-License-Identifier: GPL-3.0-only
package app

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// planEventRecorder records the plan:update events a restore publishes.
type planEventRecorder struct {
	mu     sync.Mutex
	events []string
}

func (r *planEventRecorder) Emit(name string, payload any) {
	if name != "plan:update" {
		return
	}
	encoded, _ := json.Marshal(payload)
	r.mu.Lock()
	r.events = append(r.events, string(encoded))
	r.mu.Unlock()
}

func (r *planEventRecorder) planUpdates() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

// TestSessionSnapshotRestoresItsPlanAfterReopen pins the long-running plan
// contract: a plan written in one app lifetime is exactly where the work left it
// when the session is opened in the next one, and the restored position is still
// a valid anchor for the plan tool's report.
func TestSessionSnapshotRestoresItsPlanAfterReopen(t *testing.T) {
	t.Parallel()
	a := newIsolatedTestApp(t)
	if err := a.ensureInitialized(); err != nil {
		t.Fatal(err)
	}
	const id = "plan-survives-reopen"
	steps := []string{"Inspect the entry point", "Change the behavior", "Verify the change"}
	if _, err := a.handlePlan(id, PlanRequest{Steps: &steps}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.handlePlan(id, PlanRequest{Finish: &PlanFinish{Title: "Change the behavior"}}); err != nil {
		t.Fatal(err)
	}
	revision := a.planRevisionFor(id)
	if err := a.SaveSession(SessionSnapshot{
		ID:        id,
		Title:     "long plan",
		Workspace: a.config.Workspace,
		Messages:  []map[string]any{{"role": "user", "content": "work the plan"}},
	}); err != nil {
		t.Fatal(err)
	}

	reopened := newAppState(a.AppDataDir())
	recorder := &planEventRecorder{}
	reopened.events = recorder
	snapshot, err := reopened.LoadSession(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Plan) != len(steps) || snapshot.Plan[2].Status != "in_progress" || snapshot.PlanRevision != revision {
		t.Fatalf("snapshot did not carry the plan position: %#v (revision %d)", snapshot.Plan, snapshot.PlanRevision)
	}
	got := reopened.GetPlan(id)
	if len(got) != len(steps) || got[0].Status != "done" || got[1].Status != "done" || got[2].Title != steps[2] {
		t.Fatalf("reopened plan = %#v", got)
	}
	if reopened.planRevisionFor(id) != revision {
		t.Fatalf("reopened revision = %d, want %d", reopened.planRevisionFor(id), revision)
	}
	// The panel learns plans from plan:update alone, so the restore publishes the
	// saved list through the same event rather than leaving a load-only path.
	updates := recorder.planUpdates()
	if len(updates) != 1 || !strings.Contains(updates[0], steps[2]) || !strings.Contains(updates[0], fmt.Sprintf(`"revision":%d`, revision)) {
		t.Fatalf("restore did not publish the plan: %v", updates)
	}
	// Opening the session again must not republish a plan that is already live.
	if _, err := reopened.LoadSession(id); err != nil {
		t.Fatal(err)
	}
	if got := len(recorder.planUpdates()); got != 1 {
		t.Fatalf("a second load republished the plan (%d events)", got)
	}
	// The restored position must still be a valid anchor for a report: the model
	// names a title from history and the plan moves on from there.
	if _, err := reopened.handlePlan(id, PlanRequest{Finish: &PlanFinish{Title: steps[2]}}); err != nil {
		t.Fatalf("a report against the restored plan failed: %v", err)
	}
	if got := reopened.GetPlan(id); len(got) != len(steps) || got[2].Status != "done" {
		t.Fatalf("the restored plan did not move on: %#v", got)
	}
}

// TestSessionSnapshotRestoresAClearedPlan: clearing a plan is a write that must
// survive a reopen too. The empty list carries no steps, so the saved revision
// is what tells a cleared plan from one that never existed.
func TestSessionSnapshotRestoresAClearedPlan(t *testing.T) {
	t.Parallel()
	a := newIsolatedTestApp(t)
	if err := a.ensureInitialized(); err != nil {
		t.Fatal(err)
	}
	const id = "plan-cleared-before-reopen"
	steps := []string{"Only step"}
	if _, err := a.handlePlan(id, PlanRequest{Steps: &steps}); err != nil {
		t.Fatal(err)
	}
	empty := []string{}
	if _, err := a.handlePlan(id, PlanRequest{Steps: &empty}); err != nil {
		t.Fatal(err)
	}
	revision := a.planRevisionFor(id)
	if revision == 0 {
		t.Fatal("clearing a plan must bump the revision")
	}
	if err := a.SaveSession(SessionSnapshot{
		ID:        id,
		Title:     "cleared plan",
		Workspace: a.config.Workspace,
		Messages:  []map[string]any{{"role": "user", "content": "drop the plan"}},
	}); err != nil {
		t.Fatal(err)
	}

	reopened := newAppState(a.AppDataDir())
	snapshot, err := reopened.LoadSession(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Plan) != 0 || snapshot.PlanRevision != revision {
		t.Fatalf("snapshot lost the cleared plan: %#v (revision %d)", snapshot.Plan, snapshot.PlanRevision)
	}
	if got := reopened.GetPlan(id); len(got) != 0 {
		t.Fatalf("a cleared plan reopened as %#v", got)
	}
	if reopened.planRevisionFor(id) != revision {
		t.Fatalf("cleared plan revision = %d, want %d", reopened.planRevisionFor(id), revision)
	}
}

// TestRestorePlanFromSnapshotKeepsALivePlan: loading a session must never roll a
// plan back under a run that is still writing it.
func TestRestorePlanFromSnapshotKeepsALivePlan(t *testing.T) {
	t.Parallel()
	a := newModelTestApp(t)
	steps := []string{"Live step"}
	if _, err := a.handlePlan("session-1", PlanRequest{Steps: &steps}); err != nil {
		t.Fatal(err)
	}
	liveRevision := a.planRevisionFor("session-1")
	a.restorePlanFromSnapshot("session-1", []PlanStep{{Title: "Saved step", Status: "done"}}, liveRevision+7)
	if got := a.GetPlan("session-1"); len(got) != 1 || got[0].Title != "Live step" || got[0].Status != "in_progress" {
		t.Fatalf("restore overwrote a live plan: %#v", got)
	}
	if a.planRevisionFor("session-1") != liveRevision {
		t.Fatalf("restore moved a live revision: %d, want %d", a.planRevisionFor("session-1"), liveRevision)
	}
}

// TestSessionSnapshotKeepsAPlanLongerThanTheHistoryWindow: one model-facing
// plan block carries at most planModelRowLimit rows, so a panel rebuilt from
// tool text would reopen a long plan truncated. The saved snapshot keeps the
// whole list.
func TestSessionSnapshotKeepsAPlanLongerThanTheHistoryWindow(t *testing.T) {
	t.Parallel()
	a := newIsolatedTestApp(t)
	if err := a.ensureInitialized(); err != nil {
		t.Fatal(err)
	}
	const id = "plan-longer-than-a-window"
	steps := make([]string, planModelRowLimit+10)
	for i := range steps {
		steps[i] = fmt.Sprintf("Step %02d", i+1)
	}
	if _, err := a.handlePlan(id, PlanRequest{Steps: &steps}); err != nil {
		t.Fatal(err)
	}
	if err := a.SaveSession(SessionSnapshot{
		ID:        id,
		Title:     "long plan",
		Workspace: a.config.Workspace,
		Messages:  []map[string]any{{"role": "user", "content": "work the long plan"}},
	}); err != nil {
		t.Fatal(err)
	}

	reopened := newAppState(a.AppDataDir())
	if _, err := reopened.LoadSession(id); err != nil {
		t.Fatal(err)
	}
	got := reopened.GetPlan(id)
	if len(got) != len(steps) {
		t.Fatalf("reopened plan has %d steps, want %d", len(got), len(steps))
	}
	for i, step := range got {
		if step.Title != steps[i] {
			t.Fatalf("step %d = %q, want %q", i+1, step.Title, steps[i])
		}
	}
}

// TestSaveSessionKeepsACallerPlanWhenTheBackendHasNone: a headless or API caller
// may hand the backend a plan it never ran, and that copy must not be dropped by
// the snapshot write. The backend's own plan always wins when it has one.
func TestSaveSessionKeepsACallerPlanWhenTheBackendHasNone(t *testing.T) {
	t.Parallel()
	a := newIsolatedTestApp(t)
	if err := a.ensureInitialized(); err != nil {
		t.Fatal(err)
	}
	const id = "caller-supplied-plan"
	steps := []PlanStep{{Title: "Imported step", Status: "in_progress"}}
	if err := a.SaveSession(SessionSnapshot{
		ID:           id,
		Title:        "imported",
		Workspace:    a.config.Workspace,
		Messages:     []map[string]any{{"role": "user", "content": "imported session"}},
		Plan:         steps,
		PlanRevision: 5,
	}); err != nil {
		t.Fatal(err)
	}

	reopened := newAppState(a.AppDataDir())
	snapshot, err := reopened.LoadSession(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Plan) != 1 || snapshot.Plan[0].Title != "Imported step" || snapshot.PlanRevision != 5 {
		t.Fatalf("caller plan was dropped: %#v (revision %d)", snapshot.Plan, snapshot.PlanRevision)
	}
	if got := reopened.GetPlan(id); len(got) != 1 || got[0].Title != "Imported step" {
		t.Fatalf("caller plan did not restore: %#v", got)
	}
}
