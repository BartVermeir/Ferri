/**
 * manage.js — confirm() gate for the delete button on the manage page (data-confirm attribute)
 */

(function () {
  'use strict';

  document.addEventListener('submit', function (e) {
    var btn = e.submitter;
    if (btn && btn.dataset.confirm && !window.confirm(btn.dataset.confirm)) {
      e.preventDefault();
    }
  });
})();
