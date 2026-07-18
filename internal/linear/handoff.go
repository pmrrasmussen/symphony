package linear

// This file contains the intentionally small Linear capability exposed to a
// running Codex session. It is not a GraphQL proxy despite the compatibility
// tool name: every query and mutation is fixed here, and the issue/project/
// team are bound before Codex is launched.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
)

const maxHandoffCommentBytes = 8 << 10

// Handoff owns the Linear side of the Codex client tool. Its settings callback
// is read only when a session starts; Session then keeps a policy snapshot.
type Handoff struct {
	settings func() config.Settings
	client   *http.Client
}

func NewHandoff(settings func() config.Settings) *Handoff {
	return &Handoff{settings: settings, client: newHTTPClient(nil)}
}

func (h *Handoff) Enabled() bool {
	return strings.TrimSpace(h.settings().Tracker.HandoffState) != ""
}

// Prepare verifies that the issue is still in the configured project, finds
// the configured team's target state, and freezes the policy for one session.
// No Codex child is started when this fails.
func (h *Handoff) Prepare(ctx context.Context, issue domain.Issue) (*HandoffSession, error) {
	s := h.settings()
	if strings.TrimSpace(s.Tracker.HandoffState) == "" {
		return nil, nil
	}
	if err := validateProvider(s.Tracker.Provider); err != nil {
		return nil, err
	}
	if strings.TrimSpace(issue.ID) == "" {
		return nil, trackerError("invalid_handoff_issue", "active issue ID is missing")
	}
	projectSlug := strings.TrimSpace(stringValue(s.Tracker.Provider["project_slug"]))
	if projectSlug == "" {
		return nil, trackerError("invalid_tracker_config", "linear project_slug is missing")
	}
	active, err := h.readIssue(ctx, s, issue.ID)
	if err != nil {
		return nil, err
	}
	if active.ID != issue.ID || active.ProjectSlug() != projectSlug || active.TeamID() == "" {
		return nil, trackerError("handoff_scope", "active issue is outside the configured Linear project")
	}
	stateID, err := h.resolveState(ctx, s, active.TeamID(), s.Tracker.HandoffState)
	if err != nil {
		return nil, err
	}
	comment, err := s.RenderHandoffComment(active.toDomain())
	if err != nil {
		return nil, trackerError("invalid_handoff_config", "could not render configured handoff comment")
	}
	return &HandoffSession{
		client: h.client, settings: s, issue: active, targetStateID: stateID,
		handoffComment: comment,
	}, nil
}

// HandoffSession is the fixed authority granted to one app-server session.
// It has no method that accepts an issue, project, endpoint, or credential.
type HandoffSession struct {
	client         *http.Client
	settings       config.Settings
	issue          handoffIssue
	targetStateID  string
	handoffComment string
}

// MatchesSecret lets the Codex launcher remove inherited values containing the
// configured credential without exposing the credential itself across the
// adapter boundary. It is never sent to Codex, returned by a tool, or logged.
func (s *HandoffSession) MatchesSecret(candidate string) bool {
	value, _ := s.settings.Tracker.Provider["api_key"].(string)
	return value != "" && strings.Contains(candidate, value)
}

type ToolResult struct {
	Success bool
	Data    any
}

// Call accepts only the three typed tool operations. json.RawMessage exists so
// Codex protocol decoding stays in its adapter; this function never accepts a
// GraphQL document or arbitrary target identifiers.
func (s *HandoffSession) Call(ctx context.Context, arguments json.RawMessage) (ToolResult, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(arguments, &raw); err != nil || raw == nil {
		return ToolResult{}, trackerError("handoff_request", "tool arguments must be a JSON object")
	}
	for key := range raw {
		if key != "operation" && key != "body" {
			return ToolResult{}, trackerError("handoff_request", "tool arguments contain an unsupported field")
		}
	}
	var input struct {
		Operation string `json:"operation"`
		Body      string `json:"body"`
	}
	if err := json.Unmarshal(arguments, &input); err != nil || strings.TrimSpace(input.Operation) == "" {
		return ToolResult{}, trackerError("handoff_request", "tool arguments have invalid field types")
	}
	operation := strings.TrimSpace(input.Operation)
	switch operation {
	case "read":
		if strings.TrimSpace(input.Body) != "" {
			return ToolResult{}, trackerError("handoff_request", "read does not accept body")
		}
		return ToolResult{Success: true, Data: s.issue.metadata()}, nil
	case "handoff":
		if strings.TrimSpace(input.Body) != "" {
			return ToolResult{}, trackerError("handoff_request", "handoff does not accept body")
		}
		if s.handoffComment != "" {
			if err := s.ensureMutable(ctx); err != nil {
				return ToolResult{}, err
			}
			if err := s.comment(ctx, s.handoffComment); err != nil {
				return ToolResult{}, err
			}
		}
		if err := s.ensureMutable(ctx); err != nil {
			return ToolResult{}, err
		}
		if err := s.transition(ctx); err != nil {
			return ToolResult{}, err
		}
		return ToolResult{Success: true, Data: map[string]any{"issue": s.issue.metadata(), "handoff_state": s.settings.Tracker.HandoffState}}, nil
	case "comment":
		if err := validateComment(input.Body); err != nil {
			return ToolResult{}, err
		}
		if err := s.ensureMutable(ctx); err != nil {
			return ToolResult{}, err
		}
		if err := s.comment(ctx, input.Body); err != nil {
			return ToolResult{}, err
		}
		return ToolResult{Success: true, Data: map[string]any{"issue": s.issue.metadata(), "commented": true}}, nil
	default:
		return ToolResult{}, trackerError("handoff_request", "unsupported linear handoff operation")
	}
}

// ensureMutable re-reads the bound issue immediately before each mutation. A
// human change since session setup (including a transition to Done) wins over
// the agent: the request is rejected before any mutation is sent.
func (s *HandoffSession) ensureMutable(ctx context.Context) error {
	current, err := readHandoffIssue(ctx, s.client, s.settings, s.issue.ID)
	if err != nil {
		return err
	}
	if current.ProjectSlug() != s.issue.ProjectSlug() || current.TeamID() != s.issue.TeamID() {
		return trackerError("handoff_scope", "active issue scope changed after session setup")
	}
	if !strings.EqualFold(strings.TrimSpace(current.State.Name), strings.TrimSpace(s.issue.State.Name)) {
		return trackerError("handoff_scope", "active issue state changed after session setup")
	}
	for _, active := range s.settings.Tracker.ActiveStates {
		if strings.EqualFold(strings.TrimSpace(current.State.Name), strings.TrimSpace(active)) {
			return nil
		}
	}
	return trackerError("handoff_scope", "active issue is no longer in a workflow active state")
}

func validateComment(body string) error {
	if strings.TrimSpace(body) == "" {
		return trackerError("handoff_request", "comment body must not be empty")
	}
	if len([]byte(body)) > maxHandoffCommentBytes {
		return trackerError("handoff_request", "comment body is too large")
	}
	return nil
}

func (s *HandoffSession) transition(ctx context.Context) error {
	response, err := requestWithSettings(ctx, s.client, s.settings, handoffTransitionQuery, map[string]any{
		"issueID": s.issue.ID, "stateID": s.targetStateID,
	})
	if err != nil {
		return err
	}
	var payload struct {
		Data struct {
			IssueUpdate struct {
				Success bool `json:"success"`
			} `json:"issueUpdate"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response, &payload); err != nil || !payload.Data.IssueUpdate.Success {
		return trackerError("handoff_response", "Linear did not accept the configured handoff state")
	}
	return nil
}

func (s *HandoffSession) comment(ctx context.Context, body string) error {
	response, err := requestWithSettings(ctx, s.client, s.settings, handoffCommentQuery, map[string]any{
		"issueID": s.issue.ID, "body": body,
	})
	if err != nil {
		return err
	}
	var payload struct {
		Data struct {
			CommentCreate struct {
				Success bool `json:"success"`
			} `json:"commentCreate"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response, &payload); err != nil || !payload.Data.CommentCreate.Success {
		return trackerError("handoff_response", "Linear did not accept the issue comment")
	}
	return nil
}

func (h *Handoff) readIssue(ctx context.Context, s config.Settings, issueID string) (handoffIssue, error) {
	return readHandoffIssue(ctx, h.client, s, issueID)
}

func readHandoffIssue(ctx context.Context, client *http.Client, s config.Settings, issueID string) (handoffIssue, error) {
	response, err := requestWithSettings(ctx, client, s, handoffReadQuery, map[string]any{"issueID": issueID})
	if err != nil {
		return handoffIssue{}, err
	}
	var payload struct {
		Data struct {
			Issue *handoffIssue `json:"issue"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response, &payload); err != nil || payload.Data.Issue == nil {
		return handoffIssue{}, trackerError("handoff_scope", "Linear did not return the active issue")
	}
	if err := payload.Data.Issue.valid(); err != nil {
		return handoffIssue{}, err
	}
	return *payload.Data.Issue, nil
}

func (h *Handoff) resolveState(ctx context.Context, s config.Settings, teamID, target string) (string, error) {
	response, err := requestWithSettings(ctx, h.client, s, handoffStatesQuery, map[string]any{"teamID": teamID})
	if err != nil {
		return "", err
	}
	var payload struct {
		Data struct {
			Team *struct {
				ID     string `json:"id"`
				States struct {
					Nodes []struct {
						ID   string `json:"id"`
						Name string `json:"name"`
					} `json:"nodes"`
				} `json:"states"`
			} `json:"team"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response, &payload); err != nil || payload.Data.Team == nil || strings.TrimSpace(payload.Data.Team.ID) != teamID {
		return "", trackerError("handoff_scope", "Linear did not return the active issue team")
	}
	var found string
	for _, state := range payload.Data.Team.States.Nodes {
		if strings.EqualFold(strings.TrimSpace(state.Name), strings.TrimSpace(target)) {
			if found != "" || strings.TrimSpace(state.ID) == "" {
				return "", trackerError("handoff_scope", "configured handoff state is ambiguous")
			}
			for _, terminal := range s.Tracker.TerminalStates {
				if strings.EqualFold(strings.TrimSpace(state.Name), strings.TrimSpace(terminal)) {
					return "", trackerError("handoff_scope", "configured handoff state is terminal")
				}
			}
			found = strings.TrimSpace(state.ID)
		}
	}
	if found == "" {
		return "", trackerError("handoff_scope", "configured handoff state is not in the active issue team")
	}
	return found, nil
}

type handoffIssue struct {
	ID          string `json:"id"`
	Identifier  string `json:"identifier"`
	Title       string `json:"title"`
	Description string `json:"description"`
	URL         string `json:"url"`
	Project     *struct {
		SlugID string `json:"slugId"`
	} `json:"project"`
	Team *struct {
		ID string `json:"id"`
	} `json:"team"`
	State *struct {
		Name string `json:"name"`
	} `json:"state"`
}

func (i handoffIssue) valid() error {
	if strings.TrimSpace(i.ID) == "" || strings.TrimSpace(i.Identifier) == "" || strings.TrimSpace(i.Title) == "" || i.Project == nil || i.Team == nil || i.State == nil {
		return trackerError("handoff_scope", "Linear returned an incomplete active issue")
	}
	return nil
}
func (i handoffIssue) ProjectSlug() string { return strings.TrimSpace(i.Project.SlugID) }
func (i handoffIssue) TeamID() string      { return strings.TrimSpace(i.Team.ID) }
func (i handoffIssue) toDomain() domain.Issue {
	return domain.Issue{ID: i.ID, Identifier: i.Identifier, Title: i.Title, Description: i.Description, URL: i.URL, State: i.State.Name}
}
func (i handoffIssue) metadata() map[string]string {
	return map[string]string{"id": i.ID, "identifier": i.Identifier, "title": i.Title, "description": i.Description, "url": i.URL, "state": i.State.Name}
}

// Keep every GraphQL operation fixed and intentionally small. Variables are
// generated solely from the already-bound session, never from tool input.
const handoffReadQuery = `query SymphonyLinearHandoffIssue($issueID: String!) { issue(id: $issueID) { id identifier title description url project { slugId } team { id } state { name } } }`
const handoffStatesQuery = `query SymphonyLinearHandoffStates($teamID: String!) { team(id: $teamID) { id states { nodes { id name } } } }`
const handoffTransitionQuery = `mutation SymphonyLinearHandoffTransition($issueID: String!, $stateID: String!) { issueUpdate(id: $issueID, input: {stateId: $stateID}) { success } }`
const handoffCommentQuery = `mutation SymphonyLinearHandoffComment($issueID: String!, $body: String!) { commentCreate(input: {issueId: $issueID, body: $body}) { success } }`
