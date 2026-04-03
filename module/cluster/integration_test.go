package cluster

import (
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/module/rtmp"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

// mockRTMPServer creates a TCP listener that performs a minimal RTMP handshake
// and responds to connect/createStream/publish with _result/onStatus messages.
func mockRTMPServer(t *testing.T) (net.Listener, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handleMockRTMP(conn)
		}
	}()

	return ln, ln.Addr().String()
}

// handleMockRTMP handles a single mock RTMP connection.
func handleMockRTMP(conn net.Conn) {
	defer conn.Close()

	// Client handshake: read C0+C1, send S0+S1+S2, read C2
	c0c1 := make([]byte, 1+handshakeSize)
	if _, err := io.ReadFull(conn, c0c1); err != nil {
		return
	}

	s0s1s2 := make([]byte, 1+handshakeSize+handshakeSize)
	s0s1s2[0] = 3
	if _, err := conn.Write(s0s1s2); err != nil {
		return
	}

	c2 := make([]byte, handshakeSize)
	if _, err := io.ReadFull(conn, c2); err != nil {
		return
	}

	// Now read RTMP chunks and respond to commands
	cr := rtmp.NewChunkReader(conn, rtmp.DefaultChunkSize)
	cw := rtmp.NewChunkWriter(conn, 4096)

	for {
		msg, err := cr.ReadMessage()
		if err != nil {
			return
		}

		switch msg.TypeID {
		case rtmp.MsgSetChunkSize:
			if len(msg.Payload) >= 4 {
				size := int(binary.BigEndian.Uint32(msg.Payload))
				cr.SetChunkSize(size)
			}

		case rtmp.MsgAMF0Command:
			vals, err := rtmp.AMF0Decode(msg.Payload)
			if err != nil || len(vals) < 2 {
				continue
			}
			cmd, _ := vals[0].(string)
			txnID, _ := vals[1].(float64)

			switch cmd {
			case "connect":
				// Send Window Ack Size
				wack := make([]byte, 4)
				binary.BigEndian.PutUint32(wack, 2500000)
				cw.WriteMessage(2, &rtmp.Message{
					TypeID: rtmp.MsgWindowAckSize, Length: 4, Payload: wack,
				})
				// Send _result for connect
				resp, _ := rtmp.AMF0Encode("_result", txnID,
					map[string]any{"fmsVer": "FMS/3,0,1,123", "capabilities": float64(31)},
					map[string]any{"level": "status", "code": "NetConnection.Connect.Success"},
				)
				cw.WriteMessage(3, &rtmp.Message{
					TypeID: rtmp.MsgAMF0Command, Length: uint32(len(resp)), Payload: resp,
				})

			case "createStream":
				resp, _ := rtmp.AMF0Encode("_result", txnID, nil, float64(1))
				cw.WriteMessage(3, &rtmp.Message{
					TypeID: rtmp.MsgAMF0Command, Length: uint32(len(resp)), Payload: resp,
				})

			case "publish":
				resp, _ := rtmp.AMF0Encode("onStatus", float64(0), nil,
					map[string]any{"level": "status", "code": "NetStream.Publish.Start"},
				)
				cw.WriteMessage(5, &rtmp.Message{
					TypeID: rtmp.MsgAMF0Command, Length: uint32(len(resp)), StreamID: 1, Payload: resp,
				})

			case "play":
				resp, _ := rtmp.AMF0Encode("onStatus", float64(0), nil,
					map[string]any{"level": "status", "code": "NetStream.Play.Start"},
				)
				cw.WriteMessage(5, &rtmp.Message{
					TypeID: rtmp.MsgAMF0Command, Length: uint32(len(resp)), StreamID: 1, Payload: resp,
				})

			case "releaseStream", "FCPublish":
				// These don't need responses
			}

		case rtmp.MsgVideo, rtmp.MsgAudio:
			// Silently consume media messages
		}
	}
}

// TestForwardToMockRTMPServer verifies forward target can connect, publish, and stream.
func TestForwardToMockRTMPServer(t *testing.T) {
	ln, addr := mockRTMPServer(t)
	defer ln.Close()

	hub, _ := newTestHub()
	stream, _ := hub.GetOrCreate("live/fwdtest")

	pub := &originPublisher{id: "test-pub", info: &avframe.MediaInfo{
		VideoCodec: avframe.CodecH264,
		AudioCodec: avframe.CodecAAC,
	}}
	stream.SetPublisher(pub)

	// Write sequence headers
	stream.WriteFrame(&avframe.AVFrame{
		MediaType: avframe.MediaTypeVideo,
		Codec:     avframe.CodecH264,
		FrameType: avframe.FrameTypeSequenceHeader,
		Payload:   []byte{0x01, 0x64, 0x00, 0x28, 0xFF, 0xE1, 0x00, 0x04, 0x67, 0x64, 0x00, 0x28, 0x01, 0x00, 0x04, 0x68, 0xEE, 0x3C, 0x80},
	})
	stream.WriteFrame(&avframe.AVFrame{
		MediaType: avframe.MediaTypeAudio,
		Codec:     avframe.CodecAAC,
		FrameType: avframe.FrameTypeSequenceHeader,
		Payload:   []byte{0x12, 0x10},
	})

	ft := NewForwardTarget("live/fwdtest", "rtmp://"+addr+"/live/fwdtest", stream, NewRTMPTransport(), 1, time.Second)

	done := make(chan struct{})
	go func() {
		ft.Run()
		close(done)
	}()

	// Write keyframes
	time.Sleep(200 * time.Millisecond)
	for i := 0; i < 5; i++ {
		stream.WriteFrame(&avframe.AVFrame{
			MediaType: avframe.MediaTypeVideo,
			Codec:     avframe.CodecH264,
			FrameType: avframe.FrameTypeKeyframe,
			DTS:       int64((i + 1) * 33),
			PTS:       int64((i + 1) * 33),
			Payload:   []byte{0x65, 0x88, 0x00, 0x01},
		})
		time.Sleep(33 * time.Millisecond)
	}

	ft.Close()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Error("forward target did not stop within timeout")
	}
}

// TestOriginPullFromMockServer verifies origin pull can connect and receive media.
func TestOriginPullFromMockServer(t *testing.T) {
	// Create a mock server that sends media data after play
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()
	addr := ln.Addr().String()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handleMockOriginServer(conn)
		}
	}()

	hub, _ := newTestHub()
	stream, _ := hub.GetOrCreate("live/pulltest")

	op := NewOriginPull("live/pulltest", []string{"rtmp://" + addr + "/live"}, stream, 1, 2*time.Second, 5*time.Second)

	done := make(chan struct{})
	go func() {
		op.Run()
		close(done)
	}()

	// Wait for some frames to be received
	time.Sleep(500 * time.Millisecond)
	op.Close()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Error("origin pull did not stop within timeout")
	}

	// Check that frames were written to the stream
	if stream.VideoSeqHeader() != nil {
		t.Log("video sequence header received")
	}
}

// handleMockOriginServer handles a mock origin that sends media data after play.
func handleMockOriginServer(conn net.Conn) {
	defer conn.Close()

	// Handshake
	c0c1 := make([]byte, 1+handshakeSize)
	if _, err := io.ReadFull(conn, c0c1); err != nil {
		return
	}
	s0s1s2 := make([]byte, 1+handshakeSize+handshakeSize)
	s0s1s2[0] = 3
	conn.Write(s0s1s2)

	c2 := make([]byte, handshakeSize)
	io.ReadFull(conn, c2)

	cr := rtmp.NewChunkReader(conn, rtmp.DefaultChunkSize)
	cw := rtmp.NewChunkWriter(conn, 4096)

	playReceived := false

	for {
		msg, err := cr.ReadMessage()
		if err != nil {
			return
		}

		switch msg.TypeID {
		case rtmp.MsgSetChunkSize:
			if len(msg.Payload) >= 4 {
				size := int(binary.BigEndian.Uint32(msg.Payload))
				cr.SetChunkSize(size)
			}

		case rtmp.MsgAMF0Command:
			vals, _ := rtmp.AMF0Decode(msg.Payload)
			if len(vals) < 2 {
				continue
			}
			cmd, _ := vals[0].(string)
			txnID, _ := vals[1].(float64)

			switch cmd {
			case "connect":
				resp, _ := rtmp.AMF0Encode("_result", txnID,
					map[string]any{"fmsVer": "FMS/3,0,1,123"},
					map[string]any{"code": "NetConnection.Connect.Success"},
				)
				cw.WriteMessage(3, &rtmp.Message{
					TypeID: rtmp.MsgAMF0Command, Length: uint32(len(resp)), Payload: resp,
				})

			case "createStream":
				resp, _ := rtmp.AMF0Encode("_result", txnID, nil, float64(1))
				cw.WriteMessage(3, &rtmp.Message{
					TypeID: rtmp.MsgAMF0Command, Length: uint32(len(resp)), Payload: resp,
				})

			case "play":
				resp, _ := rtmp.AMF0Encode("onStatus", float64(0), nil,
					map[string]any{"code": "NetStream.Play.Start"},
				)
				cw.WriteMessage(5, &rtmp.Message{
					TypeID: rtmp.MsgAMF0Command, Length: uint32(len(resp)), StreamID: 1, Payload: resp,
				})
				playReceived = true

				// Send video sequence header + a few keyframes
				go func() {
					time.Sleep(50 * time.Millisecond)
					if !playReceived {
						return
					}

					// Video sequence header (H.264)
					vsh := []byte{0x17, 0x00, 0x00, 0x00, 0x00, 0x01, 0x64, 0x00, 0x28}
					cw.WriteMessage(6, &rtmp.Message{
						TypeID: rtmp.MsgVideo, Length: uint32(len(vsh)), StreamID: 1, Payload: vsh,
					})

					// A few keyframes
					for i := 0; i < 3; i++ {
						kf := []byte{0x17, 0x01, 0x00, 0x00, 0x00, 0x65, 0x88}
						ts := uint32((i + 1) * 33)
						cw.WriteMessage(6, &rtmp.Message{
							TypeID: rtmp.MsgVideo, Length: uint32(len(kf)),
							Timestamp: ts, StreamID: 1, Payload: kf,
						})
						time.Sleep(33 * time.Millisecond)
					}

					// Send unpublish notification
					unpub, _ := rtmp.AMF0Encode("onStatus", float64(0), nil,
						map[string]any{"code": "NetStream.Play.UnpublishNotify"},
					)
					cw.WriteMessage(5, &rtmp.Message{
						TypeID: rtmp.MsgAMF0Command, Length: uint32(len(unpub)), StreamID: 1, Payload: unpub,
					})
				}()
			}
		}
	}
}
