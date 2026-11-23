// SPDX-License-Identifier: GPL-3.0-only
package app

import (
	"encoding/json"
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"
)

func TestGoalCheckpointMessageContainsAuthoritativeResumeState(t *testing.T) {
	goal := Goal{
		Title: "Implement feature", Details: "Implement and verify the requested feature", Workspace: "C:/private/workspace",
		Acceptance: []string{"tests pass", "output file exists"}, Status: GoalStatusActive,
		PreflightRequired: true, PreflightReady: true, PreflightSummary: "understood the goal and split it into two stages",
		Progress: "first stage passed; implementing second stage",
		Stages: []GoalStage{
			{Title: "Inspect", Objective: "Inspect existing behavior", Status: "completed", Evidence: "Existing behavior was inspected.", Verifications: []GoalVerification{{Name: "inspection", ExitCode: 0, Passed: true}}},
			{Title: "Implement", Objective: "Implement and test the feature", Status: "active", FailuresSinceReview: 1, Verifications: []GoalVerification{{Name: "unit tests", ExitCode: 1, Passed: false}}},
		},
		Verifications: []GoalVerification{{Name: "integration", ExitCode: 1, Passed: false}},
	}
	message := goalCheckpointMessage(goal)
	if message == nil || message.Role != "user" {
		t.Fatalf("checkpoint message = %#v, want a user message", message)
	}
	for _, want := range []string{"<ally-goal-checkpoint>", "authoritative", "tests pass", "output file exists", "first stage passed", "understood the goal and split it into two stages", `"preflight_ready":true`, `"status":"completed"`, `"status":"active"`, `"passed":false`} {
		if !strings.Contains(message.Content, want) {
			t.Errorf("checkpoint is missing %q: %s", want, message.Content)
		}
	}
	if strings.Contains(message.Content, goal.Workspace) {
		t.Errorf("checkpoint unexpectedly includes workspace path: %s", message.Content)
	}
	var parsed map[string]any
	payload := strings.TrimSuffix(strings.TrimPrefix(message.Content, "<ally-goal-checkpoint>\n"), "\n</ally-goal-checkpoint>")
	jsonStart := strings.Index(payload, "\n{")
	if jsonStart < 0 || json.Unmarshal([]byte(payload[jsonStart+1:]), &parsed) != nil {
		t.Fatalf("checkpoint payload does not contain valid JSON: %s", payload)
	}
}

func TestAppendGoalCheckpointDoesNotMutateHistory(t *testing.T) {
	history := []openai.ChatCompletionMessage{{Role: "user", Content: "original"}}
	checkpoint := &openai.ChatCompletionMessage{Role: "user", Content: "checkpoint"}
	request := appendGoalCheckpoint(history, checkpoint)
	if len(request) != 2 || request[1].Content != "checkpoint" {
		t.Fatalf("request = %#v, want history followed by checkpoint", request)
	}
	if len(history) != 1 || history[0].Content != "original" {
		t.Fatalf("request checkpoint was persisted into history: %#v", history)
	}
}
