// SPDX-License-Identifier: GPL-3.0-only
package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	toolshared "ally-dev/internal/tools/shared"
)

// GoalStatus is the lifecycle state of a persisted goal.
type GoalStatus string

const (
	GoalStatusPending   GoalStatus = "pending"
	GoalStatusActive    GoalStatus = "active"
	GoalStatusPaused    GoalStatus = "paused"
	GoalStatusNeedsUser GoalStatus = "needs_user"
	GoalStatusCompleted GoalStatus = "completed"
	GoalStatusFailed    GoalStatus = "failed"
)

func (s GoalStatus) Valid() bool {
	switch s {
	case GoalStatusPending, GoalStatusActive, GoalStatusPaused, GoalStatusNeedsUser, GoalStatusCompleted, GoalStatusFailed:
		return true
	default:
		return false
	}
}

// GoalStage is a persisted checkpoint; completion requires the recorded Ally-run checks.
type GoalStage struct {
	Title               string              `json:"title"`
	Objective           string              `json:"objective,omitempty"`
	Status              string              `json:"status"`
	Evidence            string              `json:"evidence,omitempty"`
	Attempts            int                 `json:"attempts,omitempty"`
	FailuresSinceReview int                 `json:"failures_since_review,omitempty"`
	ReviewRequired      bool                `json:"review_required,omitempty"`
	Revision            int                 `json:"revision,omitempty"`
	ReviewHistory       []GoalStageRevision `json:"review_history,omitempty"`
	Verifications       []GoalVerification  `json:"verifications,omitempty"`
}

type GoalStageRevision struct {
	Decision     string    `json:"decision"`
	OldTitle     string    `json:"old_title"`
	OldObjective string    `json:"old_objective,omitempty"`
	NewTitle     string    `json:"new_title,omitempty"`
	NewObjective string    `json:"new_objective,omitempty"`
	Rationale    string    `json:"rationale"`
	At           time.Time `json:"at"`
}

type GoalDeliverable struct {
	Path       string    `json:"path"`
	Size       int64     `json:"size"`
	SHA256     string    `json:"sha256"`
	VerifiedAt time.Time `json:"verified_at"`
}

// GoalVerificationSpec is a bounded command requested for final verification.
type GoalVerificationSpec struct {
	Name    string `json:"name"`
	Command string `json:"command"`
}

// GoalVerification stores only the result metadata, not command text or output that may contain secrets.
type GoalVerification struct {
	Name       string    `json:"name"`
	ExitCode   int       `json:"exit_code"`
	Passed     bool      `json:"passed"`
	VerifiedAt time.Time `json:"verified_at"`
}

// Goal contains user-authored goal text and lifecycle metadata. Credentials must not be stored here.
type Goal struct {
	ID              string             `json:"id"`
	SessionID       string             `json:"session_id"`
	Workspace       string             `json:"workspace"`
	Title           string             `json:"title"`
	Details         string             `json:"details,omitempty"`
	Acceptance      []string           `json:"acceptance,omitempty"`
	Stages          []GoalStage        `json:"stages,omitempty"`
	Verifications   []GoalVerification `json:"verifications,omitempty"`
	Deliverables    []GoalDeliverable  `json:"deliverables,omitempty"`
	DeliverySummary string             `json:"delivery_summary,omitempty"`
	Model           *ModelIdentity     `json:"model,omitempty"`
	Progress        string             `json:"progress,omitempty"`
	Status          GoalStatus         `json:"status"`
	RunCount        int                `json:"run_count"`
	MaxRuns         int                `json:"max_runs"`
	Revision        uint64             `json:"revision"`
	ActiveRunID     string             `json:"active_run_id,omitempty"`
	StopReason      string             `json:"stop_reason,omitempty"`
	CreatedAt       time.Time          `json:"created_at"`
	UpdatedAt       time.Time          `json:"updated_at"`
}

const (
	maxGoalRecords                  = 1000
	maxGoalIDRunes                  = 128
	maxGoalTitleRunes               = 512
	maxGoalAcceptanceRunes          = toolshared.MaxGoalAcceptanceCriterionChars
	maxGoalDetailsRunes             = 16 * 1024
	maxGoalProgressRunes            = 4000
	maxGoalAcceptanceItems          = toolshared.MaxGoalAcceptanceCriteria
	maxGoalStageTitleRunes          = 200
	maxGoalStageEvidenceRunes       = 2000
	maxGoalStageItems               = 50
	maxGoalVerificationItems        = 10
	maxGoalVerificationNameRunes    = 120
	maxGoalVerificationCommandRunes = 2000
	maxGoalStageRevisionHistory     = 20
	maxGoalDeliverables             = 32
	maxGoalDeliveryPathRunes        = 4096
	maxGoalDeliverySummaryRunes     = 4000
	maxGoalStageAttempts            = 100
	GoalStageReviewFailureThreshold = 3
	goalStageReviewFailureThreshold = GoalStageReviewFailureThreshold
	maxGoalStageVerificationResults = 100
	maxGoalRunBudget                = 20
	maxGoalStoreBytes               = 32 * 1024 * 1024
)

var (
	ErrGoalNotFound = errors.New("goal not found")
	ErrGoalExists   = errors.New("goal already exists")
)

// GoalStore serializes mutations and publishes them only after atomic disk persistence.
type GoalStore struct {
	mu    sync.RWMutex
	path  string
	goals map[string]Goal
}

func NewGoalStore(path string) (*GoalStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("goal store path is empty")
	}
	s := &GoalStore{path: path, goals: make(map[string]Goal)}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Size() > maxGoalStoreBytes {
		return nil, errors.New("goal store exceeds size limit")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) > maxGoalStoreBytes {
		return nil, errors.New("goal store exceeds size limit")
	}
	var records []Goal
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, fmt.Errorf("decode goals: %w", err)
	}
	if len(records) > maxGoalRecords {
		return nil, fmt.Errorf("goal record limit exceeded")
	}
	recovered := false
	for _, g := range records {
		if err := validateGoal(g); err != nil {
			return nil, fmt.Errorf("invalid stored goal: %w", err)
		}
		if _, exists := s.goals[g.ID]; exists {
			return nil, fmt.Errorf("duplicate stored goal id %q", g.ID)
		}
		if g.Status == GoalStatusActive || g.Status == GoalStatusPending {
			g.Status = GoalStatusPaused
			g.Revision++
			g.ActiveRunID = ""
			g.StopReason = "应用上次未正常完成该目标；检查工作区和会话后可继续。"
			g.UpdatedAt = time.Now().UTC()
			recovered = true
		}
		s.goals[g.ID] = cloneGoal(g)
	}
	if recovered {
		if err := s.persist(s.goals); err != nil {
			return nil, fmt.Errorf("persist recovered goals: %w", err)
		}
	}
	return s, nil
}

func validateGoal(g Goal) error {
	if strings.TrimSpace(g.ID) == "" || len([]rune(g.ID)) > maxGoalIDRunes {
		return errors.New("goal ID is empty or too long")
	}
	if strings.TrimSpace(g.SessionID) == "" || len([]rune(g.SessionID)) > maxGoalIDRunes {
		return errors.New("goal session ID is empty or too long")
	}
	if strings.TrimSpace(g.Workspace) == "" || len([]rune(g.Workspace)) > 4096 {
		return errors.New("goal workspace is empty or too long")
	}
	if strings.TrimSpace(g.Title) == "" || len([]rune(g.Title)) > maxGoalTitleRunes {
		return errors.New("goal title is empty or too long")
	}
	if len([]rune(g.Details)) > maxGoalDetailsRunes {
		return errors.New("goal details are too long")
	}
	if g.Model != nil && strings.TrimSpace(g.Model.Model) == "" {
		return errors.New("goal model identity is invalid")
	}
	if len([]rune(g.Progress)) > maxGoalProgressRunes {
		return errors.New("goal progress is too long")
	}
	if len(g.Acceptance) > maxGoalAcceptanceItems {
		return errors.New("too many goal acceptance criteria")
	}
	for _, criterion := range g.Acceptance {
		if strings.TrimSpace(criterion) == "" || len([]rune(criterion)) > maxGoalAcceptanceRunes {
			return errors.New("goal acceptance criteria must be non-empty and bounded")
		}
	}
	if len(g.Stages) > maxGoalStageItems {
		return errors.New("too many goal stages")
	}
	activeStages := 0
	for _, stage := range g.Stages {
		if strings.TrimSpace(stage.Title) == "" || len([]rune(stage.Title)) > maxGoalStageTitleRunes || len([]rune(stage.Objective)) > maxGoalStageEvidenceRunes || len([]rune(stage.Evidence)) > maxGoalStageEvidenceRunes {
			return errors.New("goal stage title, objective or evidence is invalid")
		}
		if stage.Attempts < 0 || stage.Attempts > maxGoalStageAttempts || stage.FailuresSinceReview < 0 || stage.FailuresSinceReview > goalStageReviewFailureThreshold || stage.Revision < 0 || stage.Revision > maxGoalStageRevisionHistory {
			return errors.New("goal stage attempt or revision counters are out of range")
		}
		if stage.ReviewRequired != (stage.FailuresSinceReview >= goalStageReviewFailureThreshold) {
			return errors.New("goal stage review flag does not match its failure count")
		}
		if len(stage.ReviewHistory) > maxGoalStageRevisionHistory || len(stage.Verifications) > maxGoalStageVerificationResults {
			return errors.New("goal stage review history or verification results exceed limits")
		}
		for _, review := range stage.ReviewHistory {
			if review.Decision != "revise" && review.Decision != "continue" && review.Decision != "needs_user" || strings.TrimSpace(review.Rationale) == "" || len([]rune(review.Rationale)) > maxGoalStageEvidenceRunes || len([]rune(review.OldTitle)) > maxGoalStageTitleRunes || len([]rune(review.NewTitle)) > maxGoalStageTitleRunes || len([]rune(review.OldObjective)) > maxGoalStageEvidenceRunes || len([]rune(review.NewObjective)) > maxGoalStageEvidenceRunes {
				return errors.New("goal stage review record is invalid")
			}
		}
		for _, verification := range stage.Verifications {
			if strings.TrimSpace(verification.Name) == "" || len([]rune(verification.Name)) > maxGoalVerificationNameRunes {
				return errors.New("goal stage verification result is invalid")
			}
		}
		switch stage.Status {
		case "pending":
		case "active":
			activeStages++
		case "completed":
			if strings.TrimSpace(stage.Evidence) == "" {
				return errors.New("completed goal stages require evidence")
			}
		default:
			return fmt.Errorf("invalid goal stage status %q", stage.Status)
		}
	}
	for i := 1; i < len(g.Stages); i++ {
		if g.Stages[i].Status != "pending" && g.Stages[i-1].Status != "completed" {
			return errors.New("goal stages must proceed in order")
		}
	}
	if activeStages > 1 {
		return errors.New("only one goal stage may be active")
	}
	if len(g.Verifications) > maxGoalVerificationItems {
		return errors.New("too many goal verification results")
	}
	for _, verification := range g.Verifications {
		if strings.TrimSpace(verification.Name) == "" || len([]rune(verification.Name)) > maxGoalVerificationNameRunes {
			return errors.New("goal verification result name is invalid or too long")
		}
	}
	if len(g.Deliverables) > maxGoalDeliverables || len([]rune(g.DeliverySummary)) > maxGoalDeliverySummaryRunes {
		return errors.New("goal deliverable list or delivery summary exceeds limits")
	}
	for _, deliverable := range g.Deliverables {
		if strings.TrimSpace(deliverable.Path) == "" || len([]rune(deliverable.Path)) > maxGoalDeliveryPathRunes || deliverable.Size < 0 || len(deliverable.SHA256) != 64 {
			return errors.New("goal deliverable record is invalid")
		}
	}
	if g.MaxRuns < 1 || g.MaxRuns > maxGoalRunBudget {
		return errors.New("goal run budget is out of range")
	}
	if g.RunCount < 0 || g.RunCount > g.MaxRuns {
		return errors.New("goal run count is out of range")
	}
	if !g.Status.Valid() {
		return fmt.Errorf("invalid goal status %q", g.Status)
	}
	return nil
}

func cloneGoal(g Goal) Goal {
	g.Acceptance = append([]string(nil), g.Acceptance...)
	g.Stages = append([]GoalStage(nil), g.Stages...)
	for i := range g.Stages {
		g.Stages[i].ReviewHistory = append([]GoalStageRevision(nil), g.Stages[i].ReviewHistory...)
		g.Stages[i].Verifications = append([]GoalVerification(nil), g.Stages[i].Verifications...)
	}
	g.Verifications = append([]GoalVerification(nil), g.Verifications...)
	g.Deliverables = append([]GoalDeliverable(nil), g.Deliverables...)
	if g.Model != nil {
		model := *g.Model
		g.Model = &model
	}
	return g
}

func (s *GoalStore) List() []Goal {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Goal, 0, len(s.goals))
	for _, g := range s.goals {
		out = append(out, cloneGoal(g))
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt) || (out[i].CreatedAt.Equal(out[j].CreatedAt) && out[i].ID < out[j].ID)
	})
	return out
}

func (s *GoalStore) Get(id string) (Goal, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	g, ok := s.goals[id]
	return cloneGoal(g), ok
}

func (s *GoalStore) persist(next map[string]Goal) error {
	if len(next) > maxGoalRecords {
		return errors.New("goal record limit exceeded")
	}
	records := make([]Goal, 0, len(next))
	for _, g := range next {
		records = append(records, g)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}
	if len(data) > maxGoalStoreBytes {
		return errors.New("goal store exceeds size limit")
	}
	return writeAtomicBytes(s.path, data, 0o600)
}

func (s *GoalStore) Create(g Goal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if g.MaxRuns == 0 {
		g.MaxRuns = maxGoalRunBudget
	}
	if err := validateGoal(g); err != nil {
		return err
	}
	if _, ok := s.goals[g.ID]; ok {
		return ErrGoalExists
	}
	if len(s.goals) >= maxGoalRecords {
		return errors.New("goal record limit exceeded")
	}
	now := time.Now().UTC()
	if g.CreatedAt.IsZero() {
		g.CreatedAt = now
	}
	if g.Revision == 0 {
		g.Revision = 1
	}
	g.UpdatedAt = now
	next := make(map[string]Goal, len(s.goals)+1)
	for k, v := range s.goals {
		next[k] = cloneGoal(v)
	}
	next[g.ID] = cloneGoal(g)
	if err := s.persist(next); err != nil {
		return err
	}
	s.goals = next
	return nil
}

// Update applies mutate to a private copy while holding the store lock; persistence precedes publication.
func (s *GoalStore) Update(id string, mutate func(*Goal) error) error {
	if mutate == nil {
		return errors.New("goal update callback is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.goals[id]
	if !ok {
		return ErrGoalNotFound
	}
	nextGoal := cloneGoal(current)
	if err := mutate(&nextGoal); err != nil {
		return err
	}
	if nextGoal.ID != id {
		return errors.New("goal ID cannot be changed")
	}
	if err := validateGoal(nextGoal); err != nil {
		return err
	}
	nextGoal.CreatedAt = current.CreatedAt
	nextGoal.Revision = current.Revision
	nextGoal.UpdatedAt = current.UpdatedAt
	if reflect.DeepEqual(current, nextGoal) {
		return nil
	}
	nextGoal.Revision++
	nextGoal.UpdatedAt = time.Now().UTC()
	next := make(map[string]Goal, len(s.goals))
	for k, v := range s.goals {
		next[k] = cloneGoal(v)
	}
	next[id] = cloneGoal(nextGoal)
	if err := s.persist(next); err != nil {
		return err
	}
	s.goals = next
	return nil
}

func (s *GoalStore) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.goals[id]; !ok {
		return ErrGoalNotFound
	}
	next := make(map[string]Goal, len(s.goals)-1)
	for k, v := range s.goals {
		if k != id {
			next[k] = cloneGoal(v)
		}
	}
	if err := s.persist(next); err != nil {
		return err
	}
	s.goals = next
	return nil
}
