package gb28181

import (
	"encoding/json"
	"log/slog"
	"os"
	"sync"
	"time"
)

// DeviceRegistry stores and manages GB28181 device registrations.
type DeviceRegistry struct {
	mu      sync.RWMutex
	devices map[string]*Device

	keepaliveTimeout time.Duration
	dumpFile         string
	done             chan struct{}
}

// NewDeviceRegistry creates a new device registry.
func NewDeviceRegistry(keepaliveTimeout time.Duration, dumpFile string) *DeviceRegistry {
	if keepaliveTimeout <= 0 {
		keepaliveTimeout = 180 * time.Second
	}
	r := &DeviceRegistry{
		devices:          make(map[string]*Device),
		keepaliveTimeout: keepaliveTimeout,
		dumpFile:         dumpFile,
		done:             make(chan struct{}),
	}
	return r
}

// Register adds or updates a device registration.
func (r *DeviceRegistry) Register(deviceID, remoteAddr, transport string) *Device {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	d, ok := r.devices[deviceID]
	if !ok {
		d = &Device{
			DeviceID:     deviceID,
			RegisteredAt: now,
			Channels:     make(map[string]*Channel),
		}
		r.devices[deviceID] = d
		slog.Info("device registered", "module", "gb28181", "device", deviceID)
	}

	d.RemoteAddr = remoteAddr
	d.Transport = transport
	d.LastKeepalive = now
	d.Status = DeviceStatusOnline
	return d
}

// Unregister removes a device.
func (r *DeviceRegistry) Unregister(deviceID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.devices, deviceID)
	slog.Info("device unregistered", "module", "gb28181", "device", deviceID)
}

// Keepalive updates the keepalive timestamp for a device.
func (r *DeviceRegistry) Keepalive(deviceID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if d, ok := r.devices[deviceID]; ok {
		d.LastKeepalive = time.Now()
		d.Status = DeviceStatusOnline
	}
}

// Get returns a device by ID, or nil if not found.
func (r *DeviceRegistry) Get(deviceID string) *Device {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.devices[deviceID]
}

// UpdateChannels replaces the channels for a device.
func (r *DeviceRegistry) UpdateChannels(deviceID string, channels map[string]*Channel) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if d, ok := r.devices[deviceID]; ok {
		d.Channels = channels
		slog.Info("channels updated", "module", "gb28181", "device", deviceID, "count", len(channels))
	}
}

// FindChannel searches all devices for a channel by ID.
func (r *DeviceRegistry) FindChannel(channelID string) (*Device, *Channel) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, d := range r.devices {
		if ch, ok := d.Channels[channelID]; ok {
			return d, ch
		}
	}
	return nil, nil
}

// All returns a snapshot of all devices.
func (r *DeviceRegistry) All() []*Device {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]*Device, 0, len(r.devices))
	for _, d := range r.devices {
		result = append(result, d)
	}
	return result
}

// AllChannels returns all channels across all devices.
func (r *DeviceRegistry) AllChannels() []*Channel {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var result []*Channel
	for _, d := range r.devices {
		for _, ch := range d.Channels {
			result = append(result, ch)
		}
	}
	return result
}

// StartMonitor starts the background keepalive checker.
// The onOffline callback is invoked for each device that goes offline.
func (r *DeviceRegistry) StartMonitor(onOffline func(deviceID string)) {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				r.checkKeepalives(onOffline)
			case <-r.done:
				return
			}
		}
	}()
}

// Stop stops the monitor and optionally dumps to file.
func (r *DeviceRegistry) Stop() {
	close(r.done)
	if r.dumpFile != "" {
		r.DumpToFile()
	}
}

func (r *DeviceRegistry) checkKeepalives(onOffline func(string)) {
	r.mu.Lock()
	var offlineDevices []string
	now := time.Now()
	for id, d := range r.devices {
		if d.Status == DeviceStatusOnline && now.Sub(d.LastKeepalive) > r.keepaliveTimeout {
			d.Status = DeviceStatusOffline
			offlineDevices = append(offlineDevices, id)
			slog.Warn("device offline", "module", "gb28181", "device", id,
				"channels", len(d.Channels))
		}
	}
	r.mu.Unlock()

	for _, id := range offlineDevices {
		if onOffline != nil {
			onOffline(id)
		}
	}
}

// DumpToFile writes the device registry to a JSON file.
func (r *DeviceRegistry) DumpToFile() {
	if r.dumpFile == "" {
		return
	}
	r.mu.RLock()
	data, err := json.MarshalIndent(r.devices, "", "  ")
	r.mu.RUnlock()
	if err != nil {
		slog.Error("device registry dump failed", "module", "gb28181", "error", err)
		return
	}
	if err := os.WriteFile(r.dumpFile, data, 0o644); err != nil {
		slog.Error("device registry dump write failed", "module", "gb28181", "error", err)
	}
}

// RestoreFromFile loads the device registry from a JSON file.
func (r *DeviceRegistry) RestoreFromFile() {
	if r.dumpFile == "" {
		return
	}
	data, err := os.ReadFile(r.dumpFile)
	if err != nil {
		return // file doesn't exist is fine
	}
	var devices map[string]*Device
	if err := json.Unmarshal(data, &devices); err != nil {
		slog.Warn("device registry restore failed", "module", "gb28181", "error", err)
		return
	}
	r.mu.Lock()
	r.devices = devices
	// Mark all restored devices as offline until they re-register
	for _, d := range r.devices {
		d.Status = DeviceStatusOffline
	}
	r.mu.Unlock()
	slog.Info("device registry restored", "module", "gb28181", "devices", len(devices))
}
