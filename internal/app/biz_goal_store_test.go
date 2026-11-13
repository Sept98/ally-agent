// SPDX-License-Identifier: GPL-3.0-only
package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestGoalStoreReloadAndCRUD(t *testing.T) {
	path := filepath.Join(t.TempDir(), "goals.json")
	s, err := NewGoalStore(path)
	if err != nil {
		t.Fatal(err)
	}
	g := Goal{ID: "g1", SessionID: "s1", Workspace: t.TempDir(), Title: "Build feature", Details: "scope", Status: GoalStatusPending}
	if err := s.Create(g); err != nil {
		t.Fatal(err)
	}
	if err := s.Update("g1", func(g *Goal) error { g.Status = GoalStatusActive; return nil }); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewGoalStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reloaded.Get("g1")
	if !ok || got.Status != GoalStatusPaused || got.Title != g.Title || got.ActiveRunID != "" {
		t.Fatalf("unexpected safe recovery after restart: %+v %v", got, ok)
	}
	if err := reloaded.Remove("g1"); err != nil {
		t.Fatal(err)
	}
	if len(reloaded.List()) != 0 {
		t.Fatal("remove did not take effect")
	}
}

func TestGoalStoreFailedWriteDoesNotPublish(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "goals.json")
	s, err := NewGoalStore(path)
	if err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	s.path = filepath.Join(blocker, "goals.json")
	if err := s.Create(Goal{ID: "g", SessionID: "s", Workspace: dir, Title: "title", Status: GoalStatusPending}); err == nil {
		t.Fatal("expected persistence failure")
	}
	if _, ok := s.Get("g"); ok || len(s.List()) != 0 {
		t.Fatal("failed create published in-memory state")
	}
}

func TestGoalStoreValidation(t *testing.T) {
	s, err := NewGoalStore(filepath.Join(t.TempDir(), "goals.json"))
	if err != nil {
		t.Fatal(err)
	}
	base := Goal{ID: "x", SessionID: "s", Workspace: t.TempDir(), Title: "title", Status: GoalStatusPending}
	cases := []Goal{
		func() Goal { g := base; g.Status = "unknown"; return g }(),
		func() Goal { g := base; g.Title = ""; return g }(),
		func() Goal { g := base; g.Title = string(make([]byte, maxGoalTitleRunes+1)); return g }(),
		func() Goal { g := base; g.Details = string(make([]byte, maxGoalDetailsRunes+1)); return g }(),
		func() Goal {
			g := base
			g.Acceptance = []string{strings.Repeat("x", maxGoalAcceptanceRunes+1)}
			return g
		}(),
	}
	for _, g := range cases {
		if err := s.Create(g); err == nil {
			t.Fatalf("accepted invalid goal: %+v", g)
		}
	}
	if !GoalStatusCompleted.Valid() || GoalStatus("bad").Valid() {
		t.Fatal("status validation incorrect")
	}
}

func TestGoalStorePersistsReviewVerificationAndDeliverableMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "goals.json")
	store, err := NewGoalStore(path)
	if err != nil {
		t.Fatal(err)
	}
	verifiedAt := time.Now().UTC().Truncate(time.Second)
	goal := Goal{ID: "metadata", SessionID: "session", Workspace: t.TempDir(), Title: "delivery", Acceptance: []string{"checks pass"}, Status: GoalStatusPaused, MaxRuns: 5,
		Stages: []GoalStage{{Title: "Revised check", Objective: "Check observable output", Status: "active", Attempts: 3, Revision: 1,
			ReviewHistory: []GoalStageRevision{{Decision: "revise", OldTitle: "Old check", OldObjective: "Old objective", NewTitle: "Revised check", NewObjective: "Check observable output", Rationale: "Clarified expected output", At: verifiedAt}},
			Verifications: []GoalVerification{{Name: "stage tests", ExitCode: 0, Passed: true, VerifiedAt: verifiedAt}}}},
		Verifications: []GoalVerification{{Name: "integration", ExitCode: 0, Passed: true, VerifiedAt: verifiedAt}},
		Deliverables:  []GoalDeliverable{{Path: "artifact.txt", Size: 12, SHA256: strings.Repeat("a", 64), VerifiedAt: verifiedAt}}, DeliverySummary: "Artifact verified."}
	if err := store.Create(goal); err != nil {
		t.Fatal(err)
	}
	got, ok := store.Get(goal.ID)
	if !ok || got.Stages[0].ReviewHistory[0].Decision != "revise" || !got.Stages[0].Verifications[0].Passed || got.Verifications[0].Name != "integration" || got.Deliverables[0].SHA256 != strings.Repeat("a", 64) || got.DeliverySummary != "Artifact verified." {
		t.Fatalf("metadata was not returned: %+v, %v", got, ok)
	}
	got.Stages[0].ReviewHistory[0].Rationale = "mutated"
	got.Stages[0].Verifications[0].Name = "mutated"
	got.Verifications[0].Name = "mutated"
	got.Deliverables[0].Path = "mutated"
	listed := store.List()
	listed[0].Stages[0].ReviewHistory[0].Decision = "mutated"
	persisted, ok := store.Get(goal.ID)
	if !ok || persisted.Stages[0].ReviewHistory[0].Rationale != "Clarified expected output" || persisted.Stages[0].Verifications[0].Name != "stage tests" || persisted.Verifications[0].Name != "integration" || persisted.Deliverables[0].Path != "artifact.txt" {
		t.Fatalf("returned metadata aliases stored state: %+v", persisted)
	}
	reloaded, err := NewGoalStore(path)
	if err != nil {
		t.Fatal(err)
	}
	disk, ok := reloaded.Get(goal.ID)
	if !ok || disk.Stages[0].ReviewHistory[0].NewObjective != "Check observable output" || !disk.Stages[0].Verifications[0].Passed || !disk.Verifications[0].Passed || disk.Deliverables[0].Size != 12 || disk.Deliverables[0].SHA256 != strings.Repeat("a", 64) || disk.DeliverySummary != "Artifact verified." {
		t.Fatalf("metadata did not survive JSON reload: %+v, %v", disk, ok)
	}
}

func TestGoalStoreAcceptsLegacyGoalWithoutNewMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "goals.json")
	legacy := `[{"id":"legacy","session_id":"s","workspace":"/workspace","title":"old goal","status":"paused","max_runs":5}]`
	if err := os.WriteFile(path, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := NewGoalStore(path)
	if err != nil {
		t.Fatalf("legacy goal without stage/review/delivery fields rejected: %v", err)
	}
	got, ok := store.Get("legacy")
	if !ok || got.Status != GoalStatusPaused || len(got.Stages) != 0 || len(got.Deliverables) != 0 {
		t.Fatalf("unexpected legacy goal: %+v, %v", got, ok)
	}
}

func TestGoalStoreConcurrentUpdatesAndDuplicateID(t *testing.T) {
	s, err := NewGoalStore(filepath.Join(t.TempDir(), "goals.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Create(Goal{ID: "same", SessionID: "s", Workspace: t.TempDir(), Title: "base", Status: GoalStatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(Goal{ID: "same", SessionID: "s", Workspace: t.TempDir(), Title: "duplicate", Status: GoalStatusActive}); !errors.Is(err, ErrGoalExists) {
		t.Fatalf("expected duplicate error, got %v", err)
	}
	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- s.Update("same", func(g *Goal) error { g.Details += fmt.Sprintf("%d,", i); return nil })
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	got, _ := s.Get("same")
	if len(got.Details) == 0 {
		t.Fatal("concurrent updates missing")
	}
	reloaded, err := NewGoalStore(s.path)
	if err != nil {
		t.Fatal(err)
	}
	disk, _ := reloaded.Get("same")
	if disk.Details != got.Details {
		t.Fatal("memory and disk diverged")
	}
}
