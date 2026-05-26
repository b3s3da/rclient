// rclient panel client.
//
// Security note: untrusted strings (hostname, os, shell ids from agents) are
// always written via textContent. Shell output is binary and goes straight to
// xterm.write(...) which interprets terminal escape sequences but never HTML.
"use strict";

const $ = (id) => document.getElementById(id);
const agents = new Map();    // agent_id -> AgentInfo
let selected = null;          // currently selected agent_id
let ws = null;
let reconnectDelay = 1000;

// shells: shell_id -> { agentID, term, fit, pane, tab, alive }
const shells = new Map();
let activeShell = null;

function fmtBytes(mb) {
  if (mb >= 1024) return (mb / 1024).toFixed(1) + " GB";
  return mb + " MB";
}
function fmtUptime(sec) {
  const d = Math.floor(sec / 86400);
  const h = Math.floor((sec % 86400) / 3600);
  const m = Math.floor((sec % 3600) / 60);
  if (d) return `${d}d ${h}h`;
  if (h) return `${h}h ${m}m`;
  return `${m}m`;
}

function setStatus(text, cls) {
  const s = $("status");
  s.textContent = text;
  s.className = "status " + (cls || "");
}

function el(tag, props, ...children) {
  const node = document.createElement(tag);
  if (props) {
    for (const [k, v] of Object.entries(props)) {
      if (k === "class") node.className = v;
      else if (k === "text") node.textContent = v;
      else if (k.startsWith("on")) node[k] = v;
      else node.setAttribute(k, v);
    }
  }
  for (const c of children) {
    if (c == null) continue;
    node.appendChild(typeof c === "string" ? document.createTextNode(c) : c);
  }
  return node;
}

// uuid helper, browser-only.
function uuid() {
  if (crypto.randomUUID) return crypto.randomUUID();
  // Fallback for older browsers.
  return "xxxxxxxxxxxx4xxxyxxxxxxxxxxxxxxx".replace(/[xy]/g, (c) => {
    const r = Math.random() * 16 | 0;
    const v = c === "x" ? r : (r & 0x3 | 0x8);
    return v.toString(16);
  });
}

// --- agents list ---

function renderAgents() {
  const list = $("agents");
  list.replaceChildren();
  const sorted = [...agents.values()].sort((a, b) =>
    (a.hostname || a.agent_id).localeCompare(b.hostname || b.agent_id));
  for (const a of sorted) {
    const row = el("div", {
      class: "agent" + (a.connected ? " online" : "") + (a.agent_id === selected ? " active" : ""),
      onclick: () => selectAgent(a.agent_id),
    },
      el("span", { class: "pulse" }),
      el("span", { class: "name" },
        el("div", { class: "host", text: a.hostname || a.agent_id }),
        el("div", { class: "meta", text: `${a.os || "?"}/${a.arch || "?"}` }),
      ),
    );
    list.appendChild(row);
  }
}

function selectAgent(id) {
  if (selected === id) return;
  selected = id;
  renderAgents();
  const a = agents.get(id);
  if (!a) return;
  const info = $("info");
  info.replaceChildren(
    el("span", { class: "eyebrow", text: a.connected ? "Online" : "Offline" }),
    el("h1", null,
      el("span", { class: "grad", text: a.hostname || a.agent_id }),
    ),
    el("div", {
      class: "sub",
      text: `${a.agent_id} · ${a.os}/${a.arch} · kernel ${a.kernel || "?"} · agent ${a.version || "?"}`,
    }),
  );
  $("metrics").classList.remove("hidden");
  $("shell").classList.remove("hidden");

  // Hide every shell pane that doesn't belong to this agent and surface one
  // for it; if there isn't one yet, open one automatically.
  let firstForAgent = null;
  for (const sh of shells.values()) {
    const visible = sh.agentID === id;
    sh.tab.style.display = visible ? "" : "none";
    sh.pane.classList.toggle("active", false);
    if (visible && !firstForAgent) firstForAgent = sh;
  }
  if (firstForAgent) {
    setActiveShell(firstForAgent.id);
  } else if (a.connected) {
    openShell(id);
  }
  renderMetrics(a.last_metrics);
}

function renderMetrics(m) {
  if (!m) {
    for (const k of ["m_cpu", "m_mem", "m_disk", "m_load", "m_up"]) $(k).textContent = "—";
    return;
  }
  $("m_cpu").textContent = m.cpu_percent.toFixed(1) + "%";
  $("m_mem").textContent = `${m.mem_percent.toFixed(1)}% (${fmtBytes(m.mem_used_mb)} / ${fmtBytes(m.mem_total_mb)})`;
  $("m_disk").textContent = m.disk_percent.toFixed(1) + "%";
  $("m_load").textContent = `${m.load1.toFixed(2)} ${m.load5.toFixed(2)} ${m.load15.toFixed(2)}`;
  $("m_up").textContent = fmtUptime(m.uptime_sec);
}

// --- shells / xterm ---

function openShell(agentID) {
  const id = uuid();
  // Wait for the terminal font so xterm measures cell width against the
  // real glyphs, otherwise the first paint is misaligned.
  if (document.fonts && document.fonts.load) {
    document.fonts.load("13.5px 'JetBrains Mono'").catch(() => {});
  }
  const term = new Terminal({
    fontFamily: "'JetBrains Mono', ui-monospace, SFMono-Regular, Menlo, Consolas, monospace",
    fontSize: 13.5,
    lineHeight: 1.25,
    letterSpacing: 0,
    cursorBlink: true,
    cursorStyle: "bar",
    convertEol: true,
    scrollback: 5000,
    theme: {
      background: "rgba(0,0,0,0)", // transparent — let glass show through
      foreground: "#e9ecf3",
      cursor: "#00e0ff",
      cursorAccent: "#06080f",
      selectionBackground: "rgba(123,92,255,0.35)",

      // Normal ANSI palette — leans toward our cyan/violet brand on the
      // bright end without making `ls` colours unreadable.
      black:   "#1c2230",
      red:     "#ef6b6b",
      green:   "#5fd49a",
      yellow:  "#f0c674",
      blue:    "#7aa6ff",
      magenta: "#c792ea",
      cyan:    "#67e8f9",
      white:   "#cdd3e1",

      brightBlack:   "#5a6675",
      brightRed:     "#ff8c8c",
      brightGreen:   "#86efac",
      brightYellow:  "#fde68a",
      brightBlue:    "#93c5fd",
      brightMagenta: "#e9c5ff",
      brightCyan:    "#a7f3ff",
      brightWhite:   "#f4f7fb",
    },
  });
  const fit = new FitAddon.FitAddon();
  term.loadAddon(fit);

  const pane = el("div", { class: "term-pane" });
  $("terms").appendChild(pane);
  term.open(pane);
  fit.fit();

  // Refit whenever the pane changes size (layout shifts, devtools, etc.).
  // Without this the last terminal row drifts out of the viewport when the
  // window or sidebar geometry changes between paints.
  const ro = new ResizeObserver(() => {
    try { fit.fit(); } catch (_) {}
  });
  ro.observe(pane);

  // Tab UI.
  const a = agents.get(agentID);
  const label = (a && a.hostname) ? a.hostname : "shell";
  const tabLabel = el("span", { text: `${label} #${[...shells.values()].filter(s => s.agentID === agentID).length + 1}` });
  const closeBtn = el("span", {
    class: "x",
    text: "×",
    onclick: (e) => { e.stopPropagation(); closeShell(id); },
  });
  const tab = el("div", {
    class: "tab",
    onclick: () => setActiveShell(id),
  }, tabLabel, closeBtn);
  $("tabs").appendChild(tab);

  const sh = { id, agentID, term, fit, pane, tab, ro, alive: true };
  shells.set(id, sh);

  // Send initial open with the current geometry.
  send({
    type: "panel_shell_open",
    data: {
      agent_id: agentID, shell_id: id,
      cols: term.cols, rows: term.rows,
    },
  });

  // Wire keyboard input.
  term.onData((data) => {
    if (!sh.alive) return;
    send({
      type: "panel_shell_input",
      data: {
        agent_id: agentID, shell_id: id,
        data: btoa(unescape(encodeURIComponent(data))),
      },
    });
  });

  // Send winsize updates when the terminal is resized.
  term.onResize(({ cols, rows }) => {
    if (!sh.alive) return;
    send({
      type: "panel_shell_resize",
      data: { agent_id: agentID, shell_id: id, cols, rows },
    });
  });

  setActiveShell(id);

  // Once the font finishes loading the cell metrics change — refit so the
  // active terminal aligns properly even if it opened before the font
  // arrived.
  if (document.fonts && document.fonts.ready) {
    document.fonts.ready.then(() => {
      try { fit.fit(); } catch (_) {}
    });
  }
  return sh;
}

function setActiveShell(id) {
  const sh = shells.get(id);
  if (!sh) return;
  for (const s of shells.values()) {
    const isMe = s.id === id;
    s.pane.classList.toggle("active", isMe);
    s.tab.classList.toggle("active", isMe);
  }
  activeShell = id;
  // Refit and refocus shortly so the layout settles first. We do it in two
  // ticks: the first lets the pane become `display: block` so xterm can
  // measure it, the second handles any rounding the FitAddon got wrong on
  // the very first measurement (cell height with line-height fractions).
  requestAnimationFrame(() => {
    try { sh.fit.fit(); } catch (_) {}
    requestAnimationFrame(() => {
      try { sh.fit.fit(); } catch (_) {}
      sh.term.focus();
    });
  });
}

function closeShell(id) {
  const sh = shells.get(id);
  if (!sh) return;
  if (sh.alive) {
    send({
      type: "panel_shell_close",
      data: { agent_id: sh.agentID, shell_id: id },
    });
  }
  destroyShell(id);
}

function destroyShell(id) {
  const sh = shells.get(id);
  if (!sh) return;
  sh.alive = false;
  try { sh.ro.disconnect(); } catch (_) {}
  try { sh.term.dispose(); } catch (_) {}
  sh.pane.remove();
  sh.tab.remove();
  shells.delete(id);
  if (activeShell === id) {
    // Activate any remaining shell for the same agent, otherwise none.
    const next = [...shells.values()].find((s) => s.agentID === selected);
    activeShell = null;
    if (next) setActiveShell(next.id);
  }
}

// Refit all visible terminals on window resize.
window.addEventListener("resize", () => {
  for (const s of shells.values()) {
    try { s.fit.fit(); } catch (_) {}
  }
});

$("newtab").addEventListener("click", () => {
  if (!selected) return;
  openShell(selected);
});

$("logout").addEventListener("click", async () => {
  try {
    const base = location.pathname.replace(/\/?$/, "/");
    await fetch(base + "api/logout", { method: "POST", credentials: "same-origin" });
  } catch (_) { /* ignore */ }
  location.href = location.pathname.replace(/\/?$/, "") + "/login";
});

// --- add device modal ---

const modal = $("modal");
function openModal() { modal.classList.remove("hidden"); modal.setAttribute("aria-hidden", "false"); }
function closeModal() { modal.classList.add("hidden"); modal.setAttribute("aria-hidden", "true"); }

modal.addEventListener("click", (e) => {
  if (e.target.dataset && "close" in e.target.dataset) closeModal();
});
window.addEventListener("keydown", (e) => {
  if (e.key === "Escape" && !modal.classList.contains("hidden")) closeModal();
});

$("addbtn").addEventListener("click", async () => {
  const base = location.pathname.replace(/\/?$/, "/");
  let data;
  try {
    const res = await fetch(base + "api/connect", { credentials: "same-origin" });
    if (!res.ok) throw new Error("HTTP " + res.status);
    data = await res.json();
  } catch (e) {
    alert("Couldn't fetch connect token: " + e.message);
    return;
  }
  $("connectblob").value = data.connect;
  // Connect blob is URL-safe base64 (only [A-Za-z0-9_-]), so it doesn't
  // need shell quoting — keep the line copy-pasteable as-is.
  $("connectcmd").value = "sudo ./rclient-agent install --connect " + data.connect;
  openModal();
  // Pre-select the blob so a single Ctrl+A or click into it grabs the lot.
  setTimeout(() => $("connectblob").select(), 30);
});

document.addEventListener("click", async (e) => {
  const btn = e.target.closest("[data-copy]");
  if (!btn) return;
  const target = document.querySelector(btn.dataset.copy);
  if (!target) return;
  try {
    await navigator.clipboard.writeText(target.value);
  } catch (_) {
    target.select();
    document.execCommand("copy");
  }
  const original = btn.textContent;
  btn.textContent = "Copied";
  btn.classList.add("copied");
  setTimeout(() => {
    btn.textContent = original;
    btn.classList.remove("copied");
  }, 1400);
});

// --- transport ---

function send(env) {
  if (ws && ws.readyState === 1) ws.send(JSON.stringify(env));
}

function connect() {
  const proto = location.protocol === "https:" ? "wss" : "ws";
  const base = location.pathname.replace(/\/?$/, "");
  ws = new WebSocket(`${proto}://${location.host}${base}/ws`);

  ws.onopen = () => {
    setStatus("connected", "ok");
    reconnectDelay = 1000;
  };
  ws.onclose = () => {
    setStatus("disconnected — retrying", "err");
    // Mark every shell as not alive so we stop sending into a dead socket.
    for (const sh of shells.values()) sh.alive = false;
    setTimeout(connect, reconnectDelay);
    reconnectDelay = Math.min(reconnectDelay * 2, 15000);
  };
  ws.onerror = () => { /* close will follow */ };
  ws.onmessage = (e) => {
    const env = JSON.parse(e.data);
    handle(env);
  };
}

function decodeB64(s) {
  // Decode base64 to a UTF-8 string. Output is fed straight into xterm.write
  // which understands ANSI escape sequences but never interprets HTML.
  const bin = atob(s);
  const bytes = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
  return new TextDecoder("utf-8", { fatal: false }).decode(bytes);
}

function handle(env) {
  switch (env.type) {
    case "agent_list":
      agents.clear();
      for (const a of (env.data.agents || [])) agents.set(a.agent_id, a);
      renderAgents();
      break;
    case "agent_connected":
      agents.set(env.data.agent_id, env.data);
      renderAgents();
      break;
    case "agent_disconnected": {
      const a = agents.get(env.data.agent_id);
      if (a) { a.connected = false; agents.set(a.agent_id, a); }
      renderAgents();
      // Shells for that agent are now dead; drop them.
      for (const sh of [...shells.values()]) {
        if (sh.agentID === env.data.agent_id) {
          sh.term.write("\r\n\x1b[31m[agent disconnected]\x1b[0m\r\n");
          sh.alive = false;
        }
      }
      break;
    }
    case "agent_metrics": {
      const a = agents.get(env.data.agent_id);
      if (a) {
        a.last_metrics = env.data.metrics;
        agents.set(a.agent_id, a);
      }
      if (env.data.agent_id === selected) renderMetrics(env.data.metrics);
      break;
    }
    case "agent_shell_output": {
      const sh = shells.get(env.data.output.shell_id);
      if (!sh) return;
      sh.term.write(decodeB64(env.data.output.data));
      break;
    }
    case "agent_shell_exit": {
      const sh = shells.get(env.data.exit.shell_id);
      if (!sh) return;
      const reason = env.data.exit.reason || "";
      sh.term.write(`\r\n\x1b[2m[exit ${env.data.exit.exit_code}${reason ? ": " + reason : ""}]\x1b[0m\r\n`);
      sh.alive = false;
      break;
    }
  }
}

connect();
