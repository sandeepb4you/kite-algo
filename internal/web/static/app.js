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
        refreshCounts();
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

    // ws.js fires this on an order or a fill, so the position and order panels
    // reflect a trade at once instead of on their next poll.
    document.body.addEventListener("refresh-panels", function () {
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

  // --- tab counts ---------------------------------------------------------
  //
  // Tab switching itself is pure CSS, so it always works. These badges are the
  // one part that needs scripting: the counts live in the tab bar, which sits
  // outside the polled regions and would otherwise go stale.
  //
  // Counting rendered rows rather than trusting a server-supplied number means
  // the badge can never disagree with the table beneath it.
  function refreshCounts() {
    document.querySelectorAll("[data-count-of]").forEach(function (badge) {
      var panel = document.querySelector(badge.getAttribute("data-count-of"));
      if (!panel) return;
      badge.textContent = panel.querySelectorAll("tbody tr").length;
    });
  }

  // --- double-click BUY / SELL to send -------------------------------------
  //
  // A single click only selects the side; a double click selects it and submits
  // the ticket. This goes through requestSubmit() rather than submit() on
  // purpose: requestSubmit fires the form's submit event, so the ticket still
  // runs HTML validation (no symbol, no order) and still hits the confirm
  // dialog that live mode attaches. A double click can therefore never bypass
  // the live-mode confirmation — it only saves reaching for the button.
  document.addEventListener("dblclick", function (ev) {
    var label = ev.target.closest ? ev.target.closest(".side-toggle label") : null;
    if (!label) return;

    var input = document.getElementById(label.getAttribute("for"));
    if (input) input.checked = true;

    var form = document.getElementById("ticket");
    if (!form) return;

    // Clear the text selection a double click leaves behind.
    if (window.getSelection) { window.getSelection().removeAllRanges(); }

    if (form.requestSubmit) {
      form.requestSubmit();
    } else {
      // Older browsers: dispatch the event so the same handler runs.
      form.dispatchEvent(new Event("submit", { cancelable: true, bubbles: true }));
    }
  });

  // --- option chain → order ticket ----------------------------------------
  //
  // Clicking a premium loads that contract into the ticket. Typing an option
  // symbol by hand is both tedious and the easiest way to trade the wrong
  // strike or the wrong expiry, since the symbols differ by a few characters.
  //
  // Delegated from the document so it keeps working after a poll replaces the
  // chain markup.
  function pickContract(cell) {
    // The cell is a submit button carrying the symbol as its form value, so the
    // page works without scripting. Read that same value here.
    var symbol = cell.value || cell.getAttribute("data-pick");
    if (!symbol) return false;

    var input = document.getElementById("symbol");
    if (!input) return false; // no ticket on this page: let the form submit
    input.value = symbol;

    // Show the lot size so the operator can see what one lot means here.
    var hint = document.getElementById("lot-hint");
    var lot = cell.getAttribute("data-lot");
    if (hint && lot && lot !== "0") {
      var lots = parseInt(document.getElementById("lots").value, 10) || 1;
      hint.textContent = symbol + " · lot size " + lot + " · " +
        lots + " lot(s) = " + lots * parseInt(lot, 10) + " qty";
    }

    document.querySelectorAll(".chain-cell.picked").forEach(function (el) {
      el.classList.remove("picked");
    });
    cell.classList.add("picked");

    // Bring the ticket into view on narrow screens, where the chain and the
    // ticket are stacked rather than side by side.
    if (window.innerWidth < 860) {
      input.scrollIntoView({ behavior: "smooth", block: "center" });
    }
    input.focus({ preventScroll: window.innerWidth >= 860 });
    return true;
  }

  // Intercept the submit so the ticket fills without a page reload. If anything
  // here fails to handle it, we do NOT preventDefault — the browser submits the
  // form and the server fills the ticket instead. The feature degrades to a
  // reload rather than to nothing.
  document.addEventListener("click", function (ev) {
    var cell = ev.target.closest ? ev.target.closest(".chain-cell") : null;
    if (!cell) return;
    if (pickContract(cell)) ev.preventDefault();
  });

  // The chain selectors submit their form on change, which the CSP forbids
  // doing with an inline onchange attribute. A visible Load button is the
  // no-JavaScript path; this just saves the extra click.
  document.addEventListener("change", function (ev) {
    var el = ev.target;
    if (el && el.form && el.form.classList.contains("chain-controls") &&
        (el.id === "underlying" || el.id === "expiry")) {
      el.form.submit();
    }
  });

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
