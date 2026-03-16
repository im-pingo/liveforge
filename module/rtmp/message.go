package rtmp

// RTMP message type IDs.
const (
	MsgSetChunkSize     uint8 = 1
	MsgAbort            uint8 = 2
	MsgAck              uint8 = 3
	MsgUserControl      uint8 = 4
	MsgWindowAckSize    uint8 = 5
	MsgSetPeerBandwidth uint8 = 6
	MsgAudio            uint8 = 8
	MsgVideo            uint8 = 9
	MsgAMF0Data         uint8 = 18
	MsgAMF0Command      uint8 = 20
)

// Default chunk size per RTMP spec.
const DefaultChunkSize = 128

// Message represents a fully reassembled RTMP message.
type Message struct {
	TypeID    uint8
	Length    uint32
	Timestamp uint32
	StreamID  uint32
	Payload   []byte
}
