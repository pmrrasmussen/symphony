package claude

import "time"

// Timer is this package's timing seam, and it mirrors internal/coordinator's for
// the same reason: the one bound this backend enforces itself -- a turn's budget,
// see turn.stream -- is scheduled through it, so a test can decide when the
// budget elapses instead of waiting a real one out. The tests that waited proved
// their bound by sleeping longer than it, which made the assertion depend on how
// much CPU the test got (PMR-96, PMR-110).
//
// AfterFunc returns the stop function of the timer it scheduled rather than a
// named handle type. That is deliberate: internal/codex declares the identical
// interface, and because the return type is the builtin func() bool, one fake in
// internal/agenttest satisfies both without either backend importing it. Stop
// must prevent a callback that has not started from running, exactly as
// time.Timer.Stop does.
//
// There is deliberately no Clock beside it. The wall clock is read here only for
// event timestamps, a reported tool-call duration, and a rate-limit reset the CLI
// supplies as an absolute time -- values an operator or the scheduler reads, none
// of which this backend decides anything by, so a second seam would have no
// assertion behind it.
type Timer interface {
	AfterFunc(d time.Duration, f func()) (stop func() bool)
}

type realTimer struct{}

func (realTimer) AfterFunc(d time.Duration, f func()) func() bool {
	return time.AfterFunc(d, f).Stop
}
