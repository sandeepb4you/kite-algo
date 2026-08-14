// Candlestick chart on a plain canvas.
//
// Deliberately hand-rolled rather than pulling in a charting library: the CSP
// forbids loading anything from another origin, and vendoring a megabyte of
// third-party JavaScript into a page that can place orders is a poor trade for
// what amounts to drawing rectangles.
//
// Usage: <canvas id="chart" data-candles="/api/candles?..."></canvas>
(function () {
  "use strict";

  var PAD = { top: 12, right: 60, bottom: 24, left: 8 };

  function css(name, fallback) {
    var v = getComputedStyle(document.documentElement).getPropertyValue(name);
    return (v && v.trim()) || fallback;
  }

  function draw(canvas, candles) {
    if (!candles.length) return;

    // Match the canvas backing store to the display size, accounting for
    // high-DPI screens — otherwise everything is blurry.
    var dpr = window.devicePixelRatio || 1;
    var cssWidth = canvas.clientWidth || 800;
    var cssHeight = canvas.height;
    canvas.width = cssWidth * dpr;
    canvas.height = cssHeight * dpr;
    canvas.style.height = cssHeight + "px";

    var ctx = canvas.getContext("2d");
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, cssWidth, cssHeight);

    var up = css("--up", "#26a96c");
    var down = css("--down", "#e5484d");
    var line = css("--line", "#2a2f3a");
    var muted = css("--muted", "#8b93a7");

    var plotW = cssWidth - PAD.left - PAD.right;
    var plotH = cssHeight - PAD.top - PAD.bottom;

    var lo = Infinity, hi = -Infinity;
    candles.forEach(function (c) {
      if (c.l < lo) lo = c.l;
      if (c.h > hi) hi = c.h;
    });
    if (!isFinite(lo) || !isFinite(hi)) return;
    if (hi === lo) { hi += 1; lo -= 1; }        // a flat series still needs a scale
    var span = hi - lo;
    lo -= span * 0.05;
    hi += span * 0.05;

    function y(price) {
      return PAD.top + plotH - ((price - lo) / (hi - lo)) * plotH;
    }

    // Horizontal gridlines with price labels on the right.
    ctx.strokeStyle = line;
    ctx.fillStyle = muted;
    ctx.font = "11px ui-monospace, monospace";
    ctx.lineWidth = 1;
    for (var i = 0; i <= 4; i++) {
      var price = lo + ((hi - lo) * i) / 4;
      var py = Math.round(y(price)) + 0.5;      // crisp 1px lines
      ctx.beginPath();
      ctx.moveTo(PAD.left, py);
      ctx.lineTo(PAD.left + plotW, py);
      ctx.stroke();
      ctx.fillText(price.toFixed(2), PAD.left + plotW + 6, py + 4);
    }

    var slot = plotW / candles.length;
    var bodyW = Math.max(1, Math.min(slot * 0.7, 14));

    candles.forEach(function (c, idx) {
      var cx = PAD.left + slot * (idx + 0.5);
      var rising = c.c >= c.o;
      ctx.strokeStyle = rising ? up : down;
      ctx.fillStyle = rising ? up : down;

      // Wick.
      ctx.beginPath();
      ctx.moveTo(Math.round(cx) + 0.5, y(c.h));
      ctx.lineTo(Math.round(cx) + 0.5, y(c.l));
      ctx.stroke();

      // Body. A doji would otherwise be invisible, so enforce 1px.
      var top = y(Math.max(c.o, c.c));
      var h = Math.max(1, Math.abs(y(c.o) - y(c.c)));
      ctx.fillRect(cx - bodyW / 2, top, bodyW, h);
    });

    // Time labels at either end.
    var fmt = function (ms) {
      var d = new Date(ms);
      return d.toLocaleString("en-IN", {
        day: "2-digit", month: "short", hour: "2-digit", minute: "2-digit",
        hour12: false, timeZone: "Asia/Kolkata",
      });
    };
    ctx.fillStyle = muted;
    ctx.fillText(fmt(candles[0].t), PAD.left, cssHeight - 8);
    var lastLabel = fmt(candles[candles.length - 1].t);
    ctx.fillText(lastLabel, PAD.left + plotW - ctx.measureText(lastLabel).width, cssHeight - 8);
  }

  function init() {
    var canvas = document.getElementById("chart");
    if (!canvas) return;
    var url = canvas.getAttribute("data-candles");
    if (!url) return;

    fetch(url, { credentials: "same-origin" })
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (candles) {
        if (!candles) return;
        draw(canvas, candles);
        // Redraw on resize so the chart stays sharp and correctly scaled.
        var timer = null;
        window.addEventListener("resize", function () {
          clearTimeout(timer);
          timer = setTimeout(function () { draw(canvas, candles); }, 150);
        });
      })
      .catch(function () { /* the table below the chart still shows the data */ });
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
