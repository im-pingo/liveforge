/* --- WebRTC Publish --- */
var publishPC = null;
var publishStream = null;
var publishSessionURL = null;
var publishStatsInterval = null;
var publishStatsPrev = null;
var publishStatsVisible = false;
var publishLabSessionID = null;
var publishLabProtocol = null;
var publishRunGeneration = 0;
var publishPreviewGeneration = 0;
var publishEndpointsReady = false;
var publishActiveProtocol = null;
var publishStarting = false;
var publishLabPollTimer = null;
var publishSourceWaitController = null;
var publishCleanupPromise = Promise.resolve();

function isCurrentPublishRun(generation, pc) {
  return generation === publishRunGeneration && (!pc || publishPC === pc);
}

function updatePublishStartButton() {
  var button = document.getElementById("btn-start-publish");
  if (!button) return;
  var protocol = document.getElementById("publish-protocol").value;
  button.disabled = !canManage("operator") || !publishEndpointsReady || publishStarting || !!publishPC;
}

function openPublishModal() {
  if (location.pathname !== "/console/publish") {
    guardConsoleNavigation(function() { location.href = "/console/publish"; });
    return;
  }
  document.getElementById("publish-overlay").classList.add("active");
  setPublishStatus("");

  // Secure context check: getUserMedia requires HTTPS or localhost.
  if (!navigator.mediaDevices || !navigator.mediaDevices.getUserMedia) {
    setPublishStatus("Camera/mic unavailable. Access console via https:// or http://localhost:" + location.port + "/console", "error");
    return;
  }

  updatePublishProtocol();

  // Filter codec options based on browser capabilities.
  if (RTCRtpSender && RTCRtpSender.getCapabilities) {
    var caps = RTCRtpSender.getCapabilities("video");
    var supported = {};
    if (caps && caps.codecs) {
      caps.codecs.forEach(function(c) { supported[c.mimeType.split("/")[1].toUpperCase()] = true; });
    }
    var sel = document.getElementById("publish-video-codec");
    for (var i = sel.options.length - 1; i >= 0; i--) {
      if (!supported[sel.options[i].value]) sel.remove(i);
    }
  }

}

function closePublishModal() {
  stopPublish();
  stopPreview();
  if (location.pathname === "/console/publish") {
    location.href = "/console";
    return;
  }
  document.getElementById("publish-overlay").classList.remove("active");
}

// Close publish modal on overlay click
document.getElementById("publish-overlay").addEventListener("click", function(e) {
  if (e.target === this) closePublishModal();
});

function setPublishStatus(msg, cls) {
  var el = document.getElementById("publish-status");
  el.textContent = msg;
  el.className = "publish-status" + (cls ? " " + cls : "");
}

function populateDeviceSelects(devices) {
  var videoSel = document.getElementById("publish-video-device");
  var audioSel = document.getElementById("publish-audio-device");

  // Remember current selections to preserve across re-enumeration.
  var prevVideo = videoSel.value;
  var prevAudio = audioSel.value;
  videoSel.innerHTML = "";
  audioSel.innerHTML = "";

  var videoIdx = 1, audioIdx = 1;
  devices.forEach(function(d) {
    var opt = document.createElement("option");
    opt.value = d.deviceId;
    if (d.kind === "videoinput") {
      opt.textContent = d.label || ("Camera " + videoIdx++);
      videoSel.appendChild(opt);
    } else if (d.kind === "audioinput") {
      opt.textContent = d.label || ("Microphone " + audioIdx++);
      audioSel.appendChild(opt);
    }
  });

  // Restore previous selections if still available.
  if (prevVideo) videoSel.value = prevVideo;
  if (prevAudio) audioSel.value = prevAudio;
}

function startPreview() {
  stopPreview();
  if (!navigator.mediaDevices || !navigator.mediaDevices.getUserMedia) return;
  var previewGeneration = ++publishPreviewGeneration;

  var videoId = document.getElementById("publish-video-device").value;
  var audioId = document.getElementById("publish-audio-device").value;
  var constraints = {
    video: videoId ? { deviceId: { exact: videoId } } : true,
    audio: audioId ? { deviceId: { exact: audioId } } : true
  };
  return navigator.mediaDevices.getUserMedia(constraints).then(function(stream) {
    if (previewGeneration !== publishPreviewGeneration || publishPC) {
      stream.getTracks().forEach(function(track) { track.stop(); });
      return null;
    }
    publishStream = stream;
    document.getElementById("publish-preview").srcObject = stream;
    var previewState = document.getElementById("preview-state");
    if (previewState) previewState.textContent = "LOCAL PREVIEW";
    // Re-enumerate to get labels after permission grant.
    return navigator.mediaDevices.enumerateDevices();
  }).then(function(devices) {
    if (devices && previewGeneration === publishPreviewGeneration) populateDeviceSelects(devices);
  }).catch(function(e) {
    if (previewGeneration === publishPreviewGeneration) setPublishStatus("Camera/mic error: " + e.message, "error");
  });
}

function stopPreview() {
  publishPreviewGeneration++;
  if (publishStream) {
    publishStream.getTracks().forEach(function(t) { t.stop(); });
    publishStream = null;
  }
  var preview = document.getElementById("publish-preview");
  if (preview) {
    preview.srcObject = null;
  }
  var previewState = document.getElementById("preview-state");
  if (previewState && !publishPC && !publishLabSessionID) previewState.textContent = "READY TO PUBLISH";
}

// Re-acquire preview when device selection changes
document.getElementById("publish-video-device").addEventListener("change", function() {
  if (!publishPC) startPreview();
});
document.getElementById("publish-audio-device").addEventListener("change", function() {
  if (!publishPC) startPreview();
});
document.getElementById("publish-protocol").addEventListener("change", updatePublishProtocol);
document.querySelectorAll(".protocol-switch [data-protocol]").forEach(function(button) {
  button.addEventListener("click", function() {
    if (publishPC || publishLabSessionID || publishStarting) return;
    document.getElementById("publish-protocol").value = button.getAttribute("data-protocol");
    updatePublishProtocol();
  });
});
document.getElementById("publish-stream-key").addEventListener("input", function() { updatePublishProtocol(true); });
document.getElementById("publish-sip-stream").addEventListener("input", function() { updatePublishProtocol(true); });
document.getElementById("publish-gb-stream").addEventListener("input", function() { updatePublishProtocol(true); });

function publishLabPath(protocol) {
  return protocol === "sip" ? "/api/v1/sipgateway/lab/sessions" : "/api/v1/gb28181/lab/sessions";
}

function publishRequest(protocol) {
  var request = protocol === "sip"
    ? {mode:"publish", device_id:document.getElementById("publish-sip-device").value.trim(), stream_key:document.getElementById("publish-sip-stream").value.trim(), codec:document.getElementById("publish-sip-codec").value}
    : {mode:"publish", device_id:document.getElementById("publish-gb-device").value.trim(), channel_id:document.getElementById("publish-gb-channel").value.trim(), stream_key:document.getElementById("publish-gb-stream").value.trim()};
  if (!validProtocolLabStreamKey(request.stream_key)) throw new Error("Stream key segments must be non-empty and cannot be . or ..");
  if (!request.device_id || (protocol === "gb28181" && !request.channel_id)) throw new Error("Enter the device and channel identifiers");
  return request;
}

function publishCodecPreferences(kind, mime, strict) {
  var caps = typeof RTCRtpSender !== "undefined" && RTCRtpSender.getCapabilities ? RTCRtpSender.getCapabilities(kind) : null;
  var codecs = caps && caps.codecs ? caps.codecs : [];
  var preferred = codecs.filter(function(codec) { return codec.mimeType.toLowerCase() === mime.toLowerCase(); });
  if (strict && !preferred.length) throw new Error("This browser cannot publish " + mime.split("/")[1] + ". Use a browser with H.264 and G.711 camera publishing support.");
  return strict ? preferred : preferred.concat(codecs.filter(function(codec) { return codec.mimeType.toLowerCase() !== mime.toLowerCase(); }));
}

function setPublishControlsBusy(busy) {
  document.querySelectorAll("#publish-workspace input, #publish-workspace select, #publish-workspace .protocol-switch button").forEach(function(control) { control.disabled = busy; });
}

function waitForPublishICE(pc, run) {
  return new Promise(function(resolve, reject) {
    var deadline = Date.now() + 10000;
    var timer = setInterval(check, 100);
    function finish(error) {
      clearInterval(timer);
      pc.removeEventListener("iceconnectionstatechange", check);
      if (error) reject(error); else resolve();
    }
    function check() {
      if (!isCurrentPublishRun(run, pc)) return finish(new Error("publish run cancelled"));
      var state = pc.iceConnectionState;
      if (state === "connected" || state === "completed") return finish();
      if (state === "failed" || state === "closed" || Date.now() >= deadline) finish(new Error("Camera connection failed or timed out"));
    }
    pc.addEventListener("iceconnectionstatechange", check);
    check();
  });
}

async function waitForPublishSource(streamKey, pc, run) {
  var controller = new AbortController();
  publishSourceWaitController = controller;
  var deadline = setTimeout(function() { controller.abort(); }, 10000);
  try {
    while (isCurrentPublishRun(run, pc) && !controller.signal.aborted) {
      try {
        var source = await apiFetch("/api/v1/streams/" + encodedStreamPath(streamKey), {signal:controller.signal});
        if (!isCurrentPublishRun(run, pc)) return;
        if (source && source.publisher && source.stats && source.stats.video_frames > 0 && source.stats.audio_frames > 0) return;
      } catch (error) {
        if (controller.signal.aborted) break;
        if (error.status !== 404) throw error;
      }
      await new Promise(function(resolve) { setTimeout(resolve, 100); });
    }
    throw new Error("Camera/microphone media did not arrive before the startup deadline");
  } finally {
    clearTimeout(deadline);
    if (publishSourceWaitController === controller) publishSourceWaitController = null;
  }
}

async function startPublish() {
  if (!canManage("operator") || publishStarting || publishPC) return;
  if (!publishEndpointsReady) { setPublishStatus("WebRTC endpoint is still loading", "error"); return; }
  var protocol = document.getElementById("publish-protocol").value;
  var request, streamKey, videoCodecs, audioCodecs;
  try {
    request = protocol === "whip" ? null : publishRequest(protocol);
    streamKey = request ? "_console/" + crypto.randomUUID() : document.getElementById("publish-stream-key").value.trim();
    // Protocol targets were validated above. WHIP retains its existing
    // non-empty key contract, including non-ASCII names escaped per segment.
    if (!streamKey) throw new Error("Enter a stream key");
    var video = request ? "H264" : document.getElementById("publish-video-codec").value;
    videoCodecs = publishCodecPreferences("video", "video/" + video, !!request);
    if (request) audioCodecs = publishCodecPreferences("audio", "audio/" + (request.codec || "PCMA"), true);
  } catch (error) { setPublishStatus(error.message, "error"); return; }

  var run = ++publishRunGeneration;
  publishActiveProtocol = protocol;
  publishStarting = true;
  setPublishControlsBusy(true);
  updatePublishStartButton();
  document.getElementById("btn-stop-publish").style.display = "inline-block";
  setPublishStatus("Connecting camera and microphone...");
  var pc;
  try {
    // Reusing a destination/device must wait until its previous protocol
    // session has released the target publisher and signaling identity.
    await publishCleanupPromise;
    if (!isCurrentPublishRun(run)) return;
    if (!publishStream) await startPreview();
    if (!isCurrentPublishRun(run)) return;
    if (!publishStream || !publishStream.getVideoTracks().length || !publishStream.getAudioTracks().length || publishStream.getTracks().some(function(track) { return track.readyState !== "live"; })) throw new Error("No camera/microphone available");
    pc = new RTCPeerConnection({iceServers:[{urls:"stun:stun.l.google.com:19302"}]});
    publishPC = pc;
    publishStream.getTracks().forEach(function(track) {
      var sender = pc.addTrack(track, publishStream);
      var transceiver = pc.getTransceivers().find(function(item) { return item.sender === sender; });
      var preferences = track.kind === "video" ? videoCodecs : audioCodecs;
      if (request && (!transceiver || !transceiver.setCodecPreferences)) throw new Error("This browser cannot select the required H.264/G.711 codecs");
      if (preferences && preferences.length && transceiver && transceiver.setCodecPreferences) transceiver.setCodecPreferences(preferences);
      track.addEventListener("ended", function() {
        if (!isCurrentPublishRun(run, pc)) return;
        cleanupPublishPC();
        setPublishStatus("Camera or microphone was disconnected", "error");
      }, {once:true});
    });
    pc.oniceconnectionstatechange = function() {
      if (!isCurrentPublishRun(run, pc)) return;
      if (["failed", "disconnected", "closed"].indexOf(pc.iceConnectionState) !== -1) {
        var message = "Disconnected: " + pc.iceConnectionState;
        cleanupPublishPC();
        setPublishStatus(message, "error");
      }
    };
    var offer = await pc.createOffer();
    if (!isCurrentPublishRun(run, pc)) return;
    await pc.setLocalDescription(offer);
    await new Promise(function(resolve) {
      if (pc.iceGatheringState === "complete") { resolve(); return; }
      var timer = setTimeout(done, 500);
      function done() { clearTimeout(timer); pc.removeEventListener("icecandidate", candidate); resolve(); }
      function candidate(event) { if (!event.candidate || event.candidate.type === "host") done(); }
      pc.addEventListener("icecandidate", candidate);
    });
    if (!isCurrentPublishRun(run, pc)) return;
    var scheme = (endpointSchemes.webrtc || location.protocol.replace(":", "")) + ":";
    var url = endpointURL(scheme, endpoints.webrtc, "/webrtc/whip/" + encodedStreamPath(streamKey));
    var response = await fetch(url, {method:"POST", headers:{"Content-Type":"application/sdp"}, body:pc.localDescription.sdp});
    var locationHeader = response.headers.get("Location");
    var sessionURL = locationHeader ? new URL(locationHeader, url).href : "";
    if (sessionURL && (new URL(sessionURL).origin !== new URL(url).origin || new URL(sessionURL).pathname.indexOf("/webrtc/session/") !== 0)) throw new Error("Unexpected camera session URL");
    // Remember a successful resource before reading its body: body read failure
    // must still delete a resource created by the server.
    if (response.status === 201 && sessionURL) {
      if (!isCurrentPublishRun(run, pc)) {
        fetch(sessionURL, {method:"DELETE", keepalive:true}).catch(function(){});
        return;
      }
      publishSessionURL = sessionURL;
    }
    var answer = await response.text();
    if (!isCurrentPublishRun(run, pc)) return;
    if (response.status !== 201 || !sessionURL) throw new Error("Camera connection " + response.status + ": " + answer);
    await pc.setRemoteDescription({type:"answer", sdp:answer});
    await waitForPublishICE(pc, run);
    if (!isCurrentPublishRun(run, pc)) return;
    if (request) {
      setPublishStatus("Waiting for camera and microphone media...");
      await waitForPublishSource(streamKey, pc, run);
      if (!isCurrentPublishRun(run, pc)) return;
      setPublishStatus("Starting " + protocol.toUpperCase() + " camera session...");
      await startLabPublish(protocol, streamKey, run, pc, request);
    }
    if (!isCurrentPublishRun(run, pc)) return;
    setPublishStatus("Publishing" + (request ? " (" + protocol.toUpperCase() + ")" : ""), "ok");
    document.getElementById("connection-dot").classList.add("ready");
    document.getElementById("connection-state").textContent = "Publishing live";
    document.getElementById("connection-note").textContent = "Camera and microphone are live";
    document.getElementById("preview-state").textContent = "LIVE / " + protocol.toUpperCase();
    document.getElementById("btn-start-publish").style.display = "none";
    document.getElementById("btn-publish-stats").style.display = "inline-block";
    startPublishStatsOverlay(pc);
  } catch (error) {
    if (isCurrentPublishRun(run)) {
      cleanupPublishPC();
      setPublishStatus(error.message, "error");
    }
  } finally {
    if (isCurrentPublishRun(run)) { publishStarting = false; updatePublishStartButton(); }
  }
}

function stopPublish() {
  cleanupPublishPC();
  setPublishStatus("");
}

async function startLabPublish(protocol, sourceStreamKey, run, pc, request) {
  if (run !== publishRunGeneration || !isCurrentPublishRun(run, pc)) return;
  request.source_stream_key = sourceStreamKey;
  var path = publishLabPath(protocol);
  var data = await apiFetch(path, {method:"POST", headers:{"Content-Type":"application/json"}, body:JSON.stringify(request)});
  var session = data && data.session ? data.session : data;
  if (!isCurrentPublishRun(run, pc)) {
    if (session && session.id) fetch(path + "/" + encodeURIComponent(session.id), {method:"DELETE", credentials:"same-origin", keepalive:true}).catch(function(){});
    return;
  }
  if (!session || !session.id) throw new Error("Protocol session was not created");
  publishLabSessionID = session.id;
  publishLabProtocol = protocol;
  if (session.state !== "active") throw new Error(session.last_error || "Protocol session is not active");
  monitorPublishLab(run, pc, path, session.id);
}

function monitorPublishLab(run, pc, path, id) {
  publishLabPollTimer = setTimeout(function() {
    if (!isCurrentPublishRun(run, pc)) return;
    apiFetch(path).then(function(data) {
      if (!isCurrentPublishRun(run, pc)) return;
      var entry = (data.sessions || []).find(function(item) { return item.session && item.session.id === id; });
      var session = entry && entry.session;
      if (!session || session.state !== "active") throw new Error(session && session.last_error || "Protocol session ended");
      monitorPublishLab(run, pc, path, id);
    }).catch(function(error) {
      if (!isCurrentPublishRun(run, pc)) return;
      cleanupPublishPC();
      setPublishStatus(error.message, "error");
    });
  }, 1000);
}

function cleanupPublishPC() {
  publishRunGeneration++;
  publishStarting = false;
  if (publishSourceWaitController) publishSourceWaitController.abort();
  publishSourceWaitController = null;
  if (publishLabPollTimer) clearTimeout(publishLabPollTimer);
  publishLabPollTimer = null;
  var pc = publishPC;
  var sessionURL = publishSessionURL;
  var labID = publishLabSessionID;
  var protocol = publishLabProtocol;
  publishPC = null;
  publishSessionURL = null;
  publishLabSessionID = null;
  publishLabProtocol = null;
  publishActiveProtocol = null;
  if (pc || sessionURL || labID) {
    var releaseCaptureConnection = function() {
      if (pc) pc.close();
      if (sessionURL) fetch(sessionURL, {method:"DELETE", keepalive:true}).catch(function(){});
    };
    if (labID && protocol) {
      // Stop the protocol before retiring its source, otherwise normal Stop
      // races source-loss handling and is recorded as a failed session.
      var controller = new AbortController();
      var timer = setTimeout(function() { controller.abort(); }, 5000);
      publishCleanupPromise = fetch(publishLabPath(protocol) + "/" + encodeURIComponent(labID), {method:"DELETE", credentials:"same-origin", keepalive:true, signal:controller.signal})
        .catch(function(){})
        .then(function() { clearTimeout(timer); releaseCaptureConnection(); });
    } else {
      releaseCaptureConnection();
    }
  }
  stopPreview();
  stopPublishStatsOverlay();
  publishStatsVisible = false;
  setPublishControlsBusy(false);
  document.getElementById("connection-dot").classList.remove("ready");
  document.getElementById("connection-state").textContent = "Ready when you are";
  document.getElementById("connection-note").textContent = "Camera and microphone stay local until you start publishing";
  document.getElementById("preview-state").textContent = "READY TO PUBLISH";
  document.getElementById("btn-start-publish").style.display = "inline-block";
  document.getElementById("btn-stop-publish").style.display = "none";
  document.getElementById("btn-publish-stats").style.display = "none";
  updatePublishStartButton();
}

window.addEventListener("pagehide", function() { stopPublish(); });

function updatePublishProtocol(skipPreview) {
  var protocol = document.getElementById("publish-protocol").value;
  document.querySelectorAll(".protocol-switch [data-protocol]").forEach(function(button) {
    var active = button.getAttribute("data-protocol") === protocol;
    button.classList.toggle("active", active);
    button.setAttribute("aria-selected", active ? "true" : "false");
  });
  var activeProtocol = publishActiveProtocol;
  if (activeProtocol && activeProtocol !== protocol) stopPublish();
  document.getElementById("publish-whip-config").hidden = protocol !== "whip";
  document.getElementById("publish-sip-config").hidden = protocol !== "sip";
  document.getElementById("publish-gb-config").hidden = protocol !== "gb28181";
  document.getElementById("publish-whip-config").classList.toggle("active", protocol === "whip");
  document.getElementById("publish-sip-config").classList.toggle("active", protocol === "sip");
  document.getElementById("publish-gb-config").classList.toggle("active", protocol === "gb28181");
  var preview = document.querySelector(".publish-preview-wrap");
  if (preview) preview.style.display = "";
  if (!publishPC && !skipPreview) startPreview();
  var isWhip = protocol === "whip";
  document.getElementById("connection-note").textContent = "Camera and microphone stay local until you start publishing";
  document.getElementById("preflight-source-label").textContent = "Camera + microphone";
  document.getElementById("preflight-transport-label").textContent = isWhip ? "WebRTC endpoint" : protocol === "sip" ? "SIP gateway" : "GB28181 gateway";
  document.getElementById("route-hint").textContent = isWhip ? "WHIP /webrtc/whip/" + document.getElementById("publish-stream-key").value.trim() : protocol === "sip" ? "SIP / " + document.getElementById("publish-sip-stream").value.trim() : "GB28181 / " + document.getElementById("publish-gb-stream").value.trim();
  setPublishStatus("");
  updatePublishStartButton();
}

/* --- Publish Stats Overlay --- */
function togglePublishStats() {
  publishStatsVisible = !publishStatsVisible;
  var btn = document.getElementById("btn-publish-stats");
  var overlay = document.getElementById("publish-stats-overlay");
  if (btn) btn.classList.toggle("active", publishStatsVisible);
  if (overlay) overlay.style.display = publishStatsVisible ? "block" : "none";
}

function startPublishStatsOverlay(pc) {
  stopPublishStatsOverlay();
  publishStatsPrev = null;
  var overlay = document.getElementById("publish-stats-overlay");
  if (overlay) overlay.style.display = publishStatsVisible ? "block" : "none";

  publishStatsInterval = setInterval(async function() {
    if (!pc || pc.connectionState === "closed" || pc.connectionState === "failed") {
      stopPublishStatsOverlay();
      return;
    }
    try {
      var report = await pc.getStats();
      var vOut = null, aOut = null, vRemote = null;
      report.forEach(function(s) {
        if (s.type === "outbound-rtp" && s.kind === "video") vOut = s;
        if (s.type === "outbound-rtp" && s.kind === "audio") aOut = s;
        if (s.type === "remote-inbound-rtp" && s.kind === "video") vRemote = s;
      });
      renderPublishStats(vOut, aOut, vRemote);
    } catch(e) {}
  }, 1000);
}

function stopPublishStatsOverlay() {
  if (publishStatsInterval) { clearInterval(publishStatsInterval); publishStatsInterval = null; }
  publishStatsPrev = null;
  var overlay = document.getElementById("publish-stats-overlay");
  if (overlay) {
    overlay.style.display = "none";
    overlay.innerHTML = '<div class="stat-row"><span class="stat-item"><span class="stat-label-s">Waiting for stats...</span></span></div>';
  }
}

function renderPublishStats(vOut, aOut, vRemote) {
  var overlay = document.getElementById("publish-stats-overlay");
  if (!overlay) return;
  if (!vOut) {
    overlay.innerHTML = '<div class="stat-row"><span class="stat-item"><span class="stat-label-s">No video stats yet</span></span></div>';
    publishStatsPrev = null;
    return;
  }

  var pv = publishStatsPrev && publishStatsPrev.v;
  var dEncoded = pv ? vOut.framesEncoded - pv.framesEncoded : "-";
  var dSent    = pv ? vOut.framesSent - pv.framesSent : "-";
  var dBytes   = pv ? vOut.bytesSent - pv.bytesSent : 0;
  var bitrate  = pv ? ((dBytes * 8) / 1e6).toFixed(2) + " Mbps" : "-";
  var dKeyFr   = pv ? vOut.keyFramesEncoded - pv.keyFramesEncoded : "-";
  var qLimit   = vOut.qualityLimitationReason || "-";
  var rtt      = vRemote && vRemote.roundTripTime != null ? (vRemote.roundTripTime * 1000).toFixed(0) + "ms" : "-";
  var jitter   = vRemote && vRemote.jitter != null ? (vRemote.jitter * 1000).toFixed(0) + "ms" : "-";
  var pktLost  = vRemote ? vRemote.packetsLost : "-";

  var aBytesD = 0;
  var pa = publishStatsPrev && publishStatsPrev.a;
  if (pa && aOut) aBytesD = aOut.bytesSent - pa.bytesSent;
  var aBitrate = pa && aOut ? ((aBytesD * 8) / 1e3).toFixed(0) + " kbps" : "-";

  var rttCls = rtt !== "-" && parseInt(rtt) > 100 ? "warn" : "";
  var qCls = qLimit !== "none" && qLimit !== "-" ? "warn" : "";

  var L = '<span class="stat-label-s">';
  var E = '</span>';

  overlay.innerHTML =
    '<div class="stat-row">' +
    '<span class="stat-item">' + L + "FPS" + E + val(dEncoded) + "</span>" +
    '<span class="stat-item">' + L + "Sent" + E + val(dSent) + "</span>" +
    '<span class="stat-item">' + L + "KF" + E + val(dKeyFr) + "</span>" +
    '<span class="stat-item">' + L + "Bitrate" + E + val(bitrate) + "</span>" +
    '<span class="stat-item">' + L + "RTT" + E + val(rtt, rttCls) + "</span>" +
    '<span class="stat-item">' + L + "Jitter" + E + val(jitter) + "</span>" +
    '<span class="stat-item">' + L + "Lost" + E + val(pktLost) + "</span>" +
    '<span class="stat-item">' + L + "Quality" + E + val(qLimit, qCls) + "</span>" +
    '<span class="stat-item">' + L + "Audio" + E + val(aBitrate) + "</span>" +
    "</div>" +
    '<div class="stat-row" style="margin-top:2px;color:#666;font-size:10px">' +
    "Total: enc=" + vOut.framesEncoded + " sent=" + vOut.framesSent +
    " kf=" + vOut.keyFramesEncoded + " bytes=" + vOut.bytesSent +
    "</div>";

  publishStatsPrev = { v: vOut, a: aOut };
}

/* --- Show publish button when webrtc endpoint available --- */
function updatePublishButton() {
  var btn = document.getElementById("btn-publish");
  if (btn) btn.style.display = endpoints.webrtc ? "inline-block" : "none";
}

/* === Management Console === */
