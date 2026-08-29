package agentstream

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// TestAnOversizedLineIsSkippedAndReadingContinues is the property both backends
// depend on and bufio.Scanner does not have: the line past the bound is reported
// as skipped rather than as a permanent error, and the lines around it are still
// delivered. Consumption continuing is the load-bearing half -- a reader that
// stopped would block the child on a full pipe.
func TestAnOversizedLineIsSkippedAndReadingContinues(t *testing.T) {
	input := "first\n" + strings.Repeat("x", MaxLine+1) + "\nlast\n"
	lines := NewLineReader(strings.NewReader(input))

	line, skipped, err := lines.Next()
	if err != nil || skipped || string(line) != "first\n" {
		t.Fatalf("line=%q skipped=%v err=%v", line, skipped, err)
	}
	if line, skipped, err = lines.Next(); err != nil || !skipped || line != nil {
		t.Fatalf("oversized line=%q skipped=%v err=%v", line, skipped, err)
	}
	if line, skipped, err = lines.Next(); err != nil || skipped || string(line) != "last\n" {
		t.Fatalf("line=%q skipped=%v err=%v", line, skipped, err)
	}
	if _, _, err = lines.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("err=%v, want EOF", err)
	}
}

// TestAFinalLineWithoutANewlineIsDelivered keeps the last thing a child said
// before it died from being dropped for want of a terminator.
func TestAFinalLineWithoutANewlineIsDelivered(t *testing.T) {
	lines := NewLineReader(strings.NewReader("only"))
	line, skipped, err := lines.Next()
	if err != nil || skipped || string(line) != "only" {
		t.Fatalf("line=%q skipped=%v err=%v", line, skipped, err)
	}
	if _, _, err = lines.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("err=%v, want EOF", err)
	}
}
