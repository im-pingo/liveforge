package config

import "time"

// Config is the root configuration for the streaming server.
type Config struct {
	Server ServerConfig `yaml:"server"`
	TLS    TLSConfig    `yaml:"tls"`
	Limits LimitsConfig `yaml:"limits"`
	RTMP   RTMPConfig   `yaml:"rtmp"`
	RTSP   RTSPConfig   `yaml:"rtsp"`
	HTTP   HTTPConfig   `yaml:"http_stream"`
	WS     WSConfig     `yaml:"websocket"`
	WebRTC WebRTCConfig `yaml:"webrtc"`
	SRT    SRTConfig    `yaml:"srt"`
	SIP     SIPConfig     `yaml:"sip"`
	GB28181 GB28181Config `yaml:"gb28181"`
	Stream  StreamConfig  `yaml:"stream"`
	Auth   AuthConfig   `yaml:"auth"`
	Notify NotifyConfig `yaml:"notify"`
	Cluster ClusterConfig `yaml:"cluster"`
	Record  RecordConfig  `yaml:"record"`
	API     APIConfig     `yaml:"api"`
	Metrics MetricsConfig `yaml:"metrics"`
}

// ServerConfig holds general server settings.
type ServerConfig struct {
	Name         string        `yaml:"name"`
	LogLevel     string        `yaml:"log_level"`
	DrainTimeout time.Duration `yaml:"drain_timeout"`
}

// TLSConfig holds TLS certificate paths.
type TLSConfig struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

// Configured returns true when both cert and key paths are set.
func (t TLSConfig) Configured() bool {
	return t.CertFile != "" && t.KeyFile != ""
}

// LimitsConfig holds resource limits.
type LimitsConfig struct {
	MaxStreams              int            `yaml:"max_streams"`
	MaxSubscribersPerStream int            `yaml:"max_subscribers_per_stream"`
	MaxConnections          int            `yaml:"max_connections"`
	MaxBitratePerStream     int            `yaml:"max_bitrate_per_stream"`
	RateLimit               RateLimitConfig `yaml:"rate_limit"`
}

// RateLimitConfig holds per-IP HTTP rate limiting settings.
type RateLimitConfig struct {
	Enabled bool    `yaml:"enabled"`
	Rate    float64 `yaml:"rate"`  // requests per second per IP
	Burst   int     `yaml:"burst"` // max burst size per IP
}

// RTMPConfig holds RTMP module settings.
type RTMPConfig struct {
	Enabled     bool               `yaml:"enabled"`
	Listen      string             `yaml:"listen"`
	ChunkSize   int                `yaml:"chunk_size"`
	TLS         *bool              `yaml:"tls,omitempty"` // nil=follow global, true=force on, false=force off
	SkipTracker *SkipTrackerConfig `yaml:"skip_tracker,omitempty"`
}

// RTSPConfig holds RTSP module settings.
type RTSPConfig struct {
	Enabled      bool               `yaml:"enabled"`
	Listen       string             `yaml:"listen"`
	RTPPortRange []int              `yaml:"rtp_port_range"`
	TLS          *bool              `yaml:"tls,omitempty"` // nil=follow global, true=force on, false=force off
	SkipTracker  *SkipTrackerConfig `yaml:"skip_tracker,omitempty"`
}

// HTTPConfig holds HTTP-FLV/TS/FMP4/HLS/DASH module settings.
type HTTPConfig struct {
	Enabled bool   `yaml:"enabled"`
	Listen  string `yaml:"listen"`
	CORS    bool   `yaml:"cors"`
	TLS     *bool  `yaml:"tls,omitempty"` // nil=follow global, true=force on, false=force off
	HLS     HLSConfig   `yaml:"hls"`
	DASH    DASHConfig  `yaml:"dash"`
	LLHLS   LLHLSConfig `yaml:"llhls"`
}

// HLSConfig holds HLS streaming settings.
type HLSConfig struct {
	SegmentDuration float64 `yaml:"segment_duration"` // seconds, default 6
	PlaylistSize    int     `yaml:"playlist_size"`    // max segments in playlist, default 5
}

// DASHConfig holds DASH streaming settings.
type DASHConfig struct {
	SegmentDuration float64 `yaml:"segment_duration"` // seconds, default 6
	PlaylistSize    int     `yaml:"playlist_size"`    // max segments in manifest, default 5
}

// LLHLSConfig holds Low-Latency HLS settings.
type LLHLSConfig struct {
	Enabled      bool    `yaml:"enabled"`
	PartDuration float64 `yaml:"part_duration"` // partial segment target duration in seconds (default 0.2)
	SegmentCount int     `yaml:"segment_count"` // full segments in playlist window (default 4)
	Container    string  `yaml:"container"`     // "fmp4" or "ts" (default "fmp4")
}

// WSConfig holds WebSocket module settings.
type WSConfig struct {
	Enabled bool   `yaml:"enabled"`
	Listen  string `yaml:"listen"`
	Path    string `yaml:"path"`
}

// WebRTCConfig holds WebRTC WHIP/WHEP settings.
type WebRTCConfig struct {
	Enabled      bool        `yaml:"enabled"`
	Listen       string      `yaml:"listen"`
	ICELite      bool        `yaml:"ice_lite"`
	ICEServers   []ICEServer `yaml:"ice_servers"`
	UDPPortRange []int       `yaml:"udp_port_range"`
	Candidates   []string    `yaml:"candidates"`
	TLS          *bool       `yaml:"tls,omitempty"` // nil=follow global, true=force on, false=force off
	GCC          GCCConfig   `yaml:"gcc"`
}

// GCCConfig holds Google Congestion Control settings for WebRTC.
type GCCConfig struct {
	Enabled        bool `yaml:"enabled"`
	InitialBitrate int  `yaml:"initial_bitrate"` // bits/sec
	MinBitrate     int  `yaml:"min_bitrate"`     // bits/sec
	MaxBitrate     int  `yaml:"max_bitrate"`     // bits/sec
}

// ICEServer holds a STUN/TURN server configuration.
type ICEServer struct {
	URLs       []string `yaml:"urls"`
	Username   string   `yaml:"username,omitempty"`
	Credential string   `yaml:"credential,omitempty"`
}

// SRTConfig holds SRT module settings.
type SRTConfig struct {
	Enabled     bool               `yaml:"enabled"`
	Listen      string             `yaml:"listen"`
	Latency     int                `yaml:"latency"`     // ms, receiver latency (default 120)
	Passphrase  string             `yaml:"passphrase"`  // AES encryption passphrase (empty = no encryption)
	PBKeyLen    int                `yaml:"pbkeylen"`    // crypto key length: 0, 16, 24, or 32
	SkipTracker *SkipTrackerConfig `yaml:"skip_tracker,omitempty"`
}

// SIPConfig holds SIP module settings.
type SIPConfig struct {
	Enabled   bool     `yaml:"enabled"`
	Listen    string   `yaml:"listen"`
	Transport []string `yaml:"transport"`
	ServerID  string   `yaml:"server_id"`
	Domain    string   `yaml:"domain"`
	Auth      SIPAuth  `yaml:"auth"`
}

// SIPAuth holds SIP digest authentication settings.
type SIPAuth struct {
	Enabled  bool   `yaml:"enabled"`
	Password string `yaml:"password"`
}

// GB28181Config holds GB28181 module settings.
type GB28181Config struct {
	Enabled         bool          `yaml:"enabled"`
	StreamPrefix    string        `yaml:"stream_prefix"`
	RTPPortRange    []int         `yaml:"rtp_port_range"`
	SSRC            SSRCConfig    `yaml:"ssrc"`
	Keepalive       KeepaliveConfig `yaml:"keepalive"`
	AutoInvite      bool          `yaml:"auto_invite"`
	CatalogInterval time.Duration `yaml:"catalog_interval"`
	DumpFile        string        `yaml:"dump_file"`
}

// SSRCConfig holds SSRC generation settings for GB28181.
type SSRCConfig struct {
	Prefix string `yaml:"prefix"`
}

// KeepaliveConfig holds device keepalive detection settings.
type KeepaliveConfig struct {
	Interval time.Duration `yaml:"interval"`
	Timeout  time.Duration `yaml:"timeout"`
}

// SkipTrackerConfig holds ring buffer skip tracking settings.
// When a subscriber is too slow to keep up, the ring buffer overwrites unread frames.
// SkipTracker counts these events in a sliding window and disconnects the subscriber
// if the threshold is exceeded. Set MaxCount <= 0 to disable.
type SkipTrackerConfig struct {
	MaxCount int           `yaml:"max_count"`
	Window   time.Duration `yaml:"window"`
}

// SlowConsumerConfig holds slow consumer frame dropping settings.
type SlowConsumerConfig struct {
	Enabled          bool               `yaml:"enabled"`
	LagWarnRatio     float64            `yaml:"lag_warn_ratio"`
	LagDropRatio     float64            `yaml:"lag_drop_ratio"`
	LagCriticalRatio float64            `yaml:"lag_critical_ratio"`
	LagRecoverRatio  float64            `yaml:"lag_recover_ratio"`
	EWMAAlpha        float64            `yaml:"ewma_alpha"`
	SendTimeRatio    float64            `yaml:"send_time_ratio"`
}

// StreamConfig holds stream-level settings.
type StreamConfig struct {
	GOPCache         bool              `yaml:"gop_cache"`
	GOPCacheNum      int               `yaml:"gop_cache_num"`
	AudioCacheMs     int               `yaml:"audio_cache_ms"`
	RingBufferSize   int               `yaml:"ring_buffer_size"`
	IdleTimeout      time.Duration     `yaml:"idle_timeout"`
	NoPublisherTimeout time.Duration   `yaml:"no_publisher_timeout"`
	SlowConsumer     SlowConsumerConfig  `yaml:"slow_consumer"`
	Simulcast        SimulcastConfig     `yaml:"simulcast"`
	Feedback         FeedbackConfig      `yaml:"feedback"`
}

// SimulcastConfig holds simulcast layer settings.
type SimulcastConfig struct {
	Enabled        bool          `yaml:"enabled"`
	AutoPauseLayer bool          `yaml:"auto_pause_layer"`
	Layers         []LayerConfig `yaml:"layers"`
}

// LayerConfig holds a single simulcast layer.
type LayerConfig struct {
	RID        string `yaml:"rid"`
	MaxBitrate int    `yaml:"max_bitrate"`
}

// FeedbackConfig holds feedback routing settings.
type FeedbackConfig struct {
	DefaultMode    string              `yaml:"default_mode"`
	AutoThresholds AutoThresholdsConfig `yaml:"auto_thresholds"`
}

// AutoThresholdsConfig holds auto feedback mode thresholds.
type AutoThresholdsConfig struct {
	PassthroughMax int `yaml:"passthrough_max"`
	AggregateMax   int `yaml:"aggregate_max"`
}

// AuthConfig holds authentication settings.
type AuthConfig struct {
	Enabled   bool            `yaml:"enabled"`
	Publish   AuthRuleConfig  `yaml:"publish"`
	Subscribe AuthRuleConfig  `yaml:"subscribe"`
	API       APIAuthConfig   `yaml:"api"`
}

// AuthRuleConfig holds auth rule for publish or subscribe.
type AuthRuleConfig struct {
	Mode     string          `yaml:"mode"`
	Token    TokenConfig     `yaml:"token"`
	Callback CallbackConfig  `yaml:"callback"`
}

// TokenConfig holds JWT token settings.
type TokenConfig struct {
	Secret    string `yaml:"secret"`
	Algorithm string `yaml:"algorithm"`
}

// CallbackConfig holds auth callback settings.
type CallbackConfig struct {
	URL     string        `yaml:"url"`
	Timeout time.Duration `yaml:"timeout"`
}

// APIAuthConfig holds API auth settings.
type APIAuthConfig struct {
	BearerToken string `yaml:"bearer_token"`
}

// NotifyConfig holds notification settings.
type NotifyConfig struct {
	HTTP          NotifyHTTPConfig      `yaml:"http"`
	WebSocket     NotifyWSConfig        `yaml:"websocket"`
	AliveInterval time.Duration         `yaml:"alive_interval"`
}

// NotifyHTTPConfig holds HTTP webhook notification settings.
type NotifyHTTPConfig struct {
	Enabled   bool                    `yaml:"enabled"`
	Endpoints []NotifyEndpointConfig  `yaml:"endpoints"`
}

// NotifyEndpointConfig holds a single webhook endpoint.
type NotifyEndpointConfig struct {
	URL     string        `yaml:"url"`
	Events  []string      `yaml:"events"`
	Secret  string        `yaml:"secret"`
	Retry   int           `yaml:"retry"`
	Timeout time.Duration `yaml:"timeout"`
}

// NotifyWSConfig holds WebSocket notification settings.
type NotifyWSConfig struct {
	Enabled bool   `yaml:"enabled"`
	Path    string `yaml:"path"`
}

// ClusterConfig holds cluster settings.
type ClusterConfig struct {
	Forward ForwardConfig     `yaml:"forward"`
	Origin  OriginConfig      `yaml:"origin"`
	SRT     ClusterSRTConfig  `yaml:"srt"`
	RTSP    ClusterRTSPConfig `yaml:"rtsp"`
	RTP     ClusterRTPConfig  `yaml:"rtp"`
	GB28181 ClusterGBConfig   `yaml:"gb28181"`
}

// ClusterGBConfig holds GB28181 cluster relay settings.
type ClusterGBConfig struct {
	PortRange     []int         `yaml:"port_range"`
	SignalingPath string        `yaml:"signaling_path"`
	RTCPInterval  time.Duration `yaml:"rtcp_interval"`
	Timeout       time.Duration `yaml:"timeout"`
}

// ClusterSRTConfig holds SRT-specific relay settings.
type ClusterSRTConfig struct {
	Latency    time.Duration `yaml:"latency"`
	Passphrase string        `yaml:"passphrase"`
	PBKeyLen   int           `yaml:"pbkeylen"`
}

// ClusterRTSPConfig holds RTSP-specific relay settings.
type ClusterRTSPConfig struct {
	Transport string `yaml:"transport"` // "tcp" or "udp"
}

// ClusterRTPConfig holds RTP direct relay settings.
type ClusterRTPConfig struct {
	PortRange     string        `yaml:"port_range"`
	SignalingPath string        `yaml:"signaling_path"`
	RTCPInterval  time.Duration `yaml:"rtcp_interval"`
	Timeout       time.Duration `yaml:"timeout"`
}

// ForwardConfig holds forward push settings.
type ForwardConfig struct {
	Enabled          bool          `yaml:"enabled"`
	Targets          []string      `yaml:"targets"`
	ScheduleURL      string        `yaml:"schedule_url"`
	SchedulePriority string        `yaml:"schedule_priority"`
	ScheduleTimeout  time.Duration `yaml:"schedule_timeout"`
	RetryMax         int           `yaml:"retry_max"`
	RetryInterval    time.Duration `yaml:"retry_interval"`
}

// OriginConfig holds origin pull settings.
type OriginConfig struct {
	Enabled          bool          `yaml:"enabled"`
	Servers          []string      `yaml:"servers"`
	ScheduleURL      string        `yaml:"schedule_url"`
	SchedulePriority string        `yaml:"schedule_priority"`
	ScheduleTimeout  time.Duration `yaml:"schedule_timeout"`
	IdleTimeout      time.Duration `yaml:"idle_timeout"`
	RetryMax         int           `yaml:"retry_max"`
	RetryDelay       time.Duration `yaml:"retry_delay"`
}

// RecordConfig holds recording settings.
type RecordConfig struct {
	Enabled        bool               `yaml:"enabled"`
	StreamPattern  string             `yaml:"stream_pattern"`
	Format         string             `yaml:"format"`
	Path           string             `yaml:"path"`
	Segment        SegmentConfig      `yaml:"segment"`
	OnFileComplete FileCompleteConfig `yaml:"on_file_complete"`
}

// SegmentConfig holds recording segmentation settings.
type SegmentConfig struct {
	Mode     string        `yaml:"mode"`
	Duration time.Duration `yaml:"duration"`
	MaxSize  string        `yaml:"max_size"`
}

// FileCompleteConfig holds file completion callback settings.
type FileCompleteConfig struct {
	URL string `yaml:"url"`
}

// MetricsConfig holds Prometheus metrics settings.
type MetricsConfig struct {
	Enabled bool   `yaml:"enabled"`
	Listen  string `yaml:"listen"`
	Path    string `yaml:"path"`
}

// APIConfig holds the management API settings.
type APIConfig struct {
	Enabled bool            `yaml:"enabled"`
	Listen  string          `yaml:"listen"`
	TLS     *bool           `yaml:"tls,omitempty"` // nil=follow global, true=force on, false=force off
	Auth    APIAuthConfig   `yaml:"auth"`
	Console ConsoleConfig   `yaml:"console"`
}

// ConsoleConfig holds console login credentials.
type ConsoleConfig struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}
