package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

func TestConsoleSettingsFlow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping browser test in short mode")
	}
	testServer := newConfigTestServer(t)

	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(),
		append(chromedp.DefaultExecAllocatorOptions[:],
			chromedp.Flag("headless", true),
			chromedp.Flag("disable-gpu", true),
			chromedp.Flag("no-sandbox", true),
			chromedp.Flag("disable-dev-shm-usage", true),
		)...,
	)
	defer allocCancel()
	browserCtx, browserCancel := chromedp.NewContext(allocCtx, chromedp.WithLogf(t.Logf))
	defer browserCancel()
	// Start Chrome before applying the UI flow deadline. Browser startup can
	// approach its own timeout on loaded CI hosts and should not consume the
	// budget for page navigation and assertions.
	if err := chromedp.Run(browserCtx); err != nil {
		if strings.Contains(err.Error(), "executable file not found") || strings.Contains(err.Error(), "websocket url timeout") {
			t.Skipf("headless Chrome unavailable: %v", err)
		}
		t.Fatalf("start browser: %v", err)
	}
	runPhase := func(name string, actions ...chromedp.Action) {
		t.Helper()
		phaseCtx, cancel := context.WithTimeout(browserCtx, 15*time.Second)
		defer cancel()
		if err := chromedp.Run(phaseCtx, actions...); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}

	runPhase("open settings",
		chromedp.Navigate(testServer.addr+"/console/login"),
		chromedp.WaitVisible("#username", chromedp.ByID),
		chromedp.SendKeys("#username", "admin", chromedp.ByID),
		chromedp.SendKeys("#password", testServer.password, chromedp.ByID),
		chromedp.Submit("form", chromedp.ByQuery),
		chromedp.WaitVisible("#tab-settings", chromedp.ByID),
		chromedp.Click("#tab-settings", chromedp.ByID),
		chromedp.WaitVisible("#view-settings", chromedp.ByID),
		chromedp.Poll(`document.getElementById("settings-server-name").value === "Config Test"`, nil),
	)

	runPhase("save non-credential settings",
		chromedp.SetValue("#settings-server-name", "Console Updated", chromedp.ByID),
		chromedp.Click("#settings-save", chromedp.ByID),
		chromedp.Poll(`document.getElementById("settings-status").textContent.indexOf("Saved") >= 0`, nil),
	)
	if got := testServer.server.Config().Server.Name; got != "Console Updated" {
		t.Fatalf("server name = %q, want Console Updated", got)
	}

	runPhase("save credential settings",
		chromedp.SetValue("#settings-publish-token", "ui-publish-secret", chromedp.ByID),
		chromedp.SetValue("#settings-publish-callback", "https://ui.example/publish", chromedp.ByID),
		chromedp.SetValue("#settings-subscribe-callback", "https://ui.example/subscribe", chromedp.ByID),
		chromedp.SetValue("#settings-current-password", testServer.password, chromedp.ByID),
		chromedp.Evaluate(`
			document.getElementById("settings-publish-mode").value = "token";
			document.getElementById("settings-publish-stage").value = "post_connect";
			document.getElementById("settings-subscribe-mode").value = "callback";
			document.getElementById("settings-subscribe-stage").value = "pre_session";
		`, nil),
		chromedp.Click("#settings-save", chromedp.ByID),
		chromedp.Poll(`document.getElementById("settings-status").textContent.indexOf("Saved") >= 0`, nil),
	)

	cfg := testServer.server.Config()
	if cfg.Server.Name != "Console Updated" || cfg.Auth.Publish.Mode != "token" || cfg.Auth.Publish.Stage != "post_connect" ||
		cfg.Auth.Subscribe.Mode != "callback" || cfg.Auth.Subscribe.Stage != "pre_session" ||
		cfg.Auth.Publish.Token.Secret != "ui-publish-secret" || cfg.Auth.Publish.Callback.URL != "https://ui.example/publish" ||
		cfg.Auth.Subscribe.Callback.URL != "https://ui.example/subscribe" || cfg.Auth.Subscribe.Token.Secret != "subscribe-token-secret" {
		t.Fatalf("runtime settings were not updated: server=%q auth=%+v", cfg.Server.Name, cfg.Auth)
	}

	runPhase("clear publish credentials",
		chromedp.Click("#settings-publish-token-clear", chromedp.ByID),
		chromedp.Click("#settings-publish-callback-clear", chromedp.ByID),
		chromedp.SetValue("#settings-current-password", testServer.password, chromedp.ByID),
		chromedp.Evaluate(`document.getElementById("settings-publish-mode").value = "none"`, nil),
		chromedp.Click("#settings-save", chromedp.ByID),
		chromedp.Poll(`
			document.getElementById("settings-publish-token-state").textContent === "Not configured" &&
			document.getElementById("settings-publish-callback-state").textContent === "Not configured"
		`, nil),
	)
	cfg = testServer.server.Config()
	if cfg.Auth.Publish.Token.Secret != "" || cfg.Auth.Publish.Callback.URL != "" || cfg.Auth.Subscribe.Token.Secret != "subscribe-token-secret" {
		t.Fatalf("UI secret clear/keep result = publish token %q callback %q subscribe token %q",
			cfg.Auth.Publish.Token.Secret, cfg.Auth.Publish.Callback.URL, cfg.Auth.Subscribe.Token.Secret)
	}

	snapshot := testServer.manager.Current()
	updateCtx, cancelUpdate := context.WithTimeout(browserCtx, 15*time.Second)
	_, updateErr := testServer.manager.Update(updateCtx, map[string]any{
		"api": map[string]any{"listen": "127.0.0.1:19090"},
	}, snapshot.Revision)
	cancelUpdate()
	if updateErr != nil {
		t.Fatalf("set restart-required desired config: %v", updateErr)
	}
	runPhase("show pending desired and effective values",
		chromedp.Evaluate(`refreshSettings()`, nil),
		chromedp.Poll(`
			document.getElementById("settings-restart").textContent.indexOf("api.listen") >= 0 &&
			document.getElementById("settings-restart").textContent.indexOf("effective=127.0.0.1:0") >= 0 &&
			document.getElementById("settings-restart").textContent.indexOf("desired=127.0.0.1:19090") >= 0
		`, nil),
	)

	runPhase("change password and return to login",
		chromedp.SetValue("#password-current", testServer.password, chromedp.ByID),
		chromedp.SetValue("#password-new", "browser-new-password", chromedp.ByID),
		chromedp.SetValue("#password-confirm", "browser-new-password", chromedp.ByID),
		chromedp.Click("#password-save", chromedp.ByID),
		chromedp.WaitVisible("#username", chromedp.ByID),
	)
}
