// Live market-data channel.
//
// The WebSocket is a latency optimisation, not a correctness dependency: every
// live region on the page also carries an htmx poll, so a broken socket makes
// the UI a few seconds stale rather than wrong. On reconnect we fire a `ws-open`
// event that those regions listen for, which resynchronises them over HTTP.
(function () {
  "use strict";

  var socket = null;
  var backoff = 1000;
  var BACKOFF_MAX = 15000;
  var watched = [];
  var closing = false;

  var dot = function () { return document.getElementById("conn-dot"); };

  function setConn(state, label) {
    var el = dot();
    if (!el) return;
    el.className = "chip chip-" + state;
    el.textContent = label;
  }

  // Symbols to stream are declared by the DOM itself: any element with
  // data-ltp is a price cell. Recomputing after every htmx swap keeps the
  // subscription set in step with whatever is actually on screen.
  function currentSymbols() {
    var out = [];
    var seen = Object.create(null);
    document.querySelectorAll("[data-ltp]").forEach(function (el) {
      var s = el.getAttribute("data-ltp");
      if (s && !seen[s]) { seen[s] = true; out.push(s); }
    });
    return out;
  }

  function syncSubscriptions() {
    if (!socket || socket.readyState !== WebSocket.OPEN) return;

    var next = currentSymbols();
    var prev = watched;
    var nextSet = Object.create(null);
    next.forEach(function (s) { nextSet[s] = true; });
    var prevSet = Object.create(null);
    prev.forEach(function (s) { prevSet[s] = true; });

    var add = next.filter(function (s) { return !prevSet[s]; });
    var del = prev.filter(function (s) { return !nextSet[s]; });

    if (add.length) socket.send(JSON.stringify({ op: "sub", symbols: add }));
    if (del.length) socket.send(JSON.stringify({ op: "unsub", symbols: del }));
    watched = next;
  }

  function connect() {
    if (closing) return;
    var proto = location.protocol === "https:" ? "wss:" : "ws:";
    socket = new WebSocket(proto + "//" + location.host + "/ws");

    socket.onopen = function () {
      backoff = 1000;
      setConn("ok", "live");
      watched = [];            // the server knows nothing about us yet
      syncSubscriptions();
      document.body.dispatchEvent(new CustomEvent("ws-open"));
    };

    socket.onclose = function () {
      setConn("bad", "reconnecting");
      if (closing) return;
      setTimeout(connect, jitter(backoff));
      backoff = Math.min(backoff * 2, BACKOFF_MAX);
    };

    socket.onerror = function () { setConn("warn", "connection error"); };

    socket.onmessage = function (ev) {
      var frame;
      try { frame = JSON.parse(ev.data); } catch (e) { return; }
      handle(frame);
    };
  }

  // Spread reconnect attempts so several tabs do not stampede together.
  function jitter(ms) { return ms * (0.8 + Math.random() * 0.4); }

  function handle(frame) {
    switch (frame.t) {
      case "ticks":   frame.d.forEach(applyTick); break;
      case "positions": applyPnL(frame.d); break;
      case "status":  applyStatus(frame.d); break;
      case "fill":
        toast("Fill: " + frame.d.TradingSymbol + " " + frame.d.Side + " " +
              frame.d.Quantity + " @ " + fmt(frame.d.Price), "ok");
        // A fill changes both the position book and the order book. Waiting for
        // the next poll would leave them up to five seconds out of date at the
        // exact moment they are being watched.
        refreshPanels();
        break;
      case "order":    refreshPanels(); break;
      case "rejected": toast("Rejected: " + frame.d.message, "err"); break;
    }
  }

  function applyTick(t) {
    document.querySelectorAll('[data-ltp="' + cssEscape(t.s) + '"]').forEach(function (el) {
      var prev = parseFloat(el.textContent.replace(/,/g, ""));
      el.textContent = fmt(t.p);
      if (!isNaN(prev) && prev !== t.p) {
        // A brief flash is the cheapest way to show which way a price moved
        // without redrawing the row and destroying the user's text selection.
        el.classList.remove("flash-up", "flash-down");
        void el.offsetWidth; // restart the CSS animation
        el.classList.add(t.p > prev ? "flash-up" : "flash-down");
      }
    });
    document.querySelectorAll('[data-chg="' + cssEscape(t.s) + '"]').forEach(function (el) {
      el.textContent = (t.c >= 0 ? "+" : "") + (t.c || 0).toFixed(2) + "%";
      el.className = el.className.replace(/pnl-\w+/g, "") +
        (t.c > 0 ? " pnl-up" : t.c < 0 ? " pnl-down" : " pnl-flat");
    });
  }

  // refreshPanels asks app.js to re-fetch every polled fragment immediately.
  //
  // Coalesced on a short timer: an order and its fill arrive back to back, and
  // a multi-leg strategy produces several at once — one refresh covers them all.
  var refreshTimer = null;
  function refreshPanels() {
    clearTimeout(refreshTimer);
    refreshTimer = setTimeout(function () {
      document.body.dispatchEvent(new CustomEvent("refresh-panels"));
    }, 120);
  }

  function applyPnL(d) {
    if (typeof d.day_pnl !== "number") return;
    var text = "₹" + (d.day_pnl >= 0 ? "+" : "") + fmt(d.day_pnl);
    var tone = d.day_pnl > 0 ? "pnl-up" : d.day_pnl < 0 ? "pnl-down" : "pnl-flat";

    // The dashboard's headline figure.
    var el = document.getElementById("day-pnl");
    if (el) {
      el.textContent = text;
      el.className = "figure " + tone;
    }
    // The header copy, visible on every page. The engine re-prices positions on
    // every tick and publishes at ~4/sec, so this tracks the market rather than
    // waiting for the 15-second header poll.
    var head = document.getElementById("day-pnl-header");
    if (head) {
      head.textContent = text;
      head.className = "mono " + tone;
    }
  }

  function applyStatus(d) {
    if (d && d.message) toast(d.message, d.level === "error" ? "err" : d.level === "warn" ? "warn" : "ok");
  }

  function toast(msg, kind) {
    var box = document.getElementById("toasts");
    if (!box) return;
    var el = document.createElement("div");
    el.className = "toast toast-" + (kind || "ok");
    el.textContent = msg;
    box.appendChild(el);
    setTimeout(function () { el.remove(); }, 6000);
  }

  function fmt(n) {
    return (typeof n === "number" ? n : 0).toLocaleString("en-IN",
      { minimumFractionDigits: 2, maximumFractionDigits: 2 });
  }

  // Trading symbols are alphanumeric, but escape defensively rather than
  // interpolating unvalidated text into a selector.
  function cssEscape(s) {
    if (window.CSS && CSS.escape) return CSS.escape(s);
    return String(s).replace(/["\\\]\[]/g, "\\$&");
  }

  function start() {
    connect();
    // Re-derive the subscription set whenever a polled fragment replaces part
    // of the DOM, so newly rendered rows start streaming and removed ones stop.
    document.body.addEventListener("fragment-swapped", syncSubscriptions);
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", start);
  } else {
    start();
  }

  window.addEventListener("beforeunload", function () {
    closing = true;
    if (socket) socket.close();
  });
})();
