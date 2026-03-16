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
	SIP    SIPConfig    `yaml:"sip"`
	Stream StreamConfig `yaml:"stream"`
	Auth   AuthConfig   `yaml:"auth"`
	Notify NotifyConfig `yaml:"notify"`
	Cluster ClusterConfig `yaml:"cluster"`
	Record RecordConfig  `yaml:"record"`
	API    APIConfig     `yaml:"api"`
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

// LimitsConfig holds resource limits.
type LimitsConfig struct {
	MaxStreams              int `yaml:"max_streams"`
	MaxSubscribersPerStream int `yaml:"max_subscribers_per_stream"`
	MaxConnections          int `yaml:"max_connections"`
	MaxBitratePerStream     int `yaml:"max_bitrate_per_stream"`
}

// RTMPConfig holds RTMP module settings.
type RTMPConfig struct {
	Enabled   bool   `yaml:"enabled"`
	Listen    string `yaml:"listen"`
	ChunkSize int    `yaml:"chunk_size"`
}

// RTSPConfig holds RTSP module settings.
type RTSPConfig struct {
	Enabled      bool   `yaml:"enabled"`
	Listen       string `yaml:"listen"`
	RTPPortRange []int  `yaml:"rtp_port_range"`
}

// HTTPConfig holds HTTP-FLV/TS/FMP4 module settings.
type HTTPConfig struct {
	Enabled bool   `yaml:"enabled"`
	Listen  string `yaml:"listen"`
	CORS    bool   `yaml:"cors"`
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
	ICEServers   []ICEServer `yaml:"ice_servers"`
	UDPPortRange []int       `yaml:"udp_port_range"`
	Candidates   []string    `yaml:"candidates"`
}

// ICEServer holds a STUN/TURN server configuration.
type ICEServer struct {
	URLs       []string `yaml:"urls"`
	Username   string   `yaml:"username,omitempty"`
	Credential string   `yaml:"credential,omitempty"`
}

// SIPConfig holds SIP module settings.
type SIPConfig struct {
	Enabled   bool     `yaml:"enabled"`
	Listen    string   `yaml:"listen"`
	Transport []string `yaml:"transport"`
}

// StreamConfig holds stream-level settings.
type StreamConfig struct {
	GOPCache         bool              `yaml:"gop_cache"`
	GOPCacheNum      int               `yaml:"gop_cache_num"`
	AudioCacheMs     int               `yaml:"audio_cache_ms"`
	RingBufferSize   int               `yaml:"ring_buffer_size"`
	MaxSkipCount     int               `yaml:"max_skip_count"`
	MaxSkipWindow    time.Duration     `yaml:"max_skip_window"`
	IdleTimeout      time.Duration     `yaml:"idle_timeout"`
	NoPublisherTimeout time.Duration   `yaml:"no_publisher_timeout"`
	Simulcast        SimulcastConfig   `yaml:"simulcast"`
	AudioOnDemand    AudioOnDemandConfig `yaml:"audio_on_demand"`
	Feedback         FeedbackConfig    `yaml:"feedback"`
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

// AudioOnDemandConfig holds audio on-demand settings.
type AudioOnDemandConfig struct {
	Enabled    bool          `yaml:"enabled"`
	PauseDelay time.Duration `yaml:"pause_delay"`
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
	Forward ForwardConfig `yaml:"forward"`
	Origin  OriginConfig  `yaml:"origin"`
}

// ForwardConfig holds forward push settings.
type ForwardConfig struct {
	Enabled       bool          `yaml:"enabled"`
	Targets       []string      `yaml:"targets"`
	ScheduleURL   string        `yaml:"schedule_url"`
	RetryMax      int           `yaml:"retry_max"`
	RetryInterval time.Duration `yaml:"retry_interval"`
}

// OriginConfig holds origin pull settings.
type OriginConfig struct {
	Enabled     bool          `yaml:"enabled"`
	Servers     []string      `yaml:"servers"`
	ScheduleURL string        `yaml:"schedule_url"`
	IdleTimeout time.Duration `yaml:"idle_timeout"`
	RetryMax    int           `yaml:"retry_max"`
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

// APIConfig holds the management API settings.
type APIConfig struct {
	Enabled bool          `yaml:"enabled"`
	Listen  string        `yaml:"listen"`
	Auth    APIAuthConfig `yaml:"auth"`
}
