package claude

import "time"

const (
	rateLimitStatusAllowed        = "allowed"
	rateLimitStatusAllowedWarning = "allowed_warning"
	rateLimitStatusRejected       = "rejected"
	rateLimitStatusUnrecognized   = "unrecognized"
)

// rateLimitStatusCategory maps CLI-supplied status text onto the complete,
// host-owned vocabulary that may cross the backend boundary. The raw status is
// still used locally for the CLI's control-flow rules, but never reaches a
// domain.Event or coordinator log.
func rateLimitStatusCategory(status string) string {
	switch status {
	case rateLimitStatusAllowed:
		return rateLimitStatusAllowed
	case rateLimitStatusAllowedWarning:
		return rateLimitStatusAllowedWarning
	case rateLimitStatusRejected:
		return rateLimitStatusRejected
	default:
		return rateLimitStatusUnrecognized
	}
}

// rateLimitRetryAfter converts the CLI's absolute reset time into a duration
// from now, floored at zero so a reset already in the past (clock skew, or a
// reset observed slightly late) never produces a negative retry delay. A
// resetsAt of zero means the CLI reported no reset at all, which the
// scheduler's own floor covers (PMR-131).
func rateLimitRetryAfter(resetsAt int64, now time.Time) time.Duration {
	if resetsAt <= 0 {
		return 0
	}
	if delay := time.Unix(resetsAt, 0).Sub(now); delay > 0 {
		return delay
	}
	return 0
}
