var sourceWritable = false;
var configEditorRevision = 0;
var configEditorBaseline = "";
var configEditorBaselineRevision = null;
var configServerDocument = null;
var configDocumentCache = null;
var configDocumentKey = null;
var configSchemaPromise = null;
var configRefreshSequence = 0;
var configPendingWrite = null;
var configApplyInFlight = false;
var configSchemaData = {};
var configCommonText = null;
var configCommonDocument = null;
var configHistoryEntry = null;
var configHistorySequence = 0;

function hasConfigDraft() {
  return document.getElementById("config-editor").value !== configEditorBaseline;
}


function updateConfigEditorControls() {
  var canOperate = sourceWritable && canManage("operator");
  document.getElementById("config-editor").readOnly = !canOperate;
  document.getElementById("config-apply").disabled = !canOperate || configApplyInFlight;
  document.getElementById("config-validate").disabled = !canManage("viewer");
  document.getElementById("config-discard").disabled = !hasConfigDraft() || configServerDocument === null || configApplyInFlight;
  document.getElementById("config-draft-state").textContent = consoleText(configApplyInFlight ? "Applying" : (hasConfigDraft() ? "Unsaved changes" : ""));
  document.getElementById("config-rollback").disabled = !canOperate || configApplyInFlight || !configHistoryEntry;
  syncCommonConfigControls();
}

function configStatusDocumentKey(data) {
  return JSON.stringify([data.source || "", data.document_revision || "", data.active_hash || "", data.active_version || "", data.pending_restart || []]);
}

function loadConfigSchema() {
  if (!configSchemaPromise) {
    configSchemaPromise = apiFetch("/api/v1/server/config/schema").catch(function(err) {
      configSchemaPromise = null;
      throw err;
    });
  }
  return configSchemaPromise;
}

function discardConfigEditor() {
  if (!hasConfigDraft() || configServerDocument === null || configApplyInFlight) return;
  showModal("Discard configuration changes", "Replace the local draft with the latest desired source document?", function() {
    if (configServerDocument === null || configApplyInFlight) return;
    var editor = document.getElementById("config-editor");
    editor.value = configServerDocument;
    configEditorBaseline = editor.value;
    configEditorBaselineRevision = configPendingWrite ? configPendingWrite.revision : (configDocumentCache && configDocumentCache.document_revision);
    configEditorRevision++;
    updateConfigEditorControls();
    modalReturnFocus = editor;
    modalReturnFocusIdentity = captureModalFocusIdentity(editor);
  });
}

function refreshRuntimeConfig(signal, options) {
  clearManagementError("config");
  options = options || {};
  var sequence = ++configRefreshSequence;
  var editorRevision = configEditorRevision;
  var editor = document.getElementById("config-editor");
  return Promise.all([
    apiFetch("/api/v1/server/config", { signal: signal }),
    loadConfigSchema()
  ]).then(function(results) {
    if (sequence !== configRefreshSequence || (signal && signal.aborted)) return null;
    var data = results[0] || {};
    var key = configStatusDocumentKey(data);
    var documentRequest = !options.forceDocument && configDocumentCache && key === configDocumentKey
      ? Promise.resolve(configDocumentCache)
      : apiFetch("/api/v1/server/config/document", { signal: signal });
    return documentRequest.then(function(documentData) {
      return {data:data, documentData:documentData || {}, schemaData:results[1] || {}, key:key};
    });
  }).then(function(results) {
    if (!results || sequence !== configRefreshSequence || (signal && signal.aborted)) return;
    var data = results.data;
    var documentData = results.documentData;
    var schemaData = results.schemaData;
    configSchemaData = schemaData;
    configDocumentCache = documentData;
    configDocumentKey = results.key;
    document.getElementById("config-enabled").textContent = consoleText(data.enabled ? "Active" : "Disabled");
    document.getElementById("config-enabled").className = "stat-value " + (data.enabled ? "health-good" : "");
    document.getElementById("config-source").textContent = data.source || "-";
    document.getElementById("config-version").textContent = data.active_version || "-";
    document.getElementById("config-failures").textContent = data.consecutive_failures || 0;
    document.getElementById("config-hash").textContent = data.active_hash || "-";
    document.getElementById("config-last-success").textContent = formatDate(data.last_success);
    document.getElementById("config-last-attempt").textContent = formatDate(data.last_attempt);
    document.getElementById("config-callback-failures").textContent = (data.callback_failures || 0) + " / dropped " + (data.dropped_callbacks || 0);
    document.getElementById("config-change-counts").textContent = (data.config_changes_accepted || 0) + " / " + (data.config_changes_rejected || 0);
    document.getElementById("config-apply-failures").textContent = data.config_changes_application_failed || 0;
    var pending = data.pending_restart || [];
    document.getElementById("config-pending-count").textContent = pending.length;
    document.getElementById("config-pending").innerHTML = pending.length
      ? pending.map(function(path) { return '<span class="ops-chip">' + esc(path) + "</span>"; }).join("")
      : '<span class="ops-field-label">' + consoleText("None") + '</span>';
    var desiredDocument = documentData.desired_document || documentData.effective_document || JSON.stringify(documentData.desired || documentData.effective || {}, null, 2);
    // A response already in flight at Apply may still contain the pre-write document.
    if (configPendingWrite) {
      if (desiredDocument === configPendingWrite.document || (documentData.document_revision &&
          (documentData.document_revision === configPendingWrite.revision || documentData.document_revision !== configPendingWrite.previousRevision))) {
        configPendingWrite = null;
      } else {
        desiredDocument = configPendingWrite.document;
      }
    }
    configServerDocument = desiredDocument;
    if (!hasConfigDraft() && configEditorRevision === editorRevision && document.activeElement !== editor) {
      editor.value = desiredDocument;
      configEditorBaseline = editor.value;
      configEditorBaselineRevision = configPendingWrite ? configPendingWrite.revision : documentData.document_revision;
    }
    sourceWritable = !!documentData.writable;
    document.getElementById("config-source-writable").textContent = consoleText(documentData.writable ? "Writable source" : "Read-only source");
    document.getElementById("config-effective-state").textContent = consoleText(pending.length ? "Applied; restart pending" : "Applied");
    document.getElementById("config-effective-document").textContent = documentData.effective_document || JSON.stringify(documentData.effective || {}, null, 2);
    document.getElementById("config-source-kind").textContent = documentData.source_details && documentData.source_details.kind ? documentData.source_details.kind : (data.source || "-");
    document.getElementById("config-source-details").textContent = JSON.stringify(documentData.source_details || {}, null, 2);
    document.getElementById("config-schema-version").textContent = schemaData.$id || "JSON Schema";
    document.getElementById("config-schema").textContent = JSON.stringify(schemaData, null, 2);
    if (data.last_error) showManagementError("config", new Error(data.last_error));
    var button = document.getElementById("config-refresh");
    button.disabled = !data.enabled || !canManage("operator");
    updateConfigEditorControls();
  }).catch(function(err) {
    if (sequence !== configRefreshSequence || (signal && signal.aborted) || (err && err.name === "AbortError")) return;
    sourceWritable = false;
    updateConfigEditorControls();
    showManagementError("config", err);
  });
}

function validateConfigEditor() {
  clearManagementError("config");
  return apiFetch("/api/v1/server/config/validate", {
    method: "POST",
    headers: {"Content-Type": "application/yaml"},
    body: document.getElementById("config-editor").value
  }).then(function() {
    showToast("Configuration is valid");
  }).catch(function(err) { showManagementError("config", err); });
}

function applyConfigEditor() {
  if (!sourceWritable || !canManage("operator") || configApplyInFlight) return Promise.resolve();
  compareConfigEditor();
  showModal("Apply configuration", "Write the complete configuration document and schedule a refresh?", function() {
    if (!sourceWritable || !canManage("operator") || configApplyInFlight) return Promise.resolve();
    var editor = document.getElementById("config-editor");
    var submittedDocument = editor.value;
    var submittedRevision = configEditorRevision;
    var previousRevision = configDocumentCache && configDocumentCache.document_revision;
    var headers = {"Content-Type": "application/yaml"};
    if (configEditorBaselineRevision) headers["If-Match"] = '"' + configEditorBaselineRevision + '"';
    configApplyInFlight = true;
    updateConfigEditorControls();
    return apiFetch("/api/v1/server/config/apply", {
      method: "POST",
      headers: headers,
      body: submittedDocument
    }).then(function(result) {
      configRefreshSequence++;
      configEditorBaseline = submittedDocument;
      configEditorBaselineRevision = result && result.document_revision;
      configServerDocument = submittedDocument;
      configPendingWrite = {document:submittedDocument, revision:result && result.document_revision, previousRevision:previousRevision};
      if (configEditorRevision === submittedRevision) editor.value = submittedDocument;
      showToast("Configuration written; refresh scheduled");
      return refreshRuntimeConfig(undefined, {forceDocument:true});
    }).catch(function(err) { showManagementError("config", err); }).finally(function() {
      configApplyInFlight = false;
      updateConfigEditorControls();
    });
  });
}

function parseConfigDocument(text) {
  var doc = LiveForgeYAML.parseDocument(text, {uniqueKeys:true});
  if (doc.errors.length || !LiveForgeYAML.isMap(doc.contents)) throw new Error("Invalid YAML");
  return doc;
}

function syncCommonConfigControls() {
  var text = document.getElementById("config-editor").value;
  if (text !== configCommonText) {
    configCommonText = text;
    try { configCommonDocument = parseConfigDocument(text); } catch (_) { configCommonDocument = null; }
  }
  document.getElementById("config-common-state").textContent = text && !configCommonDocument ? consoleText("Invalid YAML") : "";
  document.querySelectorAll("[data-config-path]").forEach(function(input) {
    input.disabled = !configCommonDocument || !sourceWritable || !canManage("operator");
    if (!configCommonDocument || document.activeElement === input) return;
    var value = configCommonDocument.getIn(input.dataset.configPath.split("."));
    if (input.type === "checkbox") input.checked = value === true;
    else input.value = typeof value === "string" || typeof value === "number" ? value : "";
  });
}

function changeCommonConfig(input) {
  if (input.disabled || input.readOnly || !sourceWritable || !canManage("operator")) return;
  var value = input.type === "checkbox" ? input.checked : input.value;
  if (input.type === "number") {
    if (!input.value || !input.checkValidity() || !Number.isSafeInteger(Number(input.value))) { input.reportValidity(); return; }
    value = Number(input.value);
  }
  var editor = document.getElementById("config-editor");
  try {
    var doc = parseConfigDocument(editor.value);
    doc.setIn(input.dataset.configPath.split("."), value);
    editor.value = doc.toString();
    editor.dispatchEvent(new Event("input"));
  } catch (_) {
    document.getElementById("config-common-state").textContent = consoleText("Invalid YAML");
  }
}

function configRedactionSchema(schema) {
  for (var i = 0; schema && schema.$ref && i < 8; i++) {
    if (!schema.$ref.startsWith("#/$defs/")) return {};
    var original = schema;
    schema = Object.assign({}, (configSchemaData.$defs || {})[schema.$ref.slice(8)], original);
    delete schema.$ref;
  }
  return schema || {};
}

function configSensitiveKey(key) {
  return /password|passwd|passphrase|token|secret|credential|api_?key|private_?key|key_file/i.test(key || "");
}

function configIdentityKey(key) {
  return ["id", "name", "username", "channel_id", "device_id"].indexOf(key) >= 0;
}

// Mark sensitive scalar values before walking aliases at their public paths.
function collectConfigSecrets(value, schema, key, state, sensitive) {
  if (++state.nodes > 20000 || state.depth > 64) throw new Error("Comparison unavailable");
  schema = configRedactionSchema(schema);
  sensitive = sensitive || schema["x-liveforge-secret"] || configSensitiveKey(key);
  if (value && typeof value === "object") {
    state.depth++;
    Object.keys(value).forEach(function(name) { collectConfigSecrets(value[name], Array.isArray(value) ? schema.items : (schema.properties || {})[name], name, state, sensitive && !configIdentityKey(name)); });
    state.depth--;
  } else if (sensitive && value !== null && value !== undefined && value !== "") {
    state.secrets.add(value);
  }
}

function redactConfigSecretTree(value, state) {
  if (++state.nodes > 20000 || state.depth > 64) throw new Error("Comparison unavailable");
  if (value && typeof value === "object") {
    state.depth++;
    var clean = Array.isArray(value) ? [] : Object.create(null);
    Object.keys(value).forEach(function(key) {
      clean[key] = configIdentityKey(key) ? redactConfigValue(value[key], {}, key, state) : redactConfigSecretTree(value[key], state);
    });
    state.depth--;
    return clean;
  }
  return state.draft && value !== "[REDACTED]" && value !== null && value !== "" ? "[REDACTED: supplied]" : "[REDACTED]";
}

function redactConfigValue(value, schema, key, state) {
  if (++state.nodes > 20000 || state.depth > 64) throw new Error("Comparison unavailable");
  schema = configRedactionSchema(schema);
  if (schema["x-liveforge-secret"] || configSensitiveKey(key) || state.secrets.has(value)) return redactConfigSecretTree(value, state);
  if (value && typeof value === "object") {
    state.depth++;
    var clean = Array.isArray(value) ? [] : Object.create(null);
    Object.keys(value).forEach(function(name) {
      clean[name] = redactConfigValue(value[name], Array.isArray(value) ? schema.items : (schema.properties || {})[name], name, state);
    });
    state.depth--;
    return clean;
  }
  if (typeof value === "string") {
    if (/^[a-z][a-z0-9+.-]*:\/\//i.test(value)) {
      try {
        var url = new URL(value);
        var maskedPath = /^\/__liveforge_redacted_path__\/[a-f0-9]{32}$/.test(url.pathname);
        var supplied = state.draft && ((url.username && url.username !== "REDACTED") || url.password ||
          (url.search && url.search !== "?__liveforge_redacted__=1") || url.hash ||
          (url.pathname !== "/" && !maskedPath));
        return url.protocol + "//" + url.host + (maskedPath ? url.pathname : url.pathname === "/" ? "/" : "/[REDACTED]") + (supplied ? " [REDACTED: supplied]" : "");
      }
      catch (_) { return "[REDACTED]"; }
    }
    if (/(?:url|uri|endpoint|address)$/i.test(key || "") && (value.indexOf("@") >= 0 || value.indexOf("?") >= 0 || value.indexOf("#") >= 0)) return "[REDACTED]";
  }
  return value;
}

function redactedConfigValue(text, draft) {
  var doc = parseConfigDocument(text);
  var value = doc.toJS({maxAliasCount:50});
  var state = {nodes:0, depth:0, secrets:new Set(), draft:!!draft};
  collectConfigSecrets(value, configSchemaData, "", state, false);
  state.nodes = 0;
  return redactConfigValue(value, configSchemaData, "", state);
}

function configChangedValues(before, after, path, changes) {
  if (JSON.stringify(before) === JSON.stringify(after)) return;
  var beforeMap = before && typeof before === "object" && !Array.isArray(before);
  var afterMap = after && typeof after === "object" && !Array.isArray(after);
  var keys = Array.from(new Set(Object.keys(beforeMap ? before : {}).concat(Object.keys(afterMap ? after : {})))).sort();
  if ((beforeMap || before == null) && (afterMap || after == null) && keys.length) {
    before = before || {}; after = after || {};
    keys.forEach(function(key) {
      configChangedValues(before[key], after[key], path.concat(key), changes);
    });
  } else {
    changes.push({path:path, before:before, after:after});
  }
}

function configReloadImpact(path) {
  var schema = configRedactionSchema(configSchemaData), impact = schema["x-liveforge-reload"];
  for (var i = 0; i < path.length; i++) {
    if (!schema.properties || !Object.prototype.hasOwnProperty.call(schema.properties, path[i])) return "";
    schema = configRedactionSchema(schema.properties[path[i]]);
    impact = schema["x-liveforge-reload"] || impact;
  }
  return {immutable:"Immutable", restart_required:"Restart required", hot_reload:"Hot reload"}[impact] || "";
}

function compareConfigEditor() {
  var baseline = document.getElementById("config-compare-source").value;
  var before = baseline === "history" ? configHistoryEntry && configHistoryEntry.document : baseline === "desired" ? configServerDocument : configDocumentCache && configDocumentCache.effective_document;
  var beforePane = document.getElementById("config-diff-before");
  var afterPane = document.getElementById("config-diff-after");
  var summary = document.getElementById("config-diff-summary");
  document.getElementById("config-comparison").hidden = false;
  beforePane.textContent = "";
  afterPane.textContent = "";
  try {
    if (!before) throw new Error("Comparison unavailable");
    var changes = [];
    configChangedValues(redactedConfigValue(before), redactedConfigValue(document.getElementById("config-editor").value, true), [], changes);
    summary.textContent = consoleText(changes.length ? "Redacted configuration changes" : "No configuration changes") + (changes.length ? " (" + changes.length + ")" : "");
    ["before", "after"].forEach(function(side) {
      document.getElementById("config-diff-" + side).textContent = changes.map(function(change) {
        var impact = configReloadImpact(change.path);
        return change.path.join(".") + (impact ? " [" + consoleText(impact) + "]" : "") + ":\n" + (change[side] === undefined ? "  -\n" : LiveForgeYAML.stringify(change[side]).split("\n").map(function(line) { return "  " + line; }).join("\n"));
      }).join("\n");
    });
  } catch (_) {
    summary.textContent = consoleText("Comparison unavailable");
  }
}

function refreshConfigHistory() {
  var button = document.getElementById("config-history-refresh");
  button.disabled = true;
  return apiFetch("/api/v1/server/config/history").then(function(entries) {
    var select = document.getElementById("config-history-select"), selected = select.value;
    select.replaceChildren();
    var empty = document.createElement("option");
    empty.value = ""; empty.textContent = consoleText(Array.isArray(entries) && entries.length ? "Select revision" : "No revisions");
    select.appendChild(empty);
    (Array.isArray(entries) ? entries : []).forEach(function(entry) {
      var option = document.createElement("option");
      option.value = entry.revision;
      option.textContent = formatDate(entry.created_at) + " / " + entry.revision.slice(0, 12) + " / " + formatBytes(entry.bytes);
      select.appendChild(option);
    });
    select.value = selected;
    if (!select.value) return selectConfigHistory();
  }).catch(function(err) { showManagementError("config", err); }).finally(function() { button.disabled = !canManage("viewer"); });
}

function selectConfigHistory() {
  var sequence = ++configHistorySequence;
  var revision = document.getElementById("config-history-select").value;
  configHistoryEntry = null;
  document.getElementById("config-history-document").hidden = true;
  document.querySelector('#config-compare-source option[value="history"]').disabled = true;
  updateConfigEditorControls();
  if (!revision) return Promise.resolve();
  return apiFetch("/api/v1/server/config/history/" + encodeURIComponent(revision)).then(function(entry) {
    if (sequence !== configHistorySequence) return;
    if (!entry || entry.revision !== revision || typeof entry.document !== "string") throw new Error("Comparison unavailable");
    configHistoryEntry = entry;
    var pane = document.getElementById("config-history-document");
    pane.textContent = entry.document;
    pane.hidden = false;
    document.querySelector('#config-compare-source option[value="history"]').disabled = false;
    updateConfigEditorControls();
  }).catch(function(err) { if (sequence === configHistorySequence) showManagementError("config", err); });
}

function rollbackConfigHistory() {
  if (!configHistoryEntry || !sourceWritable || !canManage("operator") || configApplyInFlight) return;
  var entry = configHistoryEntry;
  var revision = configPendingWrite ? configPendingWrite.revision : configDocumentCache && configDocumentCache.document_revision;
  if (!revision) return;
  showModal("Restore configuration revision", hasConfigDraft() ? "Discard the current draft, write the selected revision, and schedule a refresh?" : "Write the selected revision and schedule a refresh?", function() {
    if (!sourceWritable || !canManage("operator") || configApplyInFlight) return;
    var submittedRevision = configEditorRevision;
    configApplyInFlight = true;
    updateConfigEditorControls();
    return apiFetch("/api/v1/server/config/rollback", {
      method:"POST", headers:{"Content-Type":"application/json", "If-Match":'"' + revision + '"'}, body:JSON.stringify({revision:entry.revision})
    }).then(function(result) {
      configRefreshSequence++;
      configEditorBaseline = entry.document;
      configEditorBaselineRevision = result.document_revision;
      configServerDocument = entry.document;
      configPendingWrite = {document:entry.document, revision:result.document_revision, previousRevision:revision};
      if (configEditorRevision === submittedRevision) document.getElementById("config-editor").value = entry.document;
      showToast("Configuration written; refresh scheduled");
      return refreshRuntimeConfig(undefined, {forceDocument:true});
    }).then(refreshConfigHistory).catch(function(err) { showManagementError("config", err); }).finally(function() {
      configApplyInFlight = false;
      updateConfigEditorControls();
    });
  });
}

function refreshRuntimeConfigNow() {
  var button = document.getElementById("config-refresh");
  button.disabled = true;
  clearManagementError("config");
  apiFetch("/api/v1/server/config/refresh", { method: "POST" })
    .then(function() {
      showToast("Configuration refresh scheduled");
      setTimeout(refreshActiveView, 250);
    })
    .catch(function(err) { showManagementError("config", err); })
    .finally(function() {
      button.disabled = !canManage("operator");
    });
}
