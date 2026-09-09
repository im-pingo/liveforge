package api

import (
	"context"
	"testing"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

func consoleAwait(expression string, result any) chromedp.Action {
	return chromedp.Evaluate(expression, result, func(params *runtime.EvaluateParams) *runtime.EvaluateParams {
		return params.WithAwaitPromise(true)
	})
}

const consoleDraftFixture = `
managementRole = "admin";
window.__desired = "server:\n  name: original\n";
window.__revision = "revision-1";
window.__requests = {schema:0, document:0};
apiFetch = function(url) {
  if (url === "/api/v1/server/config") return Promise.resolve({enabled:true, source:"file", active_hash:"effective-1", document_revision:__revision});
  if (url === "/api/v1/server/config/document") {
    __requests.document++;
    return Promise.resolve({desired_document:__desired, effective_document:"server:\n  name: effective\n", writable:true, document_revision:__revision});
  }
  if (url === "/api/v1/server/config/schema") { __requests.schema++; return Promise.resolve({$id:"config-v1"}); }
  return Promise.resolve({});
};
await refreshRuntimeConfig();
`

func TestConsoleConfigDraftSurvivesBlurPollingAndViewSwitch(t *testing.T) {
	withConsoleBrowser(t, func(ctx context.Context) {
		var result struct {
			Editor       string `json:"editor"`
			DirtyGuard   bool   `json:"dirtyGuard"`
			DiscardReady bool   `json:"discardReady"`
		}
		if err := chromedp.Run(ctx,
			consoleAwait(`(async function(){`+consoleDraftFixture+`switchTab("config"); stopActiveViewPolling();})()`, nil),
			chromedp.Focus("#config-editor"),
			chromedp.SendKeys("#config-editor", "# unsaved local draft\n"),
			chromedp.Click("#tab-streams"),
			consoleAwait(`(async function(){
              stopActiveViewPolling();
              await refreshRuntimeConfig();
              switchTab("config"); stopActiveViewPolling();
              await refreshRuntimeConfig();
              var event = new Event("beforeunload", {cancelable:true});
              window.dispatchEvent(event);
              var discard = document.getElementById("config-discard");
              return {editor:document.getElementById("config-editor").value, dirtyGuard:event.defaultPrevented, discardReady:!!discard && !discard.disabled};
            })()`, &result),
		); err != nil {
			t.Fatal(err)
		}
		if result.Editor != "# unsaved local draft\nserver:\n  name: original\n" && result.Editor != "server:\n  name: original\n# unsaved local draft\n" {
			t.Fatalf("blurred draft was replaced: %#v", result)
		}
		if !result.DirtyGuard || !result.DiscardReady {
			t.Fatalf("unsaved draft controls are missing: %#v", result)
		}
	})
}

func TestConsoleConfigDraftDiscardUsesLatestSourceAndClearsGuard(t *testing.T) {
	withConsoleBrowser(t, func(ctx context.Context) {
		var result struct {
			Before  string `json:"before"`
			After   string `json:"after"`
			Guard   bool   `json:"guard"`
			Modal   bool   `json:"modal"`
			Focused bool   `json:"focused"`
		}
		err := chromedp.Run(ctx, consoleAwait(`(async function(){`+consoleDraftFixture+`
          switchTab("config"); stopActiveViewPolling();
          var editor = document.getElementById("config-editor");
          editor.value = "# draft\n"; editor.dispatchEvent(new Event("input")); editor.blur();
          __desired = "# changed source\n"; __revision = "revision-2";
          await refreshRuntimeConfig();
          var before = editor.value;
          var discard = document.getElementById("config-discard");
          if (!discard) return {before:before};
          discard.click();
          var modal = document.getElementById("modal").classList.contains("active");
          document.getElementById("modal-cancel").click();
          if (editor.value !== before) throw new Error("Cancel discarded the draft");
          discard.focus(); discard.click(); await pendingAction(); closeModal();
          var event = new Event("beforeunload", {cancelable:true}); window.dispatchEvent(event);
          return {before:before, after:editor.value, guard:event.defaultPrevented, modal:modal, focused:document.activeElement === discard || document.activeElement === editor};
        })()`, &result))
		if err != nil {
			t.Fatal(err)
		}
		if result.Before != "# draft\n" || result.After != "# changed source\n" || result.Guard || !result.Modal || !result.Focused {
			t.Fatalf("discard did not preserve/cancel/restore the expected draft state: %#v", result)
		}
	})
}

func TestConsoleConfigCachesSchemaAndReadsDocumentsOnRevisionChange(t *testing.T) {
	withConsoleBrowser(t, func(ctx context.Context) {
		var result struct {
			Schema   int    `json:"schema"`
			Document int    `json:"document"`
			Editor   string `json:"editor"`
		}
		err := chromedp.Run(ctx, consoleAwait(`(async function(){`+consoleDraftFixture+`
          await refreshRuntimeConfig(); await refreshRuntimeConfig();
          __desired = "# comment-only desired change\n"; __revision = "revision-2";
          await refreshRuntimeConfig(); await refreshRuntimeConfig();
          return {schema:__requests.schema, document:__requests.document, editor:document.getElementById("config-editor").value};
        })()`, &result))
		if err != nil {
			t.Fatal(err)
		}
		if result.Schema != 1 || result.Document != 2 || result.Editor != "# comment-only desired change\n" {
			t.Fatalf("config revision cache contract: %#v", result)
		}
	})
}

func TestConsoleConfigApplyKeepsNewerDraftThroughLaterPolls(t *testing.T) {
	withConsoleBrowser(t, func(ctx context.Context) {
		var result struct {
			Editor string `json:"editor"`
			Body   string `json:"body"`
			Guard  bool   `json:"guard"`
		}
		err := chromedp.Run(ctx, consoleAwait(`(async function(){`+consoleDraftFixture+`
          var editor = document.getElementById("config-editor");
          editor.value = "# submitted\n"; editor.dispatchEvent(new Event("input"));
          var fetchConfig = apiFetch, resolveApply, body;
          apiFetch = function(url, options) {
            if (url.endsWith("/apply")) { body = options.body; return new Promise(function(resolve){resolveApply = resolve;}); }
            return fetchConfig(url, options);
          };
          applyConfigEditor(); var apply = pendingAction();
          editor.value = "# newer draft\n"; editor.dispatchEvent(new Event("input")); editor.blur();
          resolveApply({status:"written_and_refresh_scheduled", document_revision:"revision-2"});
          await apply; closeModal();
          await refreshRuntimeConfig();
          __desired = "# submitted\n"; __revision = "revision-2";
          await refreshRuntimeConfig(); await refreshRuntimeConfig();
          var event = new Event("beforeunload", {cancelable:true}); window.dispatchEvent(event);
          return {editor:editor.value, body:body, guard:event.defaultPrevented};
        })()`, &result))
		if err != nil {
			t.Fatal(err)
		}
		if result.Editor != "# newer draft\n" || result.Body != "# submitted\n" || !result.Guard {
			t.Fatalf("a later poll lost the newer Apply draft: %#v", result)
		}
	})
}

func TestConsoleSessionExpiryStopsPollingAndPreservesDraftForSignIn(t *testing.T) {
	withConsoleBrowser(t, func(ctx context.Context) {
		var result struct {
			Requests int    `json:"requests"`
			Stopped  bool   `json:"stopped"`
			Editor   string `json:"editor"`
			SignIn   bool   `json:"signIn"`
			Resumed  bool   `json:"resumed"`
		}
		err := chromedp.Run(ctx, consoleAwait(`(async function(){
          var editor = document.getElementById("config-editor");
          editor.value = "# private draft\n"; editor.dispatchEvent(new Event("input"));
          var requests = 0, expired = true;
          window.fetch = function() {
            requests++;
            return Promise.resolve(new Response(JSON.stringify(expired ? {message:"Unauthorized"} : {data:{console_role:"operator"}}), {status:expired ? 401 : 200, headers:{"Content-Type":"application/json"}}));
          };
          activeViewInterval = setInterval(function(){}, 3000);
          await apiFetch("/api/v1/security/status").catch(function(){});
          await refreshActiveView(); startActiveViewPolling();
          var requestCount = requests;
          var stopped = activeViewInterval === null;
          var signIn = document.getElementById("console-sign-in");
          var signInReady = !!signIn && signIn.getAttribute("href") === "/console/login" && signIn.target === "_blank" && !document.getElementById("console-session-expired").hidden;
          expired = false;
          if (typeof resumeConsoleSession === "function") await resumeConsoleSession();
          var resumed = activeViewInterval !== null && managementRole === "operator";
          stopActiveViewPolling();
          return {requests:requestCount, stopped:stopped, editor:editor.value, signIn:signInReady, resumed:resumed};
        })()`, &result))
		if err != nil {
			t.Fatal(err)
		}
		if result.Requests != 1 || !result.Stopped || result.Editor != "# private draft\n" || !result.SignIn || !result.Resumed {
			t.Fatalf("session expiry/recovery contract: %#v", result)
		}
	})
}

func TestConsoleConfigApplyUsesDraftBaselineAfterRemoteSourceChange(t *testing.T) {
	withConsoleBrowser(t, func(ctx context.Context) {
		var result struct {
			Revision string `json:"revision"`
			Editor   string `json:"editor"`
			Guard    bool   `json:"guard"`
		}
		err := chromedp.Run(ctx, consoleAwait(`(async function(){`+consoleDraftFixture+`
          var editor = document.getElementById("config-editor");
          editor.value = "# private local changes\n"; editor.dispatchEvent(new Event("input"));
          __desired = "# another operator's change\n"; __revision = "revision-2";
          await refreshRuntimeConfig();
          var fetchConfig = apiFetch, revision;
          apiFetch = function(url, options) {
            if (url.endsWith("/apply")) {
              revision = options.headers["If-Match"];
              var err = new Error("configuration changed"); err.status = 409; return Promise.reject(err);
            }
            return fetchConfig(url, options);
          };
          applyConfigEditor(); await pendingAction(); closeModal();
          var event = new Event("beforeunload", {cancelable:true}); window.dispatchEvent(event);
          return {revision:revision, editor:editor.value, guard:event.defaultPrevented};
        })()`, &result))
		if err != nil {
			t.Fatal(err)
		}
		if result.Revision != `"revision-1"` || result.Editor != "# private local changes\n" || !result.Guard {
			t.Fatalf("stale draft used the polled revision or lost edits after rejection: %#v", result)
		}
	})
}

func TestConsoleConfigNavigationAndLogoutRequireDraftDiscard(t *testing.T) {
	withConsoleBrowser(t, func(ctx context.Context) {
		var result struct {
			NavigationGuard bool   `json:"navigationGuard"`
			LogoutGuard     bool   `json:"logoutGuard"`
			Cancelled       bool   `json:"cancelled"`
			Method          string `json:"method"`
			Path            string `json:"path"`
			GuardReleased   bool   `json:"guardReleased"`
		}
		err := chromedp.Run(ctx, consoleAwait(`(async function(){`+consoleDraftFixture+`
          var editor = document.getElementById("config-editor");
          editor.value = "# draft\n"; editor.dispatchEvent(new Event("input"));
          openPublishModal();
          var navigationGuard = document.getElementById("modal").classList.contains("active") && location.pathname === "/console";
          document.getElementById("modal-cancel").click();
          var form = document.getElementById("console-logout"), method = "", path = "";
          if (!form) return {navigationGuard:navigationGuard};
          form.submit = function() { method = this.method; path = new URL(this.action).pathname; };
          form.requestSubmit();
          var logoutGuard = document.getElementById("modal").classList.contains("active") && method === "";
          document.getElementById("modal-cancel").click();
          var cancelled = method === "" && editor.value === "# draft\n";
          form.requestSubmit(); await pendingAction(); closeModal();
          var event = new Event("beforeunload", {cancelable:true}); window.dispatchEvent(event);
          return {navigationGuard:navigationGuard, logoutGuard:logoutGuard, cancelled:cancelled, method:method, path:path, guardReleased:!event.defaultPrevented};
        })()`, &result))
		if err != nil {
			t.Fatal(err)
		}
		if !result.NavigationGuard || !result.LogoutGuard || !result.Cancelled || result.Method != "post" || result.Path != "/console/logout" || !result.GuardReleased {
			t.Fatalf("navigation/logout discarded a draft without confirmation or used a wrong route: %#v", result)
		}
	})
}
