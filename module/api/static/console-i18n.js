// Console text is authored in English. Keep a shared helper for dynamic labels.
function consoleText(value) {
  return value;
}

function initializeConsoleLanguage() {
  document.documentElement.lang = "en";
  // Clear preferences saved by older releases without persisting draft data.
  try { localStorage.removeItem("liveforge.console.language"); } catch (_) {}
}
