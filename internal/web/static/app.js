// Progressive enhancement only: every action works without JavaScript, and
// the server validates everything regardless of what happens here.
(function () {
  "use strict";

  function formatBytes(bytes) {
    var units = ["B", "KB", "MB", "GB", "TB", "PB"];
    var v = bytes, u = 0;
    while (v >= 1024 && u < units.length - 1) { v /= 1024; u++; }
    return u === 0 ? v + " B" : v.toFixed(1) + " " + units[u];
  }

  // Selection summary, select-all and action-button state for list forms.
  document.querySelectorAll("form[data-selectable]").forEach(function (form) {
    var boxes = Array.prototype.slice.call(form.querySelectorAll('input[type="checkbox"][name="key"]'));
    var all = form.querySelector("[data-select-all]");
    // The action buttons live in the page toolbar, outside the form, and
    // join it through their form attribute.
    var bar = document.querySelector('[data-actionbar="' + form.id + '"]');
    var summary = bar && bar.querySelector("[data-selection-summary]");
    var buttons = Array.prototype.filter.call(form.elements, function (el) {
      return el.hasAttribute("data-needs-selection");
    });

    function update() {
      var n = 0, bytes = 0;
      boxes.forEach(function (b) {
        var row = b.closest("tr");
        if (b.checked) {
          n++;
          bytes += parseInt(b.getAttribute("data-size") || "0", 10) || 0;
        }
        if (row) row.classList.toggle("selected", b.checked);
      });
      if (summary) {
        summary.textContent = n === 0 ? "Nothing selected" : n + " selected · " + formatBytes(bytes);
      }
      if (bar) bar.classList.toggle("has-selection", n > 0);
      buttons.forEach(function (btn) {
        if (btn.hasAttribute("data-locked")) return;
        var max = parseInt(btn.getAttribute("data-max") || "0", 10);
        var tooMany = max > 0 && n > max;
        btn.disabled = n === 0 || tooMany;
        btn.title = tooMany ? "Select at most " + max : "";
      });
      if (all) {
        all.checked = n > 0 && n === boxes.length;
        all.indeterminate = n > 0 && n < boxes.length;
      }
    }

    // Buttons disabled by the server (e.g. a job already running) stay so.
    buttons.forEach(function (btn) { if (btn.disabled) btn.setAttribute("data-locked", ""); });

    if (all) {
      all.addEventListener("change", function () {
        boxes.forEach(function (b) { b.checked = all.checked; });
        update();
      });
    }

    // Shift-click selects a range.
    var last = null;
    boxes.forEach(function (b) {
      b.addEventListener("click", function (e) {
        if (e.shiftKey && last && last !== b) {
          var from = boxes.indexOf(last), to = boxes.indexOf(b);
          var lo = Math.min(from, to), hi = Math.max(from, to);
          for (var i = lo; i <= hi; i++) boxes[i].checked = b.checked;
        }
        last = b;
        update();
      });
    });

    form.addEventListener("submit", function () {
      buttons.forEach(function (btn) { btn.setAttribute("aria-busy", "true"); });
    });

    update();
  });

  // Scans can take a while on a big library; say so instead of looking hung.
  document.querySelectorAll("form[data-scan-form]").forEach(function (form) {
    form.addEventListener("submit", function () {
      var menu = form.closest("details");
      if (menu) menu.open = false;
      var status = document.querySelector("[data-scan-status]");
      if (status) status.textContent = "Scanning… large libraries can take a minute";
    });
  });

  // Close an open toolbar menu on Escape or a click elsewhere.
  document.addEventListener("click", function (e) {
    document.querySelectorAll("details.menu[open]").forEach(function (d) {
      if (!d.contains(e.target)) d.open = false;
    });
  });
  document.addEventListener("keydown", function (e) {
    if (e.key !== "Escape") return;
    document.querySelectorAll("details.menu[open]").forEach(function (d) {
      d.open = false;
      d.querySelector("summary").focus();
    });
  });

  // Poll a running release check and reload when it finishes, so the
  // results appear in the table.
  var job = document.querySelector("[data-job-url]");
  if (job && job.getAttribute("data-job-running") === "true") {
    var url = job.getAttribute("data-job-url");
    var id = job.getAttribute("data-job-id");
    var text = job.querySelector("[data-job-text]");
    var bar = job.querySelector("[data-job-progress]");
    var lastDone = -1;

    var poll = function () {
      fetch(url, { credentials: "same-origin", headers: { Accept: "application/json" } })
        .then(function (r) { return r.ok ? r.json() : null; })
        .then(function (s) {
          if (!s || !s.exists || s.id !== id) return;
          if (bar) { bar.max = s.total; bar.value = s.done; }
          if (text) text.textContent = s.done + " of " + s.total + " done" + (s.current ? " · " + s.current : "");
          if (!s.running) { window.location.reload(); return; }
          lastDone = s.done;
          setTimeout(poll, 3000);
        })
        .catch(function () { setTimeout(poll, 10000); });
    };
    setTimeout(poll, 2000);
  }
})();
