var consolePages = {};

function initializeConsolePages() {
  document.querySelectorAll("[data-page]").forEach(function(nav) {
    var name = nav.dataset.page;
    consolePages[name] = {offset:0, limit:50, total:0, hasMore:false, query:"", sequence:0, serverPage:false};
    [-1, 1].forEach(function(direction) {
      var button = document.createElement("button");
      button.type = "button"; button.className = "btn";
      button.id = name + (direction < 0 ? "-previous" : "-next");
      button.textContent = direction < 0 ? "\u2190" : "\u2192";
      button.title = direction < 0 ? "Previous page" : "Next page";
      button.setAttribute("aria-label", button.title);
      button.disabled = true;
      button.addEventListener("click", function() { changeConsolePage(name, direction); });
      if (direction > 0) {
        var label = document.createElement("span"); label.id = name + "-page"; label.setAttribute("role", "status"); nav.appendChild(label);
      }
      nav.appendChild(button);
    });
    renderConsolePage(name);
  });
}

function prepareConsolePage(name, url, query) {
  var page = consolePages[name];
  var parsed = new URL(url, location.href);
  Object.keys(query || {}).forEach(function(key) { if (query[key]) parsed.searchParams.set(key, query[key]); });
  var filter = parsed.searchParams.toString();
  if (filter !== page.query) { page.query = filter; page.offset = 0; }
  parsed.searchParams.set("limit", page.limit);
  parsed.searchParams.set("offset", page.offset);
  return {url:parsed.pathname + parsed.search, sequence:++page.sequence};
}

function updateConsolePage(name, metadata) {
  if (!metadata || !Number.isFinite(metadata.total)) return;
  var page = consolePages[name];
  page.offset = metadata.offset;
  page.limit = metadata.limit;
  page.total = metadata.total;
  page.hasMore = metadata.has_more;
  page.serverPage = true;
  renderConsolePage(name);
}

function renderConsolePage(name) {
  var page = consolePages[name];
  document.getElementById(name + "-page").textContent = (page.total ? page.offset + 1 : 0) + " - " + Math.min(page.offset + page.limit, page.total) + " / " + page.total;
  document.getElementById(name + "-previous").disabled = page.offset === 0 || consoleSessionExpired;
  document.getElementById(name + "-next").disabled = !page.hasMore || consoleSessionExpired;
}

function changeConsolePage(name, direction) {
  var page = consolePages[name];
  if (consoleSessionExpired || (direction > 0 && !page.hasMore) || (direction < 0 && !page.offset)) return Promise.resolve();
  page.offset = Math.max(0, page.offset + direction * page.limit);
  return ({streams:refresh, sip:refreshSIPCalls, recordings:refreshStorage, audit:refreshSecurity})[name]();
}

function consoleAuditFilters() {
  var query = {order:"desc"};
  ["principal", "action", "result"].forEach(function(key) { query[key] = document.getElementById("audit-" + key).value.trim(); });
  ["since", "until"].forEach(function(key) {
    var value = document.getElementById("audit-" + key).value;
    if (value) query[key] = new Date(value).toISOString();
  });
  return query;
}

function exportConsoleAudit() {
  var button = document.getElementById("audit-export");
  if (!canManage("viewer")) return Promise.resolve();
  button.disabled = true;
  var query = consoleAuditFilters(); query.format = "ndjson";
  return apiFetch("/api/v1/audit", {query:query, responseType:"blob"}).then(function(blob) {
    var url = URL.createObjectURL(blob), link = document.createElement("a");
    link.href = url; link.download = "liveforge-audit.ndjson"; link.click();
    setTimeout(function() { URL.revokeObjectURL(url); }, 1000);
  }).catch(function(err) { showManagementError("security", err); }).finally(function() { button.disabled = !canManage("viewer"); });
}
