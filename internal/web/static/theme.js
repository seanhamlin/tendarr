// Loaded synchronously in <head> so the saved theme applies before first
// paint. Storage can be unavailable (private windows, blocked site data), in
// which case the page simply follows the system preference.
(function () {
  "use strict";
  var KEY = "tendarr-theme";
  var MODES = ["auto", "light", "dark"];
  var LABELS = { auto: "Auto", light: "Light", dark: "Dark" };
  var TITLES = { auto: "Theme: follow system", light: "Theme: light", dark: "Theme: dark" };

  function read() {
    try {
      var v = window.localStorage.getItem(KEY);
      return MODES.indexOf(v) >= 0 ? v : "auto";
    } catch (e) {
      return "auto";
    }
  }

  function apply(mode) {
    var root = document.documentElement;
    if (mode === "auto") {
      root.removeAttribute("data-theme");
    } else {
      root.setAttribute("data-theme", mode);
    }
  }

  function sync(mode) {
    document.querySelectorAll("[data-theme-toggle]").forEach(function (btn) {
      btn.setAttribute("data-mode", mode);
      btn.setAttribute("title", TITLES[mode] + " (click to change)");
      btn.setAttribute("aria-label", TITLES[mode]);
      var label = btn.querySelector("[data-theme-label]");
      if (label) label.textContent = LABELS[mode];
    });
  }

  apply(read());

  document.addEventListener("DOMContentLoaded", function () {
    sync(read());
    document.querySelectorAll("[data-theme-toggle]").forEach(function (btn) {
      btn.addEventListener("click", function () {
        var next = MODES[(MODES.indexOf(read()) + 1) % MODES.length];
        try {
          window.localStorage.setItem(KEY, next);
        } catch (e) {
          // Not persisted; still applies for this page.
        }
        apply(next);
        sync(next);
      });
    });
  });
})();
