// CodeDocs AI — progressive enhancements. The app must work without this file.
(function () {
  "use strict";

  document.documentElement.classList.remove("no-js");

  function initNavToggle() {
    var toggle = document.querySelector(".nav-toggle");
    var nav = document.getElementById("site-nav");
    if (!toggle || !nav) return;

    toggle.addEventListener("click", function () {
      var open = nav.classList.toggle("is-open");
      toggle.setAttribute("aria-expanded", open ? "true" : "false");
    });
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", initNavToggle);
  } else {
    initNavToggle();
  }
})();
