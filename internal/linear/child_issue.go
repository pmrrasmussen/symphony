package linear

// This file implements the optional, session-bound create_child_issue tool.
// Like the host handoff path in handoff.go, it is not a general Linear write
// API: every mutation is fixed here, and the team,
// project, and parent issue are bound to the active issue before Codex is
// launched. A dependency reference is only ever resolved against a child
// issue this same session already created, never an arbitrary Linear ID.

import (
	"context"
	"encoding/json"
	"strings"
)

const (
	maxChildIssueTitleRunes       = 255
	maxChildIssueDescriptionBytes = 20 << 10
	maxChildIssueLabels           = 20
	maxChildIssueDependencies     = 20
)

// childIssueRef is the bounded, non-sensitive record of one issue this
// session created. It is used both for the tool's returned identifier and to
// validate later dependency references against only this session's own
// children.
type childIssueRef struct {
	ID, Identifier, URL string
}

// CreateChildIssue accepts only title, description, priority, labels, and
// depends_on. It creates one new Linear issue in the session-bound project
// and team, records the active issue as its Linear parent, and optionally
// links it as blocked by earlier child issues this same session created. It
// has no field for an issue ID, project, team, or endpoint: those are always
// the values frozen when the session was prepared.
func (s *HandoffSession) CreateChildIssue(ctx context.Context, arguments json.RawMessage) (ToolResult, error) {
	if !s.childIssueCreationEnabled {
		return ToolResult{}, trackerError("handoff_request", "child issue creation is not enabled")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(arguments, &raw); err != nil || raw == nil {
		return ToolResult{}, trackerError("handoff_request", "tool arguments must be a JSON object")
	}
	for key := range raw {
		switch key {
		case "title", "description", "priority", "labels", "depends_on":
		default:
			return ToolResult{}, trackerError("handoff_request", "tool arguments contain an unsupported field")
		}
	}
	var input struct {
		Title       string   `json:"title"`
		Description string   `json:"description"`
		Priority    *int     `json:"priority"`
		Labels      []string `json:"labels"`
		DependsOn   []string `json:"depends_on"`
	}
	if err := json.Unmarshal(arguments, &input); err != nil {
		return ToolResult{}, trackerError("handoff_request", "tool arguments have invalid field types")
	}
	title := strings.TrimSpace(input.Title)
	if title == "" || len([]rune(title)) > maxChildIssueTitleRunes {
		return ToolResult{}, trackerError("handoff_request", "child issue title is invalid")
	}
	description := strings.TrimSpace(input.Description)
	if len([]byte(description)) > maxChildIssueDescriptionBytes {
		return ToolResult{}, trackerError("handoff_request", "child issue description is too large")
	}
	if input.Priority != nil && (*input.Priority < 0 || *input.Priority > 4) {
		return ToolResult{}, trackerError("handoff_request", "child issue priority must be between 0 and 4")
	}
	if len(input.Labels) > maxChildIssueLabels {
		return ToolResult{}, trackerError("handoff_request", "too many child issue labels")
	}
	if len(input.DependsOn) > maxChildIssueDependencies {
		return ToolResult{}, trackerError("handoff_request", "too many child issue dependencies")
	}

	s.handoffMu.Lock()
	defer s.handoffMu.Unlock()
	if err := s.ensureMutable(ctx); err != nil {
		return ToolResult{}, err
	}
	if s.issue.ProjectID() == "" {
		return ToolResult{}, trackerError("invalid_tracker_config", "linear active issue project could not be resolved for child issue creation")
	}
	var labelIDs []string
	if len(input.Labels) > 0 {
		ids, err := s.resolveLabels(ctx, input.Labels)
		if err != nil {
			return ToolResult{}, err
		}
		labelIDs = ids
	}
	var blockerIDs []string
	if len(input.DependsOn) > 0 {
		ids, err := s.resolveChildDependencies(input.DependsOn)
		if err != nil {
			return ToolResult{}, err
		}
		blockerIDs = ids
	}
	child, err := s.createChildIssue(ctx, title, description, input.Priority, labelIDs)
	if err != nil {
		return ToolResult{}, err
	}
	for _, blockerID := range blockerIDs {
		if err := s.linkChildBlockedBy(ctx, blockerID, child.ID); err != nil {
			return ToolResult{}, err
		}
	}
	s.recordChild(child)
	s.logChild("child_issue_created", child)
	return ToolResult{Success: true, Data: map[string]any{
		"id": child.ID, "identifier": child.Identifier, "url": child.URL, "parent_issue": s.issue.Identifier,
	}}, nil
}

// resolveLabels resolves bounded label names against only the active issue's
// team labels. An unresolved name fails the whole call rather than silently
// creating a new Linear label or applying a partial label set.
func (s *HandoffSession) resolveLabels(ctx context.Context, names []string) ([]string, error) {
	wanted := make(map[string]struct{}, len(names))
	for _, name := range names {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			return nil, trackerError("handoff_request", "child issue label must not be empty")
		}
		wanted[name] = struct{}{}
	}
	found := make(map[string]string, len(wanted))
	after := any(nil)
	seen := map[string]bool{}
	for {
		response, err := requestWithSettings(ctx, s.client, s.settings, childIssueLabelsQuery, map[string]any{
			"teamID": s.issue.TeamID(), "first": pageSize, "after": after,
		})
		if err != nil {
			return nil, err
		}
		var payload struct {
			Data struct {
				Team *struct {
					ID     string `json:"id"`
					Labels struct {
						Nodes []struct {
							ID   string `json:"id"`
							Name string `json:"name"`
						} `json:"nodes"`
						PageInfo struct {
							HasNextPage bool    `json:"hasNextPage"`
							EndCursor   *string `json:"endCursor"`
						} `json:"pageInfo"`
					} `json:"labels"`
				} `json:"team"`
			} `json:"data"`
		}
		if err := json.Unmarshal(response, &payload); err != nil || payload.Data.Team == nil || strings.TrimSpace(payload.Data.Team.ID) != s.issue.TeamID() {
			return nil, trackerError("handoff_scope", "Linear did not return the active issue team")
		}
		for _, label := range payload.Data.Team.Labels.Nodes {
			key := strings.ToLower(strings.TrimSpace(label.Name))
			if _, wantIt := wanted[key]; !wantIt {
				continue
			}
			id := strings.TrimSpace(label.ID)
			if id == "" {
				return nil, trackerError("handoff_scope", "Linear returned an invalid active issue team label")
			}
			if _, exists := found[key]; !exists {
				found[key] = id
			}
		}
		if len(found) == len(wanted) {
			break
		}
		if !payload.Data.Team.Labels.PageInfo.HasNextPage {
			break
		}
		if payload.Data.Team.Labels.PageInfo.EndCursor == nil {
			return nil, trackerError("handoff_response", "Linear returned invalid label pagination")
		}
		cursor := strings.TrimSpace(*payload.Data.Team.Labels.PageInfo.EndCursor)
		if cursor == "" || seen[cursor] {
			return nil, trackerError("handoff_response", "Linear returned invalid label pagination")
		}
		seen[cursor] = true
		after = cursor
	}
	if len(found) != len(wanted) {
		return nil, trackerError("handoff_scope", "child issue label is not configured for the active issue team")
	}
	ids := make([]string, 0, len(names))
	usedID := map[string]bool{}
	for _, name := range names {
		id := found[strings.ToLower(strings.TrimSpace(name))]
		if !usedID[id] {
			usedID[id] = true
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// resolveChildDependencies rejects any reference that is not a child issue
// this same session already created. This is what keeps depends_on bounded:
// it can never mutate or relate to an issue outside this session's own
// lineage.
func (s *HandoffSession) resolveChildDependencies(identifiers []string) ([]string, error) {
	ids := make([]string, 0, len(identifiers))
	seen := map[string]bool{}
	for _, raw := range identifiers {
		key := strings.ToLower(strings.TrimSpace(raw))
		if key == "" {
			return nil, trackerError("handoff_request", "child issue dependency must not be empty")
		}
		ref, ok := s.createdChildren[key]
		if !ok {
			return nil, trackerError("handoff_scope", "child issue dependency must reference a child issue created in this session")
		}
		if !seen[ref.ID] {
			seen[ref.ID] = true
			ids = append(ids, ref.ID)
		}
	}
	return ids, nil
}

func (s *HandoffSession) createChildIssue(ctx context.Context, title, description string, priority *int, labelIDs []string) (childIssueRef, error) {
	variables := map[string]any{
		"teamID":    s.issue.TeamID(),
		"projectID": s.issue.ProjectID(),
		"parentID":  s.issue.ID,
		"title":     title,
	}
	if description != "" {
		variables["description"] = description
	}
	if priority != nil {
		variables["priority"] = *priority
	}
	if len(labelIDs) > 0 {
		variables["labelIDs"] = labelIDs
	}
	response, err := requestWithSettings(ctx, s.client, s.settings, childIssueCreateQuery, variables)
	if err != nil {
		return childIssueRef{}, err
	}
	var payload struct {
		Data struct {
			IssueCreate struct {
				Success bool `json:"success"`
				Issue   *struct {
					ID         string `json:"id"`
					Identifier string `json:"identifier"`
					URL        string `json:"url"`
				} `json:"issue"`
			} `json:"issueCreate"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response, &payload); err != nil || !payload.Data.IssueCreate.Success || payload.Data.IssueCreate.Issue == nil {
		return childIssueRef{}, trackerError("handoff_response", "Linear did not accept the child issue")
	}
	issue := payload.Data.IssueCreate.Issue
	id, identifier, url := strings.TrimSpace(issue.ID), strings.TrimSpace(issue.Identifier), strings.TrimSpace(issue.URL)
	if id == "" || identifier == "" {
		return childIssueRef{}, trackerError("handoff_response", "Linear returned an incomplete child issue")
	}
	return childIssueRef{ID: id, Identifier: identifier, URL: url}, nil
}

// linkChildBlockedBy records that blockerID (an earlier child issue created
// by this session) blocks the just-created childID. Both IDs are always
// session-derived, never caller-supplied Linear IDs.
func (s *HandoffSession) linkChildBlockedBy(ctx context.Context, blockerID, childID string) error {
	response, err := requestWithSettings(ctx, s.client, s.settings, childIssueBlockQuery, map[string]any{
		"issueID": blockerID, "relatedIssueID": childID,
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
		return trackerError("handoff_response", "Linear did not accept the child issue dependency")
	}
	return nil
}

func (s *HandoffSession) recordChild(child childIssueRef) {
	if s.createdChildren == nil {
		s.createdChildren = map[string]childIssueRef{}
	}
	s.createdChildren[strings.ToLower(strings.TrimSpace(child.Identifier))] = child
	s.createdChildren[strings.ToLower(strings.TrimSpace(child.ID))] = child
}

// logChild is the audit log entry for a created child issue. Like the
// handoff log, it records only safe identifiers: never title, description,
// or label content.
func (s *HandoffSession) logChild(outcome string, child childIssueRef) {
	if s.logger == nil {
		return
	}
	s.logger.Info("Linear child issue", "outcome", outcome,
		"parent_issue_id", s.issue.ID, "parent_issue_identifier", s.issue.Identifier,
		"child_issue_id", child.ID, "child_issue_identifier", child.Identifier)
}

const childIssueLabelsQuery = `query SymphonyLinearChildIssueLabels($teamID: String!, $first: Int!, $after: String) { team(id: $teamID) { id labels(first: $first, after: $after) { nodes { id name } pageInfo { hasNextPage endCursor } } } }`
const childIssueCreateQuery = `mutation SymphonyLinearCreateChildIssue($teamID: String!, $projectID: String, $parentID: String!, $title: String!, $description: String, $priority: Int, $labelIDs: [String!]) { issueCreate(input: {teamId: $teamID, projectId: $projectID, parentId: $parentID, title: $title, description: $description, priority: $priority, labelIds: $labelIDs}) { success issue { id identifier url } } }`
const childIssueBlockQuery = `mutation SymphonyLinearCreateChildIssueBlock($issueID: String!, $relatedIssueID: String!) { issueRelationCreate(input: {issueId: $issueID, relatedIssueId: $relatedIssueID, type: blocks}) { success } }`
