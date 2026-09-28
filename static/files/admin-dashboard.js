/**
 * admin-dashboard.js — confirm() gate for destructive form submits (data-confirm attribute),
 * and the row checkboxes for "Delete selected" (data-select, data-select-all, data-bulk-delete).
 */

(function () {
  'use strict';

  document.addEventListener('submit', function (e) {
    var btn = e.submitter;
    if (btn && btn.dataset.confirm && !window.confirm(btn.dataset.confirm)) {
      e.preventDefault();
    }
  });

  var all = document.querySelector('[data-select-all]');
  var bulk = document.querySelector('[data-bulk-delete]');
  if (!all || !bulk) return;
  var rows = Array.prototype.slice.call(document.querySelectorAll('[data-select]'));
  var confirmText = bulk.dataset.confirm;

  function update() {
    var n = rows.filter(function (cb) { return cb.checked; }).length;
    bulk.disabled = n === 0;
    bulk.textContent = n ? 'Delete selected (' + n + ')' : 'Delete selected';
    bulk.dataset.confirm = confirmText.replace('the selected items', n === 1 ? '1 item' : n + ' items');
    all.checked = n > 0 && n === rows.length;
    all.indeterminate = n > 0 && n < rows.length;
    all.disabled = rows.length === 0;
  }

  all.addEventListener('change', function () {
    rows.forEach(function (cb) { cb.checked = all.checked; });
    update();
  });
  rows.forEach(function (cb) { cb.addEventListener('change', update); });
  update();
})();
