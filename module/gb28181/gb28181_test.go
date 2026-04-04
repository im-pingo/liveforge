package gb28181

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/ps"
	pionrtp "github.com/pion/rtp/v2"
)

// TestFullPublishFlow simulates the full GB28181 publish pipeline:
// device register -> catalog response -> RTP/PS packets -> AVFrame emission.
func TestFullPublishFlow(t *testing.T) {
	registry := NewDeviceRegistry(180*time.Second, "")
	defer registry.Stop()
	sessions := NewSessionManager()

	// 1. Device registers
	device := registry.Register("34020000001110000001", "192.168.1.100:5060", "udp")
	if device.Status != DeviceStatusOnline {
		t.Fatalf("device status = %v, want online", device.Status)
	}

	// 2. Catalog response arrives (parse from testdata fixture)
	catalogXML, err := os.ReadFile(filepath.Join("testdata", "catalog_response.xml"))
	if err != nil {
		t.Fatalf("read catalog fixture: %v", err)
	}
	resp, err := ParseCatalogResponse(catalogXML)
	if err != nil {
		t.Fatalf("parse catalog: %v", err)
	}

	// Apply catalog to registry (simulate handler.handleCatalogResponse)
	channels := make(map[string]*Channel)
	for _, item := range resp.DeviceList.Items {
		channels[item.DeviceID] = &Channel{
			ChannelID:    item.DeviceID,
			Name:         item.Name,
			Manufacturer: item.Manufacturer,
			Status:       item.Status,
			PTZType:      item.PTZType,
			Latitude:     item.Latitude,
			Longitude:    item.Longitude,
		}
	}
	registry.UpdateChannels("34020000001110000001", channels)

	// Verify channels populated
	allCh := registry.AllChannels()
	if len(allCh) != 3 {
		t.Fatalf("channels = %d, want 3", len(allCh))
	}

	dev, ch := registry.FindChannel("34020000001320000001")
	if dev == nil || ch == nil {
		t.Fatal("FindChannel returned nil")
	}
	if ch.Name != "Front Gate Camera" {
		t.Errorf("channel name = %q", ch.Name)
	}

	// 3. Simulate incoming INVITE: create session and publisher
	var receivedFrames []*avframe.AVFrame
	pub := NewPublisher("gb28181-34020000001320000001", func(f *avframe.AVFrame) {
		receivedFrames = append(receivedFrames, f)
	})

	session := &MediaSession{
		ID:        "call-001@192.168.1.100",
		DeviceID:  "34020000001110000001",
		ChannelID: "34020000001320000001",
		StreamKey: "gb28181/34020000001320000001",
		Direction: SessionDirectionInbound,
		LocalPort: 40000,
		Transport: "udp",
		State:     SessionStateStreaming,
		Publisher: pub,
	}
	sessions.Add(session)

	// 4. Simulate RTP/PS packet delivery
	muxer := ps.NewMuxer()
	startCode := []byte{0x00, 0x00, 0x00, 0x01}
	sps := []byte{0x67, 0x42, 0x00, 0x1E, 0xAB, 0x40, 0x50}
	pps := []byte{0x68, 0xCE, 0x38, 0x80}
	idr := make([]byte, 100)
	idr[0] = 0x65
	for i := 1; i < len(idr); i++ {
		idr[i] = byte(i)
	}

	var payload []byte
	payload = append(payload, startCode...)
	payload = append(payload, sps...)
	payload = append(payload, startCode...)
	payload = append(payload, pps...)
	payload = append(payload, startCode...)
	payload = append(payload, idr...)

	frame := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 1000, 1000, payload)
	psData, err := muxer.Pack(frame)
	if err != nil {
		t.Fatalf("muxer.Pack: %v", err)
	}

	// Send as single RTP packet with marker
	pkt := &pionrtp.Packet{
		Header: pionrtp.Header{
			Version:        2,
			PayloadType:    96,
			SequenceNumber: 1,
			Timestamp:      90000,
			SSRC:           12345,
			Marker:         true,
		},
		Payload: psData,
	}
	pub.FeedRTP(pkt)

	// 5. Verify frames were emitted
	if len(receivedFrames) == 0 {
		t.Fatal("no frames emitted from publisher")
	}

	hasVideo := false
	for _, f := range receivedFrames {
		if f.MediaType == avframe.MediaTypeVideo {
			hasVideo = true
		}
	}
	if !hasVideo {
		t.Error("no video frame in output")
	}

	// 6. Verify session state
	got := sessions.Get("call-001@192.168.1.100")
	if got == nil {
		t.Fatal("session not found")
	}
	if got.GetState() != SessionStateStreaming {
		t.Errorf("state = %v, want streaming", got.GetState())
	}
}

// TestDeviceKeepaliveTimeout verifies that a device goes offline after keepalive timeout
// and its sessions are closed.
func TestDeviceKeepaliveTimeout(t *testing.T) {
	registry := NewDeviceRegistry(50*time.Millisecond, "")
	defer registry.Stop()
	sessions := NewSessionManager()

	// Register device
	registry.Register("device001", "192.168.1.100:5060", "udp")

	// Add a session for the device
	sessions.Add(&MediaSession{
		ID:        "call-001",
		DeviceID:  "device001",
		ChannelID: "ch001",
		State:     SessionStateStreaming,
	})

	// Wait for keepalive to expire
	time.Sleep(100 * time.Millisecond)

	// Trigger check (simulate monitor loop)
	registry.checkKeepalives(func(deviceID string) {
		sessions.CloseByDevice(deviceID)
	})

	// Device should be offline
	dev := registry.Get("device001")
	if dev.Status != DeviceStatusOffline {
		t.Errorf("device status = %v, want offline", dev.Status)
	}

	// Session should be removed
	if sessions.Get("call-001") != nil {
		t.Error("session should be removed after device offline")
	}
}

// TestCatalogQueryParsing verifies catalog query/response round-trip using testdata fixtures.
func TestCatalogQueryParsing(t *testing.T) {
	registry := NewDeviceRegistry(180*time.Second, "")
	defer registry.Stop()

	// Register the device
	registry.Register("34020000001110000001", "192.168.1.100:5060", "udp")

	// Load and parse catalog response fixture
	catalogXML, err := os.ReadFile(filepath.Join("testdata", "catalog_response.xml"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	resp, err := ParseCatalogResponse(catalogXML)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if resp.SumNum != 3 {
		t.Errorf("SumNum = %d, want 3", resp.SumNum)
	}
	if len(resp.DeviceList.Items) != 3 {
		t.Fatalf("items = %d, want 3", len(resp.DeviceList.Items))
	}

	// Apply to registry
	channels := make(map[string]*Channel)
	for _, item := range resp.DeviceList.Items {
		channels[item.DeviceID] = &Channel{
			ChannelID:    item.DeviceID,
			Name:         item.Name,
			Manufacturer: item.Manufacturer,
			Status:       item.Status,
			PTZType:      item.PTZType,
			Latitude:     item.Latitude,
			Longitude:    item.Longitude,
		}
	}
	registry.UpdateChannels("34020000001110000001", channels)

	// Verify online channels
	allCh := registry.AllChannels()
	if len(allCh) != 3 {
		t.Errorf("total channels = %d, want 3", len(allCh))
	}

	// Verify specific channel
	_, ch := registry.FindChannel("34020000001320000002")
	if ch == nil {
		t.Fatal("channel 002 not found")
	}
	if ch.Name != "Parking Lot Camera" {
		t.Errorf("Name = %q", ch.Name)
	}
	if ch.Manufacturer != "Dahua" {
		t.Errorf("Manufacturer = %q", ch.Manufacturer)
	}
}

// TestKeepaliveFromFixture parses keepalive from testdata and updates registry.
func TestKeepaliveFromFixture(t *testing.T) {
	registry := NewDeviceRegistry(180*time.Second, "")
	defer registry.Stop()

	registry.Register("34020000001110000001", "192.168.1.100:5060", "udp")

	xmlData, err := os.ReadFile(filepath.Join("testdata", "keepalive.xml"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	msg, err := ParseKeepalive(xmlData)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	registry.Keepalive(msg.DeviceID)

	dev := registry.Get("34020000001110000001")
	if dev.Status != DeviceStatusOnline {
		t.Errorf("status = %v after keepalive", dev.Status)
	}
}

// TestAlarmFromFixture parses alarm notification from testdata.
func TestAlarmFromFixture(t *testing.T) {
	xmlData, err := os.ReadFile(filepath.Join("testdata", "alarm_notify.xml"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	alarm, err := ParseAlarmNotify(xmlData)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if alarm.DeviceID != "34020000001110000001" {
		t.Errorf("DeviceID = %q", alarm.DeviceID)
	}
	if alarm.AlarmMethod != "5" {
		t.Errorf("AlarmMethod = %q", alarm.AlarmMethod)
	}
	if alarm.AlarmType != "2" {
		t.Errorf("AlarmType = %q", alarm.AlarmType)
	}
}
