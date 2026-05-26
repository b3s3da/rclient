// Minimal login form glue. Posts JSON to /api/login under the panel prefix
// and lets the server set a session cookie on success.
"use strict";

const $ = (id) => document.getElementById(id);

function panelBase() {
  // Login lives at <prefix>/login; strip "login" off the end so we know the
  // base prefix and can derive the API path and the post-login redirect.
  return location.pathname.replace(/login\/?$/, "");
}

const form = $("loginform");
const errEl = $("err");
const btn = $("submit");

form.addEventListener("submit", async (e) => {
  e.preventDefault();
  errEl.textContent = "";
  btn.disabled = true;
  const original = btn.textContent;
  btn.textContent = "Signing in…";
  try {
    const res = await fetch(panelBase() + "api/login", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        user: $("user").value,
        password: $("password").value,
      }),
      credentials: "same-origin",
    });
    if (res.ok) {
      location.href = panelBase();
      return;
    }
    if (res.status === 429) {
      errEl.textContent = "Too many attempts — try again later.";
    } else if (res.status === 401) {
      errEl.textContent = "Invalid credentials.";
    } else {
      errEl.textContent = `Server error (${res.status}).`;
    }
  } catch (e) {
    errEl.textContent = "Network error: " + e.message;
  } finally {
    btn.disabled = false;
    btn.textContent = original;
  }
});

$("user").focus();
