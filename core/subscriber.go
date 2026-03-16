package core

// StartMode determines how a subscriber receives initial frames.
type StartMode uint8

const (
	StartModeGOP      StartMode = iota + 1 // Start from nearest keyframe
	StartModeRealtime                       // Start from current frame
)

// FeedbackMode determines how subscriber feedback is handled.
type FeedbackMode uint8

const (
	FeedbackAuto        FeedbackMode = iota // Default: auto-select based on subscriber count
	FeedbackPassthrough                      // Forward FB directly to publisher
	FeedbackAggregate                        // Aggregate FBs before forwarding
	FeedbackDrop                             // Discard FB
	FeedbackServerSide                       // Server-side adaptation, don't forward
)

// LayerPrefer determines which simulcast layer to subscribe.
type LayerPrefer uint8

const (
	LayerAuto LayerPrefer = iota
	LayerHigh
	LayerLow
)

// SubscribeOptions configures how a subscriber receives data.
type SubscribeOptions struct {
	StartMode    StartMode
	FeedbackMode FeedbackMode
	VideoLayer   LayerPrefer
	AudioEnabled bool
}

// DefaultSubscribeOptions returns sensible defaults.
func DefaultSubscribeOptions() SubscribeOptions {
	return SubscribeOptions{
		StartMode:    StartModeGOP,
		FeedbackMode: FeedbackAuto,
		VideoLayer:   LayerAuto,
		AudioEnabled: true,
	}
}

// Subscriber represents a stream consumer that receives muxed data.
type Subscriber interface {
	ID() string
	Options() SubscribeOptions
	// OnData is called when a new muxed packet is available (FLV tag, TS packet, etc).
	OnData(data []byte) error
	Close() error
}
