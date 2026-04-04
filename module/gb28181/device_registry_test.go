package gb28181

import (
	"os"
	"testing"
	"time"
)

func TestRegistryRegisterAndGet(t *testing.T) {
	r := NewDeviceRegistry(180*time.Second, "")
	defer r.Stop()

	d := r.Register("device001", "192.168.1.100:5060", "udp")
	if d.DeviceID != "device001" {
		t.Errorf("DeviceID = %q", d.DeviceID)
	}
	if d.Status != DeviceStatusOnline {
		t.Errorf("Status = %v", d.Status)
	}

	got := r.Get("device001")
	if got == nil {
		t.Fatal("Get returned nil")
	}
	if got.RemoteAddr != "192.168.1.100:5060" {
		t.Errorf("RemoteAddr = %q", got.RemoteAddr)
	}

	if r.Get("nonexistent") != nil {
		t.Error("expected nil for nonexistent device")
	}
}

func TestRegistryUnregister(t *testing.T) {
	r := NewDeviceRegistry(180*time.Second, "")
	defer r.Stop()

	r.Register("device001", "192.168.1.100:5060", "udp")
	r.Unregister("device001")

	if r.Get("device001") != nil {
		t.Error("device should be removed after unregister")
	}
}

func TestRegistryKeepalive(t *testing.T) {
	r := NewDeviceRegistry(180*time.Second, "")
	defer r.Stop()

	r.Register("device001", "192.168.1.100:5060", "udp")
	time.Sleep(10 * time.Millisecond)

	r.Keepalive("device001")
	d := r.Get("device001")
	if d.Status != DeviceStatusOnline {
		t.Errorf("Status = %v after keepalive", d.Status)
	}
}

func TestRegistryOfflineDetection(t *testing.T) {
	r := NewDeviceRegistry(50*time.Millisecond, "")
	defer r.Stop()

	r.Register("device001", "192.168.1.100:5060", "udp")

	time.Sleep(100 * time.Millisecond)

	var offlineDevice string
	r.checkKeepalives(func(id string) {
		offlineDevice = id
	})

	if offlineDevice != "device001" {
		t.Errorf("expected device001 to go offline, got %q", offlineDevice)
	}

	d := r.Get("device001")
	if d.Status != DeviceStatusOffline {
		t.Errorf("Status = %v, want offline", d.Status)
	}
}

func TestRegistryChannels(t *testing.T) {
	r := NewDeviceRegistry(180*time.Second, "")
	defer r.Stop()

	r.Register("device001", "192.168.1.100:5060", "udp")

	channels := map[string]*Channel{
		"ch001": {ChannelID: "ch001", Name: "Camera 1", Status: "ON"},
		"ch002": {ChannelID: "ch002", Name: "Camera 2", Status: "OFF"},
	}
	r.UpdateChannels("device001", channels)

	dev, ch := r.FindChannel("ch001")
	if dev == nil || ch == nil {
		t.Fatal("FindChannel returned nil")
	}
	if ch.Name != "Camera 1" {
		t.Errorf("Name = %q", ch.Name)
	}

	dev, ch = r.FindChannel("nonexistent")
	if dev != nil || ch != nil {
		t.Error("expected nil for nonexistent channel")
	}

	all := r.AllChannels()
	if len(all) != 2 {
		t.Errorf("AllChannels len = %d, want 2", len(all))
	}
}

func TestRegistryDumpRestore(t *testing.T) {
	tmpFile := t.TempDir() + "/devices.json"

	r := NewDeviceRegistry(180*time.Second, tmpFile)
	r.Register("device001", "192.168.1.100:5060", "udp")
	r.UpdateChannels("device001", map[string]*Channel{
		"ch001": {ChannelID: "ch001", Name: "Camera 1"},
	})
	r.DumpToFile()
	r.Stop()

	// Verify file exists
	if _, err := os.Stat(tmpFile); err != nil {
		t.Fatalf("dump file not created: %v", err)
	}

	// Restore into new registry
	r2 := NewDeviceRegistry(180*time.Second, tmpFile)
	r2.RestoreFromFile()
	defer r2.Stop()

	d := r2.Get("device001")
	if d == nil {
		t.Fatal("device not restored")
	}
	// Restored devices should be offline
	if d.Status != DeviceStatusOffline {
		t.Errorf("restored status = %v, want offline", d.Status)
	}
}

func TestRegistryAll(t *testing.T) {
	r := NewDeviceRegistry(180*time.Second, "")
	defer r.Stop()

	r.Register("d1", "1.1.1.1:5060", "udp")
	r.Register("d2", "2.2.2.2:5060", "tcp")

	all := r.All()
	if len(all) != 2 {
		t.Errorf("All() len = %d, want 2", len(all))
	}
}
