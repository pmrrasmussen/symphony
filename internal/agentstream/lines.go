// Package agentstream carries the two pieces of a turn's output path that both
// agent backends need and neither owns: the bounded line framing a child's
// stdout is read through, and the sink that owns a turn's event channel.
package agentstream

import (
	"bufio"
	"errors"
	"io"
)

// MaxLine bounds one line of an agent child's stdout. One line is routinely
// large -- a Claude assistant message or tool result, a Codex item/completed
// carrying aggregated command output -- so an oversized line is normal traffic
// here and is skipped rather than failing the run.
const MaxLine = 8 << 20

// LineReader reads newline-delimited output and discards any line longer than
// MaxLine instead of ending the stream.
//
// bufio.Scanner cannot do this: an over-long token is a permanent error, so
// reading stops. That matters more than it looks. An over-long line is normal
// traffic here (see MaxLine) -- and if consumption stopped, the child would
// block writing to a full pipe and the turn would hang until its timeout rather
// than finishing.
type LineReader struct {
	reader *bufio.Reader
}

func NewLineReader(r io.Reader) *LineReader {
	return &LineReader{reader: bufio.NewReaderSize(r, 64<<10)}
}

// Next returns the next line. skipped reports that a line exceeded MaxLine and
// was discarded; reading continues either way. err is io.EOF at the end.
func (l *LineReader) Next() (line []byte, skipped bool, err error) {
	var buffered []byte
	for {
		chunk, readErr := l.reader.ReadSlice('\n')
		if errors.Is(readErr, bufio.ErrBufferFull) {
			// Accumulate only up to the bound; past it, keep consuming so the
			// child never blocks, but retain nothing.
			if len(buffered) < MaxLine {
				remaining := MaxLine - len(buffered)
				if remaining > len(chunk) {
					remaining = len(chunk)
				}
				buffered = append(buffered, chunk[:remaining]...)
			} else {
				skipped = true
			}
			continue
		}
		if readErr != nil {
			if len(buffered) == 0 && len(chunk) == 0 {
				return nil, false, readErr
			}
			// A final line without a trailing newline.
			buffered = append(buffered, chunk...)
			return buffered, skipped || len(buffered) >= MaxLine, nil
		}
		buffered = append(buffered, chunk...)
		if len(buffered) > MaxLine {
			return nil, true, nil
		}
		return buffered, skipped, nil
	}
}
