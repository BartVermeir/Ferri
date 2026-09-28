/**
 * send.js — mode tab switching (send/request) and delivery mode toggle for send.html
 */

(function () {
  'use strict';

  var initialMode = document.body.dataset.mode === 'request' ? 'request' : 'send';

  function switchMode(mode) {
    document.querySelectorAll('.mode-tab').forEach(function (t) {
      t.classList.toggle('active', t.dataset.mode === mode);
    });
    document.querySelectorAll('.mode-panel').forEach(function (p) {
      p.classList.toggle('active', p.id === 'panel-' + mode);
    });
  }

  document.querySelectorAll('.mode-tab').forEach(function (tab) {
    tab.addEventListener('click', function () {
      switchMode(this.dataset.mode);
    });
  });

  switchMode(initialMode);

  // Remove ?mode= from URL so refreshing always returns to the send tab
  if (window.history && window.history.replaceState) {
    window.history.replaceState({}, '', window.location.pathname);
  }
})();

// Delivery mode toggle (send panel)
(function () {
  'use strict';

  var recipientsSection = document.getElementById('recipients-section');
  var recipientsField = document.getElementById('recipients');
  var linkOnlyInput = document.getElementById('link_only');
  var submitBtn = document.getElementById('submit-btn');

  document.querySelectorAll('.delivery-tab').forEach(function (tab) {
    tab.addEventListener('click', function () {
      var mode = this.dataset.delivery;
      document.querySelectorAll('.delivery-tab').forEach(function (t) {
        t.classList.toggle('active', t.dataset.delivery === mode);
      });
      if (mode === 'link') {
        recipientsSection.style.display = 'none';
        recipientsField.required = false;
        linkOnlyInput.value = '1';
        submitBtn.textContent = 'Upload & get link';
      } else {
        recipientsSection.style.display = '';
        recipientsField.required = true;
        linkOnlyInput.value = '0';
        submitBtn.textContent = 'Send files';
      }
    });
  });
})();
