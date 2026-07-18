// Package linear provides the narrow Linear GraphQL adapter.
package linear

import (
	"bytes"
	"context"
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
	"time"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
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

type Tracker struct {
	settings func() config.Settings
	client   *http.Client
	now      func() time.Time
}

func New(settings func() config.Settings) *Tracker {
	return &Tracker{settings: settings, client: newHTTPClient(nil), now: time.Now}
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
	project, projectOK := p["project_slug"].(string)
	if !projectOK || strings.TrimSpace(project) == "" {
		return trackerError("invalid_tracker_config", "linear project_slug is missing")
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
			"projectSlug":   strings.TrimSpace(stringValue(s.Tracker.Provider["project_slug"])),
			"first":         end - start,
			"relationFirst": pageSize,
		})
		if err != nil {
			return nil, err // Do not return a partial refresh.
		}
		issues, err := normalizeIssues(page.Nodes, assignee, s.Tracker.TerminalStates, true)
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

func (t *Tracker) listByStates(ctx context.Context, states []string) ([]domain.Issue, error) {
	states = uniqueNonEmpty(states)
	if len(states) == 0 {
		return nil, nil
	}
	s := trackerSnapshot(t.settings())
	if err := validateProvider(s.Tracker.Provider); err != nil {
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
			"projectSlug":   strings.TrimSpace(stringValue(s.Tracker.Provider["project_slug"])),
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
		issues, err := normalizeIssues(page.Nodes, assignee, s.Tracker.TerminalStates, false)
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
		return "", nil
	}
	value, ok := configured.(string)
	if !ok || strings.TrimSpace(value) == "" {
		return "", trackerError("invalid_tracker_config", "linear assignee must be a non-empty string when set")
	}
	value = strings.TrimSpace(value)
	if value != "me" {
		return value, nil
	}
	viewerID, err := t.requestViewer(ctx, s)
	if err != nil {
		return "", err
	}
	if viewerID == "" {
		return "", trackerError("tracker_response", "Linear did not provide the configured viewer identity")
	}
	return viewerID, nil
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
		return nil, trackerError("tracker_request", "Linear request failed")
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
	s.Tracker.Provider = cloneMap(s.Tracker.Provider)
	s.Tracker.ActiveStates = append([]string(nil), s.Tracker.ActiveStates...)
	s.Tracker.TerminalStates = append([]string(nil), s.Tracker.TerminalStates...)
	return s
}

func cloneMap(source map[string]any) map[string]any {
	out := make(map[string]any, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
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

func normalizeIssues(records []linearIssue, assignee string, terminalStates []string, strict bool) ([]domain.Issue, error) {
	out := make([]domain.Issue, 0, len(records))
	for _, record := range records {
		issue, err := normalizeIssue(record, assignee, terminalStates)
		if err != nil {
			if strict {
				return nil, trackerError("tracker_response", "Linear returned a malformed requested issue")
			}
			slog.Warn("dropping malformed Linear issue record", "reason", err.Error())
			continue
		}
		out = append(out, issue)
	}
	return out, nil
}

func normalizeIssue(record linearIssue, assignee string, terminalStates []string) (domain.Issue, error) {
	id, identifier, title, state := strings.TrimSpace(record.ID), strings.TrimSpace(record.Identifier), strings.TrimSpace(record.Title), strings.TrimSpace(record.State.Name)
	if id == "" || identifier == "" || title == "" || state == "" {
		return domain.Issue{}, errors.New("missing required field")
	}
	labels := make([]string, 0, len(record.Labels.Nodes))
	seenLabels := map[string]bool{}
	for _, label := range record.Labels.Nodes {
		name := norm(label.Name)
		if name != "" && !seenLabels[name] {
			seenLabels[name] = true
			labels = append(labels, name)
		}
	}
	sort.Strings(labels)

	blockers := make([]domain.Blocker, 0, len(record.InverseRelations.Nodes))
	for _, relation := range record.InverseRelations.Nodes {
		if norm(relation.Type) != "blocks" {
			continue
		}
		blockers = append(blockers, domain.Blocker{
			ID:         nullableString(relation.Issue.ID),
			Identifier: nullableString(relation.Issue.Identifier),
			State:      nullableString(relation.Issue.State.Name),
		})
	}

	assigneeID := ""
	if record.Assignee != nil {
		assigneeID = strings.TrimSpace(record.Assignee.ID)
	}
	blockersComplete := record.InverseRelations.PageInfo != nil && !record.InverseRelations.PageInfo.HasNextPage
	return domain.Issue{
		ID:           id,
		Identifier:   identifier,
		Title:        title,
		Description:  record.Description,
		Priority:     optionalInt(record.Priority),
		State:        state,
		BranchName:   nullableString(record.BranchName),
		URL:          nullableString(record.URL),
		AssigneeID:   assigneeID,
		Labels:       labels,
		BlockedBy:    blockers,
		Dispatchable: dispatchable(state, assigneeID, assignee, blockers, blockersComplete, terminalStates),
		CreatedAt:    optionalTime(record.CreatedAt),
		UpdatedAt:    optionalTime(record.UpdatedAt),
	}, nil
}

func dispatchable(state, actualAssignee, configuredAssignee string, blockers []domain.Blocker, blockersComplete bool, terminalStates []string) bool {
	if configuredAssignee != "" && actualAssignee != configuredAssignee {
		return false
	}
	// Linear's inverse `blocks` relation is a dependency only before initial
	// dispatch. An already in-progress issue remains visible for reconciliation.
	if norm(state) != "todo" {
		return true
	}
	// The relation query is bounded. Never dispatch a Todo issue when Linear
	// indicates (or fails to disprove) that additional blockers exist.
	if !blockersComplete {
		return false
	}
	terminal := map[string]bool{}
	for _, name := range terminalStates {
		terminal[norm(name)] = true
	}
	for _, blocker := range blockers {
		if blocker.State == "" || !terminal[norm(blocker.State)] {
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
func norm(value string) string           { return strings.ToLower(strings.TrimSpace(value)) }

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

const issueFields = `id identifier title description priority state { name } branchName url assignee { id } labels { nodes { name } } inverseRelations(first: $relationFirst) { nodes { type issue { id identifier state { name } } } pageInfo { hasNextPage } } createdAt updatedAt`

const queryByStates = `query SymphonyLinearPoll($projectSlug: String!, $stateNames: [String!]!, $first: Int!, $relationFirst: Int!, $after: String) { issues(filter: {project: {slugId: {eq: $projectSlug}}, state: {name: {in: $stateNames}}}, first: $first, after: $after) { nodes { ` + issueFields + ` } pageInfo { hasNextPage endCursor } } }`
const queryByIDs = `query SymphonyLinearIssuesByID($ids: [ID!]!, $projectSlug: String!, $first: Int!, $relationFirst: Int!) { issues(filter: {id: {in: $ids}, project: {slugId: {eq: $projectSlug}}}, first: $first) { nodes { ` + issueFields + ` } } }`
const viewerQuery = `query SymphonyLinearViewer { viewer { id } }`
