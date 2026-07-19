package linear

// This file contains the intentionally small Linear capability exposed to a
// running Codex session. It is not a GraphQL proxy despite the compatibility
// tool name: every query and mutation is fixed here, and the issue/project/
// team are bound before Codex is launched.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
)

const maxHandoffCommentBytes = 8 << 10

// Handoff owns the Linear side of the Codex client tool. Its settings callback
// is read only when a session starts; Session then keeps a policy snapshot.
type Handoff struct {
	settings func() config.Settings
	client   *http.Client
	logger   *slog.Logger
}

func NewHandoff(settings func() config.Settings) *Handoff {
	return &Handoff{settings: settings, client: newHTTPClient(nil), logger: slog.Default()}
}

func (h *Handoff) Enabled() bool {
	return h.settings().LinearSessionCapabilityEnabled()
}

// Prepare verifies that the issue is still in the configured project, finds
// the configured team's target state, and freezes the policy for one session.
// No Codex child is started when this fails.
func (h *Handoff) Prepare(ctx context.Context, issue domain.Issue) (*HandoffSession, error) {
	return h.PrepareWithSettings(ctx, h.settings(), issue)
}

// PrepareWithSettings binds a single repository settings snapshot to the
// session. The Codex backend calls this once before it launches the child.
func (h *Handoff) PrepareWithSettings(ctx context.Context, s config.Settings, issue domain.Issue) (*HandoffSession, error) {
	if !s.LinearSessionCapabilityEnabled() {
		return nil, nil
	}
	if err := validateProvider(s.Tracker.Provider); err != nil {
		return nil, err
	}
	if strings.TrimSpace(issue.ID) == "" {
		return nil, trackerError("invalid_handoff_issue", "active issue ID is missing")
	}
	projectSlug := strings.TrimSpace(stringValue(s.Tracker.Provider["project_slug_id"]))
	if projectSlug == "" {
		return nil, trackerError("invalid_tracker_config", "linear project_slug_id is missing")
	}
	active, err := h.readIssue(ctx, s, issue.ID)
	if err != nil {
		return nil, err
	}
	if active.ID != issue.ID || active.ProjectSlug() != projectSlug || active.TeamID() == "" {
		return nil, trackerError("handoff_scope", "active issue is outside the configured Linear project")
	}
	if strings.TrimSpace(s.Tracker.HandoffState) != "" && !stateAllowed(active.State.Name, s.Tracker.ActiveStates) && len(s.Tracker.AgentTransitions) == 0 {
		return nil, trackerError("handoff_scope", "active issue is not in a workflow active state")
	}
	if s.Tracker.ChildIssueCreation && active.ProjectID() == "" {
		return nil, trackerError("invalid_tracker_config", "linear active issue project could not be resolved for child issue creation")
	}
	stateID, comment := "", ""
	if strings.TrimSpace(s.Tracker.HandoffState) != "" {
		stateID, err = h.resolveState(ctx, s, active.TeamID(), s.Tracker.HandoffState)
		if err != nil {
			return nil, err
		}
		comment, err = s.RenderHandoffComment(active.toDomain())
		if err != nil {
			return nil, trackerError("invalid_handoff_config", "could not render configured handoff comment")
		}
		comment = strings.TrimSpace(comment)
		if strings.TrimSpace(s.Tracker.HandoffCommentTemplate) != "" {
			if err := validateComment(comment); err != nil {
				return nil, trackerError("invalid_handoff_config", "rendered handoff comment is invalid")
			}
		}
	}
	return &HandoffSession{
		client: h.client, settings: s, issue: active, targetStateID: stateID,
		handoffComment: comment, agentTransitions: copyTransitions(s.Tracker.AgentTransitions),
		childIssueCreationEnabled: s.Tracker.ChildIssueCreation, logger: h.logger,
	}, nil
}

// HandoffSession is the fixed authority granted to one app-server session.
// It has no method that accepts an issue, project, endpoint, or credential.
type HandoffSession struct {
	client                    *http.Client
	settings                  config.Settings
	issue                     handoffIssue
	targetStateID             string
	handoffComment            string
	agentTransitions          map[string]string
	childIssueCreationEnabled bool
	createdChildren           map[string]childIssueRef
	logger                    *slog.Logger
	handoffMu                 sync.Mutex
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

func copyTransitions(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	copy := make(map[string]string, len(source))
	for from, to := range source {
		copy[strings.ToLower(strings.TrimSpace(from))] = strings.TrimSpace(to)
	}
	return copy
}

// Call accepts only the four typed tool operations. json.RawMessage exists so
// Codex protocol decoding stays in its adapter; this function never accepts a
// GraphQL document or arbitrary target identifiers.
func (s *HandoffSession) Call(ctx context.Context, arguments json.RawMessage) (ToolResult, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(arguments, &raw); err != nil || raw == nil {
		return ToolResult{}, trackerError("handoff_request", "tool arguments must be a JSON object")
	}
	for key := range raw {
		if key != "operation" && key != "body" && key != "destination" {
			return ToolResult{}, trackerError("handoff_request", "tool arguments contain an unsupported field")
		}
	}
	var input struct {
		Operation   string `json:"operation"`
		Body        string `json:"body"`
		Destination string `json:"destination"`
	}
	if err := json.Unmarshal(arguments, &input); err != nil || strings.TrimSpace(input.Operation) == "" {
		return ToolResult{}, trackerError("handoff_request", "tool arguments have invalid field types")
	}
	operation := strings.TrimSpace(input.Operation)
	switch operation {
	case "read":
		if strings.TrimSpace(input.Body) != "" || strings.TrimSpace(input.Destination) != "" {
			return ToolResult{}, trackerError("handoff_request", "read does not accept input")
		}
		return ToolResult{Success: true, Data: s.issue.metadata()}, nil
	case "handoff":
		if strings.TrimSpace(input.Body) != "" || strings.TrimSpace(input.Destination) != "" {
			return ToolResult{}, trackerError("handoff_request", "handoff does not accept input")
		}
		if err := s.handoff(ctx); err != nil {
			s.log("handoff_failed")
			return ToolResult{}, err
		}
		s.log("handoff_complete")
		return ToolResult{Success: true, Data: map[string]any{"issue": s.issue.metadata(), "handoff_state": s.settings.Tracker.HandoffState}}, nil
	case "comment":
		if strings.TrimSpace(input.Destination) != "" {
			return ToolResult{}, trackerError("handoff_request", "comment does not accept destination")
		}
		body := strings.TrimSpace(input.Body)
		if err := validateComment(body); err != nil {
			return ToolResult{}, err
		}
		if err := s.ensureMutable(ctx); err != nil {
			return ToolResult{}, err
		}
		if err := s.comment(ctx, body); err != nil {
			return ToolResult{}, err
		}
		return ToolResult{Success: true, Data: map[string]any{"issue": s.issue.metadata(), "commented": true}}, nil
	case "transition":
		if strings.TrimSpace(input.Body) != "" {
			return ToolResult{}, trackerError("handoff_request", "transition does not accept body")
		}
		destination := strings.TrimSpace(input.Destination)
		if destination == "" {
			return ToolResult{}, trackerError("handoff_request", "transition destination is required")
		}
		issue, err := s.agentTransition(ctx, destination)
		if err != nil {
			return ToolResult{}, err
		}
		return ToolResult{Success: true, Data: map[string]any{"issue": issue.metadata(), "transition_state": issue.State.Name}}, nil
	default:
		return ToolResult{}, trackerError("handoff_request", "unsupported linear handoff operation")
	}
}

// agentTransition performs one configured exact edge. It deliberately has no
// issue, project, team, endpoint, or credential input: the session owns all
// of those values and refreshes the bound issue before and after the mutation.
// Linear has no cross-system transaction with the worker, so an ambiguous
// result is reconciled by the next bounded call.
func (s *HandoffSession) agentTransition(ctx context.Context, destination string) (handoffIssue, error) {
	s.handoffMu.Lock()
	defer s.handoffMu.Unlock()
	current, err := s.readScopedIssue(ctx)
	if err != nil {
		return handoffIssue{}, err
	}
	if s.isTransitionDestination(current, destination) {
		s.issue = current
		return current, nil
	}
	if _, err := s.validateTransition(ctx, current, destination); err != nil {
		return handoffIssue{}, err
	}

	// The second read closes the human-change window as far as Linear's API
	// permits. It cannot be atomic with the mutation, which is why successful
	// writes are always followed by another scoped read below.
	current, err = s.readScopedIssue(ctx)
	if err != nil {
		return handoffIssue{}, err
	}
	if s.isTransitionDestination(current, destination) {
		s.issue = current
		return current, nil
	}
	targetID, err := s.validateTransition(ctx, current, destination)
	if err != nil {
		return handoffIssue{}, err
	}
	if err := s.transitionTo(ctx, targetID); err != nil {
		return handoffIssue{}, err
	}
	updated, err := s.readScopedIssue(ctx)
	if err != nil {
		return handoffIssue{}, err
	}
	if !s.isState(updated, targetID, destination) {
		return handoffIssue{}, trackerError("handoff_response", "Linear did not apply the configured transition")
	}
	s.issue = updated
	s.log("agent_transition_complete")
	return updated, nil
}

func (s *HandoffSession) isTransitionDestination(issue handoffIssue, destination string) bool {
	if !strings.EqualFold(strings.TrimSpace(issue.State.Name), strings.TrimSpace(destination)) {
		return false
	}
	for _, configured := range s.agentTransitions {
		if strings.EqualFold(strings.TrimSpace(configured), strings.TrimSpace(destination)) {
			return true
		}
	}
	return false
}

// validateTransition resolves both current and target states in the active
// issue's team on every call. Matching names alone is insufficient because a
// stale or cross-team state ID must never be sent to Linear.
func (s *HandoffSession) validateTransition(ctx context.Context, current handoffIssue, destination string) (string, error) {
	if stateAllowed(current.State.Name, s.settings.Tracker.TerminalStates) {
		return "", trackerError("handoff_scope", "active issue is in a terminal state")
	}
	target, ok := s.agentTransitions[strings.ToLower(strings.TrimSpace(current.State.Name))]
	if !ok || !strings.EqualFold(target, destination) {
		return "", trackerError("handoff_scope", "requested Linear transition is not configured for the active issue state")
	}
	if stateAllowed(target, s.settings.Tracker.TerminalStates) {
		return "", trackerError("handoff_scope", "configured Linear transition targets a terminal state")
	}
	states, err := s.resolveTeamStates(ctx, current.TeamID())
	if err != nil {
		return "", err
	}
	sourceID, sourceOK := states[strings.ToLower(strings.TrimSpace(current.State.Name))]
	targetID, targetOK := states[strings.ToLower(strings.TrimSpace(target))]
	if !sourceOK || sourceID != current.StateID() || !targetOK {
		return "", trackerError("handoff_scope", "configured Linear transition is stale or outside the active issue team")
	}
	return targetID, nil
}

func (s *HandoffSession) resolveTeamStates(ctx context.Context, teamID string) (map[string]string, error) {
	response, err := requestWithSettings(ctx, s.client, s.settings, handoffStatesQuery, map[string]any{"teamID": teamID})
	if err != nil {
		return nil, err
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
	if err := json.Unmarshal(response, &payload); err != nil || payload.Data.Team == nil || strings.TrimSpace(payload.Data.Team.ID) != strings.TrimSpace(teamID) {
		return nil, trackerError("handoff_scope", "Linear did not return the active issue team")
	}
	states := make(map[string]string, len(payload.Data.Team.States.Nodes))
	for _, state := range payload.Data.Team.States.Nodes {
		name, id := strings.ToLower(strings.TrimSpace(state.Name)), strings.TrimSpace(state.ID)
		if name == "" || id == "" {
			return nil, trackerError("handoff_scope", "Linear returned an invalid active issue team state")
		}
		if _, exists := states[name]; exists {
			return nil, trackerError("handoff_scope", "Linear returned ambiguous active issue team states")
		}
		states[name] = id
	}
	return states, nil
}

// handoff coordinates the repository-owned completion comment and transition.
// The exact comment is durable reconciliation state in Linear: retries first
// discover it, so a failed or ambiguous transition never duplicates delivery.
func (s *HandoffSession) handoff(ctx context.Context) error {
	s.handoffMu.Lock()
	defer s.handoffMu.Unlock()
	return s.handoffLocked(ctx)
}

func (s *HandoffSession) handoffLocked(ctx context.Context) error {

	current, err := s.readScopedIssue(ctx)
	if err != nil {
		return err
	}
	if !s.isInitialState(current) && !s.isTargetState(current) {
		return trackerError("handoff_scope", "active issue state changed after session setup")
	}

	commented := s.handoffComment == ""
	if !commented {
		commented, err = s.hasHandoffComment(ctx)
		if err != nil {
			return err
		}
	}
	if s.isTargetState(current) && commented {
		return nil
	}
	if !commented {
		// Re-read immediately before mutation. A human scope/state change wins,
		// except for the configured target state, which can be reconciled.
		current, err = s.readScopedIssue(ctx)
		if err != nil {
			return err
		}
		if !s.isInitialState(current) && !s.isTargetState(current) {
			return trackerError("handoff_scope", "active issue state changed after session setup")
		}
		if err := s.comment(ctx, s.handoffComment); err != nil {
			return err
		}
	}
	if s.isTargetState(current) {
		return nil
	}
	if err := s.ensureMutable(ctx); err != nil {
		return err
	}
	return s.transition(ctx)
}

// EnsureActive revalidates the session-bound issue immediately before a
// host-side integration performs an irreversible external mutation.
func (s *HandoffSession) EnsureActive(ctx context.Context) error {
	s.handoffMu.Lock()
	defer s.handoffMu.Unlock()
	current, err := s.readScopedIssue(ctx)
	if err != nil {
		return err
	}
	if !s.isInitialState(current) && !s.isTargetState(current) {
		return trackerError("handoff_scope", "active issue state changed after session setup")
	}
	return nil
}

// EnsureMergeState re-reads the bound issue and requires it still be in the
// exact configured Merging state (mergeState). Unlike EnsureActive, this
// requires an exact match rather than "initial or handoff-target" state: a
// GitHub landing capability must never proceed once a human, an earlier
// hard-gate refusal, or a completed landing has moved the issue elsewhere.
func (s *HandoffSession) EnsureMergeState(ctx context.Context, mergeState string) error {
	s.handoffMu.Lock()
	defer s.handoffMu.Unlock()
	current, err := s.readScopedIssue(ctx)
	if err != nil {
		return err
	}
	if !strings.EqualFold(strings.TrimSpace(current.State.Name), strings.TrimSpace(mergeState)) {
		return trackerError("handoff_scope", "active issue is no longer in the configured Merging state")
	}
	return nil
}

// RefuseLanding attempts the configured mergeState -> In Review fallback
// transition (tracker.provider.agent_transitions), used only after a GitHub
// landing hard gate refuses to merge. It is deliberately narrower than the
// general agentTransition: it only ever moves the issue when its freshly
// read current state is exactly mergeState, so a human (or an earlier call)
// that already moved the issue elsewhere is never overridden. A missing or
// stale configured edge is reported by returning (false, nil): the caller's
// hard-gate refusal must still be honored even when no fallback transition
// is available or currently valid.
func (s *HandoffSession) RefuseLanding(ctx context.Context, mergeState string) (bool, error) {
	s.handoffMu.Lock()
	defer s.handoffMu.Unlock()
	current, err := s.readScopedIssue(ctx)
	if err != nil {
		return false, err
	}
	if !strings.EqualFold(strings.TrimSpace(current.State.Name), strings.TrimSpace(mergeState)) {
		return false, nil
	}
	target, ok := s.agentTransitions[strings.ToLower(strings.TrimSpace(mergeState))]
	if !ok {
		return false, nil
	}
	states, err := s.resolveTeamStates(ctx, current.TeamID())
	if err != nil {
		return false, err
	}
	sourceID, sourceOK := states[strings.ToLower(strings.TrimSpace(mergeState))]
	targetID, targetOK := states[strings.ToLower(strings.TrimSpace(target))]
	if !sourceOK || sourceID != current.StateID() || !targetOK {
		return false, nil
	}
	if err := s.transitionTo(ctx, targetID); err != nil {
		return false, err
	}
	updated, err := s.readScopedIssue(ctx)
	if err != nil {
		return false, err
	}
	if !s.isState(updated, targetID, target) {
		return false, trackerError("handoff_response", "Linear did not apply the configured Merging fallback transition")
	}
	s.issue = updated
	s.log("github_land_refused_to_review")
	return true, nil
}

// CompleteLanding moves the bound issue from the configured Merging state to
// Done after a successful GitHub merge. It is idempotent: already-Done
// returns (false, nil) so a retry after a successful merge but a failed
// completion call reconciles Done without error, and it never transitions an
// issue that has moved to a state other than Done or the exact mergeState.
func (s *HandoffSession) CompleteLanding(ctx context.Context, mergeState string) (bool, error) {
	s.handoffMu.Lock()
	defer s.handoffMu.Unlock()
	current, err := s.readScopedIssue(ctx)
	if err != nil {
		return false, err
	}
	if strings.EqualFold(strings.TrimSpace(current.State.Name), "Done") {
		return false, nil
	}
	if !strings.EqualFold(strings.TrimSpace(current.State.Name), strings.TrimSpace(mergeState)) {
		return false, trackerError("handoff_scope", "linked issue is no longer in the configured Merging state")
	}
	doneName := ""
	for _, state := range s.settings.Tracker.TerminalStates {
		if strings.EqualFold(strings.TrimSpace(state), "Done") {
			doneName = strings.TrimSpace(state)
			break
		}
	}
	if doneName == "" {
		return false, trackerError("invalid_handoff_config", "terminal state Done is required for GitHub landing completion")
	}
	doneID, err := (&Handoff{client: s.client}).resolveStateAllowTerminal(ctx, s.settings, s.issue.TeamID(), doneName)
	if err != nil {
		return false, err
	}
	if err := s.transitionTo(ctx, doneID); err != nil {
		return false, err
	}
	s.log("github_land_completed")
	return true, nil
}

// ReconcileMerged reconciles the bound issue to Done after a linked pull
// request is observed merged by the poll loop rather than by github_land_pr
// (a human merged it directly on GitHub). It is the poll-loop counterpart to
// Complete and CompleteLanding, and is idempotent with human-wins semantics:
// it moves the issue to Done only when its freshly read current state is the
// configured review handoff target OR the configured Merging state. An
// already-Done issue, or one a human has since moved anywhere else, is a quiet
// no-op returning (false, nil) rather than an error. mergeState is the
// configured Merging state, or empty when GitHub landing is not configured for
// the repository, in which case only the review-target path is eligible.
func (s *HandoffSession) ReconcileMerged(ctx context.Context, mergeState string) (bool, error) {
	s.handoffMu.Lock()
	defer s.handoffMu.Unlock()
	current, err := s.readScopedIssue(ctx)
	if err != nil {
		return false, err
	}
	if strings.EqualFold(strings.TrimSpace(current.State.Name), "Done") {
		return false, nil
	}
	eligible := s.isTargetState(current)
	if !eligible && strings.TrimSpace(mergeState) != "" && strings.EqualFold(strings.TrimSpace(current.State.Name), strings.TrimSpace(mergeState)) {
		eligible = true
	}
	if !eligible {
		return false, nil
	}
	doneName := ""
	for _, state := range s.settings.Tracker.TerminalStates {
		if strings.EqualFold(strings.TrimSpace(state), "Done") {
			doneName = strings.TrimSpace(state)
			break
		}
	}
	if doneName == "" {
		return false, trackerError("invalid_handoff_config", "terminal state Done is required for GitHub completion")
	}
	doneID, err := (&Handoff{client: s.client}).resolveStateAllowTerminal(ctx, s.settings, s.issue.TeamID(), doneName)
	if err != nil {
		return false, err
	}
	if err := s.transitionTo(ctx, doneID); err != nil {
		return false, err
	}
	s.log("github_merge_reconciled")
	return true, nil
}

// LinkAndHandoff adds the fixed PR URL exactly once, then reconciles the
// repository-owned review handoff. The URL is supplied by the trusted GitHub
// adapter, never by Codex.
func (s *HandoffSession) LinkAndHandoff(ctx context.Context, prURL string) error {
	s.handoffMu.Lock()
	defer s.handoffMu.Unlock()
	prURL = strings.TrimSpace(prURL)
	if err := validateComment(prURL); err != nil || !strings.HasPrefix(prURL, "https://") {
		return trackerError("handoff_request", "pull request URL is invalid")
	}
	current, err := s.readScopedIssue(ctx)
	if err != nil {
		return err
	}
	if !s.isInitialState(current) && !s.isTargetState(current) {
		return trackerError("handoff_scope", "active issue state changed after session setup")
	}
	linked, err := s.hasComment(ctx, prURL)
	if err != nil {
		return err
	}
	if !linked {
		if !s.isInitialState(current) {
			return trackerError("handoff_scope", "review issue is missing its bound pull request link")
		}
		if err := s.comment(ctx, prURL); err != nil {
			return err
		}
		s.log("pull_request_linked")
	}
	if err := s.handoffLocked(ctx); err != nil {
		return err
	}
	s.log("pull_request_handoff_complete")
	return nil
}

// Complete moves the bound review issue to the configured Done state. It
// returns true only for the call that performed the mutation.
func (s *HandoffSession) Complete(ctx context.Context) (bool, error) {
	s.handoffMu.Lock()
	defer s.handoffMu.Unlock()
	current, err := s.readScopedIssue(ctx)
	if err != nil {
		return false, err
	}
	if strings.EqualFold(strings.TrimSpace(current.State.Name), "Done") {
		return false, nil
	}
	if !s.isTargetState(current) {
		return false, trackerError("handoff_scope", "linked issue is no longer in the configured review state")
	}
	doneName := ""
	for _, state := range s.settings.Tracker.TerminalStates {
		if strings.EqualFold(strings.TrimSpace(state), "Done") {
			doneName = strings.TrimSpace(state)
			break
		}
	}
	if doneName == "" {
		return false, trackerError("invalid_handoff_config", "terminal state Done is required for GitHub completion")
	}
	doneID, err := (&Handoff{client: s.client}).resolveStateAllowTerminal(ctx, s.settings, s.issue.TeamID(), doneName)
	if err != nil {
		return false, err
	}
	if err := s.transitionTo(ctx, doneID); err != nil {
		return false, err
	}
	s.log("github_merge_completed")
	return true, nil
}

func (s *HandoffSession) log(outcome string) {
	if s.logger == nil {
		return
	}
	s.logger.Info("Linear handoff", "outcome", outcome, "issue_id", s.issue.ID, "issue_identifier", s.issue.Identifier)
}

// ensureMutable re-reads the bound issue immediately before each mutation. A
// human change since session setup (including a transition to Done) wins over
// the agent: the request is rejected before any mutation is sent.
func (s *HandoffSession) ensureMutable(ctx context.Context) error {
	current, err := s.readScopedIssue(ctx)
	if err != nil {
		return err
	}
	if !s.isInitialState(current) {
		return trackerError("handoff_scope", "active issue state changed after session setup")
	}
	if stateAllowed(current.State.Name, s.settings.Tracker.ActiveStates) {
		return nil
	}
	return trackerError("handoff_scope", "active issue is no longer in a workflow active state")
}

func (s *HandoffSession) readScopedIssue(ctx context.Context) (handoffIssue, error) {
	current, err := readHandoffIssue(ctx, s.client, s.settings, s.issue.ID)
	if err != nil {
		return handoffIssue{}, err
	}
	if current.ID != s.issue.ID || current.ProjectSlug() != s.issue.ProjectSlug() || current.ProjectID() != s.issue.ProjectID() || current.TeamID() != s.issue.TeamID() {
		return handoffIssue{}, trackerError("handoff_scope", "active issue scope changed after session setup")
	}
	return current, nil
}

func (s *HandoffSession) isInitialState(issue handoffIssue) bool {
	return issue.StateID() == s.issue.StateID() && strings.EqualFold(strings.TrimSpace(issue.State.Name), strings.TrimSpace(s.issue.State.Name))
}

func (s *HandoffSession) isTargetState(issue handoffIssue) bool {
	return issue.StateID() == s.targetStateID && strings.EqualFold(strings.TrimSpace(issue.State.Name), strings.TrimSpace(s.settings.Tracker.HandoffState))
}

func (s *HandoffSession) isState(issue handoffIssue, stateID, stateName string) bool {
	return issue.StateID() == strings.TrimSpace(stateID) && strings.EqualFold(strings.TrimSpace(issue.State.Name), strings.TrimSpace(stateName))
}

func stateAllowed(state string, allowed []string) bool {
	for _, candidate := range allowed {
		if strings.EqualFold(strings.TrimSpace(state), strings.TrimSpace(candidate)) {
			return true
		}
	}
	return false
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
	return s.transitionTo(ctx, s.targetStateID)
}

func (s *HandoffSession) transitionTo(ctx context.Context, stateID string) error {
	response, err := requestWithSettings(ctx, s.client, s.settings, handoffTransitionQuery, map[string]any{
		"issueID": s.issue.ID, "stateID": stateID,
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

func (s *HandoffSession) hasHandoffComment(ctx context.Context) (bool, error) {
	return s.hasComment(ctx, s.handoffComment)
}

func (s *HandoffSession) hasComment(ctx context.Context, expected string) (bool, error) {
	after := any(nil)
	seen := map[string]bool{}
	for {
		response, err := requestWithSettings(ctx, s.client, s.settings, handoffCommentsQuery, map[string]any{
			"issueID": s.issue.ID, "first": pageSize, "after": after,
		})
		if err != nil {
			return false, err
		}
		var payload struct {
			Data struct {
				Issue *struct {
					ID      string `json:"id"`
					Project *struct {
						SlugID string `json:"slugId"`
					} `json:"project"`
					Team *struct {
						ID string `json:"id"`
					} `json:"team"`
					Comments struct {
						Nodes []struct {
							Body string `json:"body"`
						} `json:"nodes"`
						PageInfo struct {
							HasNextPage bool    `json:"hasNextPage"`
							EndCursor   *string `json:"endCursor"`
						} `json:"pageInfo"`
					} `json:"comments"`
				} `json:"issue"`
			} `json:"data"`
		}
		if err := json.Unmarshal(response, &payload); err != nil || payload.Data.Issue == nil {
			return false, trackerError("handoff_response", "Linear returned invalid issue comments")
		}
		issue := payload.Data.Issue
		if issue.ID != s.issue.ID || issue.Project == nil || strings.TrimSpace(issue.Project.SlugID) != s.issue.ProjectSlug() || issue.Team == nil || strings.TrimSpace(issue.Team.ID) != s.issue.TeamID() {
			return false, trackerError("handoff_scope", "Linear returned comments outside the active issue scope")
		}
		for _, comment := range issue.Comments.Nodes {
			if strings.TrimSpace(comment.Body) == strings.TrimSpace(expected) {
				return true, nil
			}
		}
		if !issue.Comments.PageInfo.HasNextPage {
			return false, nil
		}
		if issue.Comments.PageInfo.EndCursor == nil {
			return false, trackerError("handoff_response", "Linear returned invalid comment pagination")
		}
		cursor := strings.TrimSpace(*issue.Comments.PageInfo.EndCursor)
		if cursor == "" || seen[cursor] {
			return false, trackerError("handoff_response", "Linear returned invalid comment pagination")
		}
		seen[cursor] = true
		after = cursor
	}
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
	return h.resolveStateWithPolicy(ctx, s, teamID, target, false)
}

func (h *Handoff) resolveStateAllowTerminal(ctx context.Context, s config.Settings, teamID, target string) (string, error) {
	return h.resolveStateWithPolicy(ctx, s, teamID, target, true)
}

func (h *Handoff) resolveStateWithPolicy(ctx context.Context, s config.Settings, teamID, target string, allowTerminal bool) (string, error) {
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
				if !allowTerminal && strings.EqualFold(strings.TrimSpace(state.Name), strings.TrimSpace(terminal)) {
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
		ID     string `json:"id"`
		SlugID string `json:"slugId"`
	} `json:"project"`
	Team *struct {
		ID string `json:"id"`
	} `json:"team"`
	State *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"state"`
}

func (i handoffIssue) valid() error {
	if strings.TrimSpace(i.ID) == "" || strings.TrimSpace(i.Identifier) == "" || strings.TrimSpace(i.Title) == "" || i.Project == nil || i.Team == nil || i.State == nil || i.StateID() == "" {
		return trackerError("handoff_scope", "Linear returned an incomplete active issue")
	}
	return nil
}
func (i handoffIssue) ProjectSlug() string { return strings.TrimSpace(i.Project.SlugID) }
func (i handoffIssue) ProjectID() string   { return strings.TrimSpace(i.Project.ID) }
func (i handoffIssue) TeamID() string      { return strings.TrimSpace(i.Team.ID) }
func (i handoffIssue) StateID() string     { return strings.TrimSpace(i.State.ID) }
func (i handoffIssue) toDomain() domain.Issue {
	return domain.Issue{ID: i.ID, Identifier: i.Identifier, Title: i.Title, Description: i.Description, URL: i.URL, State: i.State.Name}
}
func (i handoffIssue) metadata() map[string]string {
	return map[string]string{"id": i.ID, "identifier": i.Identifier, "title": i.Title, "description": i.Description, "url": i.URL, "state": i.State.Name}
}

// Keep every GraphQL operation fixed and intentionally small. Variables are
// generated solely from the already-bound session, never from tool input.
const handoffReadQuery = `query SymphonyLinearHandoffIssue($issueID: String!) { issue(id: $issueID) { id identifier title description url project { id slugId } team { id } state { id name } } }`
const handoffStatesQuery = `query SymphonyLinearHandoffStates($teamID: String!) { team(id: $teamID) { id states { nodes { id name } } } }`
const handoffTransitionQuery = `mutation SymphonyLinearHandoffTransition($issueID: String!, $stateID: String!) { issueUpdate(id: $issueID, input: {stateId: $stateID}) { success } }`
const handoffCommentQuery = `mutation SymphonyLinearHandoffComment($issueID: String!, $body: String!) { commentCreate(input: {issueId: $issueID, body: $body}) { success } }`
const handoffCommentsQuery = `query SymphonyLinearHandoffComments($issueID: String!, $first: Int!, $after: String) { issue(id: $issueID) { id project { slugId } team { id } comments(first: $first, after: $after) { nodes { body } pageInfo { hasNextPage endCursor } } } }`
