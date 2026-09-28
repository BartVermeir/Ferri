/**
 * request-created.js — copy-to-clipboard for the upload request links:
 * one link per [data-copy] button, all of them via [data-copy-all].
 */

(function () {
  'use strict';

  function copy(text, btn) {
    var label = btn.dataset.label || (btn.dataset.label = btn.textContent);
    function done(msg) {
      btn.textContent = msg;
      setTimeout(function () { btn.textContent = label; }, 2000);
    }

    // navigator.clipboard requires HTTPS — fall back to execCommand for HTTP
    if (navigator.clipboard && window.isSecureContext) {
      navigator.clipboard.writeText(text).then(function () { done('Copied!'); }).catch(function () { done('Copy failed'); });
    } else {
      var ta = document.createElement('textarea');
      ta.value = text;
      ta.style.position = 'fixed';
      ta.style.opacity = '0';
      document.body.appendChild(ta);
      ta.focus();
      ta.select();
      try {
        done(document.execCommand('copy') ? 'Copied!' : 'Copy failed');
      } catch (e) {
        done('Copy failed');
      }
      document.body.removeChild(ta);
    }
  }

  var links = Array.prototype.slice.call(document.querySelectorAll('[data-copy]'));
  links.forEach(function (btn) {
    btn.addEventListener('click', function () { copy(btn.dataset.copy, btn); });
  });

  var all = document.querySelector('[data-copy-all]');
  if (all) {
    all.addEventListener('click', function () {
      copy(links.map(function (btn) { return btn.dataset.title + ': ' + btn.dataset.copy; }).join('\n'), all);
    });
  }
})();
