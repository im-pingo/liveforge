function kickStream(key) {
  showModal("Kick Publisher", 'Disconnect the publisher from "' + key + '"?', function() {
    return apiFetch("/api/v1/streams/" + encodeURIComponent(key) + "/kick", { method: "POST" })
      .then(function() { refreshActiveView(); })
      .catch(function(err) { showManagementError("streams", err); });
  });
}
function deleteStream(key) {
  showModal("Delete Stream", 'Remove stream "' + key + '" entirely?', function() {
    return apiFetch("/api/v1/streams/" + encodeURIComponent(key), { method: "DELETE" })
      .then(function() { refreshActiveView(); })
      .catch(function(err) { showManagementError("streams", err); });
  });
}

/* --- Build port from listen address --- */
function portFromListen(listen) {
  if (!listen) return "";
  var i = listen.lastIndexOf(":");
  return i >= 0 ? listen.substring(i + 1) : listen;
}

function endpointAuthority(listen) {
  var value = String(listen || "").trim();
  if (!value) return location.host;
  if (value.charAt(0) === ":") return location.hostname + value;

  // Normalize wildcard listeners to the host that served the console. A
  // wildcard is a bind address, not a browser-routable hostname.
  if (value.charAt(0) === "[") {
    var close = value.indexOf("]");
    if (close > 0) {
      var host = value.substring(1, close);
      var suffix = value.substring(close + 1);
      if (host === "::" || host === "0:0:0:0:0:0:0:0" || host === "0.0.0.0" || host === "*") {
        return location.hostname + suffix;
      }
      return value;
    }
  }

  var separator = value.lastIndexOf(":");
  if (separator > 0) {
    var rawHost = value.substring(0, separator);
    if (rawHost.indexOf(":") >= 0) return "[" + rawHost + "]:" + value.substring(separator + 1);
    if (rawHost === "0.0.0.0" || rawHost === "::" || rawHost === "*") {
      return location.hostname + value.substring(separator);
    }
  }
  return value;
}

function mediaURL(pathname) {
  var scheme = location.protocol === "https:" ? "https:" : "http:";
  return scheme + "//" + endpointAuthority(endpoints.http) + pathname;
}

function websocketMediaURL(pathname) {
  var scheme = location.protocol === "https:" ? "wss:" : "ws:";
  return scheme + "//" + endpointAuthority(endpoints.http) + pathname;
}

function endpointURL(protocol, listen, pathname) {
  return protocol + "//" + endpointAuthority(listen) + pathname;
}

function hostFromListen(listen) {
  if (!listen) return location.hostname;
  var value = String(listen);
  if (value.charAt(0) === "[") {
    var close = value.indexOf("]");
    if (close > 0) value = value.substring(1, close);
  } else {
    var separator = value.lastIndexOf(":");
    if (separator > 0) value = value.substring(0, separator);
  }
  if (!value || value === "0.0.0.0" || value === "::" || value === "[::]" || value === "*") return location.hostname;
  return value;
}

/* --- WebRTC Stats Overlay --- */
function toggleStatsOverlay() {
  statsVisible = !statsVisible;
  var btn = document.getElementById('btn-stats');
  var overlay = document.getElementById('webrtc-stats-overlay');
  if (btn) btn.classList.toggle('active', statsVisible);
  if (overlay) overlay.style.display = statsVisible ? 'block' : 'none';
}

function startStatsOverlay(pc) {
  stopStatsOverlay();
  // Ensure overlay element exists inside video wrap
  var wrap = document.querySelector('.player-video-wrap');
  if (!wrap) return;
  var overlay = document.getElementById('webrtc-stats-overlay');
  if (!overlay) {
    overlay = document.createElement('div');
    overlay.id = 'webrtc-stats-overlay';
    overlay.className = 'webrtc-stats-overlay';
    overlay.innerHTML = '<div class="stat-row"><span class="stat-item"><span class="stat-label-s">Waiting for stats...</span></span></div>';
    wrap.appendChild(overlay);
  }
  overlay.style.display = statsVisible ? 'block' : 'none';

  statsPrev = null;
  statsInterval = setInterval(async function() {
    if (pc !== currentPC || !pc || pc.connectionState === 'closed' || pc.connectionState === 'failed') {
      stopStatsOverlay();
      return;
    }
    try {
      var report = await pc.getStats();
      var v = null, a = null;
      report.forEach(function(s) {
        if (s.type !== 'inbound-rtp') return;
        if (s.kind === 'video') v = s;
        if (s.kind === 'audio') a = s;
      });
      if (pc === currentPC) renderStatsOverlay(v, a);
    } catch(e) {}
  }, 1000);
}

function stopStatsOverlay() {
  if (statsInterval) { clearInterval(statsInterval); statsInterval = null; }
  statsPrev = null;
  var overlay = document.getElementById('webrtc-stats-overlay');
  if (overlay) overlay.remove();
}

function val(v, cls) {
  return '<span class="stat-val' + (cls ? ' ' + cls : '') + '">' + v + '</span>';
}

function renderStatsOverlay(v, a) {
  var overlay = document.getElementById('webrtc-stats-overlay');
  if (!overlay) return;

  if (!v) {
    if (a) {
      var audioPackets = a.packetsReceived || 0;
      var audioLost = a.packetsLost || 0;
      var audioJitter = a.jitter ? (a.jitter * 1000).toFixed(0) + 'ms' : '-';
      overlay.innerHTML = '<div class="stat-row">' +
        '<span class="stat-item"><span class="stat-label-s">A.Pkts</span>' + val(audioPackets) + '</span>' +
        '<span class="stat-item"><span class="stat-label-s">A.Lost</span>' + val(audioLost, audioLost > 0 ? 'bad' : 'good') + '</span>' +
        '<span class="stat-item"><span class="stat-label-s">A.Jit</span>' + val(audioJitter) + '</span>' +
        '</div>';
    } else {
      overlay.innerHTML = '<div class="stat-row"><span class="stat-item"><span class="stat-label-s">Waiting for media stats...</span></span></div>';
    }
    statsPrev = { v: v, a: a };
    return;
  }

  var pv = statsPrev && statsPrev.v;
  var pa = statsPrev && statsPrev.a;

  // Compute deltas
  var dDecoded   = pv ? v.framesDecoded - pv.framesDecoded : '-';
  var dDropped   = pv ? v.framesDropped - pv.framesDropped : '-';
  var dKeyFr     = pv ? v.keyFramesDecoded - pv.keyFramesDecoded : '-';
  var dPktLost   = pv ? v.packetsLost - pv.packetsLost : '-';
  var dBytes     = pv ? v.bytesReceived - pv.bytesReceived : 0;
  var bitrate    = pv ? ((dBytes * 8) / 1e6).toFixed(2) + ' Mbps' : '-';
  var dPLI       = pv ? v.pliCount - pv.pliCount : '-';
  var dNACK      = pv ? (v.nackCount || 0) - (pv.nackCount || 0) : '-';
  var dFreeze    = pv ? (v.freezeCount || 0) - (pv.freezeCount || 0) : '-';
  var jitterMs   = v.jitter ? (v.jitter * 1000).toFixed(0) + 'ms' : '-';

  // Jitter buffer (avg over last interval)
  var jitBufMs = '-';
  if (pv) {
    var dJBDelay = v.jitterBufferDelay - pv.jitterBufferDelay;
    var dJBCount = v.jitterBufferEmittedCount - pv.jitterBufferEmittedCount;
    if (dJBCount > 0) jitBufMs = ((dJBDelay / dJBCount) * 1000).toFixed(0) + 'ms';
  }

  var aLost = (a && pa) ? a.packetsLost - pa.packetsLost : '-';
  var aPackets = (a && pa) ? a.packetsReceived - pa.packetsReceived : '-';
  var aJit  = a ? (a.jitter * 1000).toFixed(0) + 'ms' : '-';

  // Color classes
  var dropCls   = (dDropped > 0)  ? 'bad'  : 'good';
  var lostCls   = (dPktLost > 0)  ? 'bad'  : 'good';
  var jbCls     = (parseInt(jitBufMs) > 200) ? 'warn' : '';
  var pliCls    = (dPLI > 0) ? 'warn' : '';
  var freezeCls = (dFreeze > 0) ? 'bad' : '';

  var L = '<span class="stat-label-s">';
  var E = '</span>';

  overlay.innerHTML =
    '<div class="stat-row">' +
    '<span class="stat-item">' + L + 'FPS' + E + val(dDecoded) + '</span>' +
    '<span class="stat-item">' + L + 'Drop' + E + val(dDropped, dropCls) + '</span>' +
    '<span class="stat-item">' + L + 'KF' + E + val(dKeyFr) + '</span>' +
    '<span class="stat-item">' + L + 'Lost' + E + val(dPktLost, lostCls) + '</span>' +
    '<span class="stat-item">' + L + 'Bitrate' + E + val(bitrate) + '</span>' +
    '<span class="stat-item">' + L + 'JitBuf' + E + val(jitBufMs, jbCls) + '</span>' +
    '<span class="stat-item">' + L + 'Freeze' + E + val(dFreeze, freezeCls) + '</span>' +
    '<span class="stat-item">' + L + 'PLI' + E + val(dPLI, pliCls) + '</span>' +
    '<span class="stat-item">' + L + 'NACK' + E + val(dNACK) + '</span>' +
    '<span class="stat-item">' + L + 'Jit' + E + val(jitterMs) + '</span>' +
    '<span class="stat-item">' + L + 'A.Pkts' + E + val(aPackets) + '</span>' +
    '<span class="stat-item">' + L + 'A.Lost' + E + val(aLost) + '</span>' +
    '<span class="stat-item">' + L + 'A.Jit' + E + val(aJit) + '</span>' +
    '</div>' +
    '<div class="stat-row" style="margin-top:2px;color:#666;font-size:10px">' +
    'Total: ' +
    'dec=' + v.framesDecoded + ' ' +
    'drop=' + v.framesDropped + ' ' +
    'kf=' + v.keyFramesDecoded + ' ' +
    'lost=' + v.packetsLost + '/' + v.packetsReceived + ' ' +
    'audio=' + (a ? a.packetsLost + '/' + a.packetsReceived : '-') + ' ' +
    'freeze=' + (v.freezeCount || 0) + ' (' + ((v.totalFreezesDuration || 0) * 1000).toFixed(0) + 'ms) ' +
    'pli=' + (v.pliCount || 0) +
    '</div>';

  statsPrev = { v: v, a: a };
}

/* --- Player logic --- */
function destroyCurrentPlayer() {
  playerGeneration++;
  if (whepStatusPoll) { clearTimeout(whepStatusPoll); whepStatusPoll = null; }
  stopStatsOverlay();
  if (currentVideoEventCleanup) {
    try { currentVideoEventCleanup(); } catch(e) {}
    currentVideoEventCleanup = null;
  }
  if (currentMSEAbort) {
    try { currentMSEAbort.abort(); } catch(e) {}
    currentMSEAbort = null;
  }
  if (currentMSESourceBuffer && currentMSEMediaSource) {
    try {
      if (currentMSESourceBuffer.updating) currentMSESourceBuffer.abort();
    } catch(e) {}
    try {
      currentMSEMediaSource.removeSourceBuffer(currentMSESourceBuffer);
    } catch(e) {}
  }
  currentMSESourceBuffer = null;
  currentMSEMediaSource = null;
  if (currentMSEObjectURL) {
    try { URL.revokeObjectURL(currentMSEObjectURL); } catch(e) {}
    currentMSEObjectURL = null;
  }
  if (currentHls) {
    try { currentHls.destroy(); } catch(e) {}
    currentHls = null;
  }
  if (currentDash) {
    try { currentDash.reset(); } catch(e) {}
    currentDash = null;
  }
  if (currentPlayer) {
    try { currentPlayer.destroy(); } catch(e) {}
    currentPlayer = null;
  }
  if (currentPC) {
    try { currentPC.close(); } catch(e) {}
    currentPC = null;
  }
  var video = document.getElementById("player-video");
  video.pause();
  video.muted = false;
  video.srcObject = null;
  video.src = "";
  video.load();
  video.autoplay = true;
  var audio = document.getElementById("player-audio");
  audio.pause();
  audio.muted = false;
  audio.srcObject = null;
  audio.src = "";
  audio.load();
  audio.autoplay = true;
  setPlayerAudioControl(null, false);
  document.getElementById("player-audio-notice").style.display = "none";
}

function isCurrentPlayer(generation) {
  return generation === playerGeneration;
}

function formatErr(e) {
  if (!e) return "unknown error";
  if (typeof e === "string") return e;
  if (e.message) return e.message;
  if (e.msg) return e.msg;
  try { return JSON.stringify(e); } catch(ex) { return String(e); }
}

function setPlayerStatus(msg, cls) {
  var el = document.getElementById("player-status");
  el.textContent = msg;
  el.className = "player-status" + (cls ? " " + cls : "");
}

function setPlayerURL(url) {
  var el = document.getElementById("player-url");
  el.textContent = url;
  el.style.display = url ? "block" : "none";
}

function setPlayerAudioControl(mediaElement, visible) {
  var button = document.getElementById("btn-player-audio");
  if (!button) return;
  if (!visible || !mediaElement) {
    button.style.display = "none";
    button.setAttribute("aria-pressed", "false");
    button.textContent = "Unmute";
    return;
  }
  button.style.display = "inline-block";
  button.setAttribute("aria-pressed", mediaElement.muted ? "false" : "true");
  button.textContent = mediaElement.muted ? "Unmute" : "Mute";
}

function togglePlayerAudio() {
  var video = document.getElementById("player-video");
  var audio = document.getElementById("player-audio");
  var mediaElement = video && video.style.display !== "none" ? video : audio;
  if (!mediaElement || !mediaElement.srcObject) return;
  mediaElement.muted = !mediaElement.muted;
  setPlayerAudioControl(mediaElement, true);
  var playPromise = mediaElement.play();
  if (playPromise && playPromise.catch) {
    playPromise.catch(function(e) {
      if (isCurrentPlayer(playerGeneration)) setPlayerStatus("Audio play error: " + formatErr(e), "error");
    });
  }
}

function streamHasVideo(streamKey) {
  var codec = normalizePlayerCodec((streamMedia[streamKey] || {}).video_codec);
  return ["h264", "avc", "avc1", "h265", "hevc", "hev1", "hvc1", "av1", "av01", "vp8", "vp08", "vp9", "vp09"].indexOf(codec) >= 0;
}

function selectStreamMediaElement(streamKey, trackKind) {
  var hasVideo = trackKind === "video" || streamHasVideo(streamKey);
  var wrap = document.querySelector('.player-video-wrap');
  var video = document.getElementById("player-video");
  var audio = document.getElementById("player-audio");
  if (wrap) wrap.classList.toggle("audio-only", !hasVideo);
  video.style.display = hasVideo ? "block" : "none";
  audio.style.display = hasVideo ? "none" : "block";
  return hasVideo ? video : audio;
}

function monitorPlayableVideo(video, generation, label, streamKey) {
  monitorPlayableMedia(video, generation, label, streamKey, true);
}

function monitorPlayableAudio(audio, generation, label, streamKey) {
  monitorPlayableMedia(audio, generation, label, streamKey, false);
}

function monitorStreamMedia(mediaElement, generation, label, streamKey) {
  var videoElement = document.getElementById("player-video");
  if (mediaElement === videoElement || streamHasVideo(streamKey)) {
    monitorPlayableVideo(mediaElement, generation, label, streamKey);
  } else {
    monitorPlayableAudio(mediaElement, generation, label, streamKey);
  }
}

function monitorPlayableMedia(mediaElement, generation, label, streamKey, expectsVideo) {
  if (currentVideoEventCleanup) {
    try { currentVideoEventCleanup(); } catch(e) {}
    currentVideoEventCleanup = null;
  }

  var finished = false;
  var frameCallbackID = null;
  var lastMediaTime = Number(mediaElement.currentTime) || 0;
  var lastProgressAt = Date.now();
  var playbackConfirmed = false;
  var playbackStalled = false;

  function cleanup() {
    if (finished) return;
    finished = true;
    clearTimeout(startupTimeoutID);
    clearInterval(stallIntervalID);
    mediaElement.removeEventListener("loadeddata", checkReady);
    mediaElement.removeEventListener("playing", checkReady);
    mediaElement.removeEventListener("timeupdate", checkReady);
    if (frameCallbackID !== null && mediaElement.cancelVideoFrameCallback) {
      try { mediaElement.cancelVideoFrameCallback(frameCallbackID); } catch(e) {}
    }
    if (currentVideoEventCleanup === cleanup) currentVideoEventCleanup = null;
  }

  function markPlaying() {
    if (!isCurrentPlayer(generation)) return;
    playbackConfirmed = true;
    playbackStalled = false;
    setPlayerStatus("Playing (" + label + ")", "ok");
  }

  function checkReady() {
    if (!isCurrentPlayer(generation)) {
      cleanup();
      return;
    }
    if (mediaElement.readyState < 2 || mediaElement.paused) return;
    if (expectsVideo && (mediaElement.videoWidth <= 0 || mediaElement.videoHeight <= 0)) return;

    var mediaTime = Number(mediaElement.currentTime) || 0;
    if (!expectsVideo && mediaElement.readyState >= 2 && mediaTime > 0) {
      markPlaying();
      return;
    }
    if (mediaTime > lastMediaTime + 0.05) {
      lastMediaTime = mediaTime;
      lastProgressAt = Date.now();
      if (!playbackConfirmed || playbackStalled) markPlaying();
    } else if (mediaTime < lastMediaTime - 0.5) {
      lastMediaTime = mediaTime;
      lastProgressAt = Date.now();
    }
  }

  var startupTimeoutID = setTimeout(function() {
    if (!isCurrentPlayer(generation)) return;
    if (!playbackConfirmed) {
      if (label === "WebRTC/WHEP") {
        setPlayerStatus("WebRTC is still waiting for a decodable keyframe", "");
      } else {
        setPlayerStatus("No advancing media received (check codec support and keyframes)", "error");
      }
    }
  }, 8000);

  var stallIntervalID = setInterval(function() {
    if (!isCurrentPlayer(generation)) {
      cleanup();
      return;
    }
    checkReady();
    if (playbackConfirmed && !playbackStalled && !mediaElement.paused && Date.now() - lastProgressAt >= 3000) {
      playbackStalled = true;
      setPlayerStatus("Playback stalled (" + label + ")", "error");
    }
  }, 500);

  currentVideoEventCleanup = cleanup;
  mediaElement.addEventListener("loadeddata", checkReady);
  mediaElement.addEventListener("playing", checkReady);
  mediaElement.addEventListener("timeupdate", checkReady);

  if (expectsVideo && mediaElement.requestVideoFrameCallback) {
    frameCallbackID = mediaElement.requestVideoFrameCallback(function() {
      checkReady();
    });
  }
  checkReady();
}

function encodedStreamPath(streamKey) {
  return String(streamKey || "").split("/").map(function(segment) {
    return encodeURIComponent(segment);
  }).join("/");
}

function validProtocolLabStreamKey(streamKey) {
  var value = String(streamKey || "");
  if (!value || value.length > 256 || !/^[\x21-\x7e]+$/.test(value)) return false;
  return value.split("/").every(function(segment) {
    return segment && segment !== "." && segment !== "..";
  });
}

function playerPlaybackPath(playback, field, fallback) {
  var value = playback && playback[field];
  return typeof value === "string" && value ? value : fallback;
}

function startMPEGTSPlayback(player, mediaElement, generation, label, streamKey) {
  var playPromise;
  try {
    playPromise = player.play();
  } catch (e) {
    if (isCurrentPlayer(generation)) setPlayerStatus("Play error: " + formatErr(e), "error");
    return;
  }
  if (!isCurrentPlayer(generation)) return;

  // HTMLMediaElement.play() may remain pending until the first decodable
  // sample arrives. Do not use that promise as the connection state gate;
  // monitor media progress independently and surface a later rejection.
  setPlayerStatus("Connected, waiting for first decoded frame...");
  monitorStreamMedia(mediaElement, generation, label, streamKey);
  if (playPromise && typeof playPromise.catch === "function") {
    playPromise.catch(function(e) {
      if (isCurrentPlayer(generation)) setPlayerStatus("Play error: " + formatErr(e), "error");
    });
  }
}

function playHTTPFLV(streamKey, playbackPath) {
  destroyCurrentPlayer();
  var generation = playerGeneration;
  var mediaElement = selectStreamMediaElement(streamKey);
  var url = mediaURL(playbackPath || "/" + encodedStreamPath(streamKey) + ".flv");
  setPlayerURL(url);
  setPlayerStatus("Connecting...");

  if (typeof mpegts === "undefined" || !mpegts.isSupported()) {
    setPlayerStatus("mpegts.js not supported in this browser", "error");
    return;
  }
  var player = null;
  try {
    player = mpegts.createPlayer({
      type: "flv",
      url: url,
      isLive: true
    });
    currentPlayer = player;
    player.on(mpegts.Events.ERROR, function(t, d, e) {
      if (!isCurrentPlayer(generation)) return;
      setPlayerStatus("Error: " + formatErr(e || d || t), "error");
    });
    player.attachMediaElement(mediaElement);
    player.load();
    startMPEGTSPlayback(player, mediaElement, generation, "HTTP-FLV", streamKey);
  } catch (e) {
    if (currentPlayer === player) destroyCurrentPlayer();
    else if (player) { try { player.destroy(); } catch (ignored) {} }
    setPlayerStatus("Play error: " + formatErr(e), "error");
    return;
  }
}

function playWSFLV(streamKey, playbackPath) {
  destroyCurrentPlayer();
  var generation = playerGeneration;
  var mediaElement = selectStreamMediaElement(streamKey);
  var url = websocketMediaURL(playbackPath || "/ws/" + encodedStreamPath(streamKey) + ".flv");
  setPlayerURL(url);
  setPlayerStatus("Connecting...");

  if (typeof mpegts === "undefined" || !mpegts.isSupported()) {
    setPlayerStatus("mpegts.js not supported in this browser", "error");
    return;
  }
  var player = null;
  try {
    player = mpegts.createPlayer({
      type: "flv",
      url: url,
      isLive: true
    });
    currentPlayer = player;
    player.on(mpegts.Events.ERROR, function(t, d, e) {
      if (!isCurrentPlayer(generation)) return;
      setPlayerStatus("Error: " + formatErr(e || d || t), "error");
    });
    player.attachMediaElement(mediaElement);
    player.load();
    startMPEGTSPlayback(player, mediaElement, generation, "WS-FLV", streamKey);
  } catch (e) {
    if (currentPlayer === player) destroyCurrentPlayer();
    else if (player) { try { player.destroy(); } catch (ignored) {} }
    setPlayerStatus("Play error: " + formatErr(e), "error");
    return;
  }
}

function playHTTPTS(streamKey, playbackPath) {
  destroyCurrentPlayer();
  var generation = playerGeneration;
  var mediaElement = selectStreamMediaElement(streamKey);
  var url = mediaURL(playbackPath || "/" + encodedStreamPath(streamKey) + ".ts");
  setPlayerURL(url);
  setPlayerStatus("Connecting...");

  if (typeof mpegts === "undefined" || !mpegts.isSupported()) {
    setPlayerStatus("mpegts.js not supported in this browser", "error");
    return;
  }
  var player = null;
  try {
    player = mpegts.createPlayer({
      type: "mpegts",
      url: url,
      isLive: true
    });
    currentPlayer = player;
    player.on(mpegts.Events.ERROR, function(t, d, e) {
      if (!isCurrentPlayer(generation)) return;
      setPlayerStatus("Error: " + formatErr(e || d || t), "error");
    });
    player.attachMediaElement(mediaElement);
    player.load();
    startMPEGTSPlayback(player, mediaElement, generation, "HTTP-TS", streamKey);
  } catch (e) {
    if (currentPlayer === player) destroyCurrentPlayer();
    else if (player) { try { player.destroy(); } catch (ignored) {} }
    setPlayerStatus("Play error: " + formatErr(e), "error");
    return;
  }
}

function fmp4MimeCandidates(streamKey) {
  var media = streamMedia[streamKey] || {};
  var videoCodec = normalizePlayerCodec(media.video_codec);
  var audioCodec = normalizePlayerCodec(media.audio_codec);
  var videoCodecs = {
    h264: "avc1.42E01E",
    avc: "avc1.42E01E",
    avc1: "avc1.42E01E",
    h265: "hvc1.1.6.L120.B0",
    hevc: "hvc1.1.6.L120.B0",
    hev1: "hvc1.1.6.L120.B0",
    hvc1: "hvc1.1.6.L120.B0"
  };
  var audioCodecs = {
    aac: "mp4a.40.2",
    opus: "opus",
    mp3: "mp4a.40.34"
  };
  var video = videoCodecs[videoCodec] || "";
  var audio = audioCodecs[audioCodec] || "";
  if (!audio && serverCapabilities.audio_transcoding &&
      (audioCodec === "g711a" || audioCodec === "g711u" || audioCodec === "pcma" || audioCodec === "pcmu")) {
    audio = audioCodecs.aac;
  }
  if (video && audio) return ['video/mp4; codecs="' + video + ', ' + audio + '"'];
  if (video) return ['video/mp4; codecs="' + video + '"'];
  if (audio) return ['audio/mp4; codecs="' + audio + '"'];
  return ["video/mp4"];
}

function playFMP4(streamKey, playbackPath) {
  destroyCurrentPlayer();
  var generation = playerGeneration;
  var url = mediaURL(playbackPath || "/" + encodedStreamPath(streamKey) + ".mp4");
  setPlayerURL(url);
  setPlayerStatus("Connecting...");

  if (!window.MediaSource) {
    setPlayerStatus("MSE not supported in this browser", "error");
    return;
  }

  var video = selectStreamMediaElement(streamKey);
  var ms = new MediaSource();
  var abortCtrl = new AbortController();
  currentMSEAbort = abortCtrl;
  currentMSEMediaSource = ms;

  var objectURL = URL.createObjectURL(ms);
  currentMSEObjectURL = objectURL;
  video.src = objectURL;

  ms.addEventListener("sourceopen", function() {
    if (!isCurrentPlayer(generation) || currentMSEMediaSource !== ms) return;
    var mimes = fmp4MimeCandidates(streamKey);
    var mime = mimes.find(function(m) { return MediaSource.isTypeSupported(m); });
    if (!mime) {
      if (isCurrentPlayer(generation) && currentMSEMediaSource === ms) {
        destroyCurrentPlayer();
        setPlayerStatus("FMP4 not supported by this browser", "error");
      }
      return;
    }

    var sb;
    try {
      sb = ms.addSourceBuffer(mime);
    } catch(e) {
      if (isCurrentPlayer(generation) && currentMSEMediaSource === ms) {
        destroyCurrentPlayer();
        setPlayerStatus("MSE add buffer error: " + e.message, "error");
      }
      return;
    }
    sb.mode = "segments";
    currentMSESourceBuffer = sb;

    var queue = [];
    var appending = false;
    var playbackMonitorStarted = false;
    var streamReader = null;
    var failed = false;
    var streamEnded = false;

    function failFMP4(message) {
      if (failed || !isCurrentPlayer(generation) || currentMSEMediaSource !== ms) return;
      failed = true;
      queue.length = 0;
      if (streamReader) {
        var reader = streamReader;
        streamReader = null;
        try { reader.cancel().catch(function(){}); } catch(e) {}
      }
      destroyCurrentPlayer();
      setPlayerStatus(message, "error");
    }

    function sourceBufferActive() {
      if (!isCurrentPlayer(generation) || currentMSEMediaSource !== ms || currentMSESourceBuffer !== sb || ms.readyState !== "open") {
        return false;
      }
      for (var i = 0; i < ms.sourceBuffers.length; i++) {
        if (ms.sourceBuffers[i] === sb) return true;
      }
      return false;
    }

    function maybeEndOfStream() {
      if (!streamEnded || appending || sb.updating || queue.length > 0 || !sourceBufferActive()) return;
      try { ms.endOfStream(); } catch(e) {}
    }

    function appendNext() {
      if (!sourceBufferActive() || appending || queue.length === 0 || sb.updating) return;
      var chunk = queue.shift();
      try {
        appending = true;
        sb.appendBuffer(chunk);
      } catch(e) {
        appending = false;
        failFMP4("Append error: " + e.message);
      }
    }

    sb.addEventListener("updateend", function() {
      if (!sourceBufferActive()) return;
      appending = false;
      appendNext();
      maybeEndOfStream();
      if (!playbackMonitorStarted && video.readyState >= 2) {
        playbackMonitorStarted = true;
        if (video.buffered.length > 0) {
          var firstBufferedStart = video.buffered.start(0);
          var firstBufferedEnd = video.buffered.end(0);
          if (video.currentTime < firstBufferedStart || video.currentTime > firstBufferedEnd) {
            video.currentTime = firstBufferedStart;
          }
        }
        video.play().then(function() {
          if (!isCurrentPlayer(generation)) return;
          setPlayerStatus("Connected, waiting for first decoded frame...");
          monitorStreamMedia(video, generation, "FMP4", streamKey);
        }).catch(function(e) {
          if (isCurrentPlayer(generation)) setPlayerStatus("Play error: " + e.message, "error");
        });
      }
    });

    sb.addEventListener("error", function() {
      failFMP4("SourceBuffer error");
    });

    fetch(url, { signal: abortCtrl.signal })
      .then(function(resp) {
        if (!resp.ok) throw new Error("HTTP " + resp.status);
        if (!resp.body) throw new Error("empty response body");
        if (!isCurrentPlayer(generation)) return null;
        setPlayerStatus("Buffering...");
        streamReader = resp.body.getReader();
        var reader = streamReader;
        function pump() {
          if (!isCurrentPlayer(generation)) {
            reader.cancel().catch(function(){});
            return Promise.resolve();
          }
          return reader.read().then(function(result) {
            if (!isCurrentPlayer(generation)) return;
            if (result.done) {
              streamReader = null;
              streamEnded = true;
              maybeEndOfStream();
              return;
            }
            if (result.value && result.value.byteLength > 0) queue.push(result.value);
            appendNext();
            return pump();
          });
        }
        return pump();
      })
      .catch(function(e) {
        if (isCurrentPlayer(generation) && e.name !== "AbortError") {
          destroyCurrentPlayer();
          setPlayerStatus("FMP4 error: " + e.message, "error");
        }
      });
  });
}

function playHLS(streamKey, playbackPath) {
  destroyCurrentPlayer();
  var generation = playerGeneration;
  var url = mediaURL(playbackPath || "/" + encodedStreamPath(streamKey) + ".m3u8");
  setPlayerURL(url);
  setPlayerStatus("Connecting...");

  var video = selectStreamMediaElement(streamKey);
  video.autoplay = false;
  video.pause();

  if (typeof Hls === "undefined") {
    setPlayerStatus("hls.js not loaded", "error");
    return;
  }

  if (Hls.isSupported()) {
    var hls = new Hls({
      liveDurationInfinity: true,
      enableWorker: true,
      lowLatencyMode: true,
      liveSyncDuration: 1.5,
      startFragPrefetch: true,
      maxBufferLength: 6,
      maxMaxBufferLength: 12
    });
    currentHls = hls;

    hls.loadSource(url);
    hls.attachMedia(video);

    var hlsPlaybackStarted = false;
    var startHLSPlayback = function() {
      if (hlsPlaybackStarted || !isCurrentPlayer(generation) || currentHls !== hls) return;
      // Count only the buffered range containing the playhead. A later range
      // after a gap cannot protect startup from an immediate underrun.
      var bufferedAhead = 0;
      for (var i = 0; video.buffered && i < video.buffered.length; i++) {
        if (video.buffered.start(i) <= video.currentTime + 0.05 && video.buffered.end(i) > video.currentTime) {
          bufferedAhead = video.buffered.end(i) - video.currentTime;
          break;
        }
      }
      if (video.readyState < 2 || bufferedAhead < 0.8) {
        setTimeout(startHLSPlayback, 100);
        return;
      }
      hlsPlaybackStarted = true;
      video.play().then(function() {
        if (!isCurrentPlayer(generation) || currentHls !== hls) return;
        setPlayerStatus("Connected, waiting for first decoded frame...");
        monitorStreamMedia(video, generation, "HLS", streamKey);
      }).catch(function(e) {
        if (!isCurrentPlayer(generation) || currentHls !== hls) return;
        setPlayerStatus("Play error: " + e.message, "error");
      });
    };
    hls.on(Hls.Events.MANIFEST_PARSED, function() {
      startHLSPlayback();
    });

    hls.on(Hls.Events.ERROR, function(event, data) {
      if (isCurrentPlayer(generation) && data.fatal) {
        setPlayerStatus("HLS error: " + data.type + " - " + data.details, "error");
      }
    });
  } else if (video.canPlayType("application/vnd.apple.mpegurl")) {
    // Native HLS support (Safari)
    video.src = url;
    var removeLoadedMetadata = null;
    var onLoadedMetadata = function() {
      if (currentVideoEventCleanup === removeLoadedMetadata) currentVideoEventCleanup = null;
      if (!isCurrentPlayer(generation) || video.src !== url) return;
      video.play().then(function() {
        if (!isCurrentPlayer(generation) || video.src !== url) return;
        setPlayerStatus("Connected, waiting for first decoded frame...");
        monitorStreamMedia(video, generation, "HLS native", streamKey);
      }).catch(function(e) {
        if (!isCurrentPlayer(generation) || video.src !== url) return;
        setPlayerStatus("Play error: " + e.message, "error");
      });
    };
    removeLoadedMetadata = function() {
      video.removeEventListener("loadedmetadata", onLoadedMetadata);
    };
    currentVideoEventCleanup = removeLoadedMetadata;
    video.addEventListener("loadedmetadata", onLoadedMetadata);
  } else {
    setPlayerStatus("HLS not supported in this browser", "error");
  }
}

function playDASH(streamKey, playbackPath) {
  destroyCurrentPlayer();
  var generation = playerGeneration;
  var url = mediaURL(playbackPath || "/" + encodedStreamPath(streamKey) + ".mpd");
  setPlayerURL(url);
  setPlayerStatus("Connecting...");

  var video = selectStreamMediaElement(streamKey);

  if (typeof dashjs === "undefined") {
    setPlayerStatus("dash.js not loaded", "error");
    return;
  }

  var player = dashjs.MediaPlayer().create();
  currentDash = player;

  player.updateSettings({
    streaming: {
      delay: {
        liveDelayFragmentCount: 1
      },
      buffer: {
        fastSwitchEnabled: true
      }
    }
  });

  player.initialize(video, url, true);

  var playbackMonitorStarted = false;
  player.on(dashjs.MediaPlayer.events.PLAYBACK_PLAYING, function() {
    if (!isCurrentPlayer(generation) || playbackMonitorStarted) return;
    playbackMonitorStarted = true;
    setPlayerStatus("Connected, waiting for first decoded frame...");
    monitorStreamMedia(video, generation, "DASH", streamKey);
  });

  player.on(dashjs.MediaPlayer.events.ERROR, function(e) {
    if (!isCurrentPlayer(generation)) return;
    setPlayerStatus("DASH error: " + (e.error ? e.error.message : "unknown"), "error");
  });
}

function whepStalledMediaKinds(feed) {
  var reference = Date.parse(feed.updated_at || "");
  if (!isFinite(reference)) reference = Date.now();
  var staleBefore = reference - 8000;
  var kinds = [];
  function isStale(expected, value) {
    if (!expected) return false;
    var timestamp = Date.parse(value || "");
    return !isFinite(timestamp) || timestamp <= staleBefore;
  }
  if (isStale(feed.expected_video, feed.last_video_at)) kinds.push("video");
  if (isStale(feed.expected_audio, feed.last_audio_at)) kinds.push("audio");
  return kinds;
}

function playWHEP(streamKey, mode, playbackPath) {
  destroyCurrentPlayer();
  var generation = playerGeneration;
  var proto = location.protocol === "https:" ? "https://" : "http://";
  var fallbackPath = "/webrtc/whep/" + encodedStreamPath(streamKey) + "?mode=" + (mode || "live");
  var url = endpointURL(proto.slice(0, -2), endpoints.webrtc, playbackPath || fallbackPath);
  var requestedMode = mode || "live";
  var modePattern = /([?&])mode=[^&]*/;
  if (modePattern.test(url)) {
    url = url.replace(modePattern, "$1mode=" + encodeURIComponent(requestedMode));
  } else {
    url += (url.indexOf("?") >= 0 ? "&" : "?") + "mode=" + encodeURIComponent(requestedMode);
  }
  setPlayerURL(url);
  setPlayerStatus("Negotiating WebRTC...");

  if (typeof RTCPeerConnection === "undefined") {
    setPlayerStatus("WebRTC is not supported in this browser", "error");
    return;
  }

  var offerAudio = whepCanOfferAudio(normalizePlayerCodec((streamMedia[streamKey] || {}).audio_codec));
  if (!offerAudio && !streamHasVideo(streamKey)) {
    setPlayerStatus(consoleText("WebRTC audio is unavailable without audio transcoding."), "error");
    return;
  }
  document.getElementById("player-audio-notice").style.display = offerAudio ? "none" : "block";

  var pc = new RTCPeerConnection({
    iceServers: [{ urls: "stun:stun.l.google.com:19302" }]
  });
  currentPC = pc;

  var remoteStream = new MediaStream();
  var receivedTrack = false;
  var playbackMonitorKind = "";
  var mediaElement = selectStreamMediaElement(streamKey);
  var hasVideo = streamHasVideo(streamKey);
  mediaElement.srcObject = remoteStream;
  // ontrack runs asynchronously, outside the click gesture that opened the
  // player. Start muted so browser autoplay policy cannot hide the media;
  // the user can restore audio with the explicit player control.
  mediaElement.muted = true;

  pc.ontrack = function(event) {
    if (!isCurrentPlayer(generation)) return;
    receivedTrack = true;
    // Streams metadata can lag behind the first WHIP video packet. Prefer the
    // negotiated track kind so a video track cannot be attached to the audio
    // fallback element while the list is still refreshing.
    if (event.track.kind === "video") {
      hasVideo = true;
      mediaElement = selectStreamMediaElement(streamKey, "video");
    } else if (event.track.kind === "audio" && !hasVideo) {
      mediaElement = selectStreamMediaElement(streamKey, "audio");
    }
    remoteStream.addTrack(event.track);
    // Rebind after the first track arrives so browsers refresh an element
    // that was initially attached to an empty MediaStream.
    mediaElement.srcObject = remoteStream;
    if (event.track.kind === "audio") setPlayerAudioControl(mediaElement, true);
    var shouldStartPlayback = event.track.kind === "video" || (!hasVideo && event.track.kind === "audio");
    if (shouldStartPlayback && playbackMonitorKind !== event.track.kind) {
      playbackMonitorKind = event.track.kind;
      var playPromise = mediaElement.play();
      monitorStreamMedia(mediaElement, generation, "WebRTC/WHEP", streamKey);
      playPromise.catch(function(e) {
        if (isCurrentPlayer(generation)) setPlayerStatus("Play error: " + e.message, "error");
      });
    }
  };

  pc.oniceconnectionstatechange = function() {
    if (!isCurrentPlayer(generation)) return;
    if (pc.iceConnectionState === "failed" || pc.iceConnectionState === "disconnected") {
      setPlayerStatus("WebRTC disconnected: " + pc.iceConnectionState, "error");
    }
  };

  pc.addTransceiver("video", { direction: "recvonly" });
  if (offerAudio) pc.addTransceiver("audio", { direction: "recvonly" });

  pc.createOffer().then(function(offer) {
    return pc.setLocalDescription(offer);
  }).then(function() {
    // Wait for at least one ICE candidate (host) or gathering complete,
    // whichever comes first. Avoids waiting for slow STUN/TURN timeouts.
    return new Promise(function(resolve) {
      if (pc.iceGatheringState === "complete") { resolve(); return; }
      var resolved = false;
      pc.onicecandidate = function(e) {
        if (!resolved && (e.candidate === null || e.candidate.type === "host")) {
          resolved = true;
          resolve();
        }
      };
      pc.onicegatheringstatechange = function() {
        if (!resolved && pc.iceGatheringState === "complete") {
          resolved = true;
          resolve();
        }
      };
      // Safety timeout: don't wait more than 500ms for candidates
      setTimeout(function() { if (!resolved) { resolved = true; resolve(); } }, 500);
    });
  }).then(function() {
    if (!isCurrentPlayer(generation)) throw new Error("playback session replaced");
    return fetch(url, {
      method: "POST",
      headers: { "Content-Type": "application/sdp" },
      body: pc.localDescription.sdp
    });
  }).then(function(resp) {
    if (resp.status !== 201) {
      return resp.text().then(function(t) { throw new Error("WHEP " + resp.status + ": " + t); });
    }
    var location = resp.headers.get("Location") || "";
    return resp.text().then(function(sdp) { return { sdp: sdp, location: location }; });
  }).then(function(answer) {
    return pc.setRemoteDescription({ type: "answer", sdp: answer.sdp }).then(function() {
      if (!answer.location || !isCurrentPlayer(generation)) return;
      var statusURL = new URL(answer.location + "/status", url).toString();
      function pollStatus() {
        if (!isCurrentPlayer(generation)) return;
        fetch(statusURL).then(function(resp) {
          if (!resp.ok) return null;
          return resp.json();
        }).then(function(data) {
          if (!data || !isCurrentPlayer(generation)) return;
          var feed = data.feed || {};
          if (feed.state === "waiting_keyframe") {
            setPlayerStatus("WebRTC connected, waiting for a keyframe...", "");
          } else if (feed.state === "sample_write_failed") {
            setPlayerStatus("WebRTC media send failed: " + (feed.last_error || "unknown error"), "error");
          } else if (feed.state === "codec_mismatch") {
            setPlayerStatus("WebRTC codec mismatch: " + (feed.last_error || "unsupported codec"), "error");
          } else if (feed.state === "generation_ended") {
            setPlayerStatus("WebRTC publisher generation ended", "error");
          } else if (feed.state === "no_media_input") {
            setPlayerStatus("WebRTC has no media input", "error");
          } else if (feed.state === "media_stalled") {
            var staleKinds = whepStalledMediaKinds(feed);
            setPlayerStatus("WebRTC media stalled" + (staleKinds.length ? " (" + staleKinds.join("/") + ")" : ""), "error");
          }
        }).catch(function() {}).then(function() {
          if (isCurrentPlayer(generation)) whepStatusPoll = setTimeout(pollStatus, 500);
        });
      }
      pollStatus();
    });
  }).then(function() {
    if (!isCurrentPlayer(generation)) return;
    setPlayerStatus(receivedTrack ? "WebRTC connected, waiting for first decoded frame..." : "WebRTC connected, waiting for media...");
    startStatsOverlay(pc);
  }).catch(function(e) {
    if (isCurrentPlayer(generation)) {
      destroyCurrentPlayer();
      setPlayerStatus("WHEP error: " + e.message, "error");
    }
  });
}

function normalizePlayerCodec(codec) {
  return String(codec || "").toLowerCase().replace(/[^a-z0-9]/g, "");
}

function prefersWHEP(videoCodec) {
  var codec = normalizePlayerCodec(videoCodec);
  return codec === "vp8" || codec === "vp08" || codec === "vp9" || codec === "vp09" || codec === "av1" || codec === "av01";
}

function addWHEPProtocols(protocols, playback) {
  if (!endpoints.webrtc) return;
  protocols.push({ id: "whep-live", label: "WebRTC", fn: function(k) { playWHEP(k, "live", playerPlaybackPath(playback, "whep_live", playerPlaybackPath(playback, "whep", ""))); } });
  protocols.push({ id: "whep-realtime", label: "WebRTC-Realtime", fn: function(k) { playWHEP(k, "realtime", playerPlaybackPath(playback, "whep_realtime", "")); } });
}

function whepCanOfferAudio(audioCodec) {
  // Only omit known unsupported audio when the server explicitly reports no
  // transcoding. Missing metadata/capabilities must still negotiate normally.
  return serverCapabilities.audio_transcoding !== false || ["aac", "mp3"].indexOf(audioCodec) < 0;
}

function addHTTPProtocols(protocols, playback) {
  if (!endpoints.http) return;
  protocols.push({ id: "http-flv", label: "HTTP-FLV", fn: function(k) { playHTTPFLV(k, playerPlaybackPath(playback, "http_flv", "")); } });
  protocols.push({ id: "ws-flv", label: "WS-FLV", fn: function(k) { playWSFLV(k, playerPlaybackPath(playback, "ws_flv", "")); } });
  protocols.push({ id: "http-ts", label: "HTTP-TS", fn: function(k) { playHTTPTS(k, playerPlaybackPath(playback, "http_ts", "")); } });
  protocols.push({ id: "fmp4", label: "FMP4", fn: function(k) { playFMP4(k, playerPlaybackPath(playback, "fmp4", "")); } });
  protocols.push({ id: "hls", label: "HLS", fn: function(k) { playHLS(k, playerPlaybackPath(playback, "hls", "")); } });
  protocols.push({ id: "dash", label: "DASH", fn: function(k) { playDASH(k, playerPlaybackPath(playback, "dash", "")); } });
}

function openPlayer(streamKey, mediaHint) {
  destroyCurrentPlayer();
  statsVisible = false;
  var btnStats = document.getElementById('btn-stats');
  if (btnStats) btnStats.classList.remove('active');
  document.getElementById("player-stream-key").textContent = streamKey;
  document.getElementById("player-overlay").classList.add("active");
  setPlayerStatus("Select a protocol to start playback");
  setPlayerURL("");

  // Build protocol tabs based on available endpoints
  var tabs = document.getElementById("proto-tabs");
  tabs.innerHTML = "";
  var protocols = [];
  var playback = arguments.length > 2 ? arguments[2] : null;
  var media = Object.assign({}, streamMedia[streamKey] || {}, mediaHint || {});
  if (mediaHint) streamMedia[streamKey] = media;
  var videoCodec = media.video_codec || "";
  var audioCodec = normalizePlayerCodec(media.audio_codec);
  var hasVideo = streamHasVideo(streamKey);
  var audioOnlyG711 = !hasVideo && ["g711a", "g711u", "pcma", "pcmu"].indexOf(audioCodec) >= 0;
  var whepPreferred = prefersWHEP(videoCodec);

  selectStreamMediaElement(streamKey);
  if (audioOnlyG711) {
    if (endpoints.webrtc) addWHEPProtocols(protocols, playback);
  } else if (whepPreferred) {
    addWHEPProtocols(protocols, playback);
  } else {
    addHTTPProtocols(protocols, playback);
    addWHEPProtocols(protocols, playback);
  }

  if (protocols.length === 0) {
    if (audioOnlyG711 && !endpoints.webrtc) {
      setPlayerStatus("G.711 audio requires WebRTC/WHEP playback", "error");
    } else if (whepPreferred && !endpoints.webrtc) {
      setPlayerStatus((videoCodec || "This video codec") + " requires WebRTC/WHEP playback", "error");
    } else {
      setPlayerStatus("No streaming endpoints available", "error");
    }
    return;
  }

  for (var i = 0; i < protocols.length; i++) {
    var tab = document.createElement("div");
    tab.className = "proto-tab";
    tab.textContent = protocols[i].label;
    tab.setAttribute("data-proto", protocols[i].id);
    (function(proto) {
      tab.addEventListener("click", function() {
        // Activate tab
        var allTabs = tabs.querySelectorAll(".proto-tab");
        for (var j = 0; j < allTabs.length; j++) allTabs[j].classList.remove("active");
        this.classList.add("active");
        // Play
        proto.fn(streamKey);
      });
    })(protocols[i]);
    tabs.appendChild(tab);
  }

  // Auto-play first protocol
  tabs.children[0].click();
}

function closePlayer() {
  destroyCurrentPlayer();
  document.getElementById("player-overlay").classList.remove("active");
}

// Close modals on Escape key
document.addEventListener("keydown", function(e) {
  if (e.key === "Escape") {
    if (document.getElementById("publish-overlay").classList.contains("active")) {
      closePublishModal();
    } else if (document.getElementById("player-overlay").classList.contains("active")) {
      closePlayer();
    } else if (document.getElementById("modal").classList.contains("active")) {
      closeModal();
    }
  }
});

// Close player on overlay click
document.getElementById("player-overlay").addEventListener("click", function(e) {
  if (e.target === this) closePlayer();
});

/* --- Server info --- */
function refreshServerInfo(signal) {
  var infoRequest = apiFetch("/api/v1/server/info", { signal: signal })
    .then(function(data) {
      if (!data) return;
      document.getElementById("server-version").textContent = "v" + (data.version || "?");
      document.getElementById("stat-uptime").textContent = formatUptime(data.uptime_sec || 0);
      publishEndpointsReady = !!(data.endpoints && data.endpoints.webrtc);
      if (data.endpoints) endpoints = data.endpoints;
	  if (data.endpoint_schemes) endpointSchemes = data.endpoint_schemes;
	  serverCapabilities = data.capabilities || {};
      if (data.modules) updateGB28181Tab(data.modules);
      updatePublishButton();
      updatePublishStartButton();
    })
    .catch(function() {});

  var statsRequest = apiFetch("/api/v1/server/stats", { signal: signal })
    .then(function(data) {
      if (!data) return;
      document.getElementById("stat-conns").textContent = data.connections || 0;
    })
    .catch(function() {});
  return Promise.all([infoRequest, statsRequest]);
}

/* --- Stream list --- */
function refresh(signal) {
  var q = (document.getElementById("stream-search") || {}).value || "";
  var url = "/api/v1/streams";
  if (q) url += "?q=" + encodeURIComponent(q);
  return apiFetch(url, { signal: signal, page:"streams" })
    .then(function(data) {
      var streams = (data && data.streams) || [];
      var nextStreamMedia = Object.create(null);
      var totalSubs = 0;
      var publishing = 0;

      for (var i = 0; i < streams.length; i++) {
        nextStreamMedia[streams[i].key] = {
          video_codec: streams[i].video_codec || "",
          audio_codec: streams[i].audio_codec || ""
        };
        var subs = streams[i].subscribers || {};
        var keys = Object.keys(subs);
        for (var j = 0; j < keys.length; j++) totalSubs += subs[keys[j]];
        if (streams[i].state === "publishing") publishing++;
      }
      streamMedia = nextStreamMedia;

      document.getElementById("stat-streams").textContent = consolePages.streams.serverPage ? consolePages.streams.total : streams.length;
      document.getElementById("stat-publishing").textContent = publishing;
      document.getElementById("stat-subs").textContent = totalSubs;
      document.getElementById("updated").textContent = new Date().toLocaleTimeString();

      renderStreams(streams);
    })
    .catch(function(err) { showManagementError("streams", err); });
}

var streamRows = Object.create(null);
var selectedStreamKey = "";
var streamTrendPoll = null;
var streamTrendRequest = null;
var streamTrendGeneration = 0;
var streamTrendState = null;

function createStreamRow() {
  var row = document.createElement("tr");
  row.className = "stream-row select-row";
  row.tabIndex = 0;
  row.setAttribute("role", "button");
  var keyCell = row.insertCell();
  keyCell.className = "stream-key-cell";
  var streamCell = document.createElement("div");
  streamCell.className = "stream-cell";
  var signal = document.createElement("span");
  signal.className = "stream-signal";
  signal.textContent = "↗";
  var streamCopy = document.createElement("div");
  var key = document.createElement("div");
  key.className = "stream-name stream-key";
  var owner = document.createElement("div");
  owner.className = "stream-owner";
  streamCopy.appendChild(key);
  streamCopy.appendChild(owner);
  streamCell.appendChild(signal);
  streamCell.appendChild(streamCopy);
  keyCell.appendChild(streamCell);
  var stateCell = row.insertCell();
  var state = document.createElement("span");
  stateCell.appendChild(state);
  var publisher = appendTextCell(row, "", "publisher-id");
  var codecCell = row.insertCell();
  var bitrate = appendTextCell(row, "-", "mono");
  var fps = appendTextCell(row, "-", "mono");
  var uptime = appendTextCell(row, "-", "mono");
  var gopCell = row.insertCell();
  var gop = document.createElement("div");
  gop.className = "gop-cell";
  gopCell.appendChild(gop);
  var subscriberCell = row.insertCell();
  var subscribers = document.createElement("div");
  subscribers.className = "subs";
  subscriberCell.appendChild(subscribers);
  var actionCell = row.insertCell();
  var actions = document.createElement("div");
  actions.className = "actions";
  actionCell.appendChild(actions);
  row._streamNodes = {key:key, owner:owner, state:state, publisher:publisher, codec:codecCell, bitrate:bitrate, fps:fps, uptime:uptime, gop:gop, subscribers:subscribers, actions:actions};
  row.addEventListener("click", function(event) {
    if (event.target.closest("button")) return;
    selectStream(row.dataset.streamKey);
  });
  row.addEventListener("keydown", function(event) {
    if (event.key !== "Enter" && event.key !== " ") return;
    event.preventDefault();
    selectStream(row.dataset.streamKey);
  });
  return row;
}

function updateStreamRow(row, s) {
  var st = s.stats || {};
  var n = row._streamNodes;
  var key = String(s.key || "");
  row.dataset.streamKey = key;
  row.classList.toggle("selected", selectedStreamKey === key);
  row.setAttribute("aria-expanded", selectedStreamKey === key ? "true" : "false");
  n.key.textContent = key;
  n.state.className = "state state-" + String(s.state || "").replace(/[^a-z0-9_-]/gi, "-");
  n.state.textContent = String(s.state || "").replace("_", " ");
  n.publisher.textContent = s.publisher || "-";
  if (n.owner) n.owner.textContent = s.publisher || "-";
  n.codec.replaceChildren();
  ["video", "audio"].forEach(function(kind) {
    var value = s[kind + "_codec"];
    if (!value) return;
    var tag = document.createElement("span");
    tag.className = "codec-tag " + kind;
    tag.textContent = value;
    n.codec.appendChild(tag);
  });
  var transcodeTasks = Array.isArray(s.transcode_tasks) ? s.transcode_tasks : [];
  transcodeTasks.forEach(function(task) {
    var transcode = document.createElement("span");
    transcode.className = "transcode-tag";
    var source = task.source_codec || "?";
    var target = task.target_codec || "?";
    var scope = task.audio_only ? "audio" : "shared";
    transcode.textContent = "↻ " + source + " → " + target + " · " + (task.state || "running") + " · " + scope;
    if (task.last_error) transcode.title = task.last_error;
    n.codec.appendChild(transcode);
  });
  if (!n.codec.childNodes.length) n.codec.textContent = "-";
  n.bitrate.textContent = formatBitrate(st.bitrate_kbps);
  n.fps.textContent = st.fps ? Number(st.fps).toFixed(1) : "-";
  n.uptime.textContent = st.uptime_sec > 0 ? formatUptime(st.uptime_sec) : "-";
  n.gop.replaceChildren();
  if (!s.video_codec && s.audio_codec) {
    n.gop.textContent = "Not applicable (audio-only)";
  } else {
    var line = document.createElement("div");
    line.className = "gop";
    var items = [
      ["generation", "#" + (s.gop_generation || 0), "gop-value-generation"],
      ["frames", (s.gop_cache_len || 0) + " fr", "gop-value-frames"],
      ["duration", ((s.gop_duration_ms || 0) / 1000).toFixed(1) + " s", "gop-value-duration"],
      ["video / audio", "V" + (s.gop_video_frames || 0) + " / A" + (s.gop_audio_frames || 0), "gop-value-media"]
    ];
    items.forEach(function(item) {
      var block = document.createElement("div"); block.className = "gop-item";
      var label = document.createElement("div"); label.className = "gop-key"; label.textContent = item[0];
      var value = document.createElement("div"); value.className = "gop-value " + item[2]; value.textContent = item[1];
      block.appendChild(label); block.appendChild(value); line.appendChild(block);
    });
    n.gop.appendChild(line);
  }
  n.subscribers.replaceChildren();
  var subscriberKeys = Object.keys(s.subscribers || {}).sort();
  subscriberKeys.forEach(function(format) {
    var count = s.subscribers[format];
    var pill = document.createElement("span");
    pill.className = count > 0 ? "sub-pill active" : "sub-pill";
    pill.textContent = format.toUpperCase() + " " + count;
    n.subscribers.appendChild(pill);
  });
  if (!subscriberKeys.length) n.subscribers.textContent = "-";
  n.actions.replaceChildren();
  if (s.state === "publishing") appendActionButton(n.actions, "Preview", "btn-play", "stream-preview", key);
  if (s.publisher && canManage("operator")) appendActionButton(n.actions, "Kick", "btn-warn", "stream-kick", key);
  if (canManage("admin")) appendActionButton(n.actions, "Delete", "btn-danger", "stream-delete", key);
}

function renderStreams(streams) {
  streams = streams || [];
  var tbody = document.getElementById("tbody");
  var empty = document.getElementById("empty");
  var seen = Object.create(null);
  var sorted = streams.slice().sort(function(a, b) { return String(a.key).localeCompare(String(b.key)); });
  sorted.forEach(function(s) {
    var key = String(s.key || "");
    seen[key] = true;
    if (!streamRows[key]) streamRows[key] = createStreamRow();
    updateStreamRow(streamRows[key], s);
  });
  Object.keys(streamRows).forEach(function(key) {
    if (!seen[key]) {
      if (streamRows[key].nextSibling && streamRows[key].nextSibling.classList.contains("stream-detail-row")) streamRows[key].nextSibling.remove();
      delete streamRows[key];
    }
  });
  if (selectedStreamKey && !seen[selectedStreamKey]) closeStreamDetail();
  sorted.forEach(function(s) {
    var row = streamRows[String(s.key || "")];
    tbody.appendChild(row);
    if (selectedStreamKey === String(s.key || "")) tbody.appendChild(streamTrendDetailRow());
  });
  empty.style.display = sorted.length ? "none" : "block";
}

function streamTrendDetailRow() {
  var existing = document.getElementById("stream-detail-row");
  if (existing) return existing;
  var row = document.createElement("tr");
  row.id = "stream-detail-row";
  row.className = "stream-detail-row";
  var cell = row.insertCell();
  cell.colSpan = 10;
  cell.innerHTML = '<section class="stream-detail-panel detail-panel open" aria-live="polite">' +
    '<div class="detail-head"><div class="detail-title"><strong>Live detail</strong><span class="detail-live">stream</span><span class="stream-detail-meta" id="stream-detail-key"></span><span class="stream-detail-meta" id="stream-detail-samples"></span></div><button class="detail-close stream-detail-close" type="button" data-action="stream-detail-close" aria-label="Close live detail">&times;</button></div>' +
    '<div class="detail-body"><div class="detail-metrics"><div class="trend-summary"><span>Window <strong>60s</strong></span><span>Cadence <strong>1s</strong></span></div><div class="trend-button active"><span class="trend-label"><i class="swatch"></i>Bitrate</span><span class="trend-current" data-current="bitrate">-</span></div><div class="trend-button"><span class="trend-label"><i class="swatch"></i>Video FPS</span><span class="trend-current" data-current="videoFps">-</span></div><div class="trend-button"><span class="trend-label"><i class="swatch"></i>Audio FPS</span><span class="trend-current" data-current="audioFps">-</span></div><div class="trend-button"><span class="trend-label"><i class="swatch"></i>GOP duration</span><span class="trend-current" data-current="gopDuration">-</span></div></div>' +
    '<div class="trend-grid"><div class="trend-card" data-metric="bitrate"><div class="trend-card-header"><span class="trend-label">Bitrate</span><span class="trend-current" data-current="bitrate">-</span></div><svg class="trend-chart" viewBox="0 0 720 82" preserveAspectRatio="none" aria-label="Bitrate trend"><line class="trend-gridline" x1="0" y1="12" x2="720" y2="12"></line><line class="trend-gridline" x1="0" y1="41" x2="720" y2="41"></line><line class="trend-gridline" x1="0" y1="70" x2="720" y2="70"></line><path class="trend-area" data-area="bitrate"></path><path class="trend-path" data-path="bitrate"></path></svg><div class="trend-range"><span data-min="bitrate">min -</span><span data-max="bitrate">max -</span></div></div>' +
    '<div class="trend-card" data-metric="video-fps"><div class="trend-card-header"><span class="trend-label">Video FPS</span><span class="trend-current" data-current="videoFps">-</span></div><svg class="trend-chart" viewBox="0 0 720 82" preserveAspectRatio="none" aria-label="Video FPS trend"><line class="trend-gridline" x1="0" y1="12" x2="720" y2="12"></line><line class="trend-gridline" x1="0" y1="41" x2="720" y2="41"></line><line class="trend-gridline" x1="0" y1="70" x2="720" y2="70"></line><path class="trend-area" data-area="videoFps"></path><path class="trend-path" data-path="videoFps"></path></svg><div class="trend-range"><span data-min="videoFps">min -</span><span data-max="videoFps">max -</span></div></div>' +
    '<div class="trend-card" data-metric="audio-fps"><div class="trend-card-header"><span class="trend-label">Audio FPS</span><span class="trend-current" data-current="audioFps">-</span></div><svg class="trend-chart" viewBox="0 0 720 82" preserveAspectRatio="none" aria-label="Audio FPS trend"><line class="trend-gridline" x1="0" y1="12" x2="720" y2="12"></line><line class="trend-gridline" x1="0" y1="41" x2="720" y2="41"></line><line class="trend-gridline" x1="0" y1="70" x2="720" y2="70"></line><path class="trend-area" data-area="audioFps"></path><path class="trend-path" data-path="audioFps"></path></svg><div class="trend-range"><span data-min="audioFps">min -</span><span data-max="audioFps">max -</span></div></div>' +
    '<div class="trend-card" data-metric="gop-duration"><div class="trend-card-header"><span class="trend-label">GOP duration</span><span class="trend-current" data-current="gopDuration">-</span></div><svg class="trend-chart" viewBox="0 0 720 82" preserveAspectRatio="none" aria-label="GOP duration trend"><line class="trend-gridline" x1="0" y1="12" x2="720" y2="12"></line><line class="trend-gridline" x1="0" y1="41" x2="720" y2="41"></line><line class="trend-gridline" x1="0" y1="70" x2="720" y2="70"></line><path class="trend-area" data-area="gopDuration"></path><path class="trend-path" data-path="gopDuration"></path></svg><div class="trend-range"><span data-min="gopDuration">min -</span><span data-max="gopDuration">max -</span></div></div></div></div>' +
    '<div class="trend-empty" id="stream-trend-empty">Waiting for stream samples...</div></section>';
  cell.querySelector('[data-action="stream-detail-close"]').addEventListener("click", closeStreamDetail);
  return row;
}

function closeStreamDetail() {
  stopStreamTrendPolling();
  selectedStreamKey = "";
  var detail = document.getElementById("stream-detail-row");
  if (detail) detail.remove();
  Object.keys(streamRows).forEach(function(key) { streamRows[key].classList.remove("selected"); streamRows[key].setAttribute("aria-expanded", "false"); });
}

function selectStream(streamKey) {
  streamKey = String(streamKey || "");
  if (!streamKey) return;
  if (selectedStreamKey === streamKey) { closeStreamDetail(); return; }
  stopStreamTrendPolling();
  selectedStreamKey = streamKey;
  streamTrendGeneration++;
  streamTrendState = {samples:{bitrate:[], videoFps:[], audioFps:[], gopDuration:[]}, previous:null, generation:null, publisher:null};
  var row = streamRows[streamKey];
  if (!row) { selectedStreamKey = ""; return; }
  Object.keys(streamRows).forEach(function(key) { streamRows[key].classList.toggle("selected", key === streamKey); streamRows[key].setAttribute("aria-expanded", key === streamKey ? "true" : "false"); });
  row.parentNode.insertBefore(streamTrendDetailRow(), row.nextSibling);
  document.getElementById("stream-detail-key").textContent = "  " + streamKey;
  if (row) row.scrollIntoView({block:"nearest"});
  startStreamTrendPolling();
}

function stopStreamTrendPolling() {
  if (streamTrendPoll) { clearInterval(streamTrendPoll); streamTrendPoll = null; }
  if (streamTrendRequest) { streamTrendRequest.abort(); streamTrendRequest = null; }
}

function startStreamTrendPolling() {
  stopStreamTrendPolling();
  if (activeTab !== "streams" || document.hidden || !selectedStreamKey) return;
  pollSelectedStream();
  streamTrendPoll = setInterval(pollSelectedStream, 1000);
}

function pollSelectedStream() {
  if (!selectedStreamKey || activeTab !== "streams" || document.hidden) return;
  if (streamTrendRequest) return;
  var key = selectedStreamKey;
  var generation = streamTrendGeneration;
  var ctrl = new AbortController();
  streamTrendRequest = ctrl;
  apiFetch("/api/v1/streams/" + encodedStreamPath(key), {signal:ctrl.signal}).then(function(data) {
    if (generation !== streamTrendGeneration || key !== selectedStreamKey || !data) return;
    var stats = data.stats || {};
    var publisher = String(data.publisher || "");
    var previous = streamTrendState.previous;
    // GOP generation advances on every keyframe. It is a cache label, not a
    // publisher lifecycle boundary, so it must not clear the rolling trend.
    // Only a publisher replacement (or a reset counter) starts a new series.
    var countersReset = previous && ((stats.video_frames != null && Number(stats.video_frames) < Number(previous.video_frames || 0)) ||
      (stats.audio_frames != null && Number(stats.audio_frames) < Number(previous.audio_frames || 0)));
    if ((streamTrendState.publisher != null && streamTrendState.publisher !== publisher) || countersReset) {
      streamTrendState.samples = {bitrate:[], videoFps:[], audioFps:[], gopDuration:[]};
      streamTrendState.previous = null;
      previous = null;
    }
    var videoFPS = previous && stats.video_frames != null ? Math.max(0, Number(stats.video_frames) - Number(previous.video_frames || 0)) : Number(stats.fps || 0);
    var audioFPS = previous && stats.audio_frames != null ? Math.max(0, Number(stats.audio_frames) - Number(previous.audio_frames || 0)) : 0;
    streamTrendState.generation = data.gop_generation;
    streamTrendState.publisher = publisher;
    streamTrendState.previous = {video_frames:stats.video_frames, audio_frames:stats.audio_frames};
    [ ["bitrate", Number(stats.bitrate_kbps || 0)], ["videoFps", videoFPS], ["audioFps", audioFPS], ["gopDuration", Number(data.gop_duration_ms || 0) / 1000] ].forEach(function(item) {
      var values = streamTrendState.samples[item[0]]; values.push(item[1]); if (values.length > 60) values.splice(0, values.length - 60);
    });
    renderStreamTrend();
  }).catch(function(err) { if (!err || err.name !== "AbortError") showManagementError("streams", err); })
    .finally(function() {
      if (streamTrendRequest === ctrl) streamTrendRequest = null;
    });
}

function renderStreamTrend() {
  var detail = document.getElementById("stream-detail-row");
  if (!detail || !streamTrendState) return;
  document.getElementById("stream-detail-key").textContent = "  " + selectedStreamKey;
  var count = streamTrendState.samples.bitrate.length;
  document.getElementById("stream-detail-samples").textContent = "  " + count + " / 60 samples";
  document.getElementById("stream-trend-empty").style.display = count ? "none" : "block";
  var labels = {bitrate:"kbps", videoFps:"fps", audioFps:"fps", gopDuration:"s"};
  Object.keys(labels).forEach(function(metric) {
    var values = streamTrendState.samples[metric] || [];
    var path = detail.querySelector('[data-path="' + metric + '"]');
    var area = detail.querySelector('[data-area="' + metric + '"]');
    var currentNodes = detail.querySelectorAll('[data-current="' + metric + '"]');
    var minNode = detail.querySelector('[data-min="' + metric + '"]');
    var maxNode = detail.querySelector('[data-max="' + metric + '"]');
    if (!values.length) { path.setAttribute("d", ""); area.setAttribute("d", ""); currentNodes.forEach(function(node) { node.textContent = "-"; }); minNode.textContent = "min -"; maxNode.textContent = "max -"; return; }
    var min = Math.min.apply(null, values), max = Math.max.apply(null, values), span = Math.max(1, max - min);
    var points = values.map(function(value, index) { var x = values.length === 1 ? 0 : index * 720 / (values.length - 1); var y = 70 - ((value - min) / span) * 58; return [x, y]; });
    var d = points.map(function(point, index) { return (index ? "L" : "M") + point[0].toFixed(1) + " " + point[1].toFixed(1); }).join(" ");
    path.setAttribute("d", d); area.setAttribute("d", d + " L720 70 L0 70 Z");
    currentNodes.forEach(function(node) { node.textContent = Number(values[values.length - 1]).toFixed(metric === "bitrate" ? 0 : 1) + " " + labels[metric]; });
    minNode.textContent = "min " + Number(min).toFixed(metric === "bitrate" ? 0 : 1);
    maxNode.textContent = "max " + Number(max).toFixed(metric === "bitrate" ? 0 : 1);
  });
}

/* --- DVR Status --- */
function refreshDVR(signal) {
  return apiFetch("/api/v1/dvr/status", { signal: signal })
    .then(function(d) {
      d = d || {};
      var card = document.getElementById("stat-dvr-card");
      if (!d.enabled) { card.style.display = "none"; return; }
      card.style.display = "";
      var sessions = d.sessions || [];
      document.getElementById("stat-dvr").textContent = sessions.length;
    })
    .catch(function(err) {
      if (!err || err.name !== "AbortError") document.getElementById("stat-dvr-card").style.display = "none";
    });
}

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

function isCurrentPublishRun(generation, pc) {
  return generation === publishRunGeneration && (!pc || publishPC === pc);
}

function updatePublishStartButton() {
  var button = document.getElementById("btn-start-publish");
  if (!button) return;
  var protocol = document.getElementById("publish-protocol").value;
  button.disabled = !canManage("operator") || (protocol === "whip" && !publishEndpointsReady);
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
  navigator.mediaDevices.getUserMedia(constraints).then(function(stream) {
    if (previewGeneration !== publishPreviewGeneration || publishPC || document.getElementById("publish-protocol").value !== "whip") {
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
    setPublishStatus("Camera/mic error: " + e.message, "error");
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
    if (publishPC || publishLabSessionID) return;
    document.getElementById("publish-protocol").value = button.getAttribute("data-protocol");
    updatePublishProtocol();
  });
});
document.getElementById("publish-stream-key").addEventListener("input", function() { updatePublishProtocol(true); });
document.getElementById("publish-sip-stream").addEventListener("input", updatePublishProtocol);
document.getElementById("publish-gb-stream").addEventListener("input", updatePublishProtocol);

function startPublish() {
  var protocol = document.getElementById("publish-protocol").value;
  if (protocol === "sip" || protocol === "gb28181") {
    startLabPublish(protocol);
    return;
  }
  if (!publishEndpointsReady) {
    setPublishStatus("WebRTC endpoint is still loading", "error");
    updatePublishStartButton();
    return;
  }
  var streamKey = document.getElementById("publish-stream-key").value.trim();
  if (!streamKey) {
    setPublishStatus("Enter a stream key", "error");
    return;
  }

  if (!publishStream) {
    setPublishStatus("No camera/mic available", "error");
    return;
  }

  var run = ++publishRunGeneration;
  setPublishStatus("Negotiating...");
  document.getElementById("btn-start-publish").disabled = true;

  var pc = new RTCPeerConnection({
    iceServers: [{ urls: "stun:stun.l.google.com:19302" }]
  });
  publishPC = pc;

  // Map codec selector value to MIME type.
  var codecMimeMap = {
    "VP8": "video/VP8", "VP9": "video/VP9",
    "H264": "video/H264", "H265": "video/H265",
    "AV1": "video/AV1"
  };
  var selectedCodec = document.getElementById("publish-video-codec").value;
  var preferredMime = codecMimeMap[selectedCodec] || "video/H264";

  publishStream.getTracks().forEach(function(track) {
    var sender = pc.addTrack(track, publishStream);
    // Apply codec preference on the video transceiver.
    if (track.kind === "video") {
      var transceiver = pc.getTransceivers().find(function(t) { return t.sender === sender; });
      if (transceiver && transceiver.setCodecPreferences) {
        try {
          var allCodecs = RTCRtpSender.getCapabilities("video").codecs || [];
          // Put preferred codec first, then everything else as fallback.
          var preferred = allCodecs.filter(function(c) {
            return c.mimeType.toLowerCase() === preferredMime.toLowerCase();
          });
          var others = allCodecs.filter(function(c) {
            return c.mimeType.toLowerCase() !== preferredMime.toLowerCase();
          });
          if (preferred.length > 0) {
            transceiver.setCodecPreferences(preferred.concat(others));
          }
        } catch (e) {
          console.warn("setCodecPreferences failed:", e);
        }
      }
    }
  });

  pc.oniceconnectionstatechange = function() {
    if (!isCurrentPublishRun(run, pc)) return;
    var state = pc.iceConnectionState;
    if (state === "connected" || state === "completed") {
      setPublishStatus("Publishing", "ok");
      document.getElementById("connection-dot").classList.add("ready");
      document.getElementById("connection-state").textContent = "Publishing live";
      document.getElementById("preview-state").textContent = "LIVE / WHIP";
      document.getElementById("btn-start-publish").style.display = "none";
      document.getElementById("btn-stop-publish").style.display = "inline-block";
      document.getElementById("btn-publish-stats").style.display = "inline-block";
      startPublishStatsOverlay(pc);
    } else if (state === "failed" || state === "disconnected" || state === "closed") {
      setPublishStatus("Disconnected: " + state, "error");
      cleanupPublishPC();
    }
  };

  var port = portFromListen(endpoints.webrtc);
  var proto = location.protocol === "https:" ? "https://" : "http://";
  var url = proto + location.hostname + ":" + port + "/webrtc/whip/" + streamKey;

  pc.createOffer().then(function(offer) {
    if (!isCurrentPublishRun(run, pc)) throw new Error("publish run cancelled");
    return pc.setLocalDescription(offer);
  }).then(function() {
    if (!isCurrentPublishRun(run, pc)) throw new Error("publish run cancelled");
    // Wait for at least one ICE candidate (host) or gathering complete,
    // whichever comes first. Avoids waiting for slow STUN/TURN timeouts.
    return new Promise(function(resolve) {
      if (pc.iceGatheringState === "complete") { resolve(); return; }
      var resolved = false;
      pc.onicecandidate = function(e) {
        if (!resolved && (e.candidate === null || e.candidate.type === "host")) {
          resolved = true;
          resolve();
        }
      };
      pc.onicegatheringstatechange = function() {
        if (!resolved && pc.iceGatheringState === "complete") {
          resolved = true;
          resolve();
        }
      };
      // Safety timeout: don't wait more than 500ms for candidates
      setTimeout(function() { if (!resolved) { resolved = true; resolve(); } }, 500);
    });
  }).then(function() {
    if (!isCurrentPublishRun(run, pc)) throw new Error("publish run cancelled");
    return fetch(url, {
      method: "POST",
      headers: { "Content-Type": "application/sdp" },
      body: pc.localDescription.sdp
    });
  }).then(function(resp) {
    var loc = resp.headers.get("Location");
    var sessionURL = loc ? proto + location.hostname + ":" + port + loc : "";
    return resp.text().then(function(sdp) {
      if (resp.status !== 201) throw new Error("WHIP " + resp.status + ": " + sdp);
      if (!isCurrentPublishRun(run, pc)) {
        if (sessionURL) fetch(sessionURL, {method:"DELETE"}).catch(function(){});
        pc.close();
        throw new Error("publish run cancelled");
      }
      publishSessionURL = sessionURL;
      return sdp;
    });
  }).then(function(sdp) {
    if (!isCurrentPublishRun(run, pc)) throw new Error("publish run cancelled");
    return pc.setRemoteDescription({ type: "answer", sdp: sdp });
  }).then(function() {
    if (!isCurrentPublishRun(run, pc)) return;
    var s = pc.iceConnectionState;
    if (s !== "connected" && s !== "completed") {
      setPublishStatus("WebRTC connected, waiting for ICE...");
    }
  }).catch(function(e) {
    if (isCurrentPublishRun(run, pc)) {
      setPublishStatus("WHIP error: " + e.message, "error");
      cleanupPublishPC();
    } else if (pc !== publishPC) {
      pc.close();
    }
  });
}

function stopPublish() {
  publishRunGeneration++;
  stopPublishStatsOverlay();
  if (publishPC) {
    publishPC.close();
    publishPC = null;
  }
  if (publishSessionURL) {
    fetch(publishSessionURL, { method: "DELETE" }).catch(function(){});
    publishSessionURL = null;
  }
  if (publishLabSessionID && publishLabProtocol) {
    var labPath = publishLabProtocol === "sip" ? "/api/v1/sipgateway/lab/sessions/" : "/api/v1/gb28181/lab/sessions/";
    apiFetch(labPath + encodeURIComponent(publishLabSessionID), {method:"DELETE"}).catch(function(){});
    publishLabSessionID = null;
    publishLabProtocol = null;
  }
  document.getElementById("btn-start-publish").style.display = "inline-block";
  updatePublishStartButton();
  document.getElementById("btn-stop-publish").style.display = "none";
  document.getElementById("btn-publish-stats").style.display = "none";
  publishStatsVisible = false;
  var btn = document.getElementById("btn-publish-stats");
  if (btn) btn.classList.remove("active");
  document.getElementById("connection-dot").classList.remove("ready");
  document.getElementById("connection-state").textContent = "Ready when you are";
  document.getElementById("preview-state").textContent = "READY TO PUBLISH";

  // Restart preview only for the legacy in-console modal.
  if (location.pathname !== "/console/publish") startPreview();
}

function updatePublishProtocol(skipPreview) {
  var protocol = document.getElementById("publish-protocol").value;
  document.querySelectorAll(".protocol-switch [data-protocol]").forEach(function(button) {
    var active = button.getAttribute("data-protocol") === protocol;
    button.classList.toggle("active", active);
    button.setAttribute("aria-selected", active ? "true" : "false");
  });
  var activeProtocol = publishLabProtocol || (publishPC ? "whip" : "");
  if (activeProtocol && activeProtocol !== protocol) stopPublish();
  document.getElementById("publish-whip-config").hidden = protocol !== "whip";
  document.getElementById("publish-sip-config").hidden = protocol !== "sip";
  document.getElementById("publish-gb-config").hidden = protocol !== "gb28181";
  document.getElementById("publish-whip-config").classList.toggle("active", protocol === "whip");
  document.getElementById("publish-sip-config").classList.toggle("active", protocol === "sip");
  document.getElementById("publish-gb-config").classList.toggle("active", protocol === "gb28181");
  var preview = document.querySelector(".publish-preview-wrap");
  if (preview) preview.style.display = protocol === "whip" ? "" : "none";
  if (protocol !== "whip") stopPreview();
  else if (!publishPC && !skipPreview) startPreview();
  var isWhip = protocol === "whip";
  document.getElementById("connection-note").textContent = isWhip ? "Camera and microphone are local only" : protocol === "sip" ? "SIP lab device publishes through RTP" : "GB28181 lab device publishes through PS/RTP";
  document.getElementById("preflight-source-label").textContent = isWhip ? "Camera + microphone" : protocol === "sip" ? "SIP lab device" : "GB28181 lab device";
  document.getElementById("preflight-transport-label").textContent = isWhip ? "WebRTC endpoint" : protocol === "sip" ? "SIP gateway" : "GB28181 gateway";
  document.getElementById("route-hint").textContent = isWhip ? "WHIP /webrtc/whip/" + document.getElementById("publish-stream-key").value.trim() : protocol === "sip" ? "SIP lab /api/v1/sipgateway/lab/sessions" : "GB lab /api/v1/gb28181/lab/sessions";
  setPublishStatus("");
  updatePublishStartButton();
}

function startLabPublish(protocol) {
  if (!canManage("operator")) return;
  var run = ++publishRunGeneration;
  var button = document.getElementById("btn-start-publish");
  var request;
  if (protocol === "sip") {
    request = {mode:document.getElementById("publish-sip-mode").value, device_id:document.getElementById("publish-sip-device").value.trim(), stream_key:document.getElementById("publish-sip-stream").value.trim(), codec:document.getElementById("publish-sip-codec").value};
  } else {
    request = {mode:document.getElementById("publish-gb-mode").value, device_id:document.getElementById("publish-gb-device").value.trim(), channel_id:document.getElementById("publish-gb-channel").value.trim(), stream_key:document.getElementById("publish-gb-stream").value.trim()};
  }
  if (!validProtocolLabStreamKey(request.stream_key)) { setPublishStatus("Stream key segments must be non-empty and cannot be . or ..", "error"); return; }
  button.disabled = true;
  setPublishStatus("Starting " + protocol.toUpperCase() + " session...");
  var path = protocol === "sip" ? "/api/v1/sipgateway/lab/sessions" : "/api/v1/gb28181/lab/sessions";
  apiFetch(path, {method:"POST", headers:{"Content-Type":"application/json"}, body:JSON.stringify(request)})
    .then(function(data) {
      var session = data && data.session ? data.session : data;
      if (!isCurrentPublishRun(run)) {
        if (session && session.id) apiFetch(path + "/" + encodeURIComponent(session.id), {method:"DELETE"}).catch(function(){});
        return;
      }
      publishLabSessionID = session && session.id;
      publishLabProtocol = protocol;
      setPublishStatus(protocol.toUpperCase() + " publishing", "ok");
      document.getElementById("connection-dot").classList.add("ready");
      document.getElementById("connection-state").textContent = "Publishing live";
      document.getElementById("preview-state").textContent = "LIVE / " + protocol.toUpperCase();
      document.getElementById("btn-start-publish").style.display = "none";
      document.getElementById("btn-stop-publish").style.display = "inline-block";
      showToast(protocol.toUpperCase() + " publish started");
    })
    .catch(function(err) {
      if (isCurrentPublishRun(run)) setPublishStatus(managementErrorMessage(err), "error");
    })
    .finally(function() {
      if (isCurrentPublishRun(run)) updatePublishStartButton();
    });
}

function cleanupPublishPC() {
  var sessionURL = publishSessionURL;
  publishPC = null;
  publishSessionURL = null;
  if (sessionURL) fetch(sessionURL, {method:"DELETE"}).catch(function(){});
  document.getElementById("connection-dot").classList.remove("ready");
  document.getElementById("connection-state").textContent = "Ready when you are";
  document.getElementById("preview-state").textContent = "READY TO PUBLISH";
  document.getElementById("btn-start-publish").style.display = "inline-block";
  updatePublishStartButton();
  document.getElementById("btn-stop-publish").style.display = "none";
  document.getElementById("btn-publish-stats").style.display = "none";
  stopPublishStatsOverlay();
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
