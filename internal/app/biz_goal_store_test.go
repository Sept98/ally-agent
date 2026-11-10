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
