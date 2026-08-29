// Package linear provides the narrow Linear GraphQL adapter.
package linear

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
	"github.com/pmrrasmussen/symphony/internal/observability"
)

const (
	pageSize        = 50
	maxResponseSize = 1 << 20
)

// Error is a portable, redacted tracker failure. Its Category is one of the
// categories described by the Symphony tracker contract.
type Error struct {
	Category   string
	Message    string
	Retryable  bool
	RetryAfter time.Duration
	oversized  bool
}

// RetryDelay lets schedulers honor a provider backoff without depending on
// Linear-specific error types.
func (e *Error) RetryDelay() time.Duration {
	if e == nil || !e.Retryable {
		return 0
	}
	return e.RetryAfter
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return "linear " + e.Category + ": " + e.Message
}

func trackerError(category, message string) error {
	return &Error{Category: category, Message: message}
}

// RefusesRequest reports whether this error rejected the caller's own request --
// its arguments, a bound they exceeded, or the scope and configuration that
// request needs -- rather than reporting how a provider round trip went. Every
// message under these three categories is a fixed string written in this
// package, with no provider or wire-decoded text in it, so a bounded session
// capability may forward Message to the agent verbatim instead of collapsing it
// into a refusal that names nothing (PMR-183). The categories are enumerated
// rather than inferred: a new category is not forwardable until it is added
// here and its messages have been read.
//
// Every other category -- the tracker_* transport, status, and response
// failures, and the handoff_response ones -- answers false. Those describe the
// host's side of a round trip the agent cannot act on, and are also the only
// ones a hostile provider response has any bearing on.
func (e *Error) RefusesRequest() bool {
	if e == nil {
		return false
	}
	switch e.Category {
	case "handoff_request", "handoff_scope", "invalid_tracker_config":
		return true
	}
	return false
}

// classifyRequestError distinguishes the three ways client.Do can fail so a
// caller-cancelled refresh (routine whenever a run ends mid-request), a
// client-side timeout, and every other transport failure are no longer the
// same undiagnosable string with the same (wrong) non-retryable verdict. The
// caller's ctx is checked first because the 30s http.Client.Timeout in
// newHTTPClient fires through the same request context Go derives for the
// deadline, so a fired client timeout and a cancelled caller context are only
// distinguishable by asking whether the caller's own ctx is the one that gave
// out.
func classifyRequestError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return &Error{Category: "tracker_canceled", Message: "Linear request was canceled"}
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return &Error{Category: "tracker_timeout", Message: "Linear request timed out", Retryable: true}
	}
	return &Error{Category: "tracker_transport", Message: "Linear request failed", Retryable: true}
}

type Tracker struct {
	settings func() config.Settings
	client   *http.Client
	now      func() time.Time
	logger   *slog.Logger

	viewerMu sync.Mutex
	viewer   *viewerResolution
}

type viewerResolution struct {
	key      [sha256.Size]byte
	ready    chan struct{}
	viewerID string
	err      error
}

func New(settings func() config.Settings) *Tracker {
	return &Tracker{settings: settings, client: newHTTPClient(nil), now: time.Now, logger: observability.Logger(nil)}
}

// SetLogger routes host-side transition edge records at the operator log
// handler instead of the process default, so a host-driven Linear state change
// lands in the same structured log as the agent-driven ones.
func (t *Tracker) SetLogger(logger *slog.Logger) {
	if logger != nil {
		t.logger = observability.Logger(logger)
	}
}

func newHTTPClient(transport http.RoundTripper) *http.Client {
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: transport,
		// The Linear key is a bearer credential. Refusing all redirects avoids
		// forwarding it to a different endpoint, including HTTPS-to-HTTP hops.
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// Validate checks the documented Linear profile. The token is deliberately
// never returned in an error message.
func (t *Tracker) Validate() error {
	return validateProvider(t.settings().Tracker.Provider)
}

func validateProvider(p map[string]any) error {
	token, tokenOK := p["api_key"].(string)
	if !tokenOK || strings.TrimSpace(token) == "" {
		return trackerError("missing_tracker_secret", "linear api_key is missing")
	}
	project, projectOK := p["project_slug_id"].(string)
	if !projectOK || strings.TrimSpace(project) == "" {
		return trackerError("invalid_tracker_config", "linear project_slug_id is missing")
	}
	if endpoint, exists := p["endpoint"]; exists {
		value, ok := endpoint.(string)
		if !ok {
			return trackerError("invalid_tracker_config", "linear endpoint must be a string")
		}
		if strings.TrimSpace(value) != "" {
			u, err := url.Parse(value)
			if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
				return trackerError("invalid_tracker_config", "linear endpoint must be an absolute HTTP(S) URL")
			}
			if u.Scheme == "http" && !isLocalHTTPHost(u.Hostname()) {
				return trackerError("invalid_tracker_config", "linear endpoint must use HTTPS unless it is a local test host")
			}
		}
	}
	if assignee, exists := p["assignee"]; exists {
		value, ok := assignee.(string)
		if !ok || strings.TrimSpace(value) == "" {
			return trackerError("invalid_tracker_config", "linear assignee must be a non-empty string when set")
		}
	}
	return nil
}

func (t *Tracker) ListCandidates(ctx context.Context, states []string) ([]domain.Issue, error) {
	return t.listByStates(ctx, states)
}

func (t *Tracker) ListTerminal(ctx context.Context, states []string) ([]domain.Issue, error) {
	return t.listByStates(ctx, states)
}

// GetIssues refreshes only IDs in the configured Linear project. It preserves
// the requested order for records which still exist in that project.
func (t *Tracker) GetIssues(ctx context.Context, ids []string) ([]domain.Issue, error) {
	ids = uniqueNonEmpty(ids)
	if len(ids) == 0 {
		return nil, nil
	}
	s := trackerSnapshot(t.settings())
	if err := validateProvider(s.Tracker.Provider); err != nil {
		t.invalidateViewer()
		return nil, err
	}
	assignee, err := t.assigneeFilter(ctx, s)
	if err != nil {
		return nil, err
	}

	byID := make(map[string]domain.Issue, len(ids))
	requested := make(map[string]bool, len(ids))
	for _, id := range ids {
		requested[id] = true
	}
	for start := 0; start < len(ids); start += pageSize {
		end := start + pageSize
		if end > len(ids) {
			end = len(ids)
		}
		page, err := t.requestIssues(ctx, s, queryByIDs, map[string]any{
			"ids":           ids[start:end],
			"projectSlug":   strings.TrimSpace(stringValue(s.Tracker.Provider["project_slug_id"])),
			"first":         end - start,
			"relationFirst": pageSize,
		})
		if err != nil {
			return nil, err // Do not return a partial refresh.
		}
		issues, err := normalizeIssues(page.Nodes, assignee, true)
		if err != nil {
			return nil, err // Do not hide malformed requested records.
		}
		for _, issue := range issues {
			if !requested[issue.ID] {
				return nil, trackerError("tracker_response", "Linear returned an unexpected issue for a scoped refresh")
			}
			byID[issue.ID] = issue
		}
	}

	out := make([]domain.Issue, 0, len(byID))
	for _, id := range ids {
		if issue, ok := byID[id]; ok {
			out = append(out, issue)
		}
	}
	return out, nil
}

// Transition moves the issue from fromState into toState using the host Linear
// credential. It re-reads the issue (scoped to the configured project) and acts
// on that read alone: the write is sent only while the fresh state still equals
// fromState, so a human who cancelled or reparked the issue after the caller's
// snapshot is never overridden — the same exact-source rule HandoffSession's
// RefuseLanding enforces. It is idempotent: an issue already in toState is a
// no-op. It reuses the same bound GraphQL primitives as the session handoff
// path (scoped read, team state resolution, and the fixed issueUpdate
// mutation), and never resolves a terminal target. It is the host-owned
// transition primitive (used by the coordinator's dispatch-time start move and
// the landing host methods); no model-invokable tool can write the tracker.
func (t *Tracker) Transition(ctx context.Context, issue domain.Issue, fromState, toState string) (domain.TransitionResult, error) {
	fromState, toState = strings.TrimSpace(fromState), strings.TrimSpace(toState)
	if strings.TrimSpace(issue.ID) == "" {
		return domain.TransitionResult{}, trackerError("invalid_transition_request", "issue ID is missing")
	}
	if fromState == "" {
		return domain.TransitionResult{}, trackerError("invalid_transition_request", "expected source state is required")
	}
	if toState == "" {
		return domain.TransitionResult{}, trackerError("invalid_transition_request", "target state is required")
	}
	s := trackerSnapshot(t.settings())
	if err := validateProvider(s.Tracker.Provider); err != nil {
		t.invalidateViewer()
		return domain.TransitionResult{}, err
	}
	projectSlug := strings.TrimSpace(stringValue(s.Tracker.Provider["project_slug_id"]))
	if projectSlug == "" {
		return domain.TransitionResult{}, trackerError("invalid_tracker_config", "linear project_slug_id is missing")
	}
	current, err := readHandoffIssue(ctx, t.client, s, issue.ID)
	if err != nil {
		return domain.TransitionResult{}, err
	}
	if current.ID != issue.ID || current.ProjectSlug() != projectSlug || current.TeamID() == "" {
		return domain.TransitionResult{}, trackerError("transition_scope", "issue is outside the configured Linear project")
	}
	freshState := strings.TrimSpace(current.State.Name)
	if strings.EqualFold(freshState, toState) {
		t.logTransitionSkip(current, toState)
		// Idempotent: the issue is already in the started state. Checked before
		// the source guard so a re-dispatch whose snapshot is one edge stale
		// still reconciles instead of reporting a refusal.
		return domain.TransitionResult{FromState: freshState, Applied: true}, nil
	}
	if !strings.EqualFold(freshState, fromState) {
		t.logTransitionRefused(current, fromState, toState)
		return domain.TransitionResult{FromState: freshState}, nil
	}
	stateID, err := resolveHandoffState(ctx, t.client, s, current.TeamID(), toState)
	if err != nil {
		return domain.TransitionResult{FromState: freshState}, err
	}
	applied, err := applyHandoffTransition(ctx, t.client, s, current.ID, stateID)
	if err != nil {
		return domain.TransitionResult{FromState: freshState}, err
	}
	if !applied {
		return domain.TransitionResult{FromState: freshState}, trackerError("transition_response", "Linear did not accept the transition")
	}
	t.logTransition(current, toState)
	return domain.TransitionResult{FromState: freshState, Applied: true}, nil
}

// logTransition records one performed host-side Linear state change so it is
// reconstructable from the operator log alone: the operation, the from/to
// state NAMES, and the issue. It is redaction-safe — state names and issue
// identifiers only, never issue title, description, or any agent text.
func (t *Tracker) logTransition(from handoffIssue, toState string) {
	if t.logger == nil {
		return
	}
	t.logger.Info("Linear transition",
		"operation", observability.OperationTransition,
		"from_state", strings.TrimSpace(from.State.Name),
		"to_state", strings.TrimSpace(toState),
		"issue_id", from.ID,
		"issue_identifier", from.Identifier,
	)
}

// logTransitionSkip records, at debug level, a host-side transition that was a
// no-op because the issue was already in the target state.
func (t *Tracker) logTransitionSkip(from handoffIssue, toState string) {
	if t.logger == nil {
		return
	}
	t.logger.Debug("Linear transition skipped",
		"operation", observability.OperationTransition,
		"from_state", strings.TrimSpace(from.State.Name),
		"to_state", strings.TrimSpace(toState),
		"issue_id", from.ID,
		"issue_identifier", from.Identifier,
	)
}

// logTransitionRefused records the withheld write when the freshly read state
// is neither the target nor the caller's expected source: someone else moved
// the issue between the caller's read and this call. It is a warning, not a
// skip, because the caller asked for an edge that no longer exists — the human
// move it deferred to is the operator-visible event.
func (t *Tracker) logTransitionRefused(current handoffIssue, fromState, toState string) {
	if t.logger == nil {
		return
	}
	t.logger.Warn("Linear transition refused: source state changed",
		"operation", observability.OperationTransition,
		"from_state", strings.TrimSpace(current.State.Name),
		"expected_from_state", strings.TrimSpace(fromState),
		"to_state", strings.TrimSpace(toState),
		"issue_id", current.ID,
		"issue_identifier", current.Identifier,
	)
}

func (t *Tracker) listByStates(ctx context.Context, states []string) ([]domain.Issue, error) {
	states = uniqueNonEmpty(states)
	if len(states) == 0 {
		return nil, nil
	}
	s := trackerSnapshot(t.settings())
	if err := validateProvider(s.Tracker.Provider); err != nil {
		t.invalidateViewer()
		return nil, err
	}
	assignee, err := t.assigneeFilter(ctx, s)
	if err != nil {
		return nil, err
	}

	var all []domain.Issue
	after := any(nil)
	first := pageSize
	seenCursors := map[string]bool{}
	for {
		page, err := t.requestIssues(ctx, s, queryByStates, map[string]any{
			"projectSlug":   strings.TrimSpace(stringValue(s.Tracker.Provider["project_slug_id"])),
			"stateNames":    states,
			"first":         first,
			"relationFirst": pageSize,
			"after":         after,
		})
		if err != nil {
			if isOversized(err) && first > 1 {
				first = max(1, first/2)
				continue
			}
			return nil, err // Atomic: never expose a partial poll.
		}
		issues, err := normalizeIssues(page.Nodes, assignee, false)
		if err != nil {
			return nil, err
		}
		all = append(all, issues...)
		if page.PageInfo == nil {
			return nil, trackerError("tracker_pagination", "Linear did not provide page information")
		}
		if !page.PageInfo.HasNextPage {
			return all, nil
		}
		cursor := strings.TrimSpace(page.PageInfo.EndCursor)
		if cursor == "" || seenCursors[cursor] {
			return nil, trackerError("tracker_pagination", "Linear returned an invalid page cursor")
		}
		seenCursors[cursor] = true
		after = cursor
	}
}

func (t *Tracker) assigneeFilter(ctx context.Context, s config.Settings) (string, error) {
	provider := s.Tracker.Provider
	configured, exists := provider["assignee"]
	if !exists {
		t.invalidateViewer()
		return "", nil
	}
	value, ok := configured.(string)
	if !ok || strings.TrimSpace(value) == "" {
		return "", trackerError("invalid_tracker_config", "linear assignee must be a non-empty string when set")
	}
	value = strings.TrimSpace(value)
	if value != "me" {
		t.invalidateViewer()
		return value, nil
	}
	return t.cachedViewer(ctx, s)
}

func (t *Tracker) cachedViewer(ctx context.Context, s config.Settings) (string, error) {
	key := viewerCacheKey(s)

	t.viewerMu.Lock()
	resolution := t.viewer
	if resolution == nil || resolution.key != key {
		resolution = &viewerResolution{key: key, ready: make(chan struct{})}
		t.viewer = resolution
		t.viewerMu.Unlock()

		viewerID, err := t.requestViewer(ctx, s)
		if err == nil && viewerID == "" {
			err = trackerError("tracker_response", "Linear did not provide the configured viewer identity")
		}

		t.viewerMu.Lock()
		resolution.viewerID = viewerID
		resolution.err = err
		close(resolution.ready)
		if err != nil && t.viewer == resolution {
			t.viewer = nil
		}
		t.viewerMu.Unlock()
		return viewerID, err
	}
	ready := resolution.ready
	viewerID := resolution.viewerID
	t.viewerMu.Unlock()

	if ready != nil {
		select {
		case <-ctx.Done():
			return "", &Error{Category: "tracker_canceled", Message: "Linear request was canceled"}
		case <-ready:
		}
		viewerID = resolution.viewerID
	}
	return viewerID, resolution.err
}

func (t *Tracker) invalidateViewer() {
	t.viewerMu.Lock()
	t.viewer = nil
	t.viewerMu.Unlock()
}

func viewerCacheKey(s config.Settings) [sha256.Size]byte {
	provider := s.Tracker.Provider
	endpoint := strings.TrimSpace(stringValue(provider["endpoint"]))
	if endpoint == "" {
		endpoint = "https://api.linear.app/graphql"
	}
	payload, _ := json.Marshal(struct {
		Endpoint   string `json:"endpoint"`
		Project    string `json:"project"`
		Credential string `json:"credential"`
		Source     string `json:"source"`
	}{
		Endpoint:   endpoint,
		Project:    strings.TrimSpace(stringValue(provider["project_slug_id"])),
		Credential: stringValue(provider["api_key"]),
		Source:     strings.TrimSpace(stringValue(provider["api_key_file"])),
	})
	return sha256.Sum256(payload)
}

func (t *Tracker) requestViewer(ctx context.Context, s config.Settings) (string, error) {
	resp, err := t.request(ctx, s, viewerQuery, nil)
	if err != nil {
		return "", err
	}
	var data struct {
		Data struct {
			Viewer struct {
				ID string `json:"id"`
			} `json:"viewer"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp, &data); err != nil {
		return "", trackerError("tracker_response", "Linear returned an invalid viewer response")
	}
	return strings.TrimSpace(data.Data.Viewer.ID), nil
}

func (t *Tracker) requestIssues(ctx context.Context, s config.Settings, query string, variables map[string]any) (issueConnection, error) {
	resp, err := t.request(ctx, s, query, variables)
	if err != nil {
		return issueConnection{}, err
	}
	var data struct {
		Data struct {
			Issues issueConnection `json:"issues"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp, &data); err != nil {
		return issueConnection{}, trackerError("tracker_response", "Linear returned an invalid issue response")
	}
	return data.Data.Issues, nil
}

func (t *Tracker) request(ctx context.Context, s config.Settings, query string, variables map[string]any) ([]byte, error) {
	return requestWithSettingsAt(ctx, t.client, s, query, variables, t.now())
}

// requestWithSettings is shared with the session-bound handoff adapter. The
// caller supplies a configuration snapshot so a reload cannot silently widen a
// running Codex session's authority.
func requestWithSettings(ctx context.Context, client *http.Client, s config.Settings, query string, variables map[string]any) ([]byte, error) {
	return requestWithSettingsAt(ctx, client, s, query, variables, time.Now())
}

func requestWithSettingsAt(ctx context.Context, client *http.Client, s config.Settings, query string, variables map[string]any, now time.Time) ([]byte, error) {
	p := s.Tracker.Provider
	if err := validateProvider(p); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return nil, trackerError("tracker_request", "could not encode Linear request")
	}
	endpoint := "https://api.linear.app/graphql"
	if configured := strings.TrimSpace(stringValue(p["endpoint"])); configured != "" {
		endpoint = configured
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, trackerError("tracker_request", "could not create Linear request")
	}
	req.Header.Set("Authorization", stringValue(p["api_key"]))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, classifyRequestError(ctx, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize+1))
	if err != nil {
		return nil, trackerError("tracker_response", "could not read Linear response")
	}
	if len(body) > maxResponseSize {
		return nil, &Error{Category: "tracker_response", Message: "Linear response exceeded the size limit", oversized: true}
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, &Error{Category: "tracker_rate_limited", Message: "Linear rate limited the request", Retryable: true, RetryAfter: retryAfter(resp.Header, now)}
	}
	if resp.StatusCode/100 != 2 {
		return nil, &Error{Category: "tracker_status", Message: fmt.Sprintf("Linear returned HTTP status %d", resp.StatusCode), Retryable: resp.StatusCode >= 500}
	}

	// We must decode GraphQL errors separately so provider error payloads never
	// become logs or public errors (they can contain user-provided issue text).
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors json.RawMessage `json:"errors"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, trackerError("tracker_response", "Linear returned malformed JSON")
	}
	if len(envelope.Errors) > 0 && string(envelope.Errors) != "null" && string(envelope.Errors) != "[]" {
		return nil, trackerError("tracker_response", "Linear returned GraphQL errors")
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return nil, trackerError("tracker_response", "Linear response did not contain data")
	}
	return body, nil
}

func trackerSnapshot(s config.Settings) config.Settings {
	s.Tracker.Provider = config.CloneMap(s.Tracker.Provider)
	s.Tracker.ActiveStates = append([]string(nil), s.Tracker.ActiveStates...)
	s.Tracker.TerminalStates = append([]string(nil), s.Tracker.TerminalStates...)
	return s
}

func isOversized(err error) bool {
	var trackerErr *Error
	return errors.As(err, &trackerErr) && trackerErr.oversized
}

// issueConnection mirrors only the GraphQL fields used by this adapter.
type issueConnection struct {
	Nodes    []linearIssue `json:"nodes"`
	PageInfo *struct {
		HasNextPage bool   `json:"hasNextPage"`
		EndCursor   string `json:"endCursor"`
	} `json:"pageInfo"`
}

type linearIssue struct {
	ID          string          `json:"id"`
	Identifier  string          `json:"identifier"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	Priority    json.RawMessage `json:"priority"`
	State       struct {
		Name string `json:"name"`
	} `json:"state"`
	BranchName string `json:"branchName"`
	URL        string `json:"url"`
	Assignee   *struct {
		ID string `json:"id"`
	} `json:"assignee"`
	Labels struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	InverseRelations struct {
		Nodes []struct {
			Type  string `json:"type"`
			Issue struct {
				ID         string `json:"id"`
				Identifier string `json:"identifier"`
				State      struct {
					Name string `json:"name"`
					Type string `json:"type"`
				} `json:"state"`
			} `json:"issue"`
		} `json:"nodes"`
		PageInfo *struct {
			HasNextPage bool `json:"hasNextPage"`
		} `json:"pageInfo"`
	} `json:"inverseRelations"`
	CreatedAt json.RawMessage `json:"createdAt"`
	UpdatedAt json.RawMessage `json:"updatedAt"`
}

func normalizeIssues(records []linearIssue, assignee string, strict bool) ([]domain.Issue, error) {
	out := make([]domain.Issue, 0, len(records))
	for _, record := range records {
		issue, err := normalizeIssue(record, assignee)
		if err != nil {
			if strict {
				return nil, trackerError("tracker_response", "Linear returned a malformed requested issue")
			}
			// normalizeIssues is a free function with no tracker in hand, so this
			// is the one record here that goes to the process logger. It still
			// goes through the redaction boundary rather than to bare slog: a
			// diagnostic derived from a provider response is exactly what the
			// boundary exists for.
			observability.Logger(nil).Warn("dropping malformed Linear issue record", "error", err)
			continue
		}
		out = append(out, issue)
	}
	return out, nil
}

func normalizeIssue(record linearIssue, assignee string) (domain.Issue, error) {
	id, identifier, title, state := strings.TrimSpace(record.ID), strings.TrimSpace(record.Identifier), strings.TrimSpace(record.Title), strings.TrimSpace(record.State.Name)
	if id == "" || identifier == "" || title == "" || state == "" {
		return domain.Issue{}, errors.New("missing required field")
	}
	labels := make([]string, 0, len(record.Labels.Nodes))
	seenLabels := map[string]bool{}
	for _, label := range record.Labels.Nodes {
		name := config.Norm(label.Name)
		if name != "" && !seenLabels[name] {
			seenLabels[name] = true
			labels = append(labels, name)
		}
	}
	sort.Strings(labels)

	blockers := make([]domain.Blocker, 0, len(record.InverseRelations.Nodes))
	for _, relation := range record.InverseRelations.Nodes {
		if config.Norm(relation.Type) != "blocks" {
			continue
		}
		stateType := nullableString(relation.Issue.State.Type)
		blockers = append(blockers, domain.Blocker{
			ID:           nullableString(relation.Issue.ID),
			Identifier:   nullableString(relation.Issue.Identifier),
			State:        nullableString(relation.Issue.State.Name),
			StateType:    stateType,
			Dispatchable: resolvedBlockerStateTypes[config.Norm(stateType)],
		})
	}

	assigneeID := ""
	if record.Assignee != nil {
		assigneeID = strings.TrimSpace(record.Assignee.ID)
	}
	blockersComplete := record.InverseRelations.PageInfo != nil && !record.InverseRelations.PageInfo.HasNextPage
	return domain.Issue{
		ID:               id,
		Identifier:       identifier,
		Title:            title,
		Description:      record.Description,
		Priority:         optionalInt(record.Priority),
		State:            state,
		BranchName:       nullableString(record.BranchName),
		URL:              nullableString(record.URL),
		AssigneeID:       assigneeID,
		Labels:           labels,
		BlockedBy:        blockers,
		Dispatchable:     dispatchable(state, assigneeID, assignee, blockers, blockersComplete),
		AssigneeMismatch: assignee != "" && assigneeID != assignee,
		CreatedAt:        optionalTime(record.CreatedAt),
		UpdatedAt:        optionalTime(record.UpdatedAt),
	}, nil
}

// resolvedBlockerStateTypes are the Linear workflow-state types a blocker can
// never leave, so an issue parked in one of them is as settled as one in a
// configured terminal state. Deciding by this classification rather than by
// matching State's display name against tracker.terminal_states means a
// resolved status the workflow config does not happen to name -- Duplicate
// today, whatever a team adds next -- still satisfies the blocker instead of
// freezing the blocked issue silently forever.
var resolvedBlockerStateTypes = map[string]bool{
	"completed": true,
	"canceled":  true,
	"cancelled": true,
	"duplicate": true,
}

func dispatchable(state, actualAssignee, configuredAssignee string, blockers []domain.Blocker, blockersComplete bool) bool {
	if configuredAssignee != "" && actualAssignee != configuredAssignee {
		return false
	}
	// Linear's inverse `blocks` relation is a dependency only before initial
	// dispatch. An already in-progress issue remains visible for reconciliation.
	if config.Norm(state) != "todo" {
		return true
	}
	// The relation query is bounded. Never dispatch a Todo issue when Linear
	// indicates (or fails to disprove) that additional blockers exist.
	if !blockersComplete {
		return false
	}
	for _, blocker := range blockers {
		if !blocker.Dispatchable {
			return false
		}
	}
	return true
}

func optionalInt(raw json.RawMessage) *int {
	var value int
	if len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &value) != nil {
		return nil
	}
	return &value
}

func optionalTime(raw json.RawMessage) *time.Time {
	var value string
	if len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &value) != nil {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil
	}
	return &parsed
}

func nullableString(value string) string { return strings.TrimSpace(value) }
func stringValue(value any) string       { text, _ := value.(string); return text }

func uniqueNonEmpty(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

func retryAfter(header http.Header, now time.Time) time.Duration {
	var delay time.Duration
	value := strings.TrimSpace(header.Get("Retry-After"))
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		delay = time.Duration(seconds) * time.Second
	} else if reset, err := http.ParseTime(value); err == nil && reset.After(now) {
		delay = reset.Sub(now)
	}
	if raw := strings.TrimSpace(header.Get("X-RateLimit-Requests-Reset")); raw != "" {
		if value, err := strconv.ParseInt(raw, 10, 64); err == nil {
			reset := time.Unix(value, 0)
			if value > 1_000_000_000_000 {
				reset = time.UnixMilli(value)
			}
			if until := reset.Sub(now); until > delay {
				delay = until
			}
		}
	}
	return max(0, delay)
}

func isLocalHTTPHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

const issueFields = `id identifier title description priority state { name } branchName url assignee { id } labels { nodes { name } } inverseRelations(first: $relationFirst) { nodes { type issue { id identifier state { name type } } } pageInfo { hasNextPage } } createdAt updatedAt`

const queryByStates = `query SymphonyLinearPoll($projectSlug: String!, $stateNames: [String!]!, $first: Int!, $relationFirst: Int!, $after: String) { issues(filter: {project: {slugId: {eq: $projectSlug}}, state: {name: {in: $stateNames}}}, first: $first, after: $after) { nodes { ` + issueFields + ` } pageInfo { hasNextPage endCursor } } }`
const queryByIDs = `query SymphonyLinearIssuesByID($ids: [ID!]!, $projectSlug: String!, $first: Int!, $relationFirst: Int!) { issues(filter: {id: {in: $ids}, project: {slugId: {eq: $projectSlug}}}, first: $first) { nodes { ` + issueFields + ` } } }`
const viewerQuery = `query SymphonyLinearViewer { viewer { id } }`
