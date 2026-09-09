var endpoints = {};
var endpointSchemes = {};
var serverCapabilities = {};
var currentPlayer = null;
var currentPC = null;
var currentMSEAbort = null;
var currentMSEMediaSource = null;
var currentMSESourceBuffer = null;
var currentMSEObjectURL = null;
var currentHls = null;
var currentDash = null;
var currentVideoEventCleanup = null;
var playerGeneration = 0;
var whepStatusPoll = null;
var streamMedia = Object.create(null);
var recordingFormats = Object.create(null);
var statsInterval = null;
var statsPrev = null;
var statsVisible = false;
var managementRole = "viewer";
var consoleSessionExpired = false;
var consoleSessionCheck = null;
var consoleSignInOpened = false;
var consoleAllowNavigation = false;
var activeTab = "streams";
var activeViewController = null;
var activeViewInterval = null;

function apiFetch(url, options) {
  options = options || {};
  if (consoleSessionExpired && !options.allowExpired) {
    var expired = new Error("Session expired");
    expired.status = 401;
    return Promise.reject(expired);
  }
  var request = {
    method: options.method || "GET",
    credentials: "same-origin",
    signal: options.signal
  };
  if (options.headers) request.headers = options.headers;
  if (options.body != null) request.body = options.body;
  var pageRequest = options.page ? prepareConsolePage(options.page, url, options.query) : null;
  if (pageRequest) url = pageRequest.url;
  else if (options.query) {
    var queryURL = new URL(url, location.href);
    Object.keys(options.query).forEach(function(key) { if (options.query[key]) queryURL.searchParams.set(key, options.query[key]); });
    url = queryURL.pathname + queryURL.search;
  }
  return fetch(url, request).then(function(response) {
    if (response.status === 401) expireConsoleSession();
    if (response.status === 204) return null;
    if (response.ok && options.responseType === "blob") return response.blob();
    var contentType = response.headers.get("Content-Type") || "";
    var parsed = contentType.indexOf("application/json") >= 0
      ? response.json().catch(function() { return null; })
      : Promise.resolve(null);
    return parsed.then(function(payload) {
      if (pageRequest && pageRequest.sequence !== consolePages[options.page].sequence) throw new DOMException("Superseded list request", "AbortError");
      if (!response.ok) {
        var detail = payload && (payload.message || payload.error);
        var err = new Error(detail || response.statusText || "Request failed");
        err.status = response.status;
        throw err;
      }
      if (payload && Object.prototype.hasOwnProperty.call(payload, "data")) {
        if (options.page) updateConsolePage(options.page, payload.pagination);
        return payload.data;
      }
      return payload;
    });
  });
}

function expireConsoleSession() {
  consoleSessionExpired = true;
  sourceWritable = false;
  stopActiveViewPolling();
  var banner = document.getElementById("console-session-expired");
  banner.hidden = false;
  banner.classList.add("show");
  applyManagementPermissions();
}

function resumeConsoleSession() {
  if (!consoleSessionExpired) return Promise.resolve();
  if (consoleSessionCheck) return consoleSessionCheck;
  document.getElementById("console-resume").disabled = true;
  consoleSessionCheck = apiFetch("/api/v1/security/status", {allowExpired:true}).then(function(status) {
    consoleSessionExpired = false;
    consoleSignInOpened = false;
    managementRole = (status && status.console_role) || "viewer";
    var banner = document.getElementById("console-session-expired");
    banner.hidden = true;
    banner.classList.remove("show");
    configDocumentKey = null;
    applyManagementPermissions();
    startActiveViewPolling();
  }).catch(function(err) { showToast(managementErrorMessage(err)); }).finally(function() {
    consoleSessionCheck = null;
    document.getElementById("console-resume").disabled = false;
  });
  return consoleSessionCheck;
}

function guardConsoleNavigation(action) {
  if (!hasConfigDraft() && !configApplyInFlight) { action(); return; }
  showModal("Leave configuration?", configApplyInFlight ? "A configuration write is still in progress. Leave this page?" : "Discard unsaved configuration changes and leave this page?", function() {
    consoleAllowNavigation = true;
    action();
  });
}

window.addEventListener("beforeunload", function(event) {
  if (!consoleAllowNavigation && (hasConfigDraft() || configApplyInFlight)) {
    event.preventDefault();
    event.returnValue = "";
  }
});

function managementErrorMessage(err) {
  var detail = err && err.message ? err.message : "Request failed";
  switch (err && err.status) {
    case 401: return "Session expired. Sign in again.";
    case 403: return "This console role cannot perform that action.";
    case 409: return "The resource changed or is not ready: " + detail;
    case 503: return "The requested module is unavailable: " + detail;
    default: return detail;
  }
}

function showManagementError(view, err) {
  if (err && err.name === "AbortError") return;
  var el = document.getElementById(view + "-error");
  if (el) {
    el.textContent = managementErrorMessage(err);
    el.classList.add("show");
  }
  showToast(managementErrorMessage(err));
}

function clearManagementError(view) {
  var el = document.getElementById(view + "-error");
  if (el) {
    el.textContent = "";
    el.classList.remove("show");
  }
}

function canManage(requiredRole) {
  if (consoleSessionExpired) return false;
  if (managementRole === "admin") return true;
  if (requiredRole === "viewer") return managementRole === "viewer" || managementRole === "operator";
  return requiredRole === "operator" && managementRole === "operator";
}

function applyManagementPermissions() {
  var controls = document.querySelectorAll("[data-permission]");
  for (var i = 0; i < controls.length; i++) {
    var allowed = canManage(controls[i].getAttribute("data-permission"));
    controls[i].disabled = !allowed;
    controls[i].title = allowed ? "" : "Insufficient console role";
  }
  updateConfigEditorControls();
  updatePublishStartButton();
  Object.keys(consolePages).forEach(renderConsolePage);
}

function abortActiveViewRequest() {
  if (activeViewController) activeViewController.abort();
  activeViewController = null;
}

function newActiveViewSignal() {
  abortActiveViewRequest();
  activeViewController = new AbortController();
  return activeViewController.signal;
}

function esc(s) {
  if (!s) return "";
  var d = document.createElement("div");
  d.appendChild(document.createTextNode(s));
  return d.innerHTML;
}

function formatUptime(sec) {
  if (sec < 60) return sec + "s";
  if (sec < 3600) return Math.floor(sec / 60) + "m";
  var h = Math.floor(sec / 3600);
  var m = Math.floor((sec % 3600) / 60);
  return h + "h " + m + "m";
}

function formatBitrate(kbps) {
  if (!kbps || kbps === 0) return "-";
  if (kbps >= 1000) return (kbps / 1000).toFixed(1) + " Mbps";
  return kbps + " kbps";
}

function formatUptime(sec) {
  sec = Math.floor(sec);
  var d = Math.floor(sec / 86400);
  var h = Math.floor((sec % 86400) / 3600);
  var m = Math.floor((sec % 3600) / 60);
  var s = sec % 60;
  if (d > 0) return d + "d " + h + "h " + m + "m";
  if (h > 0) return h + "h " + m + "m " + s + "s";
  if (m > 0) return m + "m " + s + "s";
  return s + "s";
}

/* --- Confirm modal --- */
var pendingAction = null;
var modalReturnFocus = null;
var modalReturnFocusIdentity = null;
function captureModalFocusIdentity(element) {
  if (!element || !element.getAttribute) return null;
  return {
    id: element.id || "",
    action: element.getAttribute("data-action"),
    actionID: element.getAttribute("data-action-id")
  };
}
function resolveModalReturnFocus(element, identity) {
  if (element && element.isConnected) return element;
  if (identity && identity.id) {
    var identified = document.getElementById(identity.id);
    if (identified) return identified;
  }
  if (identity && identity.action !== null && identity.actionID !== null) {
    var actions = document.querySelectorAll("[data-action][data-action-id]");
    for (var i = 0; i < actions.length; i++) {
      if (actions[i].getAttribute("data-action") === identity.action && actions[i].getAttribute("data-action-id") === identity.actionID) {
        return actions[i];
      }
    }
  }
  var tabs = document.querySelectorAll(".nav-tab");
  for (var j = 0; j < tabs.length; j++) {
    if (!tabs[j].hidden && tabs[j].getAttribute("aria-selected") === "true") return tabs[j];
  }
  return null;
}
function showModal(title, msg, onConfirm) {
  modalReturnFocus = document.activeElement;
  modalReturnFocusIdentity = captureModalFocusIdentity(modalReturnFocus);
  document.getElementById("modal-title").textContent = consoleText(title);
  document.getElementById("modal-msg").textContent = consoleText(msg);
  document.getElementById("modal").classList.add("active");
  document.getElementById("modal-confirm").disabled = false;
  pendingAction = onConfirm;
  document.getElementById("modal-confirm").focus();
}
function closeModal() {
  document.getElementById("modal").classList.remove("active");
  pendingAction = null;
  var returnFocus = resolveModalReturnFocus(modalReturnFocus, modalReturnFocusIdentity);
  modalReturnFocus = null;
  modalReturnFocusIdentity = null;
  if (returnFocus) returnFocus.focus();
}
document.getElementById("modal-cancel").addEventListener("click", closeModal);
document.getElementById("modal-confirm").addEventListener("click", function() {
  if (!pendingAction) return;
  var action = pendingAction;
  pendingAction = null;
  var button = document.getElementById("modal-confirm");
  button.disabled = true;
  Promise.resolve().then(action).finally(function() {
    button.disabled = false;
    closeModal();
  });
});
document.getElementById("modal").addEventListener("keydown", function(event) {
  if (event.key === "Escape") {
    event.preventDefault();
    closeModal();
    return;
  }
  if (event.key !== "Tab") return;
  var focusable = Array.from(this.querySelectorAll("button:not([disabled])"));
  if (!focusable.length) return;
  var first = focusable[0];
  var last = focusable[focusable.length - 1];
  if (event.shiftKey && document.activeElement === first) {
    event.preventDefault();
    last.focus();
  } else if (!event.shiftKey && document.activeElement === last) {
    event.preventDefault();
    first.focus();
  }
});
document.addEventListener("focusin", function(event) {
  var modal = document.getElementById("modal");
  if (modal.classList.contains("active") && !modal.contains(event.target)) {
    var confirm = document.getElementById("modal-confirm");
    (confirm.disabled ? document.getElementById("modal-cancel") : confirm).focus();
  }
});
