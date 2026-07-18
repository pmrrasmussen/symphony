// Package domain contains tracker-neutral types and the replaceable execution boundaries.
package domain

import (
	"context"
	"time"
)

type Blocker struct {
	ID, Identifier, State string
	Dispatchable          bool
}
type Issue struct {
	ID, Identifier, Title, Description, State, BranchName, URL, AssigneeID string
	NativeRef                                                              any
	Priority                                                               *int
	Labels                                                                 []string
	BlockedBy                                                              []Blocker
	Dispatchable                                                           bool
	CreatedAt, UpdatedAt                                                   *time.Time
}
type Usage struct{ InputTokens, OutputTokens, TotalTokens int64 }
type EventKind string

const (
	EventSessionStarted EventKind = "session_started"
	EventProgress       EventKind = "progress"
	EventUsage          EventKind = "usage"
	EventRateLimit      EventKind = "rate_limit"
	EventBlocked        EventKind = "blocked"
	EventCompleted      EventKind = "completed"
	EventFailed         EventKind = "failed"
)

type Event struct {
	Kind                                 EventKind
	At                                   time.Time
	SessionID, ThreadID, TurnID, Message string
	Usage                                Usage
	RateLimit                            map[string]any
}
type RunStatus string

const (
	RunSucceeded RunStatus = "succeeded"
	RunFailed    RunStatus = "failed"
	RunCanceled  RunStatus = "canceled"
	RunTimedOut  RunStatus = "timed_out"
	RunStalled   RunStatus = "stalled"
	RunBlocked   RunStatus = "blocked"
)

type Run struct {
	IssueID, IssueIdentifier, WorkspacePath, SessionID string
	Attempt                                            int
	StartedAt                                          time.Time
	Status                                             RunStatus
	Error                                              string
	Usage                                              Usage
}

type Tracker interface {
	ListCandidates(context.Context, []string) ([]Issue, error)
	GetIssues(context.Context, []string) ([]Issue, error)
	ListTerminal(context.Context, []string) ([]Issue, error)
}
type AgentRequest struct {
	Issue                         Issue
	Workspace, Prompt, Command    string
	ApprovalPolicy, ThreadSandbox string
	TurnSandboxPolicy             any
	TurnTimeout, ReadTimeout      time.Duration
}
type AgentSession struct{ ID, ThreadID, TurnID string }
type AgentBackend interface {
	Start(context.Context, AgentRequest) (AgentSession, <-chan Event, error)
	Continue(context.Context, AgentSession, string) (<-chan Event, error)
	Cancel(context.Context, AgentSession) error
}
type Workspace struct {
	Path, Key  string
	CreatedNow bool
}
type WorkspaceExecutor interface {
	Prepare(context.Context, Issue) (Workspace, error)
	BeforeRun(context.Context, Workspace, Issue) error
	AfterRun(context.Context, Workspace, Issue)
	Cleanup(context.Context, Issue) error
	Execute(context.Context, Workspace, string, []string) ([]byte, error)
}
