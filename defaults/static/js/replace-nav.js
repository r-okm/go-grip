// Navigate inside the preview without adding browser history entries.
// Browsers only let a script close a tab whose history holds a single entry
// (unless a script opened it), so every link followed inside the preview
// would otherwise stop go-grip from closing the tab when it shuts down. Links
// to other sites, links opened in a new tab and downloads are left alone.
(function () {
  function sameOrigin(href) {
    try {
      return new URL(href, location.href).origin === location.origin;
    } catch (e) {
      return false;
    }
  }

  // Listen in the bubble phase on window, after the link's own handlers, so
  // a handler that cancels the click still wins.
  window.addEventListener(
    "click",
    function (event) {
      if (event.defaultPrevented || event.button !== 0) return;
      if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
      var link = event.target.closest && event.target.closest("a[href]");
      if (!link || link.hasAttribute("download")) return;
      if (link.target && link.target !== "_self") return;
      if (!sameOrigin(link.href)) return;
      event.preventDefault();
      location.replace(link.href);
    }
  );
})();
