package api

import (
	"regexp"
	"strings"
	"testing"

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

func consoleAttribute(node *html.Node, name string) string {
	for _, attr := range node.Attr {
		if attr.Key == name {
			return attr.Val
		}
	}
	return ""
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

func TestConsoleManagementViewsExposeSupportedControlPlanes(t *testing.T) {
	doc, _ := consoleDocument(t)
	elements := consoleElementsByID(doc)
	for _, id := range []string{
		"nav-tabs",
		"view-streams",
		"view-gb28181",
		"view-config",
		"view-cluster",
		"view-sip",
		"view-storage",
		"view-security",
		"config-refresh",
		"sip-target-uri",
		"sip-stream-key",
		"recordings-tbody",
		"recording-detail",
		"dvr-tbody",
		"audit-tbody",
	} {
		if elements[id] == nil {
			t.Errorf("management console is missing element %q", id)
		}
	}

	views := consoleNavigationViews(doc)
	wantViews := []string{"streams", "gb28181", "config", "cluster", "sip", "storage", "security"}
	if strings.Join(views, ",") != strings.Join(wantViews, ",") {
		t.Fatalf("navigation views = %v, want %v", views, wantViews)
	}
	for _, view := range views {
		if elements["view-"+view] == nil {
			t.Errorf("navigation view %q has no matching container", view)
		}
	}
}

func TestConsoleManagementRequestsUseSessionSafeHelper(t *testing.T) {
	_, script := consoleDocument(t)
	for _, call := range []string{
		`apiFetch("/api/v1/server/config"`,
		`apiFetch("/api/v1/server/config/refresh"`,
		`apiFetch("/api/v1/cluster/status"`,
		`apiFetch("/api/v1/sipgateway/calls"`,
		`apiFetch("/api/v1/recordings"`,
		`apiFetch("/api/v1/recordings/status"`,
		`apiFetch(recordingURL(recordingID)`,
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
	for _, forbidden := range []string{"localStorage", "sessionStorage", "Authorization"} {
		if strings.Contains(script, forbidden) {
			t.Errorf("console script must not render or persist bearer credentials: found %q", forbidden)
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
