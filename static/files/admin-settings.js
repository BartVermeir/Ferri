/**
 * admin-settings.js — storage backend toggle + test-connection for admin/settings.html
 */

(function () {
  'use strict';

  function toggleSMB(val) {
    document.getElementById('smb-fields').style.display = val === 'smb' ? 'block' : 'none';
    document.getElementById('local-fields').style.display = val === 'smb' ? 'none' : 'block';
  }

  document.querySelectorAll('input[name="storage.type"]').forEach(function (input) {
    input.addEventListener('change', function () {
      toggleSMB(this.value);
    });
  });

  function testConnection() {
    var form = document.getElementById('storage-form');
    var type = form.querySelector('input[name="storage.type"]:checked').value;
    var result = document.getElementById(type === 'smb' ? 'test-result' : 'local-test-result');
    result.style.color = '#888';
    result.textContent = 'Testing…';

    var body = new URLSearchParams();
    new FormData(form).forEach(function (v, k) { body.append(k, v); });

    fetch('/admin/settings/storage/test', { method: 'POST', body: body })
      .then(function (r) { return r.json(); })
      .then(function (d) {
        if (d.ok) {
          result.style.color = 'green';
          result.textContent = '✓ Connection successful';
        } else {
          result.style.color = '#c00';
          result.textContent = '✗ ' + (d.error || 'Connection failed');
        }
      })
      .catch(function () {
        result.style.color = '#c00';
        result.textContent = '✗ Request failed';
      });
  }

  document.querySelectorAll('.test-connection-btn').forEach(function (btn) {
    btn.addEventListener('click', testConnection);
  });
})();
