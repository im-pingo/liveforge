package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
	"golang.org/x/net/html"
)

func consoleDocument(t *testing.T) (*html.Node, string) {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(string(consoleHTML)))
	if err != nil {
		t.Fatalf("parse console HTML: %v", err)
	}
	var scripts strings.Builder
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode && node.Data == "script" && node.FirstChild != nil {
			scripts.WriteString(node.FirstChild.Data)
			scripts.WriteByte('\n')
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)
	return doc, scripts.String()
}

func consoleElementsByID(doc *html.Node) map[string]*html.Node {
	elements := make(map[string]*html.Node)
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode {
			for _, attr := range node.Attr {
				if attr.Key == "id" {
					elements[attr.Val] = node
					break
				}
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)
	return elements
}

func consoleIDCounts(doc *html.Node) map[string]int {
	counts := make(map[string]int)
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode {
			for _, attr := range node.Attr {
				if attr.Key == "id" {
					counts[attr.Val]++
					break
				}
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)
	return counts
}

func consoleAttribute(node *html.Node, name string) string {
	for _, attr := range node.Attr {
		if attr.Key == name {
			return attr.Val
		}
	}
	return ""
}

func consoleHasAttribute(node *html.Node, name string) bool {
	for _, attr := range node.Attr {
		if attr.Key == name {
			return true
		}
	}
	return false
}

func consoleFunctionSource(t *testing.T, script, name string) string {
	t.Helper()
	marker := "function " + name + "("
	start := strings.Index(script, marker)
	if start < 0 {
		t.Fatalf("console script is missing function %s", name)
	}
	rest := script[start:]
	if next := strings.Index(rest[len(marker):], "\nfunction "); next >= 0 {
		return rest[:len(marker)+next]
	}
	return rest
}

func consoleNavigationViews(doc *html.Node) []string {
	var views []string
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode && node.Data == "button" {
			if view := consoleAttribute(node, "data-view"); view != "" {
				views = append(views, view)
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)
	return views
}

func consoleDirectNavigationViews(node *html.Node) []string {
	var views []string
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if child.Type != html.ElementNode || child.Data != "div" {
			continue
		}
		for tab := child.FirstChild; tab != nil; tab = tab.NextSibling {
			if tab.Type == html.ElementNode && tab.Data == "button" {
				if view := consoleAttribute(tab, "data-view"); view != "" {
					views = append(views, view)
				}
			}
		}
	}
	return views
}

func TestConsoleManagementViewsExposeSupportedControlPlanes(t *testing.T) {
	doc, _ := consoleDocument(t)
	elements := consoleElementsByID(doc)
	for _, id := range []string{
		"nav-tabs",
		"nav-workspace",
		"nav-operations",
		"nav-system",
		"view-streams",
		"view-gb28181",
		"view-config",
		"view-cluster",
		"view-sip",
		"view-storage",
		"view-security",
		"config-refresh",
		"config-editor",
		"config-validate",
		"config-apply",
		"sip-target-uri",
		"sip-stream-key",
		"sip-lab-mode",
		"sip-lab-device",
		"sip-lab-stream",
		"sip-lab-codec",
		"sip-lab-start",
		"sip-lab-sessions-tbody",
		"gb-lab-mode",
		"gb-lab-device",
		"gb-lab-channel",
		"gb-lab-stream",
		"gb-lab-start",
		"gb-lab-sessions-tbody",
		"recordings-tbody",
		"recording-detail",
		"recording-capability",
		"dvr-tbody",
		"audit-tbody",
	} {
		if elements[id] == nil {
			t.Errorf("management console is missing element %q", id)
		}
	}
	for _, group := range []struct {
		id   string
		view string
	}{
		{"nav-workspace", "streams"},
		{"nav-workspace", "gb28181"},
		{"nav-workspace", "sip"},
		{"nav-workspace", "storage"},
		{"nav-operations", "cluster"},
		{"nav-system", "config"},
		{"nav-system", "security"},
	} {
		groupNode := elements[group.id]
		found := false
		var walkGroup func(*html.Node)
		walkGroup = func(node *html.Node) {
			if node.Type == html.ElementNode && node.Data == "button" && consoleAttribute(node, "data-view") == group.view {
				found = true
			}
			for child := node.FirstChild; child != nil; child = child.NextSibling {
				walkGroup(child)
			}
		}
		walkGroup(groupNode)
		if !found {
			t.Errorf("navigation group %q does not contain %q", group.id, group.view)
		}
	}
	for _, group := range []struct {
		id   string
		want string
	}{
		{"nav-workspace", "streams,gb28181,sip,storage"},
		{"nav-operations", "cluster"},
		{"nav-system", "config,security"},
	} {
		got := strings.Join(consoleDirectNavigationViews(elements[group.id]), ",")
		if got != group.want {
			t.Errorf("navigation group %q direct views = %q, want %q", group.id, got, group.want)
		}
	}

	views := consoleNavigationViews(doc)
	wantViews := []string{"streams", "gb28181", "sip", "storage", "cluster", "config", "security"}
	if strings.Join(views, ",") != strings.Join(wantViews, ",") {
		t.Fatalf("navigation views = %v, want %v", views, wantViews)
	}
	for _, view := range views {
		tab := elements["tab-"+view]
		panel := elements["view-"+view]
		if tab == nil {
			t.Errorf("navigation view %q has no stable tab id", view)
			continue
		}
		if panel == nil {
			t.Errorf("navigation view %q has no matching container", view)
			continue
		}
		if got := consoleAttribute(tab, "aria-controls"); got != "view-"+view {
			t.Errorf("tab %q aria-controls = %q", view, got)
		}
		if got := consoleAttribute(panel, "role"); got != "tabpanel" {
			t.Errorf("panel %q role = %q, want tabpanel", view, got)
		}
		if got := consoleAttribute(panel, "aria-labelledby"); got != "tab-"+view {
			t.Errorf("panel %q aria-labelledby = %q", view, got)
		}
	}
	modal := elements["modal"]
	if modal == nil || consoleAttribute(modal, "role") != "dialog" || consoleAttribute(modal, "aria-modal") != "true" || consoleAttribute(modal, "aria-labelledby") != "modal-title" || consoleAttribute(modal, "aria-describedby") != "modal-msg" {
		t.Error("confirmation modal is missing dialog naming and modality semantics")
	}
}

func TestConsoleManagementDOMIDsAreUnique(t *testing.T) {
	doc, _ := consoleDocument(t)
	for id, count := range consoleIDCounts(doc) {
		if count != 1 {
			t.Errorf("console id %q appears %d times, want exactly once", id, count)
		}
	}
}

func TestConsoleManagementRequestsUseSessionSafeHelper(t *testing.T) {
	doc, script := consoleDocument(t)
	elements := consoleElementsByID(doc)
	for _, call := range []string{
		`apiFetch("/api/v1/server/config"`,
		`apiFetch("/api/v1/server/config/document"`,
		`apiFetch("/api/v1/server/config/schema"`,
		`apiFetch("/api/v1/server/config/refresh"`,
		`apiFetch("/api/v1/cluster/status"`,
		`apiFetch("/api/v1/sipgateway/calls"`,
		`apiFetch("/api/v1/sipgateway/lab/sessions"`,
		`apiFetch("/api/v1/gb28181/lab/sessions"`,
		`apiFetch("/api/v1/recordings"`,
		`apiFetch("/api/v1/recordings/status"`,
		`apiFetch(recordingURL(recordingID)`,
		`recordingPlayURL(recordingID)`,
		`apiFetch("/api/v1/dvr/status"`,
		`apiFetch("/api/v1/dvr/sessions/"`,
		`apiFetch("/api/v1/security/status"`,
		`apiFetch("/api/v1/audit"`,
	} {
		if !strings.Contains(script, call) {
			t.Errorf("management endpoint does not use apiFetch: %s", call)
		}
	}
	managementStart := strings.Index(script, "/* === Management Console === */")
	managementEnd := strings.Index(script, "/* === GB28181 Console === */")
	if managementStart < 0 || managementEnd <= managementStart {
		t.Fatal("management console script boundary is missing")
	}
	if strings.Contains(script[managementStart:managementEnd], "fetch(") {
		t.Error("management console bypasses apiFetch")
	}
	if !strings.Contains(script, `credentials: "same-origin"`) {
		t.Error("apiFetch does not explicitly use the same-origin console session")
	}
	if consoleAttribute(elements["config-validate"], "data-permission") != "viewer" {
		t.Error("config Validate must remain available to read-only viewer roles")
	}
	if !consoleHasAttribute(elements["config-editor"], "readonly") {
		t.Error("config editor must start read-only until a writable source is loaded")
	}
	if !consoleHasAttribute(elements["config-apply"], "disabled") {
		t.Error("config Apply must start disabled until a writable source is loaded")
	}
	if !strings.Contains(script, "documentData.desired_document || documentData.effective_document") {
		t.Error("config editor must prefer desired values so restart-required settings are not overwritten")
	}
	for _, required := range []string{
		"config-editor",
		"config-effective-document",
		"config-source-details",
		"config-schema",
		"documentData.effective_document",
		"documentData.writable",
		"var sourceWritable = false",
		"sourceWritable = !!documentData.writable",
		"updateConfigEditorControls",
	} {
		if !strings.Contains(script, required) && elements[required] == nil {
			t.Errorf("config console is missing complete-document contract %q", required)
		}
	}
	if !strings.Contains(script, "config-schema") || !strings.Contains(script, "JSON.stringify(schemaData, null, 2)") {
		t.Error("config page must render the complete runtime JSON Schema")
	}
	for _, forbidden := range []string{"localStorage", "sessionStorage", "Authorization"} {
		if strings.Contains(script, forbidden) {
			t.Errorf("console script must not render or persist bearer credentials: found %q", forbidden)
		}
	}
}

func TestConsoleProtocolLabErrorsAreVisibleAndReadable(t *testing.T) {

	doc, script := consoleDocument(t)
	elements := consoleElementsByID(doc)
	for _, tableID := range []string{"sip-lab-sessions-tbody", "gb-lab-sessions-tbody"} {
		if elements[tableID] == nil {
			t.Fatalf("protocol lab table %q is missing", tableID)
		}
	}
	for _, contract := range []string{
		`<th>Error</th>`,
		`appendTextCell(row, s.last_error || "-", "lab-error")`,
		`.lab-error`,
		`overflow-wrap: anywhere`,
	} {
		if !strings.Contains(string(consoleHTML), contract) && !strings.Contains(script, contract) {
			t.Errorf("console protocol lab error contract is missing %q", contract)
		}
	}
}

func TestConsoleConfigBrowserRendersDesiredEffectiveSchemaAndSourceDetails(t *testing.T) {
	withConsoleBrowser(t, func(browserCtx context.Context) {
		var probe struct {
			Desired        string `json:"desired"`
			Effective      string `json:"effective"`
			Pending        string `json:"pending"`
			PendingCount   string `json:"pendingCount"`
			SourceDetails  string `json:"sourceDetails"`
			Schema         string `json:"schema"`
			ApplyDisabled  bool   `json:"applyDisabled"`
			EditorReadOnly bool   `json:"editorReadOnly"`
		}
		expression := `(function() {
			managementRole = "admin";
			var desired = "# source comment\nserver:\n  name: desired\napi:\n  auth:\n    bearer_token: '[REDACTED]'\n";
			var effective = "server:\n  name: effective\n";
			apiFetch = function(url) {
				if (url === "/api/v1/server/config") return Promise.resolve({enabled:true, source:"redis", active_version:"v9", pending_restart:["server.name"]});
				if (url === "/api/v1/server/config/document") return Promise.resolve({desired_document:desired, effective_document:effective, writable:true, source_details:{kind:"redis", file:{path:"/etc/liveforge/config.yaml"}, http:{url:"https://config.example.test/liveforge"}, consul:{address:"https://consul.example.test", prefix:"liveforge"}, redis:{addr:"redis.example.test:6379", username:"liveforge", db:0, prefix:"liveforge:", hash:"config", version_key:"config:version", tls:true}}});
				if (url === "/api/v1/server/config/schema") return Promise.resolve({$id:"https://liveforge.dev/schema/v1", $defs:{config:{type:"object"}}});
				return Promise.resolve({});
			};
			window.__configProbe = null;
			refreshRuntimeConfig().then(function() {
				window.__configProbe = {
					desired: document.getElementById("config-editor").value,
					effective: document.getElementById("config-effective-document").textContent,
					pending: document.getElementById("config-pending").textContent,
					pendingCount:  document.getElementById("config-pending-count").textContent,
					sourceDetails: document.getElementById("config-source-details").textContent,
					schema:        document.getElementById("config-schema").textContent,
					applyDisabled: document.getElementById("config-apply").disabled,
					editorReadOnly: document.getElementById("config-editor").readOnly
				};
			});
			return true;
		})()`
		if err := chromedp.Run(browserCtx, chromedp.Evaluate(expression, nil), chromedp.Sleep(20*time.Millisecond), chromedp.Evaluate(`window.__configProbe`, &probe)); err != nil {
			t.Fatalf("probe complete config rendering: %v", err)
		}
		if !strings.Contains(probe.Desired, "name: desired") || !strings.Contains(probe.Desired, "[REDACTED]") || strings.Contains(probe.Desired, "source-secret") {
			t.Errorf("desired config document = %q", probe.Desired)
		}
		if !strings.Contains(probe.Effective, "name: effective") || strings.Contains(probe.Effective, "desired") {
			t.Errorf("effective config document = %q", probe.Effective)
		}
		if probe.Pending != "server.name" || probe.PendingCount != "1" {
			t.Errorf("pending restart rendering = %q/%q", probe.Pending, probe.PendingCount)
		}
		for _, sourceKind := range []string{"\"kind\": \"redis\"", "\"file\"", "\"http\"", "\"consul\"", "\"redis\""} {
			if !strings.Contains(probe.SourceDetails, sourceKind) {
				t.Errorf("source details missing %q: %s", sourceKind, probe.SourceDetails)
			}
		}
		if !strings.Contains(probe.Schema, "$defs") || !strings.Contains(probe.Schema, "config") {
			t.Errorf("schema rendering = %q", probe.Schema)
		}
		if probe.ApplyDisabled || probe.EditorReadOnly {
			t.Errorf("writable config controls disabled: apply=%v readonly=%v", probe.ApplyDisabled, probe.EditorReadOnly)
		}
	})
}

func TestConsoleConfigBrowserKeepsReadOnlySourceSafeAndValidateAvailable(t *testing.T) {
	withConsoleBrowser(t, func(browserCtx context.Context) {
		var probe struct {
			ApplyCalls     int  `json:"applyCalls"`
			ModalOpened    bool `json:"modalOpened"`
			ApplyDisabled  bool `json:"applyDisabled"`
			EditorReadOnly bool `json:"editorReadOnly"`
			Validate       bool `json:"validate"`
		}
		expression := `(function() {
			managementRole = "admin";
			var applyCalls = 0;
			apiFetch = function(url) {
				if (url === "/api/v1/server/config") return Promise.resolve({enabled:true, source:"http"});
				if (url === "/api/v1/server/config/document") return Promise.resolve({desired_document:"server:\n  name: read-only\n", effective_document:"server:\n  name: active\n", writable:false, source_details:{kind:"http"}});
				if (url === "/api/v1/server/config/schema") return Promise.resolve({$id:"https://liveforge.dev/schema/v1"});
				if (url === "/api/v1/server/config/apply") { applyCalls++; return Promise.resolve({}); }
				return Promise.resolve({});
			};
			var modalOpened = false;
			showModal = function() { modalOpened = true; };
			window.__configProbe = null;
			refreshRuntimeConfig().then(function() {
				applyConfigEditor();
				window.__configProbe = {
					applyCalls: applyCalls,
					modalOpened: modalOpened,
					applyDisabled: document.getElementById("config-apply").disabled,
					editorReadOnly: document.getElementById("config-editor").readOnly,
					validate: !document.getElementById("config-validate").disabled
				};
			});
			return true;
		})()`
		if err := chromedp.Run(browserCtx, chromedp.Evaluate(expression, nil), chromedp.Sleep(20*time.Millisecond), chromedp.Evaluate(`window.__configProbe`, &probe)); err != nil {
			t.Fatalf("probe read-only config controls: %v", err)
		}
		if probe.ApplyCalls != 0 || probe.ModalOpened || !probe.ApplyDisabled || !probe.EditorReadOnly || !probe.Validate {
			t.Fatalf("read-only config controls = %#v", probe)
		}
	})
}

func TestConsoleConfigBrowserRevokesWriteStateAfterRefreshFailure(t *testing.T) {
	withConsoleBrowser(t, func(browserCtx context.Context) {
		var probe struct {
			ApplyCalls          int  `json:"applyCalls"`
			OpenedBeforeFailure bool `json:"openedBeforeFailure"`
			ModalOpened         bool `json:"modalOpened"`
			ApplyDisabled       bool `json:"applyDisabled"`
			EditorReadOnly      bool `json:"editorReadOnly"`
		}
		expression := `(function() {
			managementRole = "admin";
			var phase = "writable";
			var applyCalls = 0;
			apiFetch = function(url) {
				if (phase === "failed") return Promise.reject(new Error("refresh failed"));
				if (url === "/api/v1/server/config") return Promise.resolve({enabled:true, source:"file"});
				if (url === "/api/v1/server/config/document") return Promise.resolve({desired_document:"server:\n  name: writable\n", effective_document:"server:\n  name: active\n", writable:true, source_details:{kind:"file"}});
				if (url === "/api/v1/server/config/schema") return Promise.resolve({$id:"https://liveforge.dev/schema/v1"});
				if (url === "/api/v1/server/config/apply") { applyCalls++; return Promise.resolve({}); }
				return Promise.resolve({});
			};
			window.__configProbe = null;
			refreshRuntimeConfig().then(function() {
				applyConfigEditor();
				var openedBeforeFailure = document.getElementById("modal").classList.contains("active");
				var staleConfirmation = pendingAction;
				sourceWritable = false;
				return Promise.resolve(staleConfirmation()).then(function() {
					closeModal();
					phase = "failed";
					return refreshRuntimeConfig().then(function() {
						applyConfigEditor();
						window.__configProbe = {
							applyCalls: applyCalls,
							openedBeforeFailure: openedBeforeFailure,
							modalOpened: document.getElementById("modal").classList.contains("active"),
							applyDisabled: document.getElementById("config-apply").disabled,
							editorReadOnly: document.getElementById("config-editor").readOnly
						};
					});
				});
			});
			return true;
		})()`
		if err := chromedp.Run(browserCtx, chromedp.Evaluate(expression, nil), chromedp.Sleep(20*time.Millisecond), chromedp.Evaluate(`window.__configProbe`, &probe)); err != nil {
			t.Fatalf("probe failed config refresh controls: %v", err)
		}
		if probe.ApplyCalls != 0 || !probe.OpenedBeforeFailure || probe.ModalOpened || !probe.ApplyDisabled || !probe.EditorReadOnly {
			t.Fatalf("failed refresh config controls = %#v", probe)
		}
	})
}

func TestConsoleConfigApplyDoesNotOverwriteNewerEditorRevisionWithStaleDesiredSnapshot(t *testing.T) {
	withConsoleBrowser(t, func(browserCtx context.Context) {
		var probe struct {
			Editor    string `json:"editor"`
			ApplyBody string `json:"applyBody"`
		}
		expression := `(function() {
			managementRole = "admin";
			sourceWritable = true;
			var applyResolve;
			var applyBody = "";
			apiFetch = function(url, options) {
				if (url === "/api/v1/server/config/apply") {
					applyBody = options.body;
					return new Promise(function(resolve) { applyResolve = resolve; });
				}
				if (url === "/api/v1/server/config") return Promise.resolve({enabled:true, source:"file"});
				if (url === "/api/v1/server/config/document") return Promise.resolve({desired_document:"server:\n  name: stale-desired\n", effective_document:"server:\n  name: active\n", writable:true, source_details:{kind:"file"}});
				if (url === "/api/v1/server/config/schema") return Promise.resolve({$id:"https://liveforge.dev/schema/v1"});
				return Promise.resolve({});
			};
			showModal = function(_title, _message, confirm) { pendingAction = confirm; };
			var editor = document.getElementById("config-editor");
			editor.value = "server:\n  name: submitted\n";
			editor.dispatchEvent(new Event("input", {bubbles:true}));
			applyConfigEditor();
			var applyPromise = pendingAction();
			editor.value = "server:\n  name: newer-local-edit\n";
			editor.dispatchEvent(new Event("input", {bubbles:true}));
			document.body.focus();
			applyResolve({status:"written_and_refresh_scheduled"});
			window.__configProbe = null;
			applyPromise.then(function() {
				window.__configProbe = {editor:editor.value, applyBody:applyBody};
			});
			return true;
		})()`
		if err := chromedp.Run(browserCtx, chromedp.Evaluate(expression, nil), chromedp.Sleep(50*time.Millisecond), chromedp.Evaluate(`window.__configProbe`, &probe)); err != nil {
			t.Fatalf("exercise Apply/editor revision race: %v", err)
		}
		if !strings.Contains(probe.ApplyBody, "name: submitted") {
			t.Fatalf("Apply body=%q, want submitted revision", probe.ApplyBody)
		}
		if !strings.Contains(probe.Editor, "name: newer-local-edit") {
			t.Fatalf("editor=%q, newer local edit was overwritten by stale desired snapshot", probe.Editor)
		}
	})
}

func TestConsoleConfigApplyPreservesSubmittedEditorWhenDesiredSnapshotIsStale(t *testing.T) {
	withConsoleBrowser(t, func(browserCtx context.Context) {
		var probe struct {
			Editor    string `json:"editor"`
			ApplyBody string `json:"applyBody"`
		}
		expression := `(function() {
			managementRole = "admin";
			sourceWritable = true;
			var applyResolve;
			var applyBody = "";
			apiFetch = function(url, options) {
				if (url === "/api/v1/server/config/apply") {
					applyBody = options.body;
					return new Promise(function(resolve) { applyResolve = resolve; });
				}
				if (url === "/api/v1/server/config") return Promise.resolve({enabled:true, source:"file"});
				if (url === "/api/v1/server/config/document") return Promise.resolve({desired_document:"server:\n  name: stale-desired\n", effective_document:"server:\n  name: active\n", writable:true, source_details:{kind:"file"}});
				if (url === "/api/v1/server/config/schema") return Promise.resolve({$id:"https://liveforge.dev/schema/v1"});
				return Promise.resolve({});
			};
			showModal = function(_title, _message, confirm) { pendingAction = confirm; };
			var editor = document.getElementById("config-editor");
			editor.value = "server:\n  name: submitted\n";
			editor.dispatchEvent(new Event("input", {bubbles:true}));
			applyConfigEditor();
			var applyPromise = pendingAction();
			document.body.focus();
			applyResolve({status:"written_and_refresh_scheduled"});
			window.__configProbe = null;
			applyPromise.then(function() {
				window.__configProbe = {editor:editor.value, applyBody:applyBody};
			});
			return true;
		})()`
		if err := chromedp.Run(browserCtx, chromedp.Evaluate(expression, nil), chromedp.Sleep(50*time.Millisecond), chromedp.Evaluate(`window.__configProbe`, &probe)); err != nil {
			t.Fatalf("exercise Apply/stale desired race: %v", err)
		}
		if !strings.Contains(probe.ApplyBody, "name: submitted") {
			t.Fatalf("Apply body=%q, want submitted document", probe.ApplyBody)
		}
		if !strings.Contains(probe.Editor, "name: submitted") {
			t.Fatalf("editor=%q, submitted document was overwritten by stale desired snapshot", probe.Editor)
		}
	})
}

func TestConsoleStorageHidesRecordingPlaybackWhenDisabledAndKeepsFMP4Action(t *testing.T) {
	withConsoleBrowser(t, func(browserCtx context.Context) {
		var probe struct {
			DisabledActions int    `json:"disabledActions"`
			EnabledActions  int    `json:"enabledActions"`
			Format          string `json:"format"`
			PlayURL         string `json:"playURL"`
		}
		expression := `(function() {
			managementRole = "admin";
			var item = {id:"archive/cam.mp4", stream_key:"live/cam", format:"fmp4", state:"completed"};
			renderRecordings([item], false);
			var disabledActions = document.querySelectorAll('[data-action="recording-play"]').length;
			renderRecordings([item], true);
			var enabledActions = document.querySelectorAll('[data-action="recording-play"]').length;
			return {disabledActions:disabledActions, enabledActions:enabledActions, format:recordingFormats[item.id], playURL:recordingPlayURL(item.id)};
		})()`
		if err := chromedp.Run(browserCtx, chromedp.Evaluate(expression, &probe)); err != nil {
			t.Fatalf("probe storage playback actions: %v", err)
		}
		if probe.DisabledActions != 0 || probe.EnabledActions != 1 || probe.Format != "fmp4" || probe.PlayURL != "/api/v1/recordings/archive/cam.mp4?action=play" {
			t.Fatalf("storage playback actions = %#v", probe)
		}
	})
}

func TestConsoleRecordingActionURLsPreserveActionLookingIDs(t *testing.T) {
	withConsoleBrowser(t, func(browserCtx context.Context) {
		var probe struct {
			Play     string `json:"play"`
			Download string `json:"download"`
		}
		expression := `({
			play: recordingPlayURL("archive/play"),
			download: recordingDownloadURL("archive/download")
		})`
		if err := chromedp.Run(browserCtx, chromedp.Evaluate(expression, &probe)); err != nil {
			t.Fatalf("probe recording action URLs: %v", err)
		}
		if probe.Play != "/api/v1/recordings/archive/play?action=play" || probe.Download != "/api/v1/recordings/archive/download?action=download" {
			t.Fatalf("recording action URLs = %#v", probe)
		}
	})
}

func TestConsoleAudioOnlyPlaybackUsesWHEP(t *testing.T) {
	doc, script := consoleDocument(t)
	elements := consoleElementsByID(doc)
	if elements["player-audio"] == nil {
		t.Fatal("player modal is missing the audio playback element")
	}
	for _, contract := range []string{
		`document.getElementById("player-audio")`,
		`audio.srcObject = null`,
		`audio.src = ""`,
		`var audioOnlyG711 = !hasVideo && ["g711a", "g711u", "pcma", "pcmu"].indexOf(audioCodec) >= 0`,
		`addWHEPProtocols(protocols, playback)`,
		`monitorPlayableAudio`,
		`remoteStream.addTrack(event.track);`,
		`mediaElement.srcObject = remoteStream;`,
		`if (!expectsVideo && mediaElement.readyState >= 2 && mediaTime > 0)`,
		`markPlaying();`,
		"var playPromise = mediaElement.play();\n      monitorStreamMedia(mediaElement, generation, \"WebRTC/WHEP\", streamKey);",
		`streamMedia[s.stream_key] = { video_codec: "H264", audio_codec: s.codec || "" }`,
		`streamMedia[s.stream_key] = { video_codec: "H264", audio_codec: "G711A" }`,
		`appendActionButton(actions, "Preview", "btn-play", "gb-lab-preview", s.stream_key)`,
		`["h264", "avc", "avc1", "h265", "hevc", "hev1", "hvc1", "av1", "av01", "vp8", "vp08", "vp9", "vp09"]`,
		`button.dataset.videoCodec || "H264"`,
		`button.dataset.audioCodec || ""`,
		`function openPlayer(streamKey, mediaHint)`,
		`preview.playbackMetadata = view.playback`,
		`button.playbackMetadata`,
	} {
		if !strings.Contains(script, contract) {
			t.Errorf("audio-only playback contract is missing %q", contract)
		}
	}
}

func TestConsoleUsesLiveWHEPAsDefaultAndKeepsRealtimeOption(t *testing.T) {

	_, script := consoleDocument(t)
	start := strings.Index(script, "function addWHEPProtocols")
	if start < 0 {
		t.Fatal("addWHEPProtocols is missing")
	}
	end := strings.Index(script[start:], "function addHTTPProtocols")
	if end < 0 {
		t.Fatal("addWHEPProtocols boundary is missing")
	}
	block := script[start : start+end]
	live := strings.Index(block, `id: "whep-live"`)
	realtime := strings.Index(block, `id: "whep-realtime"`)
	if live < 0 || realtime < 0 {
		t.Fatalf("WHEP tabs missing from addWHEPProtocols: %q", block)
	}
	if live > realtime {
		t.Fatalf("default WHEP tab is not live-first: live=%d realtime=%d", live, realtime)
	}
	if !strings.Contains(block, `playWHEP(k, "live"`) {
		t.Fatal("default WHEP tab does not call live mode")
	}
	if !strings.Contains(block, `playWHEP(k, "realtime"`) {
		t.Fatal("realtime WHEP diagnostic tab was removed")
	}
}

func TestConsoleWHEPTabsUseDistinctPlaybackMetadata(t *testing.T) {
	withConsoleBrowser(t, func(browserCtx context.Context) {
		var calls []struct {
			Mode string `json:"mode"`
			Path string `json:"path"`
		}
		expression := `(function() {
			var original = playWHEP;
			var captured = [];
			try {
				playWHEP = function(_streamKey, mode, path) { captured.push({mode:mode, path:path}); };
				endpoints.webrtc = location.host;
				var protocols = [];
				addWHEPProtocols(protocols, {
					whep:"/webrtc/whep/live/camera?mode=live",
					whep_live:"/webrtc/whep/live/camera?mode=live&source=explicit-live",
					whep_realtime:"/webrtc/whep/live/camera?mode=realtime&source=explicit-realtime"
				});
				protocols[0].fn("live/camera");
				protocols[1].fn("live/camera");
				return captured;
			} finally {
				playWHEP = original;
			}
		})()`
		if err := chromedp.Run(browserCtx, chromedp.Evaluate(expression, &calls)); err != nil {
			t.Fatalf("exercise WHEP protocol metadata: %v", err)
		}
		if len(calls) != 2 {
			t.Fatalf("WHEP play calls = %#v, want live and realtime", calls)
		}
		if calls[0].Mode != "live" || calls[0].Path != "/webrtc/whep/live/camera?mode=live&source=explicit-live" {
			t.Fatalf("live WHEP call = %#v", calls[0])
		}
		if calls[1].Mode != "realtime" || calls[1].Path != "/webrtc/whep/live/camera?mode=realtime&source=explicit-realtime" {
			t.Fatalf("realtime WHEP call = %#v", calls[1])
		}
	})
}

func TestConsoleWHEPStalledStatusNamesOnlyStaleExpectedMedia(t *testing.T) {
	withConsoleBrowser(t, func(browserCtx context.Context) {
		var got [][]string
		expression := `[
			whepStalledMediaKinds({
				expected_video:true,
				expected_audio:true,
				updated_at:"2026-08-29T08:00:10Z",
				last_video_at:"2026-08-29T08:00:01Z",
				last_audio_at:"2026-08-29T08:00:09Z"
			}),
			whepStalledMediaKinds({
				expected_video:true,
				expected_audio:true,
				updated_at:"2026-08-29T08:00:10Z",
				last_video_at:"2026-08-29T08:00:09Z"
			}),
			whepStalledMediaKinds({
				expected_video:false,
				expected_audio:true,
				updated_at:"2026-08-29T08:00:10Z",
				last_audio_at:"2026-08-29T08:00:01Z"
			})
		]`
		if err := chromedp.Run(browserCtx, chromedp.Evaluate(expression, &got)); err != nil {
			t.Fatalf("evaluate WHEP stalled media diagnostics: %v", err)
		}
		want := [][]string{{"video"}, {"audio"}, {"audio"}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("stalled media kinds = %#v, want %#v", got, want)
		}
	})
}

func TestConsoleWHEPVideoAutoplayFallbackKeepsAudioControl(t *testing.T) {
	doc, script := consoleDocument(t)
	elements := consoleElementsByID(doc)
	if elements["btn-player-audio"] == nil {
		t.Fatal("player modal is missing the audio control")
	}
	for _, contract := range []string{
		`function togglePlayerAudio()`,
		`mediaElement.muted = true`,
		`mediaElement.play()`,
		`btn-player-audio`,
		`setPlayerAudioControl`,
	} {
		if !strings.Contains(script, contract) && !strings.Contains(string(consoleHTML), contract) {
			t.Errorf("WHEP autoplay fallback contract is missing %q", contract)
		}
	}
}

func TestConsoleManagementActionsAreConfirmedAndPermissionAware(t *testing.T) {
	_, script := consoleDocument(t)
	for _, contract := range []struct {
		name    string
		pattern string
	}{
		{"config refresh", `(?s)function refreshRuntimeConfigNow\([^}]*apiFetch\("/api/v1/server/config/refresh",\s*\{\s*method:\s*"POST"`},
		{"SIP dial", `(?s)function dialSIPCall\([^}]*apiFetch\("/api/v1/sipgateway/calls",\s*\{\s*method:\s*"POST"`},
		{"SIP hangup", `(?s)function hangupSIPCall\([^}]*showModal\([^}]*apiFetch\("/api/v1/sipgateway/calls/"[^}]*method:\s*"DELETE"`},
		{"recording delete", `(?s)function deleteRecording\([^}]*showModal\([^}]*apiFetch\("/api/v1/recordings/"[^}]*method:\s*"DELETE"`},
		{"stream kick promise", `(?s)function kickStream\([^}]*showModal\([^}]*return apiFetch\("/api/v1/streams/"`},
		{"stream delete promise", `(?s)function deleteStream\([^}]*showModal\([^}]*return apiFetch\("/api/v1/streams/"`},
	} {
		if !regexp.MustCompile(contract.pattern).MatchString(script) {
			t.Errorf("%s contract is missing", contract.name)
		}
	}
	for _, status := range []string{"case 401:", "case 403:", "case 409:", "case 503:"} {
		if !strings.Contains(script, status) {
			t.Errorf("typed management error handling is missing %s", status)
		}
	}
	if !strings.Contains(script, "canManage") || !strings.Contains(script, "applyManagementPermissions") {
		t.Error("management controls are not permission-aware")
	}
}

func TestConsoleManagementDynamicActionsUseDOMBindings(t *testing.T) {
	_, script := consoleDocument(t)
	for _, function := range []string{"renderStreams", "renderSIPCalls", "renderRecordings", "renderDVR", "renderDevicesTable", "renderSessionsTable"} {
		source := consoleFunctionSource(t, script, function)
		if strings.Contains(source, "onclick=") {
			t.Errorf("%s still constructs inline click handlers", function)
		}
	}
	for _, required := range []string{`dataset.action`, `dataset.actionId`, `addEventListener("click"`, "dynamicActionHandlers"} {
		if !strings.Contains(script, required) {
			t.Errorf("dynamic DOM action binding is missing %q", required)
		}
	}
}

type consoleDynamicActionProbe struct {
	Actions            []string          `json:"actions"`
	Values             map[string]string `json:"values"`
	Invoked            map[string]string `json:"invoked"`
	InlineHandlerCount int               `json:"inlineHandlerCount"`
	InjectedAttribute  bool              `json:"injectedAttribute"`
}

type consoleInteractionProbe struct {
	ArrowSelected       string `json:"arrowSelected"`
	ArrowFocus          string `json:"arrowFocus"`
	HomeSelected        string `json:"homeSelected"`
	EndSelected         string `json:"endSelected"`
	InitialModalFocus   string `json:"initialModalFocus"`
	TabWrapFocus        string `json:"tabWrapFocus"`
	ShiftTabWrapFocus   string `json:"shiftTabWrapFocus"`
	ContainedModalFocus string `json:"containedModalFocus"`
	EscapeClosed        bool   `json:"escapeClosed"`
	RestoredFocus       string `json:"restoredFocus"`
	PendingDisabled     bool   `json:"pendingDisabled"`
	PendingOpen         bool   `json:"pendingOpen"`
	SettledDisabled     bool   `json:"settledDisabled"`
	SettledOpen         bool   `json:"settledOpen"`
}

type consoleRerenderedActionFocusProbe struct {
	ModalInitiallyOpen bool   `json:"modalInitiallyOpen"`
	TriggerReplaced    bool   `json:"triggerReplaced"`
	EscapeClosed       bool   `json:"escapeClosed"`
	FocusedReplacement bool   `json:"focusedReplacement"`
	FocusedTag         string `json:"focusedTag"`
	FocusedAction      string `json:"focusedAction"`
	FocusedActionID    string `json:"focusedActionId"`
}

type consoleRemovedActionFocusProbe struct {
	ModalInitiallyOpen bool   `json:"modalInitiallyOpen"`
	TriggerRemoved     bool   `json:"triggerRemoved"`
	EscapeClosed       bool   `json:"escapeClosed"`
	FocusedID          string `json:"focusedId"`
}

type consoleProtocolLabMediaProbe struct {
	CacheHeader   string `json:"cacheHeader"`
	CacheText     string `json:"cacheText"`
	AudioOnlyText string `json:"audioOnlyText"`
	GOPBar        bool   `json:"gopBar"`
	SIPAudioRTP   string `json:"sipAudioRTP"`
	SIPVideoRTP   string `json:"sipVideoRTP"`
	SIPVideoCodec string `json:"sipVideoCodec"`
	SIPAudioCodec string `json:"sipAudioCodec"`
	GBVideoCodec  string `json:"gbVideoCodec"`
	GBAudioCodec  string `json:"gbAudioCodec"`
}

func withConsoleBrowser(t *testing.T, run func(context.Context)) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping management console browser behavior in short mode")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/console" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(consoleHTML)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/console/static/") {
			w.Header().Set("Content-Type", "application/javascript")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	defer server.Close()

	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-dev-shm-usage", true),
	)...)
	defer allocCancel()
	browserCtx, browserCancel := chromedp.NewContext(allocCtx)
	defer browserCancel()
	if err := chromedp.Run(browserCtx, chromedp.Navigate(server.URL+"/console"), chromedp.WaitReady("body")); err != nil {
		if strings.Contains(err.Error(), "websocket url timeout") || strings.Contains(err.Error(), "executable file not found") {
			t.Skipf("headless Chrome unavailable: %v", err)
		}
		t.Fatalf("open management console: %v", err)
	}
	if err := chromedp.Run(browserCtx, chromedp.Evaluate(`stopActiveViewPolling()`, nil)); err != nil {
		t.Fatalf("stop console polling: %v", err)
	}
	run(browserCtx)
}

func TestConsoleManagementBrowserBehavior(t *testing.T) {
	withConsoleBrowser(t, func(browserCtx context.Context) {
		hostile := "quote\" apostrophe' slash\\ line\nreturn\rseparator\u2028paragraph\u2029<>&"
		hostileJSON, err := json.Marshal(hostile)
		if err != nil {
			t.Fatal(err)
		}
		var dynamic consoleDynamicActionProbe
		dynamicExpression := fmt.Sprintf(`(function() {
			var hostile = %s;
			managementRole = "admin";
			renderStreams([{key:hostile,state:"publishing",publisher:"publisher",subscribers:{},stats:{}}]);
			renderSIPCalls([{call_id:hostile,direction:"outbound",stream_key:hostile,state:"active"}]);
			renderRecordings([{id:hostile,stream_key:hostile,state:"completed"}]);
			renderDVR({sessions:[{stream_key:hostile,live:true,segments:1}]});
			gbExpandedDevices[hostile] = true;
			gbDeviceChannels[hostile] = [{channel_id:hostile,name:hostile,status:"ON",ptz_type:1}];
			renderDevicesTable([{device_id:hostile,status:"online",channel_count:1}]);
			renderSessionsTable([{id:hostile,channel_id:hostile,stream_key:hostile,direction:"play",state:"streaming"}]);
			var nodes = Array.from(document.querySelectorAll("[data-action-id]"));
			var invoked = {};
			nodes.forEach(function(node) {
				dynamicActionHandlers[node.dataset.action] = function(value) { invoked[node.dataset.action] = value; };
				node.click();
			});
			var values = {};
			nodes.forEach(function(node) { values[node.dataset.action] = node.dataset.actionId; });
			return {
				actions: nodes.map(function(node) { return node.dataset.action; }).sort(),
				values: values,
				invoked: invoked,
				inlineHandlerCount: nodes.filter(function(node) { return node.hasAttribute("onclick"); }).length,
				injectedAttribute: !!document.querySelector("[data-injected]")
			};
		})()`, hostileJSON)
		if err := chromedp.Run(browserCtx, chromedp.Evaluate(dynamicExpression, &dynamic)); err != nil {
			t.Fatalf("probe hostile management identifiers: %v", err)
		}
		wantActions := []string{"dvr-detail", "dvr-play", "gb-catalog", "gb-close-session", "gb-delete-device", "gb-play", "gb-playback", "gb-preview", "gb-ptz", "gb-stop", "gb-toggle-device", "recording-delete", "recording-detail", "recording-download", "recording-play", "sip-detail", "sip-hangup", "stream-delete", "stream-kick", "stream-preview"}
		if strings.Join(dynamic.Actions, ",") != strings.Join(wantActions, ",") {
			t.Fatalf("dynamic actions = %v, want %v", dynamic.Actions, wantActions)
		}
		if dynamic.InlineHandlerCount != 0 || dynamic.InjectedAttribute {
			t.Fatalf("unsafe dynamic DOM: inline=%d injected=%v", dynamic.InlineHandlerCount, dynamic.InjectedAttribute)
		}
		for _, action := range wantActions {
			if dynamic.Values[action] != hostile || dynamic.Invoked[action] != hostile {
				t.Errorf("action %q value=%q invoked=%q", action, dynamic.Values[action], dynamic.Invoked[action])
			}
		}

		var interaction consoleInteractionProbe
		interactionExpression := `(function() {
			dynamicActionHandlers = createDynamicActionHandlers();
			updateGB28181Tab([]);
			switchTab("streams");
			var streamTab = document.getElementById("tab-streams");
			streamTab.focus();
			streamTab.dispatchEvent(new KeyboardEvent("keydown", {key:"ArrowRight", bubbles:true}));
			var arrowSelected = activeTab;
			var arrowFocus = document.activeElement.id;
			document.activeElement.dispatchEvent(new KeyboardEvent("keydown", {key:"Home", bubbles:true}));
			var homeSelected = activeTab;
			document.activeElement.dispatchEvent(new KeyboardEvent("keydown", {key:"End", bubbles:true}));
			var endSelected = activeTab;

			switchTab("config");
			var trigger = document.getElementById("config-refresh");
			trigger.disabled = false;
			trigger.focus();
			showModal("Confirm action", "Confirm description", function() { return Promise.resolve(); });
			var initialModalFocus = document.activeElement.id;
			document.activeElement.dispatchEvent(new KeyboardEvent("keydown", {key:"Tab", bubbles:true}));
			var tabWrapFocus = document.activeElement.id;
			document.activeElement.dispatchEvent(new KeyboardEvent("keydown", {key:"Tab", shiftKey:true, bubbles:true}));
			var shiftTabWrapFocus = document.activeElement.id;
			document.getElementById("stream-search").focus();
			var containedModalFocus = document.activeElement.id;
			document.activeElement.dispatchEvent(new KeyboardEvent("keydown", {key:"Escape", bubbles:true}));
			var escapeClosed = !document.getElementById("modal").classList.contains("active");
			var restoredFocus = document.activeElement.id;

			window.__resolveConsoleMutation = null;
			apiFetch = function() { return new Promise(function(resolve) { window.__resolveConsoleMutation = resolve; }); };
			trigger.focus();
			kickStream("deferred/stream");
			document.getElementById("modal-confirm").click();
			var pendingDisabled = document.getElementById("modal-confirm").disabled;
			var pendingOpen = document.getElementById("modal").classList.contains("active");
			return {
				arrowSelected:arrowSelected, arrowFocus:arrowFocus, homeSelected:homeSelected, endSelected:endSelected,
				initialModalFocus:initialModalFocus, tabWrapFocus:tabWrapFocus, shiftTabWrapFocus:shiftTabWrapFocus,
				containedModalFocus:containedModalFocus,
				escapeClosed:escapeClosed, restoredFocus:restoredFocus,
				pendingDisabled:pendingDisabled, pendingOpen:pendingOpen
			};
		})()`
		if err := chromedp.Run(browserCtx, chromedp.Evaluate(interactionExpression, &interaction)); err != nil {
			t.Fatalf("probe keyboard and deferred behavior: %v", err)
		}
		var settled struct {
			Disabled bool `json:"disabled"`
			Open     bool `json:"open"`
		}
		if err := chromedp.Run(browserCtx,
			chromedp.Evaluate(`window.__resolveConsoleMutation({})`, nil),
			chromedp.Sleep(10*time.Millisecond),
			chromedp.Evaluate(`({disabled:document.getElementById("modal-confirm").disabled,open:document.getElementById("modal").classList.contains("active")})`, &settled),
		); err != nil {
			t.Fatalf("settle deferred destructive action: %v", err)
		}
		interaction.SettledDisabled = settled.Disabled
		interaction.SettledOpen = settled.Open
		if interaction.ArrowSelected != "sip" || interaction.ArrowFocus != "tab-sip" || interaction.HomeSelected != "streams" || interaction.EndSelected != "security" {
			t.Errorf("tab keyboard behavior = %#v", interaction)
		}
		if interaction.InitialModalFocus != "modal-confirm" || interaction.TabWrapFocus != "modal-cancel" || interaction.ShiftTabWrapFocus != "modal-confirm" || interaction.ContainedModalFocus != "modal-confirm" || !interaction.EscapeClosed || interaction.RestoredFocus != "config-refresh" {
			t.Errorf("modal focus behavior = %#v", interaction)
		}
		if !interaction.PendingDisabled || !interaction.PendingOpen || interaction.SettledDisabled || interaction.SettledOpen {
			t.Errorf("deferred destructive behavior = %#v", interaction)
		}
	})
}

func TestConsoleProtocolLabMediaAndCacheRendering(t *testing.T) {
	withConsoleBrowser(t, func(browserCtx context.Context) {
		var probe consoleProtocolLabMediaProbe
		expression := `(function() {
			renderStreams([{
				key:"sip/lab", state:"publishing", publisher:"sip", subscribers:{}, stats:{},
				gop_cache_len:26, gop_video_frames:25, gop_audio_frames:1, gop_duration_ms:960, gop_generation:7,
				video_codec:"H264", audio_codec:"AAC"
			}, {
				key:"audio-only", state:"publishing", publisher:"audio", subscribers:{}, stats:{},
				video_codec:"", audio_codec:"PCMA", gop_cache_len:0, gop_video_frames:0, gop_audio_frames:0, gop_duration_ms:0, gop_generation:0
			}]);
			renderSIPLabSessions([{session:{
				id:"sip-1", device_id:"d1", mode:"publish", stream_key:"sip/lab", state:"active", codec:"PCMA",
				audio_rtp_packets_sent:50, audio_rtp_packets_received:2,
				video_rtp_packets_sent:25, video_rtp_packets_received:1
			}, playback:{available:true}}]);
			renderGBLabSessions([{session:{
				id:"gb-1", device_id:"d2", channel_id:"c1", mode:"publish", stream_key:"gb/lab", state:"active"
			}, playback:{available:true}}]);
			var rows = Array.from(document.querySelectorAll("#tbody tr"));
			var rowFor = function(key) { return rows.find(function(row) { return row.cells[0].textContent === key; }); };
			var videoRow = rowFor("sip/lab");
			var audioRow = rowFor("audio-only");
			var sipCells = document.querySelectorAll("#sip-lab-sessions-tbody tr td");
			return {
				cacheHeader: document.querySelector("#view-streams thead th:nth-child(8)").textContent.trim(),
				cacheText: videoRow ? videoRow.cells[7].textContent : "",
				audioOnlyText: audioRow ? audioRow.cells[7].textContent : "",
				gopBar: !!document.querySelector("#tbody .gop-bar"),
				sipAudioRTP: sipCells[5] ? sipCells[5].textContent : "",
				sipVideoRTP: sipCells[6] ? sipCells[6].textContent : "",
				sipVideoCodec: (streamMedia["sip/lab"] || {}).video_codec || "",
				sipAudioCodec: (streamMedia["sip/lab"] || {}).audio_codec || "",
				gbVideoCodec: (streamMedia["gb/lab"] || {}).video_codec || "",
				gbAudioCodec: (streamMedia["gb/lab"] || {}).audio_codec || ""
			};
		})()`
		if err := chromedp.Run(browserCtx, chromedp.Evaluate(expression, &probe)); err != nil {
			t.Fatalf("probe protocol lab media rendering: %v", err)
		}
		if probe.CacheHeader != "GOP Cache" || probe.CacheText != "GOP #7 26 frames / 1.0s V25 A1" || probe.AudioOnlyText != "Not applicable (audio-only)" || probe.GOPBar {
			t.Errorf("cache rendering = header %q text %q", probe.CacheHeader, probe.CacheText)
		}
		if probe.SIPAudioRTP != "50 tx / 2 rx" || probe.SIPVideoRTP != "25 tx / 1 rx" {
			t.Errorf("SIP track RTP = audio %q video %q", probe.SIPAudioRTP, probe.SIPVideoRTP)
		}
		if probe.SIPVideoCodec != "H264" || probe.SIPAudioCodec != "PCMA" {
			t.Errorf("SIP media hint = %s/%s, want H264/PCMA", probe.SIPVideoCodec, probe.SIPAudioCodec)
		}
		if probe.GBVideoCodec != "H264" || probe.GBAudioCodec != "G711A" {
			t.Errorf("GB28181 media hint = %s/%s, want H264/G711A", probe.GBVideoCodec, probe.GBAudioCodec)
		}
	})
}

func TestConsoleProtocolLabPreviewUsesEscapedPlaybackMetadata(t *testing.T) {
	withConsoleBrowser(t, func(browserCtx context.Context) {
		const streamKey = "tenant/cam?variant#one%raw"
		var probe struct {
			SIPURL      string `json:"sipURL"`
			GBURL       string `json:"gbURL"`
			FallbackURL string `json:"fallbackURL"`
		}
		expression := `(function() {
			endpoints.http = location.host;
			var streamKey = "tenant/cam?variant#one%raw";
			var playback = {
				available:true,
				http_flv:"/tenant/cam%3Fvariant%23one%25raw.flv",
				ws_flv:"/ws/tenant/cam%3Fvariant%23one%25raw.flv",
				http_ts:"/tenant/cam%3Fvariant%23one%25raw.ts",
				fmp4:"/tenant/cam%3Fvariant%23one%25raw.mp4",
				hls:"/tenant/cam%3Fvariant%23one%25raw.m3u8",
				dash:"/tenant/cam%3Fvariant%23one%25raw.mpd"
			};
			renderSIPLabSessions([{session:{
				id:"sip-escaped", device_id:"d1", mode:"publish", stream_key:streamKey, state:"active", codec:"PCMA"
			}, playback:playback}]);
			document.querySelector('[data-action="sip-lab-preview"]').click();
			var sipURL = document.getElementById("player-url").textContent;
			closePlayer();

			renderGBLabSessions([{session:{
				id:"gb-escaped", device_id:"d2", channel_id:"c1", mode:"publish", stream_key:streamKey, state:"active"
			}, playback:playback}]);
			document.querySelector('[data-action="gb-lab-preview"]').click();
			var gbURL = document.getElementById("player-url").textContent;
			closePlayer();

			openPlayer(streamKey, {video_codec:"H264", audio_codec:"AAC"});
			return {
				sipURL:sipURL,
				gbURL:gbURL,
				fallbackURL:document.getElementById("player-url").textContent
			};
		})()`
		if err := chromedp.Run(browserCtx, chromedp.Evaluate(expression, &probe)); err != nil {
			t.Fatalf("probe escaped protocol lab preview URLs: %v", err)
		}
		const escapedPath = "/tenant/cam%3Fvariant%23one%25raw.flv"
		for label, got := range map[string]string{
			"SIP Lab":  probe.SIPURL,
			"GB Lab":   probe.GBURL,
			"fallback": probe.FallbackURL,
		} {
			if !strings.HasSuffix(got, escapedPath) {
				t.Errorf("%s preview URL = %q, want suffix %q", label, got, escapedPath)
			}
		}
	})
}

func TestConsoleProtocolLabRejectsAmbiguousStreamKeySegmentsBeforeRequest(t *testing.T) {
	withConsoleBrowser(t, func(browserCtx context.Context) {
		var probe struct {
			Requests      int      `json:"requests"`
			MissingErrors []string `json:"missingErrors"`
		}
		expression := `(function() {
			managementRole = "admin";
			var requests = 0;
			apiFetch = function() { requests++; return new Promise(function() {}); };
			document.getElementById("sip-lab-device").value = "d1";
			document.getElementById("gb-lab-device").value = "34020000001320000001";
			document.getElementById("gb-lab-channel").value = "34020000001320000002";
			var invalid = ["/tenant/cam", "tenant/cam/", "tenant//cam", "tenant/./cam", "tenant/../cam", ".", ".."];
			var missingErrors = [];
			invalid.forEach(function(streamKey) {
				document.getElementById("sip-lab-stream").value = streamKey;
				startSIPLab({preventDefault:function() {}});
				if (!document.getElementById("sip-lab-error").textContent) missingErrors.push("sip:" + streamKey);
				document.getElementById("gb-lab-stream").value = streamKey;
				startGBLab({preventDefault:function() {}});
				if (!document.getElementById("gb-lab-error").textContent) missingErrors.push("gb:" + streamKey);
			});
			document.getElementById("sip-lab-stream").value = "tenant/.../sip";
			startSIPLab({preventDefault:function() {}});
			document.getElementById("gb-lab-stream").value = "tenant/.../gb";
			startGBLab({preventDefault:function() {}});
			return {requests:requests, missingErrors:missingErrors};
		})()`
		if err := chromedp.Run(browserCtx, chromedp.Evaluate(expression, &probe)); err != nil {
			t.Fatalf("probe protocol lab stream-key validation: %v", err)
		}
		if probe.Requests != 2 {
			t.Errorf("protocol lab API requests = %d, want only the two valid stream keys", probe.Requests)
		}
		if len(probe.MissingErrors) != 0 {
			t.Errorf("invalid stream keys without Console errors: %v", probe.MissingErrors)
		}
	})
}

func TestConsoleManagementModalRestoresFocusAfterRecordingRerender(t *testing.T) {
	withConsoleBrowser(t, func(browserCtx context.Context) {
		const recordingID = `archive/focus"race\\clip.mp4`
		recordingJSON, err := json.Marshal(recordingID)
		if err != nil {
			t.Fatal(err)
		}
		var probe consoleRerenderedActionFocusProbe
		setupExpression := fmt.Sprintf(`(function() {
			var recording = {id:%s,stream_key:"live/focus",state:"completed"};
			managementRole = "admin";
			switchTab("storage");
			stopActiveViewPolling();
			renderRecordings([recording]);
			var original = Array.from(document.querySelectorAll("[data-action]"))
				.find(function(node) { return node.dataset.action === "recording-delete" && node.dataset.actionId === recording.id; });
			original.focus();
			original.click();
			var modalInitiallyOpen = document.getElementById("modal").classList.contains("active") &&
				document.activeElement === document.getElementById("modal-confirm");
			renderRecordings([recording]);
			var replacement = Array.from(document.querySelectorAll("[data-action]"))
				.find(function(node) { return node.dataset.action === "recording-delete" && node.dataset.actionId === recording.id; });
			window.__modalFocusRace = {
				modalInitiallyOpen: modalInitiallyOpen,
				triggerReplaced: replacement !== original,
				replacement: replacement
			};
		})()`, recordingJSON)
		resultExpression := `({
			modalInitiallyOpen: window.__modalFocusRace.modalInitiallyOpen,
			triggerReplaced: window.__modalFocusRace.triggerReplaced,
				escapeClosed: !document.getElementById("modal").classList.contains("active"),
				focusedReplacement: document.activeElement === window.__modalFocusRace.replacement,
				focusedTag: document.activeElement.tagName,
				focusedAction: document.activeElement.dataset.action || "",
				focusedActionId: document.activeElement.dataset.actionId || ""
			})`
		if err := chromedp.Run(browserCtx,
			chromedp.Evaluate(setupExpression, nil),
			chromedp.KeyEvent(kb.Escape),
			chromedp.Evaluate(resultExpression, &probe),
		); err != nil {
			t.Fatalf("probe modal focus after recording refresh: %v", err)
		}
		if !probe.ModalInitiallyOpen || !probe.TriggerReplaced || !probe.EscapeClosed || !probe.FocusedReplacement || probe.FocusedAction != "recording-delete" || probe.FocusedActionID != recordingID {
			t.Fatalf("modal focus after recording refresh = %#v, want focus on equivalent replacement recording-delete action %q", probe, recordingID)
		}
	})
}

func TestConsoleManagementModalFallsBackToSelectedTabWhenActionDisappears(t *testing.T) {
	withConsoleBrowser(t, func(browserCtx context.Context) {
		var probe consoleRemovedActionFocusProbe
		setupExpression := `(function() {
			managementRole = "admin";
			switchTab("storage");
			stopActiveViewPolling();
			renderRecordings([{id:"archive/removed.mp4",stream_key:"live/focus",state:"completed"}]);
			var original = Array.from(document.querySelectorAll("[data-action]"))
				.find(function(node) { return node.dataset.action === "recording-delete" && node.dataset.actionId === "archive/removed.mp4"; });
			original.focus();
			original.click();
			window.__removedModalFocusRace = {
				modalInitiallyOpen: document.getElementById("modal").classList.contains("active") &&
					document.activeElement === document.getElementById("modal-confirm"),
				original: original
			};
			renderRecordings([]);
		})()`
		resultExpression := `({
			modalInitiallyOpen: window.__removedModalFocusRace.modalInitiallyOpen,
			triggerRemoved: !window.__removedModalFocusRace.original.isConnected,
			escapeClosed: !document.getElementById("modal").classList.contains("active"),
			focusedId: document.activeElement.id
		})`
		if err := chromedp.Run(browserCtx,
			chromedp.Evaluate(setupExpression, nil),
			chromedp.KeyEvent(kb.Escape),
			chromedp.Evaluate(resultExpression, &probe),
		); err != nil {
			t.Fatalf("probe modal fallback after recording removal: %v", err)
		}
		if !probe.ModalInitiallyOpen || !probe.TriggerRemoved || !probe.EscapeClosed || probe.FocusedID != "tab-storage" {
			t.Fatalf("modal focus after recording removal = %#v, want selected Storage tab fallback", probe)
		}
	})
}

func TestConsoleManagementPollingStopsForHiddenViews(t *testing.T) {
	doc, script := consoleDocument(t)
	for _, required := range []string{
		"new AbortController()",
		"abortActiveViewRequest()",
		`document.addEventListener("visibilitychange"`,
		"startActiveViewPolling()",
		"stopActiveViewPolling()",
	} {
		if !strings.Contains(script, required) {
			t.Errorf("bounded view polling is missing %q", required)
		}
	}

	switchSource := consoleFunctionSource(t, script, "switchTab")
	for _, required := range []string{
		`getAttribute("data-view") === name`,
		`querySelectorAll("[id^=\"view-\"]")`,
		`view.hidden = view.id !== "view-" + name`,
		"stopActiveViewPolling()",
		"startActiveViewPolling()",
	} {
		if !strings.Contains(switchSource, required) {
			t.Errorf("switchTab does not bind every navigation view: missing %q", required)
		}
	}

	refreshSource := consoleFunctionSource(t, script, "refreshActiveView")
	if !strings.Contains(refreshSource, "document.hidden") || !strings.Contains(refreshSource, "activeViewRefreshers[activeTab]") {
		t.Error("active view refresh is not visibility-bound and selected by activeTab")
	}
	startSource := consoleFunctionSource(t, script, "startActiveViewPolling")
	for _, required := range []string{"stopActiveViewPolling()", "document.hidden", "activeViewInterval = setInterval(refreshActiveView"} {
		if !strings.Contains(startSource, required) {
			t.Errorf("active view polling start is missing %q", required)
		}
	}
	stopSource := consoleFunctionSource(t, script, "stopActiveViewPolling")
	for _, required := range []string{"clearInterval(activeViewInterval)", "abortActiveViewRequest()"} {
		if !strings.Contains(stopSource, required) {
			t.Errorf("active view polling stop is missing %q", required)
		}
	}

	for _, view := range consoleNavigationViews(doc) {
		if !regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(view) + `\s*:`).MatchString(script) {
			t.Errorf("active polling has no refresher for view %q", view)
		}
	}
	for _, obsolete := range []string{"gbInterval", "streamsInterval", "serverInfoInterval"} {
		if strings.Contains(script, obsolete) {
			t.Errorf("console retains obsolete global polling state %q", obsolete)
		}
	}
	initStart := strings.Index(script, "/* === Init === */")
	if initStart < 0 {
		t.Fatal("console init boundary is missing")
	}
	if strings.Contains(script[initStart:], "setInterval(") {
		t.Error("console init creates polling intervals outside the active-view lifecycle")
	}
}
