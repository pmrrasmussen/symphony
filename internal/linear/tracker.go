// Package linear provides the narrow Linear GraphQL adapter.
package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/pmrrasmussen/symphony/internal/config"
	"github.com/pmrrasmussen/symphony/internal/domain"
)

type Tracker struct {
	settings func() config.Settings
	client   *http.Client
}

func New(settings func() config.Settings) *Tracker {
	return &Tracker{settings: settings, client: &http.Client{Timeout: 30 * time.Second}}
}
func (t *Tracker) Validate() error {
	p := t.settings().Tracker.Provider
	if token, _ := p["api_key"].(string); strings.TrimSpace(token) == "" {
		return fmt.Errorf("linear api_key is missing")
	}
	if project, _ := p["project_slug"].(string); strings.TrimSpace(project) == "" {
		return fmt.Errorf("linear project_slug is missing")
	}
	if endpoint, _ := p["endpoint"].(string); endpoint != "" && !strings.HasPrefix(endpoint, "https://") && !strings.HasPrefix(endpoint, "http://") {
		return fmt.Errorf("invalid linear endpoint")
	}
	return nil
}
func (t *Tracker) ListCandidates(ctx context.Context, states []string) ([]domain.Issue, error) {
	return t.list(ctx, states, nil)
}
func (t *Tracker) ListTerminal(ctx context.Context, states []string) ([]domain.Issue, error) {
	return t.list(ctx, states, nil)
}
func (t *Tracker) GetIssues(ctx context.Context, ids []string) ([]domain.Issue, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	return t.list(ctx, nil, ids)
}
func (t *Tracker) list(ctx context.Context, states, ids []string) ([]domain.Issue, error) {
	s := t.settings()
	p := s.Tracker.Provider
	token, _ := p["api_key"].(string)
	project, _ := p["project_slug"].(string)
	if err := t.Validate(); err != nil {
		return nil, err
	}
	endpoint := "https://api.linear.app/graphql"
	if e, _ := p["endpoint"].(string); e != "" {
		endpoint = e
	}
	fields := `id identifier title description priority state { name } branchName url assignee { id } labels { nodes { name } } createdAt updatedAt`
	q := ""
	vars := map[string]any{}
	if len(ids) > 0 {
		q = `query Issues($ids:[ID!]!) { issues(filter:{id:{in:$ids}}, first:250) { nodes { ` + fields + ` } } }`
		vars["ids"] = ids
	} else {
		q = `query Issues($project:String!, $states:[String!]!) { issues(filter:{project:{slugId:{eq:$project}},state:{name:{in:$states}}}, first:250) { nodes { ` + fields + ` } } }`
		vars["project"] = project
		vars["states"] = states
	}
	b, err := json.Marshal(map[string]any{"query": q, "variables": vars})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("linear graphql status %s", resp.Status)
	}
	var data struct {
		Data struct {
			Issues struct {
				Nodes []issue `json:"nodes"`
			} `json:"issues"`
		} `json:"data"`
		Errors []struct{ Message string } `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}
	if len(data.Errors) > 0 {
		return nil, fmt.Errorf("linear graphql: %s", data.Errors[0].Message)
	}
	wanted := map[string]bool{}
	for _, v := range states {
		wanted[norm(v)] = true
	}
	wantIDs := map[string]bool{}
	for _, v := range ids {
		wantIDs[v] = true
	}
	out := make([]domain.Issue, 0, len(data.Data.Issues.Nodes))
	for _, x := range data.Data.Issues.Nodes {
		if len(wanted) > 0 && !wanted[norm(x.State.Name)] {
			continue
		}
		if len(wantIDs) > 0 && !wantIDs[x.ID] {
			continue
		}
		out = append(out, x.normalized())
	}
	return out, nil
}

type issue struct {
	ID, Identifier, Title, Description, BranchName, URL string
	Priority                                            *int
	State                                               struct{ Name string }
	Assignee                                            *struct{ ID string }
	Labels                                              struct{ Nodes []struct{ Name string } }
	CreatedAt, UpdatedAt                                time.Time
}

func (x issue) normalized() domain.Issue {
	labels := []string{}
	for _, l := range x.Labels.Nodes {
		labels = append(labels, norm(l.Name))
	}
	var assignee string
	if x.Assignee != nil {
		assignee = x.Assignee.ID
	}
	return domain.Issue{ID: x.ID, Identifier: x.Identifier, Title: x.Title, Description: x.Description, Priority: x.Priority, State: x.State.Name, BranchName: x.BranchName, URL: x.URL, AssigneeID: assignee, Labels: labels, Dispatchable: true, CreatedAt: &x.CreatedAt, UpdatedAt: &x.UpdatedAt}
}
func norm(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
