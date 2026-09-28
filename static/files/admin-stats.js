/**
 * admin-stats.js — opens the statistics of a transfer or request in a dialog
 * when its type badge on the dashboard is clicked (data-stats = fragment URL).
 */

(function () {
  'use strict';

  var dialog = document.getElementById('stats-dialog');
  var body = document.getElementById('stats-body');
  if (!dialog || !body) return;

  document.addEventListener('click', function (e) {
    var btn = e.target.closest('[data-stats]');
    if (btn) {
      e.preventDefault();
      body.textContent = 'Loading…';
      dialog.showModal();
      fetch(btn.dataset.stats, { credentials: 'same-origin' })
        .then(function (res) {
          if (!res.ok) throw new Error(res.status);
          return res.text();
        })
        .then(function (html) { body.innerHTML = html; })
        .catch(function () { body.textContent = 'Could not load the statistics. Reload the page and try again.'; });
      return;
    }
    // A click on the backdrop (the dialog itself, outside its content) closes it.
    if (e.target === dialog || e.target.closest('[data-stats-close]')) dialog.close();
  });
})();
