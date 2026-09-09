function formatDate(value) {
  if (!value || value === "0001-01-01T00:00:00Z") return "-";
  var date = new Date(value);
  return isNaN(date.getTime()) ? "-" : date.toLocaleString();
}

function formatBytes(value) {
  var bytes = Number(value) || 0;
  if (bytes < 1024) return bytes + " B";
  if (bytes < 1048576) return (bytes / 1024).toFixed(1) + " KiB";
  if (bytes < 1073741824) return (bytes / 1048576).toFixed(1) + " MiB";
  return (bytes / 1073741824).toFixed(1) + " GiB";
}

function formatDurationSeconds(value) {
  var seconds = Number(value) || 0;
  return seconds > 0 ? formatUptime(seconds) : "-";
}

function appendTextCell(row, value, className) {
  var cell = row.insertCell();
  if (className) cell.className = className;
  cell.textContent = value == null ? "" : String(value);
  return cell;
}

function appendActionButton(container, label, className, action, actionID) {
  var button = document.createElement("button");
  button.type = "button";
  button.className = "btn" + (className ? " " + className : "");
  button.textContent = consoleText(label);
  button.dataset.action = action;
  button.dataset.actionId = String(actionID == null ? "" : actionID);
  container.appendChild(button);
  return button;
}

function createDynamicActionHandlers() {
  return {
    "stream-preview": function(value) { openPlayer(value); },
    "stream-kick": function(value) { kickStream(value); },
    "stream-delete": function(value) { deleteStream(value); },
    "sip-detail": function(value) { loadSIPCall(value); },
    "sip-hangup": function(value) { hangupSIPCall(value); },
    "sip-lab-preview": function(value, button) { openPlayer(value, { video_codec: button.dataset.videoCodec || "H264", audio_codec: button.dataset.audioCodec || "" }, button.playbackMetadata); },
    "sip-lab-stop": function(value) { stopSIPLab(value); },
    "recording-detail": function(value) { loadRecording(value); },
    "recording-play": function(value) { playRecording(value, recordingFormats[value]); },
    "recording-download": function(value) { downloadRecording(value); },
    "recording-delete": function(value) { deleteRecording(value); },
    "dvr-detail": function(value) { loadDVRSession(value); },
    "dvr-play": function(value) { playDVR(value); },
    "gb-toggle-device": function(value) { toggleDevice(value); },
    "gb-catalog": function(value) { gbCatalog(value); },
    "gb-delete-device": function(value) { gbDeleteDevice(value); },
    "gb-play": function(value) { gbPlay(value); },
    "gb-stop": function(value) { gbStop(value); },
    "gb-ptz": function(value) { openPTZ(value); },
    "gb-playback": function(value) { openPlayback(value); },
    "gb-preview": function(value) { openPlayer(value); },
    "gb-close-session": function(value) { gbCloseSession(value); },
    "gb-lab-preview": function(value, button) { openPlayer(value, { video_codec: button.dataset.videoCodec || "H264", audio_codec: button.dataset.audioCodec || "G711A" }, button.playbackMetadata); },
    "gb-lab-stop": function(value) { stopGBLab(value); }
  };
}

var dynamicActionHandlers = createDynamicActionHandlers();
document.addEventListener("click", function(event) {
  var button = event.target.closest("[data-action]");
  if (!button) return;
  var handler = dynamicActionHandlers[button.dataset.action];
  if (handler) handler(button.dataset.actionId, button);
});

function renderEmptyTable(tbodyID, emptyID, rows) {
  document.getElementById(tbodyID).innerHTML = rows;
  document.getElementById(emptyID).style.display = rows ? "none" : "block";
}

function refreshCluster(signal) {
  clearManagementError("cluster");
  return apiFetch("/api/v1/cluster/status", { signal: signal }).then(function(data) {
    data = data || {};
    var relays = data.relays || [];
    var peers = data.peers || [];
    document.getElementById("cluster-forwards").textContent = data.active_forwards || 0;
    document.getElementById("cluster-origins").textContent = data.active_origins || 0;
    document.getElementById("cluster-peers").textContent = peers.length;
    document.getElementById("cluster-truncated").textContent = consoleText(data.truncated ? "Truncated" : "Complete");
    document.getElementById("cluster-truncated").className = "stat-value " + (data.truncated ? "health-warn" : "health-good");
    var relayRows = relays.map(function(relay) {
      return "<tr><td>" + esc(relay.direction) + "</td><td><span class=\"codec-tag\">" + esc(relay.protocol) + "</span></td><td class=\"stream-key\">" + esc(relay.stream_key) + "</td><td class=\"mono\">" + esc(relay.endpoint) + "</td><td>" + formatDate(relay.started_at) + "</td></tr>";
    }).join("");
    renderEmptyTable("cluster-relays-tbody", "cluster-relays-empty", relayRows);
    var peerRows = peers.map(function(peer) {
      var state = peer.evicted ? '<span class="health-bad">Evicted</span>' : '<span class="health-good">Available</span>';
      return "<tr><td class=\"mono\">" + esc(peer.host) + "</td><td>" + (peer.consecutive_failures || 0) + "</td><td>" + formatDate(peer.last_attempt) + "</td><td>" + formatDate(peer.last_success) + "</td><td>" + state + "</td></tr>";
    }).join("");
    renderEmptyTable("cluster-peers-tbody", "cluster-peers-empty", peerRows);
  }).catch(function(err) { showManagementError("cluster", err); });
}

function refreshSIPCalls(signal) {
  clearManagementError("sip");
  return Promise.all([apiFetch("/api/v1/sipgateway/calls", { signal: signal, page:"sip" }), refreshSIPLab(signal)]).then(function(results) {
    var data = results[0] || {};
    data = data || {};
    var metrics = data.metrics || {};
    var calls = data.calls || [];
    document.getElementById("sip-active").textContent = metrics.active_calls || 0;
    document.getElementById("sip-inbound").textContent = metrics.active_inbound || 0;
    document.getElementById("sip-outbound").textContent = metrics.active_outbound || 0;
    document.getElementById("sip-network-failures").textContent = metrics.network_failures_total || 0;
    renderSIPCalls(calls);
  }).catch(function(err) { showManagementError("sip", err); });
}

function runSIPSelfTest() {
  var result = document.getElementById("sip-self-test-result");
  result.textContent = "Running local SIP checks...";
  return apiFetch("/api/v1/sipgateway/test")
    .then(function(report) {
      result.textContent = JSON.stringify(report, null, 2);
      result.className = "ops-detail " + (report && report.passed ? "health-good" : "health-bad");
    })
    .catch(function(err) {
      result.textContent = managementErrorMessage(err);
      result.className = "ops-detail health-bad";
    });
}

function renderSIPCalls(calls) {
  calls = calls || [];
  var tbody = document.getElementById("sip-calls-tbody");
  tbody.replaceChildren();
  document.getElementById("sip-calls-empty").style.display = calls.length ? "none" : "block";
  calls.forEach(function(call) {
    var row = tbody.insertRow();
    appendTextCell(row, call.call_id, "mono");
    appendTextCell(row, call.direction);
    appendTextCell(row, call.stream_key, "stream-key");
    appendTextCell(row, call.codec || "-");
    appendTextCell(row, call.state);
    appendTextCell(row, (call.rtp_packets_sent || 0) + " tx / " + (call.rtp_packets_received || 0) + " rx", "mono");
    var actionCell = row.insertCell();
    var actions = document.createElement("div");
    actions.className = "actions";
    appendActionButton(actions, "Details", "", "sip-detail", call.call_id);
    if (canManage("operator")) appendActionButton(actions, "Hang up", "btn-danger", "sip-hangup", call.call_id);
    actionCell.appendChild(actions);
  });
}

function renderSIPLabSessions(views) {
  views = views || [];
  var tbody = document.getElementById("sip-lab-sessions-tbody");
  var empty = document.getElementById("sip-lab-sessions-empty");
  tbody.replaceChildren();
  empty.style.display = views.length ? "none" : "block";
  views.forEach(function(view) {
    var s = view.session || {};
    if (s.stream_key) streamMedia[s.stream_key] = { video_codec: "H264", audio_codec: s.codec || "" };
    var row = tbody.insertRow();
    appendTextCell(row, s.device_id || s.identity || "-", "mono");
    appendTextCell(row, s.mode || "-");
    appendTextCell(row, s.stream_key || "-", "stream-key");
    appendTextCell(row, s.state || "-");
    appendTextCell(row, s.last_error || "-", "lab-error");
    appendTextCell(row, (s.audio_rtp_packets_sent || 0) + " tx / " + (s.audio_rtp_packets_received || 0) + " rx", "mono");
    appendTextCell(row, (s.video_rtp_packets_sent || 0) + " tx / " + (s.video_rtp_packets_received || 0) + " rx", "mono");
    appendTextCell(row, (s.rtcp_packets_sent || 0) + " tx / " + (s.rtcp_packets_received || 0) + " rx", "mono");
    appendTextCell(row, formatDate(s.last_media_at));
    var actionCell = row.insertCell();
    var actions = document.createElement("div");
    actions.className = "actions";
    if (s.stream_key && view.playback && view.playback.available) {
      var preview = appendActionButton(actions, "Preview", "btn-play", "sip-lab-preview", s.stream_key);
      preview.dataset.videoCodec = "H264";
      preview.dataset.audioCodec = s.codec || "";
      preview.playbackMetadata = view.playback;
    }
    if (canManage("operator") && s.state !== "stopped") appendActionButton(actions, "Stop", "btn-danger", "sip-lab-stop", s.id);
    actionCell.appendChild(actions);
  });
}

function refreshSIPLab(signal) {
  return apiFetch("/api/v1/sipgateway/lab/sessions", { signal: signal }).then(function(data) {
    renderSIPLabSessions((data && data.sessions) || []);
  }).catch(function(err) {
    document.getElementById("sip-lab-error").textContent = managementErrorMessage(err);
  });
}

function startSIPLab(event) {
  event.preventDefault();
  if (!canManage("operator")) return;
  var button = document.getElementById("sip-lab-start");
  button.disabled = true;
  document.getElementById("sip-lab-error").textContent = "";
  var request = {
    mode: document.getElementById("sip-lab-mode").value,
    device_id: document.getElementById("sip-lab-device").value.trim(),
    stream_key: document.getElementById("sip-lab-stream").value.trim(),
    codec: document.getElementById("sip-lab-codec").value
  };
  if (!validProtocolLabStreamKey(request.stream_key)) {
    document.getElementById("sip-lab-error").textContent = "Stream key segments must be non-empty and cannot be . or ..";
    button.disabled = !canManage("operator");
    return;
  }
  apiFetch("/api/v1/sipgateway/lab/sessions", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(request) })
    .then(function(data) { showToast("SIP lab started"); renderSIPLabSessions(data ? [data] : []); return refreshSIPLab(); })
    .catch(function(err) { document.getElementById("sip-lab-error").textContent = managementErrorMessage(err); })
    .finally(function() { button.disabled = !canManage("operator"); });
}

function stopSIPLab(id) {
  showModal("Stop SIP lab", "Stop this local fake SIP device?", function() {
    return apiFetch("/api/v1/sipgateway/lab/sessions/" + encodeURIComponent(id), { method: "DELETE" })
      .then(function() { showToast("SIP lab stopped"); return refreshSIPLab(); })
      .catch(function(err) { document.getElementById("sip-lab-error").textContent = managementErrorMessage(err); });
  });
}

function dialSIPCall(event) {
  event.preventDefault();
  var button = document.getElementById("sip-dial");
  button.disabled = true;
  clearManagementError("sip");
  apiFetch("/api/v1/sipgateway/calls", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ target_uri: document.getElementById("sip-target-uri").value.trim(), stream_key: document.getElementById("sip-stream-key").value.trim() }) })
    .then(function(data) {
      showToast("SIP call created: " + ((data && data.call_id) || "pending"));
      refreshActiveView();
    })
    .catch(function(err) { showManagementError("sip", err); })
    .finally(function() { button.disabled = !canManage("operator"); });
}

function loadSIPCall(callID) {
  clearManagementError("sip");
  apiFetch("/api/v1/sipgateway/calls/" + encodeURIComponent(callID))
    .then(function(data) { document.getElementById("sip-call-detail").textContent = JSON.stringify(data, null, 2); })
    .catch(function(err) { showManagementError("sip", err); });
}

function hangupSIPCall(callID) {
  showModal("Hang up SIP call", 'Terminate call "' + callID + '"?', function() {
    return apiFetch("/api/v1/sipgateway/calls/" + encodeURIComponent(callID), { method: "DELETE" })
      .then(function() { showToast("SIP call ended"); refreshActiveView(); })
      .catch(function(err) { showManagementError("sip", err); });
  });
}

function settledRequest(promise) {
  return promise.then(function(value) { return { value: value }; }, function(error) { return { error: error }; });
}

function recordingURL(recordingID) {
  return "/api/v1/recordings/" + String(recordingID || "").split("/").map(encodeURIComponent).join("/");
}

function recordingActionURL(recordingID, action) {
  return recordingURL(recordingID) + "?action=" + encodeURIComponent(action);
}

function recordingPlayURL(recordingID) {
  return recordingActionURL(recordingID, "play");
}

function recordingDownloadURL(recordingID) {
  return recordingActionURL(recordingID, "download");
}

function renderRecordings(items, allowPlayback) {
  items = items || [];
  recordingFormats = Object.create(null);
  document.getElementById("recording-count").textContent = consolePages.recordings.serverPage ? consolePages.recordings.total : items.length;
  var tbody = document.getElementById("recordings-tbody");
  tbody.replaceChildren();
  document.getElementById("recordings-empty").style.display = items.length ? "none" : "block";
  items.forEach(function(item) {
    recordingFormats[item.id] = item.format || "";
    var row = tbody.insertRow();
    appendTextCell(row, item.id, "mono");
    appendTextCell(row, item.stream_key || "-", "stream-key");
    appendTextCell(row, item.format || "-");
    appendTextCell(row, item.state || "-");
    appendTextCell(row, formatBytes(item.size_bytes), "mono");
    appendTextCell(row, formatDate(item.completed_at));
    var actionCell = row.insertCell();
    var actions = document.createElement("div");
    actions.className = "actions";
    appendActionButton(actions, "Details", "", "recording-detail", item.id);
    if (item.state === "completed" && allowPlayback !== false) {
      appendActionButton(actions, "Play", "btn-play", "recording-play", item.id);
      appendActionButton(actions, "Download", "", "recording-download", item.id);
    }
    if (canManage("admin")) appendActionButton(actions, "Delete", "btn-danger", "recording-delete", item.id);
    actionCell.appendChild(actions);
  });
}

function renderDVR(data) {
  data = data || {};
  var sessions = data.sessions || [];
  document.getElementById("storage-dvr-sessions").textContent = sessions.length;
  var tbody = document.getElementById("dvr-tbody");
  tbody.replaceChildren();
  document.getElementById("dvr-empty").style.display = sessions.length ? "none" : "block";
  sessions.forEach(function(session) {
    var row = tbody.insertRow();
    appendTextCell(row, session.stream_key, "stream-key");
    var stateCell = appendTextCell(row, session.live ? "Live" : "Closed");
    if (session.live) stateCell.className = "health-good";
    appendTextCell(row, session.segments || 0);
    appendTextCell(row, formatDurationSeconds(session.duration_sec));
    appendTextCell(row, formatBytes(session.bytes), "mono");
    appendTextCell(row, formatDate(session.oldest_segment) + " / " + formatDate(session.newest_segment));
    var actionCell = row.insertCell();
    appendActionButton(actionCell, "Details", "", "dvr-detail", session.stream_key);
    if (Number(session.segments) > 0) appendActionButton(actionCell, "Play", "btn-play", "dvr-play", session.stream_key);
  });
}

function refreshStorage(signal) {
  clearManagementError("storage");
  return Promise.all([
    settledRequest(apiFetch("/api/v1/recordings", { signal: signal, page:"recordings", query:{q:document.getElementById("recording-search").value.trim()} })),
    settledRequest(apiFetch("/api/v1/recordings/status", { signal: signal })),
    settledRequest(apiFetch("/api/v1/dvr/status", { signal: signal }))
  ]).then(function(results) {
    var recordingStatus = results[1].value || {};
    var recordingMetrics = recordingStatus.metrics || {};
    document.getElementById("recording-completed").textContent = recordingMetrics.files_completed || 0;
    var capability = document.getElementById("recording-capability");
    var state = recordingStatus.state || (recordingStatus.enabled ? "ready" : "disabled");
    if (results[0].value) renderRecordings(results[0].value, state !== "disabled");
    var capabilityText = "";
    var capabilityClass = "";
    if (state === "disabled") {
      capabilityText = "Recording is disabled. Enable record.enabled to create playback files.";
    } else if (state === "ready") {
      capabilityText = "Recording is ready for playback.";
      capabilityClass = "good";
    } else if (state === "degraded") {
      capabilityText = "Recording is degraded: " + (recordingStatus.reason || "storage needs attention");
      capabilityClass = "warn";
    } else {
      capabilityText = "Recording is unavailable: " + (recordingStatus.reason || "storage cannot be used");
      capabilityClass = "bad";
    }
    capability.textContent = capabilityText;
    capability.className = "ops-capability show " + capabilityClass;
    var recordHealth = recordingStatus.storage || {};
    var dvrData = results[2].value || {};
    var dvrHealth = dvrData.storage || {};
    var healthy = (recordHealth.healthy || !recordHealth.root) && (dvrHealth.healthy || !dvrHealth.root);
    var lowSpace = recordHealth.low_space || dvrHealth.low_space;
    var health = document.getElementById("storage-health");
    var dvrEnabled = dvrData.enabled === true;
    if (state === "disabled" && !dvrEnabled) {
      health.textContent = "Disabled";
      health.className = "stat-value";
    } else {
      health.textContent = consoleText(lowSpace ? "Low space" : healthy ? "Healthy" : "Unavailable");
      health.className = "stat-value " + (lowSpace ? "health-warn" : healthy ? "health-good" : "health-bad");
    }
    renderDVR(dvrData);
    var failed = results.filter(function(result) { return result.error && result.error.name !== "AbortError"; });
    if (failed.length) showManagementError("storage", failed[0].error);
  });
}

function loadRecording(recordingID) {
  clearManagementError("storage");
  return apiFetch(recordingURL(recordingID))
    .then(function(data) { document.getElementById("recording-detail").textContent = JSON.stringify(data, null, 2); })
    .catch(function(err) { showManagementError("storage", err); });
}

function playRecording(recordingID, recordingFormat) {
  destroyCurrentPlayer();
  statsVisible = false;
  var btnStats = document.getElementById("btn-stats");
  if (btnStats) btnStats.classList.remove("active");
  document.getElementById("player-stream-key").textContent = recordingID;
  document.getElementById("player-overlay").classList.add("active");
  document.getElementById("proto-tabs").replaceChildren();
  setPlayerURL(recordingPlayURL(recordingID));
  setPlayerStatus("Loading recording...");
  var video = document.getElementById("player-video");
  var format = String(recordingFormat || "").toLowerCase();
  var ext = String(recordingID).split(".").pop().toLowerCase();
  var playbackType = format || ext;
  if (playbackType === "mp4" || playbackType === "fmp4" || playbackType === "m4v") {
    video.src = recordingPlayURL(recordingID);
    video.play().then(function() { setPlayerStatus("Playing recording", "ok"); }).catch(function(err) { setPlayerStatus("Play error: " + formatErr(err), "error"); });
    return;
  }
  if ((playbackType === "flv" || playbackType === "ts" || playbackType === "hls") && typeof mpegts !== "undefined" && mpegts.isSupported()) {
    var player = mpegts.createPlayer({ type: playbackType === "flv" ? "flv" : "mpegts", url: recordingPlayURL(recordingID), isLive: false });
    player.attachMediaElement(video);
    player.load();
    player.play().then(function() { setPlayerStatus("Playing recording", "ok"); }).catch(function(err) { setPlayerStatus("Play error: " + formatErr(err), "error"); });
    currentPlayer = player;
    return;
  }
  video.src = recordingPlayURL(recordingID);
  video.play().then(function() { setPlayerStatus("Playing recording", "ok"); }).catch(function(err) { setPlayerStatus("Browser cannot play this recording: " + formatErr(err), "error"); });
}

function downloadRecording(recordingID) {
  clearManagementError("storage");
  return apiFetch(recordingDownloadURL(recordingID), { responseType: "blob" })
    .then(function(blob) {
      var url = URL.createObjectURL(blob);
      var link = document.createElement("a");
      link.href = url;
      link.download = String(recordingID).split("/").pop() || "recording";
      document.body.appendChild(link);
      link.click();
      link.remove();
      URL.revokeObjectURL(url);
    })
    .catch(function(err) { showManagementError("storage", err); });
}

function deleteRecording(recordingID) {
  showModal("Delete recording", 'Permanently delete "' + recordingID + '"?', function() {
    return apiFetch("/api/v1/recordings/" + String(recordingID).split("/").map(encodeURIComponent).join("/"), { method: "DELETE" })
      .then(function() { showToast("Recording deleted"); refreshActiveView(); })
      .catch(function(err) { showManagementError("storage", err); });
  });
}

function loadDVRSession(streamKey) {
  clearManagementError("storage");
  apiFetch("/api/v1/dvr/sessions/" + String(streamKey).split("/").map(encodeURIComponent).join("/"))
    .then(function(data) { document.getElementById("dvr-detail").textContent = JSON.stringify(data, null, 2); })
    .catch(function(err) { showManagementError("storage", err); });
}

function dvrMediaURL(streamKey) {
  var port = portFromListen(endpoints.dvr);
  if (!port) return "";
  var pathValue = String(streamKey || "").split("/").map(encodeURIComponent).join("/");
  var host = hostFromListen(endpoints.dvr);
  if (host.indexOf(":") >= 0 && host.charAt(0) !== "[") host = "[" + host + "]";
  var scheme = endpointSchemes.dvr === "https" ? "https:" : endpointSchemes.dvr === "http" ? "http:" : location.protocol;
  return scheme + "//" + host + ":" + port + "/dvr/" + pathValue + ".m3u8";
}

function playDVR(streamKey) {
  var url = dvrMediaURL(streamKey);
  if (!url) {
    showManagementError("storage", new Error("DVR media endpoint is unavailable"));
    return;
  }
  destroyCurrentPlayer();
  statsVisible = false;
  var btnStats = document.getElementById("btn-stats");
  if (btnStats) btnStats.classList.remove("active");
  document.getElementById("player-stream-key").textContent = "DVR " + streamKey;
  document.getElementById("player-overlay").classList.add("active");
  document.getElementById("proto-tabs").replaceChildren();
  setPlayerURL(url);
  setPlayerStatus("Loading DVR...");
  var video = document.getElementById("player-video");
  if (typeof Hls !== "undefined" && Hls.isSupported()) {
    var hls = new Hls();
    hls.loadSource(url);
    hls.attachMedia(video);
    hls.on(Hls.Events.MANIFEST_PARSED, function() { video.play().then(function() { setPlayerStatus("Playing DVR", "ok"); }).catch(function(err) { setPlayerStatus("Play error: " + formatErr(err), "error"); }); });
    hls.on(Hls.Events.ERROR, function(_, data) { if (data && data.fatal) setPlayerStatus("DVR error: " + formatErr(data), "error"); });
    currentHls = hls;
    return;
  }
  if (video.canPlayType("application/vnd.apple.mpegurl")) {
    video.src = url;
    video.play().then(function() { setPlayerStatus("Playing DVR", "ok"); }).catch(function(err) { setPlayerStatus("Play error: " + formatErr(err), "error"); });
    return;
  }
  setPlayerStatus("HLS is not supported in this browser", "error");
}

function refreshSecurity(signal) {
  clearManagementError("security");
  return Promise.all([
    settledRequest(apiFetch("/api/v1/security/status", { signal: signal })),
    settledRequest(apiFetch("/api/v1/audit", { signal: signal, page:"audit", query:consoleAuditFilters() }))
  ]).then(function(results) {
    if (results[0].error) {
      showManagementError("security", results[0].error);
      return;
    }
    var status = results[0].value || {};
    managementRole = status.console_role || "viewer";
    applyManagementPermissions();
    document.getElementById("security-role").textContent = managementRole;
    document.getElementById("security-tokens").textContent = (status.tokens || []).length;
    document.getElementById("security-audit-entries").textContent = status.audit_entries || 0;
    document.getElementById("security-audit-events").textContent = status.audit_events_total || 0;
    document.getElementById("security-console").textContent = consoleText(status.console_configured ? "Configured" : "Disabled");
    document.getElementById("security-legacy").textContent = consoleText(status.legacy_bearer_configured ? "Configured" : "Disabled");
    document.getElementById("security-audit").textContent = consoleText(status.audit_enabled ? "Enabled" : "Disabled");
    var persistence = status.audit_persistence || {};
    var persistenceState = document.getElementById("security-audit-persistence");
    persistenceState.textContent = consoleText(!persistence.enabled ? "Disabled" : persistence.healthy ? "Healthy" : "Unhealthy");
    persistenceState.className = "ops-field-value " + (!persistence.enabled ? "" : persistence.healthy ? "health-good" : "health-bad");
    document.getElementById("security-audit-write-failures").textContent = persistence.write_failures_total || 0;
    var persistenceError = document.getElementById("security-audit-persistence-error");
    persistenceError.textContent = persistence.error || "";
    persistenceError.hidden = !persistence.error;
    persistenceError.className = "ops-capability" + (persistence.error ? " show bad" : "");
    var tokenRows = (status.tokens || []).map(function(binding) {
      return "<tr><td class=\"mono\">" + esc(binding.name || "unnamed") + "</td><td>" + esc(binding.role || "-") + "</td></tr>";
    }).join("");
    renderEmptyTable("security-token-tbody", "security-token-empty", tokenRows);
    if (results[1].error) {
      showManagementError("security", results[1].error);
      return;
    }
    var entries = (results[1].value || []).slice();
    if (!consolePages.audit.serverPage) entries.reverse();
    var auditRows = entries.map(function(entry) {
      var resultClass = entry.result === "success" ? "health-good" : entry.result === "denied" || entry.result === "unauthorized" ? "health-bad" : "health-warn";
      return "<tr><td>" + formatDate(entry.time) + "</td><td class=\"mono\">" + esc(entry.principal) + "</td><td>" + esc(entry.role || "-") + "</td><td class=\"mono\">" + esc(entry.action) + "</td><td class=\"mono\">" + esc(entry.resource) + "</td><td class=\"" + resultClass + "\">" + esc(entry.result) + "</td></tr>";
    }).join("");
    renderEmptyTable("audit-tbody", "audit-empty", auditRows);
  });
}

function bootstrapManagementRole() {
  apiFetch("/api/v1/security/status")
    .then(function(status) {
      managementRole = (status && status.console_role) || "viewer";
      applyManagementPermissions();
    })
    .catch(function() { applyManagementPermissions(); });
}

var activeViewRefreshers = {
  streams: function(signal) { return Promise.all([refreshServerInfo(signal), refresh(signal), refreshDVR(signal)]); },
  gb28181: function(signal) { return Promise.all([refreshServerInfo(signal), refreshGB28181(signal)]); },
  config: function(signal) { return Promise.all([refreshServerInfo(signal), refreshRuntimeConfig(signal)]); },
  cluster: function(signal) { return Promise.all([refreshServerInfo(signal), refreshCluster(signal)]); },
  sip: function(signal) { return Promise.all([refreshServerInfo(signal), refreshSIPCalls(signal)]); },
  storage: function(signal) { return Promise.all([refreshServerInfo(signal), refreshStorage(signal)]); },
  security: function(signal) { return Promise.all([refreshServerInfo(signal), refreshSecurity(signal)]); }
};

function refreshActiveView() {
  if (document.hidden || consoleSessionExpired) return Promise.resolve();
  var refresher = activeViewRefreshers[activeTab];
  if (!refresher) return Promise.resolve();
  return Promise.resolve(refresher(newActiveViewSignal()));
}

function stopActiveViewPolling() {
  if (activeViewInterval) {
    clearInterval(activeViewInterval);
    activeViewInterval = null;
  }
  abortActiveViewRequest();
  if (activeTab === "streams") stopStreamTrendPolling();
}

function startActiveViewPolling() {
  stopActiveViewPolling();
  if (document.hidden || consoleSessionExpired) return;
  refreshActiveView();
  activeViewInterval = setInterval(refreshActiveView, 3000);
  if (activeTab === "streams" && selectedStreamKey) startStreamTrendPolling();
}

document.addEventListener("visibilitychange", function() {
  if (document.hidden) {
    stopActiveViewPolling();
  } else {
    startActiveViewPolling();
  }
});

/* === GB28181 Console === */
var gbHasModule = false;
var ptzTargetChannel = '';
var pbTargetChannel = '';
var gbExpandedDevices = {};
var gbDeviceChannels = {};

function updateGB28181Tab(modules) {
  gbHasModule = modules && modules.indexOf("gb28181") >= 0;
  var tab = document.getElementById("tab-gb28181");
  if (tab) tab.hidden = !gbHasModule;
  if (!gbHasModule && activeTab === "gb28181") switchTab("streams");
}

function switchTab(name) {
  var target = document.getElementById("view-" + name);
  var tabs = document.querySelectorAll(".nav-tab");
  var selected = null;
  for (var i = 0; i < tabs.length; i++) {
    if (tabs[i].getAttribute("data-view") === name) selected = tabs[i];
  }
  if (!target || !selected || selected.hidden) return;
  stopActiveViewPolling();
  activeTab = name;
  for (var j = 0; j < tabs.length; j++) {
    var active = tabs[j].getAttribute("data-view") === name;
    tabs[j].classList.toggle("active", active);
    tabs[j].setAttribute("aria-selected", active ? "true" : "false");
    tabs[j].tabIndex = active ? 0 : -1;
  }
  var views = document.querySelectorAll("[id^=\"view-\"]");
  for (var k = 0; k < views.length; k++) {
    var view = views[k];
    view.hidden = view.id !== "view-" + name;
  }
  startActiveViewPolling();
}

function visibleManagementTabs() {
  return Array.from(document.querySelectorAll(".nav-tab")).filter(function(tab) { return !tab.hidden; });
}

document.getElementById("nav-tabs").addEventListener("click", function(event) {
  var tab = event.target.closest(".nav-tab");
  if (tab && !tab.hidden) switchTab(tab.getAttribute("data-view"));
});

document.getElementById("nav-tabs").addEventListener("keydown", function(event) {
  if (!["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) return;
  var tabs = visibleManagementTabs();
  var current = tabs.indexOf(event.target.closest(".nav-tab"));
  if (current < 0) return;
  event.preventDefault();
  var next = current;
  if (event.key === "Home") next = 0;
  if (event.key === "End") next = tabs.length - 1;
  if (event.key === "ArrowRight") next = (current + 1) % tabs.length;
  if (event.key === "ArrowLeft") next = (current - 1 + tabs.length) % tabs.length;
  switchTab(tabs[next].getAttribute("data-view"));
  tabs[next].focus();
});

function refreshGB28181(signal) {
  return Promise.all([
    apiFetch("/api/v1/gb28181/devices", { signal: signal }),
    apiFetch("/api/v1/gb28181/sessions", { signal: signal }),
    refreshGBLab(signal)
  ]).then(function(results) {
    var devices = results[0] || [];
    var sessions = results[1] || [];
    renderDevicesTable(devices);
    renderSessionsTable(sessions);

    // Stats
    var online = 0, totalCh = 0;
    for (var i = 0; i < devices.length; i++) {
      if (devices[i].status === "online") online++;
      totalCh += devices[i].channel_count || 0;
    }
    document.getElementById("gb-stat-online").textContent = online;
    document.getElementById("gb-stat-devices").textContent = devices.length;
    document.getElementById("gb-stat-channels").textContent = totalCh;
    document.getElementById("gb-stat-sessions").textContent = sessions.length;
  }).catch(function(err) { showManagementError("gb28181", err); });
}

function runGB28181SelfTest() {
  var result = document.getElementById("gb-self-test-result");
  result.textContent = "Running local GB28181 checks...";
  return apiFetch("/api/v1/gb28181/test")
    .then(function(report) {
      result.textContent = JSON.stringify(report, null, 2);
      result.className = "ops-detail " + (report && report.passed ? "health-good" : "health-bad");
    })
    .catch(function(err) {
      result.textContent = managementErrorMessage(err);
      result.className = "ops-detail health-bad";
    });
}

function renderDevicesTable(devices) {
  var tbody = document.getElementById("gb-devices-tbody");
  var empty = document.getElementById("gb-devices-empty");
  tbody.replaceChildren();
  if (devices.length === 0) {
    empty.style.display = "block";
    return;
  }
  empty.style.display = "none";
  devices.forEach(function(d) {
    var badgeCls = d.status === "online" ? "badge-online" : "badge-offline";
    var expanded = gbExpandedDevices[d.device_id];
    var timeStr = d.last_keepalive ? new Date(d.last_keepalive).toLocaleTimeString() : "-";
    var row = tbody.insertRow();
    row.className = "ch-toggle";
    var toggleCell = row.insertCell();
    toggleCell.style.width = "24px";
    var toggle = appendActionButton(toggleCell, expanded ? "▼" : "▶", "", "gb-toggle-device", d.device_id);
    toggle.setAttribute("aria-expanded", expanded ? "true" : "false");
    toggle.setAttribute("aria-label", (expanded ? "Collapse " : "Expand ") + d.device_id);
    appendTextCell(row, d.device_id, "mono");
    appendTextCell(row, d.remote_addr);
    appendTextCell(row, String(d.transport || "udp").toUpperCase());
    var statusCell = row.insertCell();
    var badge = document.createElement("span");
    badge.className = "badge " + badgeCls;
    badge.textContent = d.status;
    statusCell.appendChild(badge);
    appendTextCell(row, d.channel_count || 0);
    appendTextCell(row, timeStr);
    var actionCell = row.insertCell();
    var actions = document.createElement("div");
    actions.className = "actions";
    if (canManage("operator")) appendActionButton(actions, "Catalog", "btn-play", "gb-catalog", d.device_id);
    if (canManage("admin")) appendActionButton(actions, "Delete", "btn-danger", "gb-delete-device", d.device_id);
    actionCell.appendChild(actions);

    // Expanded channel rows
    if (expanded && gbDeviceChannels[d.device_id]) {
      var channels = gbDeviceChannels[d.device_id];
      channels.forEach(function(ch) {
        var chStatus = (ch.status || "").toUpperCase() === "ON" ? "badge-on" : "badge-off";
        var channelRow = tbody.insertRow();
        channelRow.className = "channel-row";
        appendTextCell(channelRow, "");
        appendTextCell(channelRow, ch.channel_id, "mono");
        appendTextCell(channelRow, ch.name);
        appendTextCell(channelRow, "");
        var channelStatusCell = channelRow.insertCell();
        var channelBadge = document.createElement("span");
        channelBadge.className = "badge " + chStatus;
        channelBadge.textContent = ch.status || "OFF";
        channelStatusCell.appendChild(channelBadge);
        appendTextCell(channelRow, ch.ptz_type > 0 ? "PTZ-" + ch.ptz_type : "-");
        appendTextCell(channelRow, "");
        var channelActionCell = channelRow.insertCell();
        var channelActions = document.createElement("div");
        channelActions.className = "actions";
        if (canManage("operator")) {
          appendActionButton(channelActions, "Play", "btn-play", "gb-play", ch.channel_id);
          appendActionButton(channelActions, "Stop", "btn-warn", "gb-stop", ch.channel_id);
          var ptz = appendActionButton(channelActions, "PTZ", "", "gb-ptz", ch.channel_id);
          ptz.style.background = "var(--blue)";
          ptz.style.color = "#fff";
          var playback = appendActionButton(channelActions, "Playback", "", "gb-playback", ch.channel_id);
          playback.style.background = "var(--yellow)";
          playback.style.color = "#000";
        }
        channelActionCell.appendChild(channelActions);
      });
    }
  });
}

function renderSessionsTable(sessions) {
  var tbody = document.getElementById("gb-sessions-tbody");
  var empty = document.getElementById("gb-sessions-empty");
  tbody.replaceChildren();
  if (sessions.length === 0) {
    empty.style.display = "block";
    return;
  }
  empty.style.display = "none";
  sessions.forEach(function(s) {
    var stCls = s.state === "streaming" ? "state-publishing" : "state-idle";
    var row = tbody.insertRow();
    appendTextCell(row, String(s.id || "").substring(0, 20), "mono");
    appendTextCell(row, s.channel_id, "mono");
    appendTextCell(row, s.stream_key, "stream-key");
    appendTextCell(row, s.direction);
    var stateCell = row.insertCell();
    var state = document.createElement("span");
    state.className = "state " + stCls;
    state.textContent = s.state;
    stateCell.appendChild(state);
    var actionCell = row.insertCell();
    var actions = document.createElement("div");
    actions.className = "actions";
    appendActionButton(actions, "Preview", "btn-play", "gb-preview", s.stream_key);
    if (canManage("admin")) appendActionButton(actions, "Close", "btn-danger", "gb-close-session", s.id);
    actionCell.appendChild(actions);
  });
}

function renderGBLabSessions(views) {
  views = views || [];
  var tbody = document.getElementById("gb-lab-sessions-tbody");
  var empty = document.getElementById("gb-lab-sessions-empty");
  tbody.replaceChildren();
  empty.style.display = views.length ? "none" : "block";
  views.forEach(function(view) {
    var s = view.session || {};
    if (s.stream_key) streamMedia[s.stream_key] = { video_codec: "H264", audio_codec: "G711A" };
    var row = tbody.insertRow();
    appendTextCell(row, (s.device_id || "-") + " / " + (s.channel_id || "-"), "mono");
    appendTextCell(row, s.mode || "-");
    appendTextCell(row, s.stream_key || "-", "stream-key");
    appendTextCell(row, s.state || "-");
    appendTextCell(row, s.last_error || "-", "lab-error");
    appendTextCell(row, (s.rtp_packets_sent || 0) + " tx / " + (s.rtp_packets_received || 0) + " rx", "mono");
    appendTextCell(row, (s.rtcp_packets_sent || 0) + " tx / " + (s.rtcp_packets_received || 0) + " rx", "mono");
    appendTextCell(row, (s.ps_frames_sent || 0) + " tx / " + (s.ps_frames_received || 0) + " rx", "mono");
    var actionCell = row.insertCell();
    var actions = document.createElement("div");
    actions.className = "actions";
    if (s.stream_key && view.playback && view.playback.available) {
      var preview = appendActionButton(actions, "Preview", "btn-play", "gb-lab-preview", s.stream_key);
      preview.dataset.videoCodec = "H264";
      preview.dataset.audioCodec = "G711A";
      preview.playbackMetadata = view.playback;
    }
    if (canManage("operator") && s.state !== "stopped") appendActionButton(actions, "Stop", "btn-danger", "gb-lab-stop", s.id);
    actionCell.appendChild(actions);
  });
}

function refreshGBLab(signal) {
  return apiFetch("/api/v1/gb28181/lab/sessions", { signal: signal }).then(function(data) {
    renderGBLabSessions((data && data.sessions) || []);
  }).catch(function(err) {
    document.getElementById("gb-lab-error").textContent = managementErrorMessage(err);
  });
}

function startGBLab(event) {
  event.preventDefault();
  if (!canManage("operator")) return;
  var button = document.getElementById("gb-lab-start");
  button.disabled = true;
  document.getElementById("gb-lab-error").textContent = "";
  var request = {
    mode: document.getElementById("gb-lab-mode").value,
    device_id: document.getElementById("gb-lab-device").value.trim(),
    channel_id: document.getElementById("gb-lab-channel").value.trim(),
    stream_key: document.getElementById("gb-lab-stream").value.trim()
  };
  if (!validProtocolLabStreamKey(request.stream_key)) {
    document.getElementById("gb-lab-error").textContent = "Stream key segments must be non-empty and cannot be . or ..";
    button.disabled = !canManage("operator");
    return;
  }
  apiFetch("/api/v1/gb28181/lab/sessions", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(request) })
    .then(function(data) { showToast("GB28181 lab started"); renderGBLabSessions(data ? [data] : []); return refreshGBLab(); })
    .catch(function(err) { document.getElementById("gb-lab-error").textContent = managementErrorMessage(err); })
    .finally(function() { button.disabled = !canManage("operator"); });
}

function stopGBLab(id) {
  showModal("Stop GB28181 lab", "Stop this local fake GB28181 device?", function() {
    return apiFetch("/api/v1/gb28181/lab/sessions/" + encodeURIComponent(id), { method: "DELETE" })
      .then(function() { showToast("GB28181 lab stopped"); return refreshGBLab(); })
      .catch(function(err) { document.getElementById("gb-lab-error").textContent = managementErrorMessage(err); });
  });
}

/* Device actions */
function toggleDevice(deviceID) {
  if (gbExpandedDevices[deviceID]) {
    delete gbExpandedDevices[deviceID];
    refreshGB28181();
    return;
  }
  gbExpandedDevices[deviceID] = true;
  apiFetch("/api/v1/gb28181/channels?device_id=" + encodeURIComponent(deviceID))
    .then(function(channels) {
      gbDeviceChannels[deviceID] = channels || [];
      refreshGB28181();
    });
}

function gbCatalog(deviceID) {
  apiFetch("/api/v1/gb28181/channels/" + encodeURIComponent(deviceID) + "/catalog", { method: "POST" })
    .then(function() { showToast("Catalog query sent to " + deviceID); refreshGB28181(); });
}

function gbDeleteDevice(deviceID) {
  showModal("Delete Device", 'Unregister device "' + deviceID + '" and close all sessions?', function() {
    return apiFetch("/api/v1/gb28181/devices/" + encodeURIComponent(deviceID), { method: "DELETE" })
      .then(function() { delete gbExpandedDevices[deviceID]; delete gbDeviceChannels[deviceID]; refreshActiveView(); })
      .catch(function(err) { showManagementError("gb28181", err); });
  });
}

/* Channel actions */
function gbPlay(channelID) {
  apiFetch("/api/v1/gb28181/channels/" + encodeURIComponent(channelID) + "/play", { method: "POST" })
    .then(function(data) {
      if (data.error) { showToast("Error: " + data.error); return; }
      showToast("Playing: " + (data.stream_key || channelID));
      refreshGB28181();
      if (data.stream_key) {
        setTimeout(function() { openPlayer(data.stream_key); }, 1500);
      }
    })
    .catch(function(err) { showManagementError("gb28181", err); });
}

function gbStop(channelID) {
  showModal("Stop Play", 'Stop live view for "' + channelID + '"?', function() {
    return apiFetch("/api/v1/gb28181/channels/" + encodeURIComponent(channelID) + "/play", { method: "DELETE" })
      .then(function() { refreshActiveView(); })
      .catch(function(err) { showManagementError("gb28181", err); });
  });
}

/* PTZ */
function openPTZ(channelID) {
  ptzTargetChannel = channelID;
  document.getElementById("ptz-channel-label").textContent = channelID;
  document.getElementById("ptz-overlay").classList.add("active");
}

function closePTZ() {
  document.getElementById("ptz-overlay").classList.remove("active");
  ptzTargetChannel = '';
}

function sendPTZ(command) {
  if (!ptzTargetChannel) return;
  var hSpeed = parseInt(document.getElementById("ptz-hspeed").value) || 64;
  var vSpeed = parseInt(document.getElementById("ptz-vspeed").value) || 64;
  return apiFetch("/api/v1/gb28181/channels/" + encodeURIComponent(ptzTargetChannel) + "/ptz", {
    method: "POST",
    headers: {"Content-Type": "application/json"},
    body: JSON.stringify({command: command, h_speed: hSpeed, v_speed: vSpeed})
  }).catch(function(err) { showManagementError("gb28181", err); });
}

// PTZ speed slider display
document.getElementById("ptz-hspeed").addEventListener("input", function() {
  document.getElementById("ptz-hspeed-val").textContent = this.value;
});
document.getElementById("ptz-vspeed").addEventListener("input", function() {
  document.getElementById("ptz-vspeed-val").textContent = this.value;
});

/* Playback */
function openPlayback(channelID) {
  pbTargetChannel = channelID;
  document.getElementById("pb-channel-label").textContent = channelID;
  // Default: last hour
  var now = new Date();
  var oneHourAgo = new Date(now.getTime() - 3600000);
  document.getElementById("pb-start").value = oneHourAgo.toISOString().slice(0, 16);
  document.getElementById("pb-end").value = now.toISOString().slice(0, 16);
  document.getElementById("pb-overlay").classList.add("active");
}

function closePlayback() {
  document.getElementById("pb-overlay").classList.remove("active");
  pbTargetChannel = '';
}

function gbStartPlayback() {
  if (!pbTargetChannel) return;
  var startVal = document.getElementById("pb-start").value;
  var endVal = document.getElementById("pb-end").value;
  if (!startVal || !endVal) { showToast("Please select start and end time"); return; }

  var startTime = new Date(startVal).toISOString();
  var endTime = new Date(endVal).toISOString();

  apiFetch("/api/v1/gb28181/channels/" + encodeURIComponent(pbTargetChannel) + "/playback", {
    method: "POST",
    headers: {"Content-Type": "application/json"},
    body: JSON.stringify({start_time: startTime, end_time: endTime})
  })
    .then(function(data) {
      closePlayback();
      if (data.error) { showToast("Error: " + data.error); return; }
      showToast("Playback started: " + (data.stream_key || ""));
      refreshGB28181();
      if (data.stream_key) {
        setTimeout(function() { openPlayer(data.stream_key); }, 1500);
      }
    })
    .catch(function(err) { showManagementError("gb28181", err); });
}

/* Session actions */
function gbCloseSession(sessionID) {
  showModal("Close Session", 'Force close session "' + sessionID.substring(0, 20) + '..."?', function() {
    return apiFetch("/api/v1/gb28181/sessions/" + encodeURIComponent(sessionID), { method: "DELETE" })
      .then(function() { refreshActiveView(); })
      .catch(function(err) { showManagementError("gb28181", err); });
  });
}

/* Toast */
function showToast(msg) {
  var el = document.getElementById("toast");
  el.textContent = consoleText(msg);
  el.classList.add("show");
  setTimeout(function() { el.classList.remove("show"); }, 3000);
}
