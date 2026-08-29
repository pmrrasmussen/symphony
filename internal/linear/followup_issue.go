package linear

// This file implements the optional, session-bound create_followup_issue
// tool. It is not a general Linear write API: the team, project, Backlog state,
// and originating issue are bound before Codex is launched, and the only
// caller-selected relationship is a bounded relation to that originating
// issue.

import (
	"context"
	"encoding/json"
	"strings"
)

// MaxFollowupIssueTitleRunes is the single source for both the check below and
// the advertised create_followup_issue schema, which reads it from
// internal/capability. MaxFollowupIssueBodyRunes bounds the rendered body the
// description and acceptance criteria are combined into, so it is not a
// per-field schema bound; an invariant test asserts the per-field schema bounds
// cannot sum past it.
//
// Both bounds count code points, because that is the unit a JSON Schema
// maxLength counts: a byte bound here would make the advertised per-field
// bounds sum past this one for any non-ASCII text, so a schema-valid
// description could still be refused (PMR-183). The rendered body therefore
// reaches Linear as at most four times this many bytes, which is the size of
// one ordinary GraphQL mutation and not a payload worth a second bound.
const (
	MaxFollowupIssueTitleRunes = 255
	MaxFollowupIssueBodyRunes  = 20 << 10
)

type followupIssueRef struct {
	ID, Identifier, URL string
}

// CreateFollowupIssue accepts only a title, description, acceptance criteria,
// and optional relationship. It creates one ordinary, parentless Linear issue
// in the session-bound project and team, always in Backlog. The relationship,
// when present, can only target the session-bound originating issue.
func (s *HandoffSession) CreateFollowupIssue(ctx context.Context, arguments json.RawMessage) (ToolResult, error) {
	if !s.followupIssueCreationEnabled {
		return ToolResult{}, trackerError("handoff_request", "follow-up issue creation is not enabled")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(arguments, &raw); err != nil || raw == nil {
		return ToolResult{}, trackerError("handoff_request", "tool arguments must be a JSON object")
	}
	for key := range raw {
		switch key {
		case "title", "description", "acceptance_criteria", "relationship":
		default:
			return ToolResult{}, trackerError("handoff_request", "tool arguments contain an unsupported field")
		}
	}
	var input struct {
		Title              string `json:"title"`
		Description        string `json:"description"`
		AcceptanceCriteria string `json:"acceptance_criteria"`
		Relationship       string `json:"relationship"`
	}
	if err := json.Unmarshal(arguments, &input); err != nil {
		return ToolResult{}, trackerError("handoff_request", "tool arguments have invalid field types")
	}
	title := strings.TrimSpace(input.Title)
	if title == "" || len([]rune(title)) > MaxFollowupIssueTitleRunes {
		return ToolResult{}, trackerError("handoff_request", "follow-up issue title is invalid")
	}
	description := strings.TrimSpace(input.Description)
	acceptanceCriteria := strings.TrimSpace(input.AcceptanceCriteria)
	if description == "" || acceptanceCriteria == "" {
		return ToolResult{}, trackerError("handoff_request", "follow-up issue description and acceptance criteria are required")
	}
	body := description + "\n\n## Acceptance criteria\n\n" + acceptanceCriteria
	if len([]rune(body)) > MaxFollowupIssueBodyRunes {
		return ToolResult{}, trackerError("handoff_request", "follow-up issue description is too large")
	}
	relationship := strings.TrimSpace(input.Relationship)
	switch relationship {
	case "", "related", "blocked_by_current":
	default:
		return ToolResult{}, trackerError("handoff_request", "follow-up issue relationship is invalid")
	}

	s.handoffMu.Lock()
	defer s.handoffMu.Unlock()
	if err := s.ensureMutable(ctx); err != nil {
		return ToolResult{}, err
	}
	if s.issue.ProjectID() == "" || s.followupStateID == "" {
		return ToolResult{}, trackerError("invalid_tracker_config", "linear follow-up issue scope could not be resolved")
	}
	followup, err := s.createFollowupIssue(ctx, title, body)
	if err != nil {
		return ToolResult{}, err
	}
	if relationship != "" {
		if err := s.linkFollowupToCurrent(ctx, followup.ID, relationship); err != nil {
			s.logFollowup("followup_issue_created_relation_failed", followup, relationship)
			return ToolResult{}, err
		}
	}
	s.logFollowup("followup_issue_created", followup, relationship)
	return ToolResult{Success: true, Data: map[string]any{
		"id": followup.ID, "identifier": followup.Identifier, "url": followup.URL,
		"state": "Backlog", "originating_issue": s.issue.Identifier, "relationship": relationship,
	}}, nil
}

func (s *HandoffSession) createFollowupIssue(ctx context.Context, title, description string) (followupIssueRef, error) {
	variables := map[string]any{
		"teamID": s.issue.TeamID(), "projectID": s.issue.ProjectID(),
		"stateID": s.followupStateID, "title": title, "description": description,
	}
	response, err := requestWithSettings(ctx, s.client, s.settings, followupIssueCreateQuery, variables)
	if err != nil {
		return followupIssueRef{}, err
	}
	var payload struct {
		Data struct {
			IssueCreate struct {
				Success bool `json:"success"`
				Issue   *struct {
					ID         string `json:"id"`
					Identifier string `json:"identifier"`
					URL        string `json:"url"`
					Project    *struct {
						ID string `json:"id"`
					} `json:"project"`
					Team *struct {
						ID string `json:"id"`
					} `json:"team"`
					State *struct {
						ID   string `json:"id"`
						Name string `json:"name"`
					} `json:"state"`
					Parent *struct {
						ID string `json:"id"`
					} `json:"parent"`
				} `json:"issue"`
			} `json:"issueCreate"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response, &payload); err != nil || !payload.Data.IssueCreate.Success || payload.Data.IssueCreate.Issue == nil {
		return followupIssueRef{}, trackerError("handoff_response", "Linear did not accept the follow-up issue")
	}
	issue := payload.Data.IssueCreate.Issue
	id, identifier, url := strings.TrimSpace(issue.ID), strings.TrimSpace(issue.Identifier), strings.TrimSpace(issue.URL)
	if id == "" || identifier == "" || issue.Project == nil || issue.Team == nil || issue.State == nil || issue.Parent != nil ||
		strings.TrimSpace(issue.Project.ID) != s.issue.ProjectID() || strings.TrimSpace(issue.Team.ID) != s.issue.TeamID() ||
		strings.TrimSpace(issue.State.ID) != s.followupStateID || !strings.EqualFold(strings.TrimSpace(issue.State.Name), "Backlog") {
		return followupIssueRef{}, trackerError("handoff_response", "Linear returned an out-of-scope follow-up issue")
	}
	return followupIssueRef{ID: id, Identifier: identifier, URL: url}, nil
}

// linkFollowupToCurrent accepts no issue identifiers from the caller. For a
// dependency, the current issue blocks the follow-up; a related link is
// symmetric in Linear but still uses the same fixed current/follow-up pair.
func (s *HandoffSession) linkFollowupToCurrent(ctx context.Context, followupID, relationship string) error {
	query := followupIssueRelatedQuery
	if relationship == "blocked_by_current" {
		query = followupIssueBlockedByCurrentQuery
	}
	response, err := requestWithSettings(ctx, s.client, s.settings, query, map[string]any{
		"issueID": s.issue.ID, "relatedIssueID": followupID,
	})
	if err != nil {
		return err
	}
	var payload struct {
		Data struct {
			IssueRelationCreate struct {
				Success bool `json:"success"`
			} `json:"issueRelationCreate"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response, &payload); err != nil || !payload.Data.IssueRelationCreate.Success {
		return trackerError("handoff_response", "Linear did not accept the follow-up issue relationship")
	}
	return nil
}

// logFollowup records only safe identifiers and the bounded relationship enum,
// never worker-supplied issue content or credentials.
func (s *HandoffSession) logFollowup(outcome string, followup followupIssueRef, relationship string) {
	if s.logger == nil {
		return
	}
	s.logger.Info("Linear follow-up issue", "outcome", outcome,
		"originating_issue_id", s.issue.ID, "originating_issue_identifier", s.issue.Identifier,
		"followup_issue_id", followup.ID, "followup_issue_identifier", followup.Identifier,
		"relationship", relationship)
}

const followupIssueCreateQuery = `mutation SymphonyLinearCreateFollowupIssue($teamID: String!, $projectID: String!, $stateID: String!, $title: String!, $description: String!) { issueCreate(input: {teamId: $teamID, projectId: $projectID, stateId: $stateID, title: $title, description: $description}) { success issue { id identifier url project { id } team { id } state { id name } parent { id } } } }`
const followupIssueRelatedQuery = `mutation SymphonyLinearCreateFollowupIssueRelated($issueID: String!, $relatedIssueID: String!) { issueRelationCreate(input: {issueId: $issueID, relatedIssueId: $relatedIssueID, type: related}) { success } }`
const followupIssueBlockedByCurrentQuery = `mutation SymphonyLinearCreateFollowupIssueBlockedByCurrent($issueID: String!, $relatedIssueID: String!) { issueRelationCreate(input: {issueId: $issueID, relatedIssueId: $relatedIssueID, type: blocks}) { success } }`
