/**
 * send.js — send.html: mode tabs (send/request), delivery toggle, "From" line,
 * "Additional options" summary and growing message field
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

// "From" line: name and email are kept in this browser after a submit and
// shown as one line; "change" shows the fields again.
(function () {
  'use strict';

  var KEY = 'ferri.from';

  function load() {
    try {
      var v = JSON.parse(window.localStorage.getItem(KEY) || 'null');
      return v && typeof v.email === 'string' ? v : null;
    } catch (e) {
      return null;
    }
  }

  function save(name, email) {
    try {
      window.localStorage.setItem(KEY, JSON.stringify({ name: name, email: email }));
    } catch (e) { /* storage unavailable: the fields stay visible next time */ }
  }

  var stored = load();

  document.querySelectorAll('.from').forEach(function (block) {
    var line = block.querySelector('.from-line');
    var who = block.querySelector('.from-who');
    var fields = block.querySelector('.from-fields');
    var nameInput = block.querySelector('input[autocomplete="name"]');
    var emailInput = block.querySelector('input[type="email"]');
    var form = block.closest('form');

    function showFields() {
      line.hidden = true;
      fields.hidden = false;
    }

    // defaultValue keeps the values after form.reset().
    if (stored && stored.email) {
      nameInput.defaultValue = nameInput.value = stored.name || '';
      emailInput.defaultValue = emailInput.value = stored.email;
      if (emailInput.checkValidity()) {
        who.textContent = stored.name ? stored.name + ' <' + stored.email + '>' : stored.email;
        line.hidden = false;
        fields.hidden = true;
      }
    }

    block.querySelector('.from-change').addEventListener('click', function () {
      showFields();
      emailInput.focus();
    });

    // Fires only when the browser's own checks pass.
    form.addEventListener('submit', function () {
      nameInput.defaultValue = nameInput.value;
      emailInput.defaultValue = emailInput.value;
      save(nameInput.value.trim(), emailInput.value.trim());
    });
  });
})();

// Hidden fields that fail the browser's checks are shown, so the browser can
// point at them.
(function () {
  'use strict';

  document.querySelectorAll('.mode-panel form').forEach(function (form) {
    form.addEventListener('invalid', function (e) {
      var details = e.target.closest('details');
      if (details) details.open = true;
      var fields = e.target.closest('.from-fields');
      if (fields && fields.hidden) {
        fields.hidden = false;
        fields.parentNode.querySelector('.from-line').hidden = true;
      }
    }, true);
  });
})();

// "Additional options" shows the current choices next to its title.
(function () {
  'use strict';

  document.querySelectorAll('details.more-options').forEach(function (details) {
    var form = details.closest('form');
    var out = details.querySelector('.options-summary');
    if (!out) return;

    function update() {
      var parts = [];
      var expiry = form.querySelector('select[name="expiry_hours"]');
      if (expiry && expiry.selectedIndex >= 0) {
        parts.push('expires after ' + expiry.options[expiry.selectedIndex].text);
      }
      var count = form.querySelector('input[name="link_count"]');
      if (count) {
        var n = parseInt(count.value, 10) || 1;
        parts.push(n === 1 ? '1 link' : n + ' links');
      }
      var password = form.querySelector('input[name="password"]');
      if (password) parts.push(password.value ? 'password set' : 'no password');
      var diag = form.querySelector('#diag-upload');
      if (diag && diag.checked) parts.push('integrity check');
      out.textContent = '(' + parts.join(', ') + ')';
    }

    form.addEventListener('input', update);
    form.addEventListener('change', update);
    form.addEventListener('reset', function () { setTimeout(update, 0); });
    update();
  });
})();

// Message fields with class "grow" get taller as text is typed.
(function () {
  'use strict';

  document.querySelectorAll('textarea.grow').forEach(function (ta) {
    function fit() {
      ta.style.height = 'auto';
      ta.style.height = (ta.scrollHeight + ta.offsetHeight - ta.clientHeight) + 'px';
    }
    ta.addEventListener('input', fit);
    ta.form.addEventListener('reset', function () { setTimeout(fit, 0); });
  });
})();
