package api

import (
	"time"

	"github.com/im-pingo/liveforge/config"
)

type configView struct {
	Revision       string                        `json:"revision"`
	Source         configSourceView              `json:"source"`
	PendingRestart []string                      `json:"pending_restart"`
	Desired        configValuesView              `json:"desired"`
	Effective      configValuesView              `json:"effective"`
	Reload         map[string]config.ReloadClass `json:"reload"`
	CSRFToken      string                        `json:"csrf_token,omitempty"`
}

type configValuesView struct {
	Server serverConfigView `json:"server"`
	Auth   mediaAuthView    `json:"auth"`
	API    apiConfigView    `json:"api"`
}

type configSourceView struct {
	Name     string    `json:"name"`
	LoadedAt time.Time `json:"loaded_at"`
}

type serverConfigView struct {
	Name     string `json:"name"`
	LogLevel string `json:"log_level"`
}

type mediaAuthView struct {
	Enabled   bool         `json:"enabled"`
	Publish   authRuleView `json:"publish"`
	Subscribe authRuleView `json:"subscribe"`
}

type authRuleView struct {
	Mode     string           `json:"mode"`
	Stage    string           `json:"stage"`
	Token    authTokenView    `json:"token"`
	Callback authCallbackView `json:"callback"`
}

type authTokenView struct {
	Algorithm        string `json:"algorithm"`
	SecretConfigured bool   `json:"secret_configured"`
}

type authCallbackView struct {
	URLConfigured bool   `json:"url_configured"`
	Timeout       string `json:"timeout"`
}

type apiConfigView struct {
	Enabled      bool              `json:"enabled"`
	Listen       string            `json:"listen"`
	PprofEnabled bool              `json:"pprof_enabled"`
	Auth         apiAuthConfigView `json:"auth"`
	Console      consoleConfigView `json:"console"`
}

type apiAuthConfigView struct {
	Enabled               bool `json:"enabled"`
	BearerTokenConfigured bool `json:"bearer_token_configured"`
}

type consoleConfigView struct {
	Username                        string `json:"username"`
	PasswordConfigured              bool   `json:"password_configured"`
	PasswordHashed                  bool   `json:"password_hashed"`
	AllowInsecureDefaultCredentials bool   `json:"allow_insecure_default_credentials"`
}

func buildConfigView(snapshot config.Snapshot, csrfToken string) configView {
	return configView{
		Revision: snapshot.Revision,
		Source: configSourceView{
			Name:     snapshot.Source,
			LoadedAt: snapshot.LoadedAt,
		},
		PendingRestart: append([]string(nil), snapshot.PendingRestart...),
		Desired:        buildConfigValuesView(snapshot.Desired),
		Effective:      buildConfigValuesView(snapshot.Effective),
		Reload:         configReloadMetadata(),
		CSRFToken:      csrfToken,
	}
}

func buildConfigValuesView(cfg *config.Config) configValuesView {
	if cfg == nil {
		return configValuesView{}
	}
	return configValuesView{
		Server: serverConfigView{
			Name:     cfg.Server.Name,
			LogLevel: cfg.Server.LogLevel,
		},
		Auth: mediaAuthView{
			Enabled:   cfg.Auth.Enabled,
			Publish:   buildAuthRuleView(cfg.Auth.Publish),
			Subscribe: buildAuthRuleView(cfg.Auth.Subscribe),
		},
		API: apiConfigView{
			Enabled:      cfg.API.Enabled,
			Listen:       cfg.API.Listen,
			PprofEnabled: cfg.API.PprofEnabled,
			Auth: apiAuthConfigView{
				Enabled:               cfg.API.Auth.Enabled,
				BearerTokenConfigured: cfg.API.Auth.BearerToken != "",
			},
			Console: consoleConfigView{
				Username:                        cfg.API.Console.Username,
				PasswordConfigured:              cfg.API.Console.Password != "" || cfg.API.Console.PasswordHash != "",
				PasswordHashed:                  cfg.API.Console.PasswordHash != "",
				AllowInsecureDefaultCredentials: cfg.API.Console.AllowInsecureDefaultCredentials,
			},
		},
	}
}

func configReloadMetadata() map[string]config.ReloadClass {
	return map[string]config.ReloadClass{
		"server.name":                                    config.ReloadHot,
		"server.log_level":                               config.ReloadHot,
		"auth.enabled":                                   config.ReloadHot,
		"auth.publish.mode":                              config.ReloadHot,
		"auth.publish.stage":                             config.ReloadHot,
		"auth.publish.token.secret":                      config.ReloadHot,
		"auth.publish.token.algorithm":                   config.ReloadHot,
		"auth.publish.callback.url":                      config.ReloadHot,
		"auth.publish.callback.timeout":                  config.ReloadHot,
		"auth.subscribe.mode":                            config.ReloadHot,
		"auth.subscribe.stage":                           config.ReloadHot,
		"auth.subscribe.token.secret":                    config.ReloadHot,
		"auth.subscribe.token.algorithm":                 config.ReloadHot,
		"auth.subscribe.callback.url":                    config.ReloadHot,
		"auth.subscribe.callback.timeout":                config.ReloadHot,
		"api.enabled":                                    config.ReloadRestart,
		"api.listen":                                     config.ReloadRestart,
		"api.pprof_enabled":                              config.ReloadHot,
		"api.auth.enabled":                               config.ReloadHot,
		"api.auth.bearer_token":                          config.ReloadHot,
		"api.console.username":                           config.ReloadHot,
		"api.console.password":                           config.ReloadHot,
		"api.console.password_hash":                      config.ReloadHot,
		"api.console.allow_insecure_default_credentials": config.ReloadHot,
	}
}

func buildAuthRuleView(rule config.AuthRuleConfig) authRuleView {
	return authRuleView{
		Mode:  rule.Mode,
		Stage: rule.Stage,
		Token: authTokenView{
			Algorithm:        rule.Token.Algorithm,
			SecretConfigured: rule.Token.Secret != "",
		},
		Callback: authCallbackView{
			URLConfigured: rule.Callback.URL != "",
			Timeout:       rule.Callback.Timeout.String(),
		},
	}
}
