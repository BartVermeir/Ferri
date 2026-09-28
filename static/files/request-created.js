/**
 * request-created.js — copy-to-clipboard for the upload request link
 */

(function () {
  'use strict';

  document.getElementById('copy-btn').addEventListener('click', function () {
    var btn = document.getElementById('copy-btn');
    var url = document.getElementById('upload-url').textContent.trim();

    function onSuccess() {
      btn.textContent = 'Copied!';
      setTimeout(function () { btn.textContent = 'Copy link'; }, 2000);
    }
    function onFail() {
      btn.textContent = 'Copy failed';
      setTimeout(function () { btn.textContent = 'Copy link'; }, 2000);
    }

    // navigator.clipboard requires HTTPS — fall back to execCommand for HTTP
    if (navigator.clipboard && window.isSecureContext) {
      navigator.clipboard.writeText(url).then(onSuccess).catch(onFail);
    } else {
      var ta = document.createElement('textarea');
      ta.value = url;
      ta.style.position = 'fixed';
      ta.style.opacity = '0';
      document.body.appendChild(ta);
      ta.focus();
      ta.select();
      try {
        document.execCommand('copy') ? onSuccess() : onFail();
      } catch (e) {
        onFail();
      }
      document.body.removeChild(ta);
    }
  });
})();
