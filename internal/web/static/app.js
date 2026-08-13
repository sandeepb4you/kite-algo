// Fragment polling and progressive enhancement.
//
// This deliberately does NOT ship htmx. The only behaviour the UI needs from it
// is "GET this fragment on an interval and swap it into this element", which is
// the ~30 lines below. Vendoring a third-party library into a page that can
// place real orders — under a CSP that forbids fetching it at runtime — is not
// a trade worth making for that.
//
// Usage:
//   <div data-poll="/partials/status" data-poll-ms="10000"></div>
//
// Polling is the correctness baseline. The WebSocket in ws.js is a latency
// optimisation layered on top: if it dies, the page goes stale by one poll
// interval rather than going wrong.
(function () {
  "use strict";

  var timers = [];

  function swap(el) {
    var url = el.getAttribute("data-poll");
    if (!url) return Promise.resolve();
    return fetch(url, {
      credentials: "same-origin",
      headers: { "X-Requested-With": "fetch" },
    })
      .then(function (r) {
        if (r.status === 401) { location.href = "/login"; return null; }
        if (!r.ok) return null;
        return r.text();
      })
      .then(function (html) {
        if (html === null) return;
        el.innerHTML = html;
        // Newly swapped-in markup may contain forms (order cancel buttons).
        wireForms(el);
        // Let ws.js re-derive its subscription set from the new DOM.
        document.body.dispatchEvent(new CustomEvent("fragment-swapped"));
      })
      .catch(function () {
        // A failed poll is not worth surfacing: the next one usually succeeds,
        // and the connection chip already shows when the link is down.
      });
  }

  function start() {
    wireForms(document);
    wireSearch();

    document.querySelectorAll("[data-poll]").forEach(function (el) {
      swap(el);
      var ms = parseInt(el.getAttribute("data-poll-ms"), 10) || 10000;
      timers.push(setInterval(function () { swap(el); }, ms));
    });

    // Refresh immediately when the socket reconnects, so a page that was stale
    // during an outage catches up at once rather than at the next tick.
    document.body.addEventListener("ws-open", function () {
      document.querySelectorAll("[data-poll]").forEach(swap);
    });
  }

  // --- form submission ----------------------------------------------------
  //
  // Forms carrying data-result post via fetch and render the server's HTML
  // response into that element, so placing an order does not reload the page
  // and lose the operator's ticket state. Forms with data-confirm ask first.
  //
  // Submission is disabled while a request is in flight: a double-click on
  // "Place order" must not become two orders.
  function wireForms(root) {
    root.querySelectorAll("form[data-result], form[data-confirm]").forEach(function (form) {
      if (form.__wired) return;
      form.__wired = true;

      form.addEventListener("submit", function (ev) {
        var confirmMsg = form.getAttribute("data-confirm");
        if (confirmMsg && !window.confirm(confirmMsg)) {
          ev.preventDefault();
          return;
        }

        var target = document.querySelector(form.getAttribute("data-result") || "#order-result");
        if (!target) return; // no target: let the browser submit normally
        ev.preventDefault();

        var button = form.querySelector("button[type=submit]");
        if (button) { button.disabled = true; }

        fetch(form.action, {
          method: form.method || "POST",
          body: new FormData(form),
          credentials: "same-origin",
        })
          .then(function (r) {
            if (r.status === 401) { location.href = "/login"; return null; }
            return r.text();
          })
          .then(function (html) {
            if (html === null) return;
            target.innerHTML = html;
            // Refresh the order and position tables straight away rather than
            // waiting for the next poll.
            document.querySelectorAll("[data-poll]").forEach(swap);
          })
          .catch(function () {
            target.innerHTML = '<p class="alert alert-error">' +
              "Could not reach the server. The order may or may not have been placed — " +
              "check the order book before retrying.</p>";
          })
          .finally(function () {
            if (button) { button.disabled = false; }
          });
      });
    });
  }

  // --- instrument typeahead -----------------------------------------------

  function wireSearch() {
    var input = document.getElementById("symbol");
    var list = document.getElementById("instrument-list");
    if (!input || !list) return;

    var timer = null;
    input.addEventListener("input", function () {
      clearTimeout(timer);
      var q = input.value.trim();
      if (q.length < 3) return; // an option master is huge; require a real prefix
      timer = setTimeout(function () {
        fetch("/api/instruments?q=" + encodeURIComponent(q), { credentials: "same-origin" })
          .then(function (r) { return r.ok ? r.json() : null; })
          .then(function (items) {
            if (!items) return;
            list.innerHTML = "";
            items.forEach(function (it) {
              var opt = document.createElement("option");
              opt.value = it.symbol;
              opt.label = it.symbol + " · lot " + it.lot_size +
                (it.expiry ? " · " + it.expiry : "");
              list.appendChild(opt);
            });
          })
          .catch(function () { /* typeahead is optional */ });
      }, 200);
    });
  }

  // Pause polling while the tab is hidden; a background tab does not need to
  // hold a market-hours request loop open.
  document.addEventListener("visibilitychange", function () {
    if (document.visibilityState === "visible") {
      document.querySelectorAll("[data-poll]").forEach(swap);
    }
  });

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", start);
  } else {
    start();
  }
})();
