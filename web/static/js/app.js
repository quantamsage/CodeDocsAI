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

  // Directories the server skips anyway. Leaving them out of folder uploads
  // avoids sending (often huge) dependency trees over the network.
  var IGNORED_DIRS = [".git", ".hg", ".svn", "__macosx", "node_modules", "bower_components", ".venv", "venv",
    "__pycache__", ".mypy_cache", ".pytest_cache", ".tox", ".gradle", ".idea", ".vscode", ".vs", ".next",
    ".nuxt", ".terraform", ".cache"];

  function isIgnored(relativePath) {
    var segments = relativePath.split("/");
    for (var i = 0; i < segments.length - 1; i++) {
      if (IGNORED_DIRS.indexOf(segments[i].toLowerCase()) !== -1) return true;
    }
    return false;
  }

  function formatBytes(n) {
    var units = ["B", "KiB", "MiB", "GiB"];
    var i = 0;
    while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
    return (i === 0 ? n : n.toFixed(1)) + " " + units[i];
  }

  function initUploadForm() {
    var form = document.querySelector("form[data-upload]");
    if (!form) return;
    var filesInput = form.querySelector("#files");
    var folderInput = form.querySelector("#folder");
    var summary = form.querySelector("[data-upload-summary]");
    var submit = form.querySelector("[data-upload-submit]");
    var maxRequest = parseInt(form.getAttribute("data-max-request"), 10) || 0;
    var skippedDirs = 0;

    function selected() {
      var list = [];
      [filesInput, folderInput].forEach(function (input) {
        if (input && input.files) Array.prototype.push.apply(list, input.files);
      });
      return list;
    }

    function update() {
      var list = selected();
      var total = list.reduce(function (sum, f) { return sum + f.size; }, 0);
      if (list.length === 0 && skippedDirs === 0) { summary.hidden = true; return; }
      var text = list.length + (list.length === 1 ? " file" : " files") + " selected (" + formatBytes(total) + ").";
      if (skippedDirs > 0) text += " " + skippedDirs + " files in dependency or version-control folders were left out.";
      if (maxRequest && total > maxRequest) text += " This exceeds the " + formatBytes(maxRequest) + " upload limit.";
      summary.textContent = text;
      summary.hidden = false;
    }

    // Drop files inside ignored directories from a folder selection.
    if (folderInput && typeof DataTransfer === "function") {
      folderInput.addEventListener("change", function () {
        var kept = new DataTransfer();
        skippedDirs = 0;
        Array.prototype.forEach.call(folderInput.files, function (f) {
          if (isIgnored(f.webkitRelativePath || f.name)) { skippedDirs++; } else { kept.items.add(f); }
        });
        if (skippedDirs > 0) folderInput.files = kept.files;
        update();
      });
    }
    if (filesInput) filesInput.addEventListener("change", update);

    form.addEventListener("submit", function (event) {
      if (selected().length === 0) {
        event.preventDefault();
        summary.textContent = "Choose at least one file, folder or ZIP archive to upload.";
        summary.hidden = false;
        return;
      }
      submit.disabled = true;
      submit.textContent = "Uploading…";
    });
  }

  function init() {
    initNavToggle();
    initUploadForm();
    initJobStatus();
  }

  // Polls a running job's progress and reloads the page when it finishes,
  // so the results (or the error) appear without a manual refresh.
  function initJobStatus() {
    var box = document.querySelector("[data-job-status]");
    if (!box || box.getAttribute("data-active") !== "true" || typeof fetch !== "function") return;
    var url = box.getAttribute("data-url");
    var field = function (name) { return box.querySelector('[data-job-field="' + name + '"]'); };
    var failures = 0;

    function poll() {
      fetch(url, { headers: { Accept: "application/json" }, credentials: "same-origin" })
        .then(function (res) {
          if (!res.ok) throw new Error("HTTP " + res.status);
          return res.json();
        })
        .then(function (data) {
          failures = 0;
          var job = data.job;
          if (!job) return;
          if (!job.active) {
            window.location.reload();
            return;
          }
          if (field("message")) field("message").textContent = job.message;
          if (field("status")) field("status").textContent = job.status;
          if (field("progress")) field("progress").value = job.percent;
          setTimeout(poll, 2000);
        })
        .catch(function () {
          failures += 1;
          if (failures < 5) setTimeout(poll, 5000);
        });
    }
    setTimeout(poll, 2000);
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
