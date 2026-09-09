// Translate UI text nodes in place so language changes never replace live controls.
var consoleLanguage = "en";
var consoleTranslations = {
  "Console":"控制台", "API connected":"API 已连接", "Workspace":"工作区", "Operations":"运维", "System":"系统",
  "Streams":"流列表", "SIP Calls":"SIP 通话", "Storage":"存储", "Cluster":"集群", "Configuration":"配置", "Security & Audit":"安全与审计",
  "Open Publish Workspace":"打开推流工作区", "Sign out":"退出登录", "Sign in":"登录", "Resume":"恢复", "Session expired.":"登录已过期。",
  "Runtime Config":"运行时配置", "Refresh":"刷新", "\u21bb Refresh":"\u21bb 刷新", "Loader":"加载器", "Source":"来源", "Version":"版本", "Failures":"失败次数",
  "Active hash":"当前哈希", "Last success":"上次成功", "Last attempt":"上次尝试", "Callback failures":"回调失败", "Accepted / rejected":"接受 / 拒绝", "Apply failures":"应用失败",
  "Pending Restart":"等待重启", "None":"无", "Desired Source Document":"期望配置文档", "Read-only":"只读", "Validate":"校验", "Apply & Refresh":"应用并刷新",
  "Discard changes":"放弃修改", "Effective Applied Document":"已生效配置文档", "Applied":"已应用", "Selected Source Details":"配置源详情", "Configuration Schema":"配置规范",
  "Common Settings":"常用设置", "Server name":"服务器名称", "Log level":"日志级别", "Maximum streams":"最大流数", "Maximum connections":"最大连接数", "GOP cache":"GOP 缓存", "Recording":"录制", "Metrics":"监控指标",
  "Configuration Changes":"配置变更", "Effective applied document":"已生效配置文档", "Latest desired document":"最新期望配置文档", "Selected history revision":"选中的历史版本", "Compare draft":"比较草稿", "Before":"变更前", "Draft":"草稿",
  "Configuration History":"配置历史", "Select revision":"选择版本", "Load history":"加载历史", "Restore revision":"恢复版本", "No revisions":"暂无历史版本",
  "Unsaved changes":"有未保存的修改", "Applying":"正在应用", "Writable source":"可写配置源", "Read-only source":"只读配置源", "Applied; restart pending":"已应用；等待重启",
  "Invalid YAML":"YAML 格式无效", "No configuration changes":"无配置变更", "Redacted configuration changes":"已脱敏的配置变更", "Comparison unavailable":"无法比较配置",
  "Apply configuration":"应用配置", "Write the complete configuration document and schedule a refresh?":"写入完整配置文档并安排刷新？",
  "Configuration is valid":"配置校验通过", "Configuration written; refresh scheduled":"配置已写入；已安排刷新", "Configuration refresh scheduled":"已安排配置刷新",
  "Discard configuration changes":"放弃配置修改", "Replace the local draft with the latest desired source document?":"使用最新期望配置文档替换本地草稿？",
  "Restore configuration revision":"恢复配置版本", "Write the selected revision and schedule a refresh?":"写入选中的历史版本并安排刷新？",
  "Discard the current draft, write the selected revision, and schedule a refresh?":"放弃当前草稿，写入选中的历史版本并安排刷新？",
  "Leave configuration?":"离开配置页面？", "Discard unsaved configuration changes and leave this page?":"放弃未保存的配置修改并离开此页面？", "A configuration write is still in progress. Leave this page?":"配置仍在写入中。离开此页面？",
  "Session expired. Sign in again.":"登录已过期，请重新登录。", "This console role cannot perform that action.":"当前角色无权执行此操作。", "Insufficient console role":"当前角色权限不足",
  "Active Streams":"活跃流", "Publishing":"推流中", "Total Subscribers":"订阅总数", "Connections":"连接数", "Uptime":"运行时间", "DVR Sessions":"DVR 会话", "Search stream key":"搜索流标识",
  "Stream Key":"流标识", "Stream":"流", "Protocol":"协议", "Video":"视频", "Audio":"音频", "Subscribers":"订阅者", "Actions":"操作", "Preview":"预览", "Kick":"踢出", "Delete":"删除", "No active streams":"暂无活跃流",
  "Devices":"设备", "Channels":"通道", "Sessions":"会话", "Device ID":"设备 ID", "Channel ID":"通道 ID", "Name":"名称", "Status":"状态", "Transport":"传输", "Address":"地址", "Direction":"方向", "State":"状态", "No registered devices":"暂无注册设备", "No active media sessions":"暂无活跃媒体会话",
  "Cluster Relays":"集群转发", "Forward Relays":"前向转发", "Origin Relays":"回源转发", "Peers":"节点", "Snapshot":"快照", "Complete":"完整", "Peer Nodes":"集群节点", "Relay Paths":"转发路径",
  "Recent Calls":"最近通话", "Active Calls":"活跃通话", "Target URI":"目标 URI", "Dial":"呼叫", "Hang up":"挂断", "Detail":"详情", "Details":"详情", "Started":"开始时间", "Ended":"结束时间", "No calls":"暂无通话",
  "Recordings":"录制文件", "Recording Files":"录制文件", "Format":"格式", "Size":"大小", "Duration":"时长", "Play":"播放", "Download":"下载", "No recordings":"暂无录制文件", "Recording Detail":"录制详情", "DVR Detail":"DVR 详情",
  "Security":"安全", "Recent Audit":"最近审计", "API Authentication":"API 认证", "Console Role":"控制台角色", "Stream Authentication":"流认证", "Rate Limiting":"速率限制", "Audit Log":"审计日志", "Action":"操作", "Actor":"操作者", "Resource":"资源", "Result":"结果", "Time":"时间",
  "Back to Console":"返回控制台", "Publish Workspace":"推流工作区", "Start Publishing":"开始推流", "Stop Publishing":"停止推流", "Camera":"摄像头", "Microphone":"麦克风", "Resolution":"分辨率", "Frame Rate":"帧率", "Bitrate":"码率", "Start":"开始", "Stop":"停止", "Close":"关闭", "Cancel":"取消", "Confirm":"确认",
  "Language":"语言", "Management views":"管理视图", "Comparison baseline":"比较基准", "Configuration revision":"配置版本", "Desired source configuration document":"期望配置文档"
};
Object.assign(consoleTranslations, {
  "Video only: source audio requires transcoding.":"仅视频：源音频需要转码。",
  "WebRTC audio is unavailable without audio transcoding.":"未启用音频转码，无法播放此流的 WebRTC 音频。",
  "Named Tokens":"命名令牌", "Audit Entries":"审计记录", "Audit Events":"审计事件", "Console login":"控制台登录", "Legacy bearer":"旧版令牌", "Audit":"审计", "Active principal":"当前身份", "console session":"控制台会话", "Role Bindings":"角色绑定", "Role":"角色", "No named bearer bindings":"暂无命名令牌绑定", "No audit entries":"暂无审计记录", "Truncated":"已截断",
  "Immutable at runtime":"运行时不可修改", "Immutable":"不可修改", "Restart required":"需要重启", "Hot reload":"热更新",
  "Audit persistence":"审计持久化", "Audit write failures":"审计写入失败", "Unhealthy":"异常",
  "Active":"运行中", "Disabled":"已禁用", "Enabled":"已启用", "Configured":"已配置", "Ready":"就绪", "Healthy":"正常", "Unavailable":"不可用", "Low space":"空间不足",
  "Publishing on page":"本页推流数", "Subscribers on page":"本页订阅者", "Search recordings":"搜索录制文件",
  "Previous page":"上一页", "Next page":"下一页", "Stream pages":"流列表分页", "Call pages":"通话分页", "Recording pages":"录制文件分页", "Audit pages":"审计分页",
  "All results":"全部结果", "Principal":"操作者", "From":"起始时间", "Until":"结束时间", "Filter":"筛选", "Export NDJSON":"导出 NDJSON"
});
var consoleLocalizedNodes = [];
var consoleLocalizedAttributes = [];

function consoleText(value) {
  return consoleLanguage === "zh-CN" && Object.prototype.hasOwnProperty.call(consoleTranslations, value) ? consoleTranslations[value] : value;
}

function initializeConsoleLanguage() {
  var walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
  while (walker.nextNode()) {
    var node = walker.currentNode;
    if (!node.parentElement || node.parentElement.closest("script, style, pre, code, textarea")) continue;
    var value = node.textContent.trim();
    if (Object.prototype.hasOwnProperty.call(consoleTranslations, value)) consoleLocalizedNodes.push({node:node, value:value, original:node.textContent});
  }
  document.querySelectorAll("[aria-label], [placeholder], [title]").forEach(function(element) {
    ["aria-label", "placeholder", "title"].forEach(function(attribute) {
      var value = element.getAttribute(attribute);
      if (Object.prototype.hasOwnProperty.call(consoleTranslations, value)) consoleLocalizedAttributes.push({element:element, attribute:attribute, value:value});
    });
  });
  var language = "en";
  try { language = localStorage.getItem("liveforge.console.language") || "en"; } catch (_) {}
  setConsoleLanguage(language);
  document.getElementById("console-language").addEventListener("change", function() { setConsoleLanguage(this.value); });
}

function setConsoleLanguage(language) {
  consoleLanguage = language === "zh-CN" ? "zh-CN" : "en";
  document.documentElement.lang = consoleLanguage;
  document.getElementById("console-language").value = consoleLanguage;
  try { localStorage.setItem("liveforge.console.language", consoleLanguage); } catch (_) {}
  consoleLocalizedNodes.forEach(function(item) {
    if (!item.node.isConnected) return;
    var current = item.node.textContent.trim();
    if (current === item.value || current === consoleTranslations[item.value]) item.node.textContent = item.original.replace(item.value, consoleText(item.value));
  });
  consoleLocalizedAttributes.forEach(function(item) { item.element.setAttribute(item.attribute, consoleText(item.value)); });
  document.querySelectorAll("#config-enabled, #config-source-writable, #config-effective-state, #config-pending .ops-field-label, #security-console, #security-legacy, #security-audit, #security-audit-persistence, #storage-health, [data-action]").forEach(function(element) {
    var value = element.textContent.trim();
    var key = Object.keys(consoleTranslations).find(function(key) { return key === value || consoleTranslations[key] === value; });
    if (key) element.textContent = consoleText(key);
  });
  updateConfigEditorControls();
  if (!document.getElementById("config-comparison").hidden) compareConfigEditor();
}
