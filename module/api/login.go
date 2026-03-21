package api

var loginHTML = []byte(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>LiveForge - Login</title>
<style>
:root { --bg: #0f0f0f; --surface: #1a1a1a; --border: #2a2a2a; --text: #e0e0e0; --text2: #888; --accent: #6c9eff; --red: #f87171; }
* { margin: 0; padding: 0; box-sizing: border-box; }
body { font-family: -apple-system, BlinkMacSystemFont, "SF Pro Display", "Segoe UI", system-ui, sans-serif; background: var(--bg); color: var(--text); min-height: 100vh; display: flex; align-items: center; justify-content: center; }
.card { background: var(--surface); border: 1px solid var(--border); border-radius: 12px; padding: 40px; width: 360px; }
.logo { display: flex; align-items: center; gap: 12px; margin-bottom: 32px; justify-content: center; }
.logo svg { width: 28px; height: 28px; }
.logo h1 { font-size: 18px; font-weight: 600; }
.logo span { color: var(--text2); font-weight: 400; }
label { display: block; font-size: 13px; color: var(--text2); margin-bottom: 6px; text-transform: uppercase; letter-spacing: 0.5px; }
input { width: 100%; padding: 10px 14px; background: var(--bg); border: 1px solid var(--border); border-radius: 6px; color: var(--text); font-size: 14px; margin-bottom: 20px; outline: none; }
input:focus { border-color: var(--accent); }
button { width: 100%; padding: 10px; background: var(--accent); color: #fff; border: none; border-radius: 6px; font-size: 14px; font-weight: 500; cursor: pointer; }
button:hover { opacity: 0.9; }
</style>
</head>
<body>
<div class="card">
  <div class="logo">
    <svg viewBox="0 0 28 28" fill="none"><rect width="28" height="28" rx="6" fill="#6c9eff" fill-opacity="0.15"/><path d="M8 10l4-3v14l-4-3V10zM14 7l6 4v6l-6 4V7z" fill="#6c9eff"/></svg>
    <h1>LiveForge <span>Console</span></h1>
  </div>
  <form method="POST" action="/console/login">
    <label for="username">Username</label>
    <input type="text" id="username" name="username" autocomplete="username" required autofocus>
    <label for="password">Password</label>
    <input type="password" id="password" name="password" autocomplete="current-password" required>
    <button type="submit">Sign In</button>
  </form>
</div>
</body>
</html>`)

var loginFailHTML = []byte(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>LiveForge - Login</title>
<style>
:root { --bg: #0f0f0f; --surface: #1a1a1a; --border: #2a2a2a; --text: #e0e0e0; --text2: #888; --accent: #6c9eff; --red: #f87171; }
* { margin: 0; padding: 0; box-sizing: border-box; }
body { font-family: -apple-system, BlinkMacSystemFont, "SF Pro Display", "Segoe UI", system-ui, sans-serif; background: var(--bg); color: var(--text); min-height: 100vh; display: flex; align-items: center; justify-content: center; }
.card { background: var(--surface); border: 1px solid var(--border); border-radius: 12px; padding: 40px; width: 360px; }
.logo { display: flex; align-items: center; gap: 12px; margin-bottom: 32px; justify-content: center; }
.logo svg { width: 28px; height: 28px; }
.logo h1 { font-size: 18px; font-weight: 600; }
.logo span { color: var(--text2); font-weight: 400; }
label { display: block; font-size: 13px; color: var(--text2); margin-bottom: 6px; text-transform: uppercase; letter-spacing: 0.5px; }
input { width: 100%; padding: 10px 14px; background: var(--bg); border: 1px solid var(--border); border-radius: 6px; color: var(--text); font-size: 14px; margin-bottom: 20px; outline: none; }
input:focus { border-color: var(--accent); }
button { width: 100%; padding: 10px; background: var(--accent); color: #fff; border: none; border-radius: 6px; font-size: 14px; font-weight: 500; cursor: pointer; }
button:hover { opacity: 0.9; }
.error { background: rgba(248,113,113,0.1); border: 1px solid rgba(248,113,113,0.3); color: var(--red); padding: 10px 14px; border-radius: 6px; font-size: 13px; margin-bottom: 20px; }
</style>
</head>
<body>
<div class="card">
  <div class="logo">
    <svg viewBox="0 0 28 28" fill="none"><rect width="28" height="28" rx="6" fill="#6c9eff" fill-opacity="0.15"/><path d="M8 10l4-3v14l-4-3V10zM14 7l6 4v6l-6 4V7z" fill="#6c9eff"/></svg>
    <h1>LiveForge <span>Console</span></h1>
  </div>
  <form method="POST" action="/console/login">
    <div class="error">Invalid username or password</div>
    <label for="username">Username</label>
    <input type="text" id="username" name="username" autocomplete="username" required autofocus>
    <label for="password">Password</label>
    <input type="password" id="password" name="password" autocomplete="current-password" required>
    <button type="submit">Sign In</button>
  </form>
</div>
</body>
</html>`)
