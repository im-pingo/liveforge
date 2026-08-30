package core

import (
	"context"
	"errors"
	"fmt"

	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/util"
)

// ErrReaderOverwritten identifies a reader that lost continuity before it
// could consume the retained frame at its cursor.
var ErrReaderOverwritten = errors.New("stream reader overwritten")

// ReaderOverwrittenError reports how many ring positions were lost. The
// reader is advanced to the live cursor before this error is returned.
type ReaderOverwrittenError struct {
	Count int64
}

func (e *ReaderOverwrittenError) Error() string {
	return fmt.Sprintf("%s by %d frame(s)", ErrReaderOverwritten, e.Count)
}

func (e *ReaderOverwrittenError) Is(target error) bool {
	return target == ErrReaderOverwritten
}

// ReadFrameContext reads one source frame while preserving the atomic
// overwrite result. A continuity loss never returns the retained slot as if
// it were valid media.
func ReadFrameContext(ctx context.Context, reader *util.RingReader[*avframe.AVFrame]) (*avframe.AVFrame, bool, error) {
	if reader == nil {
		return nil, false, errors.New("nil stream reader")
	}
	result := reader.ReadResultContext(ctx)
	if !result.OK {
		return nil, false, nil
	}
	if result.Overwritten > 0 {
		reader.AdvanceToLive()
		return nil, false, &ReaderOverwrittenError{Count: result.Overwritten}
	}
	return result.Value, true, nil
}

// TryReadFrame reads one immediately available source frame with the same
// overwrite contract as ReadFrameContext.
func TryReadFrame(reader *util.RingReader[*avframe.AVFrame]) (*avframe.AVFrame, bool, error) {
	if reader == nil {
		return nil, false, errors.New("nil stream reader")
	}
	result := reader.TryReadResult()
	if !result.OK {
		return nil, false, nil
	}
	if result.Overwritten > 0 {
		reader.AdvanceToLive()
		return nil, false, &ReaderOverwrittenError{Count: result.Overwritten}
	}
	return result.Value, true, nil
}
