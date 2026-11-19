// Markdown page: Esc follows the link back to the containing folder
// (#grip-back), unless Esc already means something else here.
(function () {
  function isFormField(el) {
    if (!el || !el.tagName) return false;
    var tag = el.tagName.toLowerCase();
    return tag === "input" || tag === "textarea" || tag === "select" || el.isContentEditable;
  }

  // MathJax closes its context menu and its dialogs on Esc. Leave the key to
  // them while focus is inside math, a menu or a dialog, or while one is open.
  var mathJaxSelector =
    'mjx-container, mjx-dialog, .mjx-dialog, [class^="CtxtMenu_"], [class*=" CtxtMenu_"]';

  function inMathJax(el) {
    return !!(el && el.closest && el.closest(mathJaxSelector));
  }

  function mathJaxOverlayOpen() {
    return !!document.querySelector(
      '.CtxtMenu_MenuFrame, .CtxtMenu_Menu, mjx-dialog, .mjx-dialog, dialog[open]'
    );
  }

  // With --replace-history, move without adding a history entry, so the tab
  // can still be closed when go-grip stops. See replace-nav.js.
  function navigate(href) {
    if (document.documentElement.hasAttribute("data-grip-replace-nav")) {
      window.location.replace(href);
    } else {
      window.location.href = href;
    }
  }

  function onKeyDown(event) {
    if (event.key !== "Escape" || event.defaultPrevented || event.isComposing) return;
    if (event.ctrlKey || event.altKey || event.metaKey || event.shiftKey) return;
    if (isFormField(event.target) || inMathJax(event.target)) return;
    if (mathJaxOverlayOpen()) return;

    var link = document.getElementById("grip-back");
    if (!link) return;
    event.preventDefault();
    navigate(link.href);
  }

  document.addEventListener("keydown", onKeyDown);
})();
