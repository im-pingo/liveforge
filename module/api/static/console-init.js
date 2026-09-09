/* === Init === */
initializeConsolePages();
initializeConsoleLanguage();
document.getElementById("config-validate").addEventListener("click", validateConfigEditor);
document.getElementById("config-apply").addEventListener("click", applyConfigEditor);
document.getElementById("config-editor").addEventListener("input", function() {
  configEditorRevision++;
  updateConfigEditorControls();
  document.getElementById("config-comparison").hidden = true;
});
document.getElementById("config-discard").addEventListener("click", discardConfigEditor);
document.querySelectorAll("[data-config-path]").forEach(function(input) { input.addEventListener("change", function() { changeCommonConfig(this); }); });
document.getElementById("config-compare").addEventListener("click", compareConfigEditor);
document.getElementById("config-compare-source").addEventListener("change", compareConfigEditor);
document.getElementById("config-history-refresh").addEventListener("click", refreshConfigHistory);
document.getElementById("config-history-select").addEventListener("change", selectConfigHistory);
document.getElementById("config-rollback").addEventListener("click", rollbackConfigHistory);
document.getElementById("recording-search").addEventListener("input", function() { refreshStorage(); });
document.getElementById("audit-filter-form").addEventListener("submit", function(event) { event.preventDefault(); refreshSecurity(); });
document.getElementById("audit-export").addEventListener("click", exportConsoleAudit);
document.getElementById("console-resume").addEventListener("click", resumeConsoleSession);
document.getElementById("console-sign-in").addEventListener("click", function() { consoleSignInOpened = true; });
window.addEventListener("focus", function() { if (consoleSessionExpired && consoleSignInOpened) resumeConsoleSession(); });
document.getElementById("console-logout").addEventListener("submit", function(event) {
  event.preventDefault();
  var form = this;
  guardConsoleNavigation(function() { form.submit(); });
});
document.getElementById("sip-self-test").addEventListener("click", runSIPSelfTest);
document.getElementById("gb-self-test").addEventListener("click", runGB28181SelfTest);
applyManagementPermissions();
bootstrapManagementRole();
switchTab("streams");

if (location.pathname === "/console/publish") {
  stopActiveViewPolling();
  document.body.classList.add("publish-route");
  refreshServerInfo();
  openPublishModal();
}

// Show self-signed cert banner when using HTTPS with auto-generated cert.
if (location.protocol === "https:") {
  fetch("/console/cert.pem", { method: "HEAD" }).then(function(r) {
    if (r.ok) document.getElementById("cert-banner").style.display = "block";
  }).catch(function(){});
}
