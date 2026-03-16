package core

import "github.com/im-pingo/liveforge/pkg/avframe"

// Publisher represents a stream source that feeds AVFrames into a Stream.
type Publisher interface {
	// ID returns a unique identifier for this publisher.
	ID() string
	// MediaInfo returns the codec information for this publisher.
	MediaInfo() *avframe.MediaInfo
	// Close disconnects the publisher.
	Close() error
}
