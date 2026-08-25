package claude

import (
	"bufio"
	"errors"
	"io"
)

// lineReader reads newline-delimited output and discards any line longer than
// maxLine instead of ending the stream.
//
// bufio.Scanner cannot do this: an over-long token is a permanent error, so
// reading stops. That matters more than it looks. A single assistant message or
// tool result is one line and is routinely large, so an over-long line is normal
// traffic here -- and if consumption stopped, the child would block writing to a
// full pipe and the turn would hang until its timeout rather than finishing.
type lineReader struct {
	reader *bufio.Reader
}

func newLineReader(r io.Reader) *lineReader {
	return &lineReader{reader: bufio.NewReaderSize(r, 64<<10)}
}

// next returns the next line. skipped reports that a line exceeded maxLine and
// was discarded; reading continues either way. err is io.EOF at the end.
func (l *lineReader) next() (line []byte, skipped bool, err error) {
	var buffered []byte
	for {
		chunk, readErr := l.reader.ReadSlice('\n')
		if errors.Is(readErr, bufio.ErrBufferFull) {
			// Accumulate only up to the bound; past it, keep consuming so the
			// child never blocks, but retain nothing.
			if len(buffered) < maxLine {
				remaining := maxLine - len(buffered)
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
			return buffered, skipped || len(buffered) >= maxLine, nil
		}
		buffered = append(buffered, chunk...)
		if len(buffered) > maxLine {
			return nil, true, nil
		}
		return buffered, skipped, nil
	}
}
