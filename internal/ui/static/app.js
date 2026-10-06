/* Inbox UI for email-api. Vanilla JS, no build step. Talks to the keyed API on
   the same origin; the key lives in localStorage only. */
(() => {
  const $ = (id) => document.getElementById(id);
  const KEY = "email-api-key";
  const state = { box: "inbox", q: "", messages: [], selected: null, current: null, loadingMore: false, before: null, html: true, imagesLoaded: false };

  // ── api ──────────────────────────────────────────────────────────────
  const api = {
    key: () => localStorage.getItem(KEY) || "",
    async req(path, opts = {}) {
      const res = await fetch(path, { ...opts, headers: { "X-API-Key": api.key(), ...(opts.headers || {}) } });
      if (res.status === 401) { showKeyDialog("That key was not accepted."); throw new Error("unauthorized"); }
      return res;
    },
    async json(path, opts) { const r = await api.req(path, opts); const b = await r.json().catch(() => ({})); if (!r.ok) throw new Error(b.error || r.statusText); return b; },
  };

  // ── key dialog ───────────────────────────────────────────────────────
  function showKeyDialog(msg) {
    const d = $("key-dialog"); $("key-error").textContent = msg || ""; $("key-error").hidden = !msg;
    if (!d.open) d.showModal();
    $("key-input").focus();
  }
  $("key-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    localStorage.setItem(KEY, $("key-input").value.trim());
    try { await api.json("/inbox?limit=1"); $("key-dialog").close(); $("key-input").value = ""; boot(); }
    catch (err) { if (err.message !== "unauthorized") showKeyDialog(err.message); }
  });
  $("signout").addEventListener("click", () => { localStorage.removeItem(KEY); location.reload(); });

  // ── helpers ──────────────────────────────────────────────────────────
  const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
  const fmtSize = (n) => n >= 1 << 20 ? (n / (1 << 20)).toFixed(1) + " MB" : n >= 1024 ? Math.round(n / 1024) + " KB" : n + " B";
  function fmtTime(iso) {
    const d = new Date(iso), now = new Date();
    const sameDay = d.toDateString() === now.toDateString();
    if (sameDay) return d.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" });
    if (now - d < 6 * 864e5) return d.toLocaleDateString([], { weekday: "short" });
    return d.toLocaleDateString([], { month: "short", day: "numeric", year: d.getFullYear() === now.getFullYear() ? undefined : "numeric" });
  }
  const fmtFull = (iso) => new Date(iso).toLocaleString([], { weekday: "short", month: "short", day: "numeric", year: "numeric", hour: "numeric", minute: "2-digit" });
  let toastTimer;
  function toast(msg) { const t = $("toast"); t.textContent = msg; t.hidden = false; clearTimeout(toastTimer); toastTimer = setTimeout(() => (t.hidden = true), 2600); }

  // ── list ─────────────────────────────────────────────────────────────
  function listQuery(before) {
    const p = new URLSearchParams({ limit: "50" });
    if (state.box === "unread") p.set("unread", "true");
    p.set("suspicious", state.box === "suspicious" ? "true" : "false");
    if (state.q) p.set("q", state.q);
    if (before) p.set("before", before);
    return "/inbox?" + p;
  }
  async function loadList({ append = false } = {}) {
    const before = append ? state.messages.at(-1)?.received_at : null;
    const d = await api.json(listQuery(before));
    state.messages = append ? state.messages.concat(d.messages) : d.messages;
    $("count-unread").textContent = d.unread ? String(d.unread) : "";
    $("more").hidden = d.messages.length < 50;
    renderList();
  }
  function renderList() {
    const ol = $("messages");
    ol.innerHTML = state.messages.map((m) => `
      <li class="row${m.read ? "" : " is-unread"}${m.suspicious ? " is-suspicious" : ""}${m.id === state.selected ? " is-selected" : ""}" data-id="${m.id}" tabindex="-1">
        <span class="dot"></span>
        <span class="sender">${esc(m.from_name || m.from)}</span>
        <span class="time">${esc(fmtTime(m.received_at))}</span>
        <span class="subj">${esc(m.subject)}${m.attachments ? `<svg class="clip" width="13" height="13" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round"><path d="M14.5 8.5 8.7 14.3a2.6 2.6 0 0 1-3.7-3.7l6.6-6.6a1.7 1.7 0 0 1 2.4 2.4L7.6 12.8"/></svg>` : ""}</span>
        <span class="prev">${esc(m.preview)}</span>
      </li>`).join("");
    const empty = $("empty");
    const msgs = { inbox: "Nothing here yet. Mail to any address at nathanblatter.com will show up.", unread: "You're caught up.", suspicious: "No suspicious mail." };
    empty.hidden = state.messages.length > 0;
    empty.textContent = state.q && !state.messages.length ? `Nothing matches “${state.q}”.` : msgs[state.box];
  }
  $("messages").addEventListener("click", (e) => { const li = e.target.closest(".row"); if (li) open(li.dataset.id); });
  $("more").addEventListener("click", () => loadList({ append: true }));
  $("refresh").addEventListener("click", () => loadList());
  let searchTimer;
  $("search").addEventListener("input", (e) => { clearTimeout(searchTimer); searchTimer = setTimeout(() => { state.q = e.target.value.trim(); loadList(); }, 220); });
  document.querySelectorAll(".box").forEach((b) => b.addEventListener("click", () => {
    document.querySelectorAll(".box").forEach((x) => x.classList.toggle("is-active", x === b));
    state.box = b.dataset.box; closeReader(); loadList();
  }));

  // ── reader ───────────────────────────────────────────────────────────
  async function open(id) {
    state.selected = id; renderList();
    const m = await api.json("/inbox/" + id);
    state.current = m; state.imagesLoaded = false; state.html = !!m.html;
    $("reader-blank").hidden = true; $("message").hidden = false;
    $("app").classList.add("is-reading");
    $("m-subject").textContent = m.subject;
    $("m-from").innerHTML = m.from_name ? `${esc(m.from_name)} <em>${esc(m.from)}</em>` : esc(m.from);
    const to = (m.to || []).join(", "), cc = (m.cc || []).length ? ` · cc ${m.cc.join(", ")}` : "";
    $("m-to").textContent = "to " + to + cc;
    $("m-date").textContent = fmtFull(m.date || m.received_at);
    $("m-date").dateTime = m.date || m.received_at;
    const auth = $("m-auth");
    const bad = m.spf !== "pass" && m.dkim !== "pass";
    auth.className = "auth" + (bad ? " is-bad" : "");
    auth.textContent = bad ? "Unverified sender" : `Verified · ${[m.spf === "pass" && "SPF", m.dkim === "pass" && "DKIM", m.dmarc === "pass" && "DMARC"].filter(Boolean).join(", ")}`;
    $("unread").textContent = "Mark unread";
    $("view-toggle").hidden = !(m.html && m.text);
    renderBody();
    renderAttachments(m);
    $("m-headers").innerHTML = Object.entries(m.headers || {}).sort().map(([k, v]) => `<dt>${esc(k)}</dt><dd>${esc(v)}</dd>`).join("");
    $("reader").scrollTop = 0;
    if (!m.read) { await api.json(`/inbox/${id}/read`, { method: "POST" }); const row = state.messages.find((x) => x.id === id); if (row) row.read = true; renderList(); bumpUnread(-1); }
  }
  function bumpUnread(d) { const c = $("count-unread"); const n = Math.max(0, (parseInt(c.textContent) || 0) + d); c.textContent = n ? String(n) : ""; }
  function closeReader() { state.selected = null; state.current = null; $("message").hidden = true; $("reader-blank").hidden = false; $("app").classList.remove("is-reading"); renderList(); }
  $("back").addEventListener("click", closeReader);

  function renderBody() {
    const m = state.current, body = $("m-body");
    body.innerHTML = "";
    $("images-notice").hidden = true;
    $("view-toggle").textContent = state.html ? "View as text" : "View as HTML";
    if (!state.html || !m.html) {
      const p = document.createElement("p"); p.className = "text"; p.textContent = m.text || "(no text)"; body.appendChild(p); return;
    }
    let html = m.html, blocked = 0;
    // Inline images: cid: → data URLs fetched through the keyed API.
    const cidMap = state.cidMap || {};
    html = html.replace(/(src\s*=\s*["']?)cid:([^"'\s>]+)/gi, (_, pre, cid) => pre + (cidMap[cid] || "data:,"));
    if (!state.imagesLoaded) {
      html = html.replace(/(<img\b[^>]*?\s)src(\s*=\s*["']?https?:)/gi, (_, a, b) => { blocked++; return a + "data-blocked-src" + b; });
      html = html.replace(/url\((["']?)https?:[^)]*\)/gi, () => { blocked++; return "none"; });
    }
    $("images-notice").hidden = blocked === 0;
    const doc = `<!doctype html><html><head><meta charset="utf-8">
      <meta http-equiv="Content-Security-Policy" content="default-src 'none'; img-src data: https: http:; style-src 'unsafe-inline'; font-src https:;">
      <base target="_blank"><style>body{margin:0;padding:4px;font:16px/1.5 -apple-system,Helvetica,Arial,sans-serif;color:#111;overflow-wrap:anywhere}img{max-width:100%;height:auto}</style></head>
      <body>${html}</body></html>`;
    const f = document.createElement("iframe");
    // allow-same-origin only so the height can be read; scripts stay
    // disabled by the sandbox and by the CSP inside the document.
    f.setAttribute("sandbox", "allow-same-origin allow-popups allow-popups-to-escape-sandbox");
    f.setAttribute("referrerpolicy", "no-referrer");
    f.srcdoc = doc;
    const fit = () => { try { f.style.height = Math.min(6000, f.contentDocument.documentElement.scrollHeight + 24) + "px"; } catch (_) {} };
    f.addEventListener("load", () => { fit(); setTimeout(fit, 400); setTimeout(fit, 1500); });
    body.appendChild(f);
  }
  $("load-images").addEventListener("click", () => { state.imagesLoaded = true; renderBody(); });
  $("view-toggle").addEventListener("click", () => { state.html = !state.html; renderBody(); });

  async function renderAttachments(m) {
    const ul = $("m-attachments");
    ul.innerHTML = (m.attachments || []).map((a) => `
      <li><a class="attachment" href="#" data-aid="${a.id}" data-name="${esc(a.filename)}">
        <svg width="14" height="14" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round"><path d="M14.5 8.5 8.7 14.3a2.6 2.6 0 0 1-3.7-3.7l6.6-6.6a1.7 1.7 0 0 1 2.4 2.4L7.6 12.8"/></svg>
        <span class="name">${esc(a.filename)}</span><span class="size">${fmtSize(a.size)}</span></a></li>`).join("");
    // Resolve inline images so the HTML view can show them.
    state.cidMap = {};
    const inline = (m.attachments || []).filter((a) => a.content_id);
    if (inline.length) {
      await Promise.all(inline.map(async (a) => {
        const r = await api.req(`/inbox/${m.id}/attachments/${a.id}`); const b = await r.blob();
        state.cidMap[a.content_id] = await new Promise((res) => { const fr = new FileReader(); fr.onload = () => res(fr.result); fr.readAsDataURL(b); });
      }));
      if (state.current === m && state.html) renderBody();
    }
  }
  $("m-attachments").addEventListener("click", async (e) => {
    const a = e.target.closest(".attachment"); if (!a) return; e.preventDefault();
    const r = await api.req(`/inbox/${state.current.id}/attachments/${a.dataset.aid}`); const b = await r.blob();
    const u = URL.createObjectURL(b); const l = document.createElement("a"); l.href = u; l.download = a.dataset.name; l.click(); setTimeout(() => URL.revokeObjectURL(u), 10000);
  });

  $("unread").addEventListener("click", async () => {
    const m = state.current; if (!m) return;
    await api.json(`/inbox/${m.id}/read?read=false`, { method: "POST" });
    const row = state.messages.find((x) => x.id === m.id); if (row) row.read = false; bumpUnread(1);
    toast("Marked unread"); closeReader();
  });
  $("delete").addEventListener("click", async () => {
    const m = state.current; if (!m) return;
    if (!confirm(`Delete “${m.subject}”? This also removes its attachments.`)) return;
    await api.json(`/inbox/${m.id}`, { method: "DELETE" });
    const i = state.messages.findIndex((x) => x.id === m.id);
    state.messages.splice(i, 1); toast("Deleted");
    const next = state.messages[i] || state.messages[i - 1];
    if (next) open(next.id); else closeReader();
  });

  // ── compose ──────────────────────────────────────────────────────────
  let files = [];
  function openCompose(reply) {
    const f = $("compose-form"); f.reset(); files = []; $("c-files-list").textContent = ""; $("c-error").hidden = true;
    $("compose-title").textContent = reply ? "Reply" : "New message";
    if (reply) {
      const m = reply;
      const myAddr = (m.to || []).find((t) => t.endsWith("@nathanblatter.com")) || "nathan@nathanblatter.com";
      $("c-from-local").value = myAddr.split("@")[0];
      $("c-to").value = m.reply_to || m.from;
      $("c-subject").value = /^re:/i.test(m.subject) ? m.subject : "Re: " + m.subject;
      const quoted = (m.text || "").trim().split("\n").map((l) => "> " + l).join("\n");
      $("c-body").value = `\n\nOn ${fmtFull(m.date || m.received_at)}, ${m.from_name || m.from} wrote:\n${quoted}`;
      f.dataset.replyTo = m.message_id || ""; f.dataset.references = [m.references, m.message_id].filter(Boolean).join(" ");
    } else { f.dataset.replyTo = ""; f.dataset.references = ""; }
    $("compose").showModal();
    (reply ? $("c-body") : $("c-to")).focus();
    if (reply) $("c-body").setSelectionRange(0, 0);
  }
  $("compose-btn").addEventListener("click", () => openCompose());
  $("reply").addEventListener("click", () => state.current && openCompose(state.current));
  $("compose-close").addEventListener("click", () => $("compose").close());
  $("c-files").addEventListener("change", (e) => { files = [...e.target.files]; $("c-files-list").textContent = files.map((f) => `${f.name} (${fmtSize(f.size)})`).join(", "); });
  const b64 = (file) => new Promise((res, rej) => { const fr = new FileReader(); fr.onload = () => res(fr.result.split(",")[1]); fr.onerror = rej; fr.readAsDataURL(file); });
  $("compose-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const f = e.target, btn = $("c-send"); btn.disabled = true; btn.textContent = "Sending…"; $("c-error").hidden = true;
    try {
      const msg = {
        from: `Nathan Blatter <${$("c-from-local").value.trim()}@nathanblatter.com>`,
        to: $("c-to").value, subject: $("c-subject").value, text: $("c-body").value,
      };
      if ($("c-cc").value.trim()) msg.cc = $("c-cc").value;
      if (f.dataset.replyTo) msg.headers = { "In-Reply-To": f.dataset.replyTo, References: f.dataset.references };
      if (files.length) msg.attachments = await Promise.all(files.map(async (x) => ({ filename: x.name, content: await b64(x), content_type: x.type || undefined })));
      const r = await api.json("/send", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(msg) });
      $("compose").close();
      toast(r.status === "queued" ? `Queued (${r.reason.replace("_", " ")}) — it will go out when the relay is back` : r.links?.length ? `Sent with ${r.links.length} download link(s)` : "Sent");
    } catch (err) { $("c-error").textContent = err.message; $("c-error").hidden = false; }
    finally { btn.disabled = false; btn.textContent = "Send"; }
  });

  // ── keyboard ─────────────────────────────────────────────────────────
  document.addEventListener("keydown", (e) => {
    if (document.querySelector("dialog[open]") || e.metaKey || e.ctrlKey || e.altKey) return;
    const typing = /^(input|textarea)$/i.test(e.target.tagName);
    if (typing) { if (e.key === "Escape") e.target.blur(); return; }
    const i = state.messages.findIndex((m) => m.id === state.selected);
    const go = (n) => { const m = state.messages[n]; if (m) { open(m.id); document.querySelector(`.row[data-id="${m.id}"]`)?.scrollIntoView({ block: "nearest" }); } };
    switch (e.key) {
      case "j": case "ArrowDown": e.preventDefault(); go(i < 0 ? 0 : i + 1); break;
      case "k": case "ArrowUp": e.preventDefault(); go(i < 0 ? 0 : i - 1); break;
      case "Enter": if (i >= 0) open(state.messages[i].id); break;
      case "r": if (state.current) openCompose(state.current); break;
      case "c": openCompose(); break;
      case "u": $("unread").click(); break;
      case "#": case "Delete": case "Backspace": if (state.current) { e.preventDefault(); $("delete").click(); } break;
      case "/": e.preventDefault(); $("search").focus(); break;
      case "R": loadList(); break;
      case "Escape": closeReader(); break;
    }
  });

  // ── boot ─────────────────────────────────────────────────────────────
  async function health() {
    try { const h = await (await fetch("/health")).json(); const el = $("health"); el.classList.toggle("is-degraded", h.status !== "ok"); el.textContent = `${h.sent_today}/${h.daily_budget} sent today${h.queued ? ` · ${h.queued} queued` : ""}`; el.title = JSON.stringify(h, null, 1); } catch (_) {}
  }
  async function boot() {
    $("app").hidden = false;
    await loadList(); health();
    setInterval(() => { if (!document.hidden) { loadList(); health(); } }, 60000);
  }
  if (!api.key()) showKeyDialog(); else boot();
})();
