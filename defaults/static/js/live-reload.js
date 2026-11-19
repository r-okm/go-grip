// Live reload: go-grip streams a "reload" event when something this page
// shows has changed (the Markdown file, an image it references, or for a
// folder page, the folder's entries), and a "close" event when it shuts down.
// With --no-reload the page still listens for "close", but never reloads.
(function () {
  if (!window.EventSource) return;

  var reload = document.documentElement.hasAttribute("data-grip-reload");

  var pagePath = location.pathname;
  try {
    pagePath = decodeURIComponent(pagePath);
  } catch (e) {}
  var url = "/__grip/events?path=" + encodeURIComponent(pagePath);

  var source = null;

  function showStopped() {
    if (document.getElementById("grip-stopped")) return;
    var note = document.createElement("div");
    note.id = "grip-stopped";
    note.setAttribute("role", "status");
    note.textContent =
      "go-grip has stopped. This preview no longer updates; close the tab when you are done.";
    var style = note.style;
    style.position = "fixed";
    // At the bottom, clear of the fixed controls at the top right.
    style.bottom = "0";
    style.left = "0";
    style.right = "0";
    style.zIndex = "1000";
    style.padding = "8px 16px";
    style.textAlign = "center";
    style.font = "14px/20px -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif";
    style.background = "var(--bgColor-muted, #f6f8fa)";
    style.borderTop = "1px solid var(--borderColor-default, #d1d9e0)";
    document.body.appendChild(note);
  }

  function connect() {
    var es = new EventSource(url);
    var lostConnection = false;

    es.addEventListener("reload", function () {
      es.close();
      location.reload();
    });

    // The server is shutting down. Browsers only let a script close a tab
    // that another program opened and that has not navigated; any other tab
    // stays open, and the stream reconnects and reloads if the server comes
    // back. A dropped connection alone is not a reason to close: it also
    // happens when the server restarts.
    es.addEventListener("close", function () {
      window.close();
      // window.close() returns even when the browser refuses; the page is
      // still here then, so say that the preview is no longer live.
      setTimeout(showStopped, 300);
    });

    es.onerror = function () {
      lostConnection = true;
    };

    // The connection came back after an error: the server probably
    // restarted, so the page may be stale. Without live reload, only drop
    // the stopped note.
    es.onopen = function () {
      if (!lostConnection) return;
      lostConnection = false;
      if (reload) {
        es.close();
        location.reload();
        return;
      }
      var note = document.getElementById("grip-stopped");
      if (note) note.remove();
    };

    source = es;
  }

  // Leaving the page. Chrome's back/forward cache freezes the page instead of
  // unloading it, and would keep this stream's connection open. A few frozen
  // pages use up the browser's per-host connection limit and stall the next
  // navigation, so release the connection. Closing an EventSource keeps the
  // page eligible for the cache; an unload handler would not.
  window.addEventListener("pagehide", function () {
    if (source) {
      source.close();
      source = null;
    }
  });

  // Back from the cache. Files may have changed while the page was frozen
  // and the server cannot tell which ones, so load the page fresh. On
  // localhost that costs one render, and the new page opens its own stream.
  // Without live reload, keep the page as it was and only reopen the stream.
  window.addEventListener("pageshow", function (event) {
    if (!event.persisted) return;
    if (reload) {
      location.reload();
    } else if (!source) {
      connect();
    }
  });

  connect();
})();
