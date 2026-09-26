/**
 * upload.js — Ferri TUS upload client
 *
 * Every file is its own TUS upload, one after another, so each one resumes on
 * its own. Files from a folder carry their path in it ("Series/day1/img.jpg");
 * the server keeps that structure and builds a ZIP on download. DEC-035.
 */

(function () {
  'use strict';

  const isUploadRequest = typeof window.FERRI_UPLOAD_TOKEN === 'string';

  if (isUploadRequest) {
    initUploadRequestPage();
  } else {
    initSendPage();
  }

  // ── File management ──────────────────────────────────────────────────────────

  // Maintains a cumulative list of files across multiple selections.
  // Each item is {file, path}: path is the name inside the upload, with the
  // folder prefix ("series/day1/img001.jpg") for files that came from a folder.
  function FileCollection() {
    this.items = [];
  }

  FileCollection.prototype.add = function (items) {
    var existing = this.items.map(function (it) { return it.path + '|' + it.file.size; });
    items.forEach(function (it) {
      var key = it.path + '|' + it.file.size;
      if (existing.indexOf(key) === -1) {
        existing.push(key);
        this.items.push(it);
      }
    }, this);
  };

  FileCollection.prototype.remove = function (index) {
    this.items.splice(index, 1);
  };

  FileCollection.prototype.clear = function () {
    this.items = [];
  };

  FileCollection.prototype.count = function () {
    return this.items.length;
  };

  FileCollection.prototype.totalSize = function () {
    return this.items.reduce(function (sum, it) { return sum + it.file.size; }, 0);
  };

  // hasFolder: at least one file came from a folder (its path has a "/").
  FileCollection.prototype.hasFolder = function () {
    return this.items.some(function (it) { return it.path.indexOf('/') !== -1; });
  };

  // ── Limits ───────────────────────────────────────────────────────────────────

  // readLimits takes the server limits from data attributes on el.
  function readLimits(el) {
    return {
      maxFiles: parseInt(el && el.dataset.maxFiles, 10) || 5000,
      maxBytes: Number(el && el.dataset.maxBytes) || Infinity,
    };
  }

  // buildUploads turns the collection into what uploadFiles sends: one item
  // per file, named by its path. Throws a readable message when the
  // selection is past the limits.
  function buildUploads(collection, limits) {
    if (collection.count() > limits.maxFiles) {
      throw new Error('At most ' + limits.maxFiles + ' files per upload; this selection has ' + collection.count() + '.');
    }
    var tooBig = collection.items.filter(function (it) { return it.file.size > limits.maxBytes; });
    if (tooBig.length > 0) {
      throw new Error(tooBig[0].path + ' is larger than the limit of ' + formatSize(limits.maxBytes) + '.');
    }
    return collection.items.map(function (it) {
      return { source: it.file, name: it.path, size: it.file.size };
    });
  }

  // confirmLargeTotal: files can each be under the limit and still add up to
  // more. That is allowed after a warning: 600 GB per transfer is what users
  // are told, not a hard rule (DEC-036).
  function confirmLargeTotal(uploads, limits) {
    var total = uploads.reduce(function (sum, u) { return sum + u.size; }, 0);
    if (total <= limits.maxBytes) return true;
    return window.confirm('Together these files are ' + formatSize(total) +
      ', more than the usual maximum of ' + formatSize(limits.maxBytes) +
      ' per transfer. Upload them anyway?');
  }

  // ── File drop zone ───────────────────────────────────────────────────────────

  // Files from the plain file picker have no folder: path is the file name.
  function fromFileList(fileList) {
    return Array.from(fileList).map(function (f) {
      return { file: f, path: f.webkitRelativePath || f.name };
    });
  }

  // readAllEntries: a directory reader returns its entries in batches (100
  // in Chrome) until it returns an empty one.
  function readAllEntries(reader) {
    return new Promise(function (resolve, reject) {
      var all = [];
      (function next() {
        reader.readEntries(function (batch) {
          if (batch.length === 0) { resolve(all); return; }
          all = all.concat(Array.from(batch));
          next();
        }, reject);
      })();
    });
  }

  // walkEntry collects every file below a dropped file or folder entry.
  async function walkEntry(entry, prefix) {
    if (entry.isFile) {
      var file = await new Promise(function (resolve, reject) { entry.file(resolve, reject); });
      return [{ file: file, path: prefix + file.name }];
    }
    if (entry.isDirectory) {
      var children = await readAllEntries(entry.createReader());
      var out = [];
      for (var i = 0; i < children.length; i++) {
        out = out.concat(await walkEntry(children[i], prefix + entry.name + '/'));
      }
      return out;
    }
    return [];
  }

  // fromDrop reads a drop event. Entries must be taken synchronously, inside
  // the event: the DataTransfer is emptied once the handler returns. Browsers
  // without the entries API fall back to the flat file list (no folders).
  function fromDrop(dataTransfer) {
    var items = dataTransfer.items ? Array.from(dataTransfer.items) : [];
    var entries = items
      .filter(function (it) { return it.kind === 'file' && it.webkitGetAsEntry; })
      .map(function (it) { return it.webkitGetAsEntry(); })
      .filter(Boolean);
    if (entries.length === 0) {
      return Promise.resolve(fromFileList(dataTransfer.files));
    }
    return Promise.all(entries.map(function (e) { return walkEntry(e, ''); }))
      .then(function (lists) { return [].concat.apply([], lists); });
  }

  function initFileDrop(dropEl, inputEl, collection, listEl, limits) {
    if (!dropEl || !inputEl) return;

    var folderBtn = document.getElementById('folder-btn');
    var folderInput = document.getElementById('folder-input');
    var noteEl = document.getElementById('file-note');
    var refresh = function () { renderFileList(collection, listEl, noteEl, limits); };

    // The zone itself takes clicks and drops. The file input used to lie on
    // top of it (invisible), so a dropped folder landed on the input, where
    // each browser handles folders its own way.
    dropEl.addEventListener('click', function (e) {
      if (e.target === inputEl) return; // the input's own click bubbling up
      inputEl.click();
    });
    dropEl.addEventListener('keydown', function (e) {
      if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); inputEl.click(); }
    });
    // A drop that misses the zone must not make the browser open the file or
    // folder in place of this page (and lose the form).
    ['dragover', 'drop'].forEach(function (type) {
      window.addEventListener(type, function (e) {
        if (!dropEl.contains(e.target)) e.preventDefault();
      });
    });

    inputEl.addEventListener('change', function () {
      collection.add(fromFileList(inputEl.files));
      refresh();
      inputEl.value = ''; // reset so same file can be added again
    });

    if (folderBtn && folderInput) {
      folderBtn.addEventListener('click', function () { folderInput.click(); });
      folderInput.addEventListener('change', function () {
        collection.add(fromFileList(folderInput.files));
        refresh();
        folderInput.value = '';
      });
    }

    dropEl.addEventListener('dragover', function (e) {
      e.preventDefault();
      dropEl.classList.add('dragover');
    });
    dropEl.addEventListener('dragleave', function () {
      dropEl.classList.remove('dragover');
    });
    dropEl.addEventListener('drop', function (e) {
      e.preventDefault();
      dropEl.classList.remove('dragover');
      if (!e.dataTransfer) return;
      fromDrop(e.dataTransfer).then(function (items) {
        collection.add(items);
        refresh();
      }, function () {
        if (noteEl) {
          noteEl.textContent = 'This folder could not be read. Try "Select a folder" instead.';
          noteEl.hidden = false;
        }
      });
    });
  }

  // Past this many rows the list scrolls on its own, so a folder of 1800
  // files does not push the upload button far down the page.
  var LIST_VISIBLE_ROWS = 20;

  function renderFileList(collection, listEl, noteEl, limits) {
    if (noteEl) {
      if (collection.count() > 1) {
        noteEl.textContent = collection.count() + ' files, ' + formatSize(collection.totalSize()) +
          (collection.hasFolder() ? '. The folder structure is kept.' : '.');
        noteEl.hidden = false;
      } else {
        noteEl.hidden = true;
      }
    }
    if (!listEl) return;
    listEl.innerHTML = '';
    collection.items.forEach(function (it, i) {
      var li = document.createElement('li');
      li.style.cssText = 'display:flex;align-items:center;gap:8px;padding:8px 12px;background:#f8f8f6;border-radius:6px;margin-top:6px;font-size:13px;';
      li.innerHTML =
        '<span>📄</span>' +
        '<span style="flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">' + escHtml(it.path) + '</span>' +
        '<span style="color:#888;flex-shrink:0">' + formatSize(it.file.size) + '</span>' +
        '<button type="button" style="background:none;border:none;cursor:pointer;color:#aaa;font-size:16px;padding:0 4px;flex-shrink:0" data-idx="' + i + '" title="Remove">×</button>';
      li.querySelector('button').addEventListener('click', function () {
        collection.remove(parseInt(this.dataset.idx));
        renderFileList(collection, listEl, noteEl, limits);
      });
      listEl.appendChild(li);
    });
    limitListHeight(listEl);
  }

  // limitListHeight caps the list at LIST_VISIBLE_ROWS rows, measured from
  // the rendered rows rather than a fixed pixel height, plus a sliver of the
  // next row so it is visible that the list scrolls.
  function limitListHeight(listEl) {
    listEl.style.maxHeight = '';
    listEl.style.overflowY = '';
    var rows = listEl.children;
    if (rows.length <= LIST_VISIBLE_ROWS) return;
    var top = listEl.getBoundingClientRect().top;
    var cut = rows[LIST_VISIBLE_ROWS].getBoundingClientRect().top;
    listEl.style.maxHeight = Math.round(cut - top + 14) + 'px';
    listEl.style.overflowY = 'auto';
  }

  // ── Send page ────────────────────────────────────────────────────────────────

  function initSendPage() {
    var form = document.getElementById('send-form');
    if (!form) return;

    var dropEl   = document.getElementById('file-drop');
    var inputEl  = document.getElementById('file-input');
    var listEl   = document.getElementById('file-list');
    var noteEl   = document.getElementById('file-note');
    var progWrap = document.getElementById('progress-wrap');
    var progFill = document.getElementById('progress-fill');
    var progLbl  = document.getElementById('progress-label');
    var resultEl = document.getElementById('result-msg');
    var limits   = readLimits(form);
    var collection = new FileCollection();
    // attempt is the transfer of the last try that did not finish. A new
    // try with the same form and files continues it, and sends only the
    // files that did not arrive yet; anything changed makes a new transfer.
    // Before, a failure at file 500 of 605 meant a new transfer from file 1.
    var attempt = null;

    initFileDrop(dropEl, inputEl, collection, listEl, limits);

    form.addEventListener('submit', async function (e) {
      e.preventDefault();

      if (collection.count() === 0) {
        showResult(resultEl, 'error', 'Please select at least one file.');
        return;
      }

      var uploads;
      try {
        uploads = buildUploads(collection, limits);
      } catch (err) {
        showResult(resultEl, 'error', err.message);
        return;
      }
      if (!confirmLargeTotal(uploads, limits)) return;

      setSubmitState(form, true);
      hideResult(resultEl); // a new try must not show the last try's error
      if (progWrap) progWrap.style.display = '';

      // One JSON field for the whole list: two form fields per file broke
      // at 500 files on the server's multipart part limit (audit M12).
      var data = new FormData(form);
      var fileList = JSON.stringify(uploads.map(function (u) { return { name: u.name, size: u.size }; }));
      data.set('files', fileList);
      var key = formKey(data);

      if (!attempt || attempt.key !== key) {
        updateProgress(progFill, progLbl, 0, 'Creating transfer…');
        try {
          var resp = await fetch('/send', { method: 'POST', body: data });
          var json = await resp.json();
          if (!resp.ok) {
            showResult(resultEl, 'error', json.error || 'Failed to create transfer.');
            setSubmitState(form, false);
            if (progWrap) progWrap.style.display = 'none';
            return;
          }
          attempt = { key: key, transferId: json.transfer_id, downloadUrl: json.download_url || null,
            manageUrl: json.manage_url || null, uploaded: {} };
        } catch (err) {
          showResult(resultEl, 'error', 'Network error. Please try again.');
          setSubmitState(form, false);
          if (progWrap) progWrap.style.display = 'none';
          return;
        }
      }
      var current = attempt;

      try {
        await uploadFiles(uploads.filter(function (u) { return !current.uploaded[uploadKey(u)]; }),
          { transfer_id: current.transferId },
          function (pct, label) { updateProgress(progFill, progLbl, pct, label); },
          function (item) { current.uploaded[uploadKey(item)] = true; });
      } catch (err) {
        showResult(resultEl, 'error', 'Upload failed: ' + err.message +
          ' Click the button again to continue: files that already arrived are not sent again.');
        setSubmitState(form, false);
        if (progWrap) progWrap.style.display = 'none';
        return;
      }
      var downloadUrl = current.downloadUrl;
      var manageUrl = current.manageUrl;
      attempt = null;

      if (progWrap) progWrap.style.display = 'none';

      if (downloadUrl) {
        showDownloadLink(resultEl, downloadUrl);
      } else {
        showResult(resultEl, 'success',
          collection.count() + ' file(s) uploaded. Recipients will receive a download link by email.');
      }
      if (manageUrl) appendManageLink(resultEl, manageUrl);

      form.reset();
      collection.clear();
      renderFileList(collection, listEl, noteEl, limits);
      setSubmitState(form, false);
      // Reset delivery toggle back to email mode
      var emailTab = document.querySelector('.delivery-tab[data-delivery="email"]');
      if (emailTab) emailTab.click();
    });
  }

  // ── Upload request page ──────────────────────────────────────────────────────

  function initUploadRequestPage() {
    var dropEl   = document.getElementById('file-drop');
    var inputEl  = document.getElementById('file-input');
    var listEl   = document.getElementById('file-list');
    var startBtn = document.getElementById('start-btn');
    var progWrap = document.getElementById('progress-wrap');
    var progFill = document.getElementById('progress-fill');
    var progLbl  = document.getElementById('progress-label');
    var resultEl = document.getElementById('result-msg');
    var limits   = readLimits(document.body);
    var collection = new FileCollection();
    // Files that already reached the server. A new attempt after an error
    // sends only the rest: they would otherwise show up twice in the request
    // (audit M13).
    var uploaded = {};

    if (!startBtn) return;

    initFileDrop(dropEl, inputEl, collection, listEl, limits);

    startBtn.addEventListener('click', async function () {
      if (collection.count() === 0) {
        showResult(resultEl, 'error', 'Please select at least one file.');
        return;
      }

      var uploads;
      try {
        uploads = buildUploads(collection, limits);
      } catch (err) {
        showResult(resultEl, 'error', err.message);
        return;
      }
      if (!confirmLargeTotal(uploads, limits)) return;
      uploads = uploads.filter(function (u) { return !uploaded[uploadKey(u)]; });

      startBtn.disabled = true;
      hideResult(resultEl);
      if (progWrap) progWrap.style.display = '';
      updateProgress(progFill, progLbl, 0, 'Starting upload…');

      try {
        await uploadFiles(uploads,
          { upload_request_token: window.FERRI_UPLOAD_TOKEN },
          function (pct, label) { updateProgress(progFill, progLbl, pct, label); },
          function (item) { uploaded[uploadKey(item)] = true; });
      } catch (err) {
        showResult(resultEl, 'error', 'Upload failed: ' + err.message +
          ' Click the button again to continue: files that already arrived are not sent again.');
        startBtn.disabled = false;
        if (progWrap) progWrap.style.display = 'none';
        return;
      }

      // All files uploaded — automatically submit the complete form.
      updateProgress(progFill, progLbl, 100, 'Finishing…');
      var completeForm = document.getElementById('complete-form');
      if (completeForm) {
        completeForm.submit();
      }
    });
  }

  // ── TUS upload engine ────────────────────────────────────────────────────────

  // RETRY_DELAYS: about 8.5 minutes in total. A deploy stops the app for up
  // to a few minutes (graceful shutdown, then start); the old 38 seconds gave
  // up long before it was back, and a 400 GB upload started over (audit M10).
  var RETRY_DELAYS = [0, 3000, 5000, 10000, 20000, 30000, 60000, 60000, 60000, 120000, 120000];

  // uploadFingerprint is the key under which tus-js-client remembers an
  // upload for resuming. It includes the transfer or request it belongs to:
  // tus-js-client's own key is only name, type, size and date, so a new
  // transfer with the same file would continue the old transfer's upload.
  function uploadFingerprint(file, name, extraMeta) {
    var owner = extraMeta.transfer_id ? 't:' + extraMeta.transfer_id : 'r:' + (extraMeta.upload_request_token || '');
    return ['ferri', owner, name, file.size, file.lastModified || 0].join('/');
  }

  // uploadKey identifies a file within one selection: its path and size.
  function uploadKey(u) { return u.name + '\u0000' + u.size; }

  // uploadFiles sends each item (a File) one after another. Progress counts
  // bytes over all items, so a big file weighs more than a small one.
  // onItemDone(item), optional, runs as each item has fully reached the server.
  function uploadFiles(uploads, extraMeta, onProgress, onItemDone) {
    return new Promise(function (resolve, reject) {
      var index = 0;
      var totalBytes = uploads.reduce(function (sum, u) { return sum + u.size; }, 0);
      var doneBytes = 0;
      var sentBytes = 0;
      var speed = speedMeter();

      function uploadNext() {
        if (index >= uploads.length) { resolve(); return; }

        var item = uploads[index];
        var num = index + 1;
        var itemBytes = -1;
        index++;

        var options = {
          endpoint: '/tus/',
          retryDelays: RETRY_DELAYS,
          chunkSize: 50 * 1024 * 1024,
          // Resumable: after an error, a new attempt, or a reload of the
          // upload page it continues where the server stopped instead of
          // starting over as a new file (audit M10, M13).
          storeFingerprintForResuming: true,
          removeFingerprintOnSuccess: true,
          fingerprint: function (file) {
            return Promise.resolve(uploadFingerprint(file, item.name, extraMeta));
          },
          metadata: Object.assign({ filename: item.name }, extraMeta),

          onProgress: function (bytesUploaded) {
            // Only bytes sent in this session count toward the speed: a
            // resumed file starts at the server's offset, a retried chunk
            // starts lower again.
            if (itemBytes >= 0 && bytesUploaded > itemBytes) sentBytes += bytesUploaded - itemBytes;
            itemBytes = bytesUploaded;
            var rate = speed(sentBytes);

            var pct = totalBytes > 0 ? Math.floor((doneBytes + bytesUploaded) / totalBytes * 100) : 0;
            if (onProgress) onProgress(pct, 'Uploading ' + item.name + ' (' + num + '/' + uploads.length + ', ' +
              formatSize(doneBytes + bytesUploaded) + ' of ' + formatSize(totalBytes) +
              (rate > 0 ? ', ' + formatSize(Math.round(rate)) + '/s' : '') + ')');
          },

          onSuccess: function () {
            doneBytes += item.size;
            if (onItemDone) onItemDone(item);
            uploadNext();
          },

          // tus-js-client's default, except that a full server (507) is
          // not retried: it will not have room seconds later either.
          onShouldRetry: function (error) {
            var status = httpStatus(error);
            if (status === 507) return false;
            return (status < 400 || status >= 500 || status === 409 || status === 423) && navigator.onLine !== false;
          },

          onError: function (error) {
            reject(new Error('file ' + num + ' of ' + uploads.length + ', ' + item.name + ': ' + serverReason(error) + '.'));
          },
        };

        var upload = new tus.Upload(item.source, options);
        upload.findPreviousUploads().then(function (previous) {
          if (previous.length > 0) upload.resumeFromPreviousUpload(previous[0]);
          upload.start();
        }, function () { upload.start(); });
      }

      uploadNext();
    });
  }

  // speedMeter returns a function that takes the bytes sent so far and gives
  // bytes per second over the last SPEED_WINDOW_MS, 0 until that is known.
  // The value changes at most every SPEED_REFRESH_MS, so it stays readable.
  var SPEED_WINDOW_MS = 5000;
  var SPEED_REFRESH_MS = 1000;

  function speedMeter() {
    var samples = [];
    var shown = 0;
    var shownAt = 0;
    return function (bytes) {
      var now = Date.now();
      samples.push({ t: now, bytes: bytes });
      while (samples.length > 1 && now - samples[1].t >= SPEED_WINDOW_MS) samples.shift();
      var dt = now - samples[0].t;
      if (now - shownAt >= SPEED_REFRESH_MS && dt >= SPEED_REFRESH_MS) {
        shown = (bytes - samples[0].bytes) * 1000 / dt;
        shownAt = now;
      }
      return shown;
    };
  }

  function httpStatus(error) {
    return error && error.originalResponse ? error.originalResponse.getStatus() : 0;
  }

  // serverReason: the server's own text for a refusal it explains (a 4xx, or
  // 507 storage full) instead of tus-js-client's "unexpected response …".
  // Anything else, such as a proxy's HTML error page, keeps the tus message.
  function serverReason(error) {
    var status = httpStatus(error);
    var body = error && error.originalResponse ? (error.originalResponse.getBody() || '').trim() : '';
    if (body && body.charAt(0) !== '<' && ((status >= 400 && status < 500) || status === 507)) {
      return body;
    }
    return (error && error.message) || String(error);
  }

  // ── UI helpers ───────────────────────────────────────────────────────────────

  // formKey is everything the send form would create a transfer from, the
  // file list included: equal keys mean a new try may continue the transfer.
  function formKey(data) {
    var parts = [];
    data.forEach(function (value, name) {
      if (typeof value === 'string') parts.push(name + '=' + value);
    });
    return parts.join('\u0000');
  }

  function updateProgress(fillEl, labelEl, pct, label) {
    if (fillEl) fillEl.style.width = pct + '%';
    if (labelEl) labelEl.textContent = label;
  }

  function hideResult(el) {
    if (el) el.style.display = 'none';
  }

  function showResult(el, type, msg) {
    if (!el) return;
    el.className = 'alert alert-' + (type === 'error' ? 'error' : 'success') + ' mt-16';
    el.textContent = msg;
    el.style.display = '';
  }

  function showDownloadLink(el, url) {
    if (!el) return;
    el.className = 'alert alert-success mt-16';
    el.style.display = '';
    el.innerHTML =
      '<div style="margin-bottom:8px;font-weight:500;">Upload complete — your download link:</div>' +
      '<div style="display:flex;gap:8px;align-items:center;">' +
        '<input type="text" readonly value="' + escHtml(url) + '" ' +
          'style="flex:1;font-size:12px;padding:6px 8px;border:1px solid #bbf7d0;' +
          'border-radius:5px;background:#f0fdf4;color:#166534;outline:none;">' +
        '<button type="button" id="copy-link-btn" ' +
          'style="padding:6px 12px;border:none;border-radius:5px;background:#166534;' +
          'color:#fff;font-size:12px;cursor:pointer;white-space:nowrap;font-family:inherit;">Copy</button>' +
      '</div>';
    var btn = el.querySelector('#copy-link-btn');
    if (btn) {
      btn.addEventListener('click', function() {
        if (navigator.clipboard && window.isSecureContext) {
          navigator.clipboard.writeText(url).then(function() {
            btn.textContent = 'Copied!';
            setTimeout(function() { btn.textContent = 'Copy'; }, 2000);
          });
        } else {
          var inp = el.querySelector('input');
          inp.select();
          document.execCommand('copy');
          btn.textContent = 'Copied!';
          setTimeout(function() { btn.textContent = 'Copy'; }, 2000);
        }
      });
    }
  }

  // appendManageLink adds the sender's manage page under the result. For a
  // link-only transfer this is the only place the sender gets it.
  function appendManageLink(el, url) {
    if (!el) return;
    var p = document.createElement('div');
    p.style.cssText = 'margin-top:10px;font-size:12px;';
    p.innerHTML = '<a href="' + escHtml(url) + '" target="_blank" rel="noopener" style="color:inherit;font-weight:500;">' +
      'Manage this transfer</a>: see who downloaded what, extend it or delete it. ' +
      'Keep this link; it only opens from the internal network.';
    el.appendChild(p);
  }

  function setSubmitState(form, disabled) {
    var btn = form.querySelector('button[type="submit"]');
    if (btn) btn.disabled = disabled;
  }

  function formatSize(bytes) {
    if (bytes < 1024) return bytes + ' B';
    if (bytes < 1048576) return (bytes / 1024).toFixed(1) + ' KB';
    if (bytes < 1073741824) return (bytes / 1048576).toFixed(1) + ' MB';
    return (bytes / 1073741824).toFixed(1) + ' GB';
  }

  function escHtml(str) {
    return str.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;').replace(/'/g, '&#39;');
  }

})();
