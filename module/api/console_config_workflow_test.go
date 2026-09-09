package api

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

func TestConsoleConfigAndSecurityFitDesktopAndMobile(t *testing.T) {
	withConsoleBrowser(t, func(ctx context.Context) {
		if err := chromedp.Run(ctx, consoleAwait(`(async function(){`+consoleDraftFixture+`
          document.getElementById("config-schema-version").textContent="https://github.com/im-pingo/liveforge/docs/config/config.schema.json";
          var previousFetch=apiFetch;
          apiFetch=function(url){
            if(url==="/api/v1/server/config")return Promise.resolve({enabled:true,source:"file",active_version:"2026-09-07T08:13:52.50387791Z",active_hash:"afd29ba586a28774d4c23d7bf9585f700797bf58c6e142176bc27703a76c24188",document_revision:__revision});
            return previousFetch(url);
          };
          await refreshRuntimeConfig();
        })()`, nil)); err != nil {
			t.Fatal(err)
		}
		for _, width := range []int64{1440, 390} {
			for _, language := range []string{"en", "zh-CN"} {
				for _, view := range []string{"config", "security"} {
					var dimensions struct{ Width, Scroll int }
					if err := chromedp.Run(ctx,
						chromedp.EmulateViewport(width, 844),
						chromedp.Evaluate(fmt.Sprintf(`setConsoleLanguage(%q);switchTab(%q);stopActiveViewPolling();`, language, view), nil),
						chromedp.Evaluate(`({Width:innerWidth,Scroll:document.documentElement.scrollWidth})`, &dimensions),
					); err != nil {
						t.Fatal(err)
					}
					if dimensions.Scroll > dimensions.Width {
						t.Errorf("%s %s at %d pixels overflows: %+v", language, view, width, dimensions)
					}
				}
			}
		}
	})
}

func TestConsoleLanguageSelectionPreservesDraftAndPersists(t *testing.T) {
	withConsoleBrowser(t, func(ctx context.Context) {
		var result struct{ Language, Label, Draft, Saved string }
		err := chromedp.Run(ctx, consoleAwait(`(async function(){`+consoleDraftFixture+`
          var editor = document.getElementById("config-editor");
          editor.value = "# local draft\n"; editor.dispatchEvent(new Event("input"));
          var language = document.getElementById("console-language");
          if (!language) return {};
          language.value = "zh-CN"; language.dispatchEvent(new Event("change"));
          var label = document.getElementById("tab-config").textContent;
          var saved = localStorage.getItem("liveforge.console.language");
          if (localStorage.length !== 1) throw new Error("Console persisted more than the locale preference");
          return {Language:document.documentElement.lang, Label:label, Draft:editor.value, Saved:saved};
        })()`, &result))
		if err != nil {
			t.Fatal(err)
		}
		if result.Language != "zh-CN" || result.Label != "配置" || result.Draft != "# local draft\n" || result.Saved != "zh-CN" {
			t.Fatalf("language switch did not preserve the draft or preference: %#v", result)
		}
		if err := chromedp.Run(ctx, chromedp.Reload(), chromedp.WaitReady("#console-language"), chromedp.Evaluate(`document.documentElement.lang`, &result.Language)); err != nil {
			t.Fatal(err)
		}
		if result.Language != "zh-CN" {
			t.Fatalf("language was not restored after reload: %q", result.Language)
		}
	})
}

func TestConsoleConfigComparisonPreservesRedactedIdentityWithoutInventingChanges(t *testing.T) {
	withConsoleBrowser(t, func(ctx context.Context) {
		var result struct{ Before, After string }
		err := chromedp.Run(ctx, consoleAwait(`(async function(){`+consoleDraftFixture+`
          var text="api:\n  auth:\n    tokens:\n      - name: operator\n        token: '[REDACTED]'\n        role: '[REDACTED]'\n";
          configDocumentCache.effective_document=text;
          var editor=document.getElementById("config-editor");editor.value=text;editor.dispatchEvent(new Event("input"));
          compareConfigEditor();var before=document.getElementById("config-diff-summary").textContent;
          editor.value=text.replace("token: '[REDACTED]'","token: entered-secret");editor.dispatchEvent(new Event("input"));
          compareConfigEditor();return {Before:before,After:document.getElementById("config-comparison").textContent};
        })()`, &result))
		if err != nil {
			t.Fatal(err)
		}
		if result.Before != "No configuration changes" || strings.Contains(result.After, "entered-secret") || !strings.Contains(result.After, "supplied") {
			t.Fatalf("secret comparison either invented changes or hid a supplied replacement: %#v", result)
		}
	})
}

func TestConsoleConfigComparisonShowsURLSecretReplacement(t *testing.T) {
	withConsoleBrowser(t, func(ctx context.Context) {
		var result struct{ Before, After string }
		if err := chromedp.Run(ctx, consoleAwait(`(async function(){`+consoleDraftFixture+`
          var original="custom:\n  endpoint: https://REDACTED@example.test/__liveforge_redacted_path__/0123456789abcdef0123456789abcdef?__liveforge_redacted__=1\n";
          configDocumentCache.effective_document=original;
          var editor=document.getElementById("config-editor");editor.value=original;editor.dispatchEvent(new Event("input"));
          compareConfigEditor();var before=document.getElementById("config-diff-summary").textContent;
          editor.value="custom:\n  endpoint: https://user:new-secret@example.test/new-private-path?token=new-token\n";editor.dispatchEvent(new Event("input"));
          compareConfigEditor();return {Before:before,After:document.getElementById("config-comparison").textContent};
        })()`, &result)); err != nil {
			t.Fatal(err)
		}
		if result.Before != "No configuration changes" || !strings.Contains(result.After, "supplied") {
			t.Fatalf("URL comparison invented or hid a replacement: %#v", result)
		}
		for _, secret := range []string{"new-secret", "new-private-path", "new-token"} {
			if strings.Contains(result.After, secret) {
				t.Fatalf("URL comparison exposed %q: %s", secret, result.After)
			}
		}
	})
}

func TestConsoleCommonConfigControlsPreserveCommentsAndUnmappedFields(t *testing.T) {
	withConsoleBrowser(t, func(ctx context.Context) {
		var result struct {
			Document, Invalid          string
			Dirty, ReadOnly, Immutable bool
		}
		err := chromedp.Run(ctx, consoleAwait(`(async function(){`+consoleDraftFixture+`
          __desired = "# retained comment\nserver:\n  name: before # identity\n  log_level: info\ncustom:\n  nested: [one, two]\nstream:\n  gop_cache: true\n"; __revision = "revision-2";
          await refreshRuntimeConfig();
          var name = document.getElementById("config-common-name"), gop = document.getElementById("config-common-gop"), log = document.getElementById("config-common-log");
          if (!name || !gop) return {};
          var immutable = name.readOnly;
          name.value = "must remain unchanged"; name.dispatchEvent(new Event("change"));
          log.value = "debug"; log.dispatchEvent(new Event("change"));
          gop.checked = false; gop.dispatchEvent(new Event("change"));
          var editor = document.getElementById("config-editor"), output = editor.value;
          var event = new Event("beforeunload", {cancelable:true}); window.dispatchEvent(event);
          editor.value = "server: [invalid\n"; editor.dispatchEvent(new Event("input"));
          name.value = "do not overwrite"; name.dispatchEvent(new Event("change"));
          var invalid = editor.value;
          managementRole = "viewer"; applyManagementPermissions();
          return {Document:output, Dirty:event.defaultPrevented, Invalid:invalid, ReadOnly:name.disabled && gop.disabled, Immutable:immutable};
        })()`, &result))
		if err != nil {
			t.Fatal(err)
		}
		for _, retained := range []string{"# retained comment", "# identity", "name: before", "log_level: debug", "nested: [ one, two ]", "gop_cache: false"} {
			if !strings.Contains(result.Document, retained) {
				t.Errorf("common edit lost %q: %s", retained, result.Document)
			}
		}
		if !result.Dirty || result.Invalid != "server: [invalid\n" || !result.ReadOnly || !result.Immutable {
			t.Fatalf("common edit bypassed draft, parse, or role guard: %#v", result)
		}
	})
}

func TestConsoleAuditPersistenceFailureIsVisible(t *testing.T) {
	withConsoleBrowser(t, func(ctx context.Context) {
		var result struct {
			State, Failures, Error string
			Visible                bool
		}
		err := chromedp.Run(ctx, consoleAwait(`(async function(){
          apiFetch=function(url) { return Promise.resolve(url.endsWith("/audit") ? [] : {console_role:"admin",audit_enabled:true,audit_persistence:{enabled:true,healthy:false,error:"write failed",write_failures_total:3}}); };
          await refreshSecurity();
          var state=document.getElementById("security-audit-persistence"), failure=document.getElementById("security-audit-write-failures"), error=document.getElementById("security-audit-persistence-error");
          return {State:state&&state.textContent,Failures:failure&&failure.textContent,Error:error&&error.textContent,Visible:!!error&&!error.hidden};
        })()`, &result))
		if err != nil {
			t.Fatal(err)
		}
		if result.State != "Unhealthy" || result.Failures != "3" || result.Error != "write failed" || !result.Visible {
			t.Fatalf("audit persistence failure was hidden: %#v", result)
		}
	})
}

func TestConsoleSecurityKeepsSelectedLanguageAfterRefresh(t *testing.T) {
	withConsoleBrowser(t, func(ctx context.Context) {
		var result struct{ State, Label, Empty string }
		if err := chromedp.Run(ctx, consoleAwait(`(async function(){
          setConsoleLanguage("zh-CN");
          apiFetch=function(url){return Promise.resolve(url.endsWith("/audit")?[]:{console_role:"admin",audit_enabled:true});};
          await refreshSecurity();
          return {State:document.getElementById("security-audit").textContent,Label:document.getElementById("security-tokens").previousElementSibling.textContent,Empty:document.getElementById("audit-empty").textContent.trim()};
        })()`, &result)); err != nil {
			t.Fatal(err)
		}
		if result.State != "已启用" || result.Label != "命名令牌" || result.Empty != "暂无审计记录" {
			t.Fatalf("security refresh lost its selected language: %#v", result)
		}
	})
}

func TestConsoleConfigComparisonMarksRestartAndImmutableChanges(t *testing.T) {
	withConsoleBrowser(t, func(ctx context.Context) {
		var result string
		err := chromedp.Run(ctx, consoleAwait(`(async function(){`+consoleDraftFixture+`
          configSchemaData={
            properties:{server:{$ref:"#/$defs/server"},record:{properties:{enabled:{"x-liveforge-reload":"restart_required"}}},metrics:{$ref:"#/$defs/metrics","x-liveforge-reload":"restart_required"}},
            $defs:{server:{properties:{name:{"x-liveforge-reload":"immutable"},log_level:{"x-liveforge-reload":"hot_reload"}}},metrics:{properties:{enabled:{type:"boolean"}}}}
          };
          configDocumentCache.effective_document="server:\n  name: original\n  log_level: info\nmetrics:\n  enabled: false\nmetrics.enabled: false\n";
          var editor=document.getElementById("config-editor");editor.value="server:\n  name: changed\n  log_level: debug\nmetrics:\n  enabled: true\nmetrics.enabled: true\nrecord:\n  enabled: true\n";editor.dispatchEvent(new Event("input"));
          compareConfigEditor();return document.getElementById("config-comparison").textContent;
        })()`, &result))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(result, "server.name [Immutable]") || !strings.Contains(result, "metrics.enabled [Restart required]") || !strings.Contains(result, "record.enabled [Restart required]") || !strings.Contains(result, "server.log_level [Hot reload]") || !strings.Contains(result, "metrics.enabled:\n") {
			t.Fatalf("reload impact missing or incorrect in comparison: %s", result)
		}
	})
}

func TestConsoleConfigComparisonRedactsDraftSecretsAndRollbackKeepsNewerDraft(t *testing.T) {
	withConsoleBrowser(t, func(ctx context.Context) {
		var result struct {
			Diff, Header, Body, Draft string
			Dirty, Guard              bool
		}
		err := chromedp.Run(ctx, consoleAwait(`(async function(){`+consoleDraftFixture+`
          var editor = document.getElementById("config-editor");
          editor.value = "server:\n  name: edited\napi:\n  auth:\n    bearer_token: &private private-token\ncustom:\n  copy: *private\n  passphrase: private-passphrase\n  endpoint: https://user:pass@example.com/private?token=123\n# secret comment\n";
          editor.dispatchEvent(new Event("input"));
          var compare = document.getElementById("config-compare");
          if (!compare) return {};
          compare.click();
          var diff = document.getElementById("config-comparison").textContent;
          var fetchConfig = apiFetch, resolveRollback, header, body;
          apiFetch = function(url, options) {
            if (url.endsWith("/history")) return Promise.resolve([{revision:"old-revision", created_at:"2026-09-07T00:00:00Z", bytes:22}]);
            if (url.endsWith("/history/old-revision")) return Promise.resolve({revision:"old-revision", document:"server:\n  name: previous\n"});
            if (url.endsWith("/rollback")) { header=options.headers["If-Match"]; body=options.body; return new Promise(function(resolve){resolveRollback=resolve;}); }
            return fetchConfig(url, options);
          };
          await refreshConfigHistory();
          var select = document.getElementById("config-history-select"); select.value="old-revision";
          await selectConfigHistory();
          rollbackConfigHistory();
          var guard = document.getElementById("modal").classList.contains("active") && body === undefined;
          document.getElementById("modal-cancel").click();
          if (editor.value.indexOf("private-token") < 0) throw new Error("Cancel lost the draft");
          rollbackConfigHistory(); var rollback = pendingAction();
          editor.value = "# newer local draft\n"; editor.dispatchEvent(new Event("input"));
          __desired = "server:\n  name: previous\n"; __revision = "revision-3";
          resolveRollback({status:"written_and_refresh_scheduled", document_revision:__revision});
          await rollback; closeModal(); await refreshRuntimeConfig();
          var event = new Event("beforeunload", {cancelable:true}); window.dispatchEvent(event);
          return {Diff:diff, Header:header, Body:body, Draft:editor.value, Dirty:event.defaultPrevented, Guard:guard};
        })()`, &result))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(result.Diff, "edited") || strings.Contains(result.Diff, "private-token") || strings.Contains(result.Diff, "private-passphrase") || strings.Contains(result.Diff, "user:pass") || strings.Contains(result.Diff, "token=123") || strings.Contains(result.Diff, "secret comment") {
			t.Fatalf("comparison leaked a draft secret or omitted changes: %q", result.Diff)
		}
		if result.Header != `"revision-1"` || result.Body != `{"revision":"old-revision"}` || result.Draft != "# newer local draft\n" || !result.Dirty || !result.Guard {
			t.Fatalf("rollback violated revision, confirmation, or newer-draft contract: %#v", result)
		}
	})
}

func TestConsolePaginationResetsFiltersAndUsesAuthenticatedAuditExport(t *testing.T) {
	withConsoleBrowser(t, func(ctx context.Context) {
		var result struct {
			First, Next, Filter, Export, Page string
			Credentials                       bool
		}
		err := chromedp.Run(ctx, consoleAwait(`(async function(){
          managementRole="admin";
          var requests=[];
          window.fetch=function(url, options) {
            requests.push({url:String(url), credentials:options.credentials});
            if (String(url).indexOf("format=ndjson") >= 0) return Promise.resolve(new Response("{}\n", {headers:{"Content-Type":"application/x-ndjson"}}));
            var data=String(url).indexOf("/streams") >= 0 ? {streams:[]} : {};
            return Promise.resolve(new Response(JSON.stringify({data:data,pagination:{limit:50,offset:0,total:125,has_more:true}}),{headers:{"Content-Type":"application/json"}}));
          };
          await refresh();
          var first=requests[0].url, page=document.getElementById("streams-page");
          if (!page) return {First:first};
          var label=page.textContent;
          await changeConsolePage("streams",1);
          var next=requests.filter(function(r){return r.url.indexOf("/streams")>=0;}).pop().url;
          document.getElementById("stream-search").value="camera & door";
          await refresh();
          var filter=requests.filter(function(r){return r.url.indexOf("/streams")>=0;}).pop().url;
          document.getElementById("audit-principal").value="operator+east";
          document.getElementById("audit-action").value="config.apply";
          document.getElementById("audit-result").value="success";
          HTMLAnchorElement.prototype.click=function(){};
          await exportConsoleAudit();
          var exportRequest=requests.filter(function(r){return r.url.indexOf("format=ndjson")>=0;}).pop();
          stopActiveViewPolling();
          return {First:first,Next:next,Filter:filter,Page:label,Export:exportRequest.url,Credentials:exportRequest.credentials==="same-origin"};
        })()`, &result))
		if err != nil {
			t.Fatal(err)
		}
		for _, part := range []string{"limit=50", "offset=0"} {
			if !strings.Contains(result.First, part) {
				t.Errorf("initial page lacks %q: %#v", part, result)
			}
		}
		if !strings.Contains(result.Next, "offset=50") || !strings.Contains(result.Filter, "offset=0") || !strings.Contains(result.Filter, "q=camera+%26+door") || !strings.Contains(result.Page, "125") || !result.Credentials {
			t.Fatalf("pagination/filter contract: %#v", result)
		}
		for _, part := range []string{"principal=operator%2Beast", "action=config.apply", "result=success", "format=ndjson"} {
			if !strings.Contains(result.Export, part) {
				t.Errorf("audit export lacks %q: %s", part, result.Export)
			}
		}
	})
}
