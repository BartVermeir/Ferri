/**
 * upload.js — Ferri TUS upload client
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

  // Maintains a cumulative list of files across multiple selections
  function FileCollection() {
    this.files = [];
  }

  FileCollection.prototype.add = function (newFiles) {
    var existing = this.files.map(function (f) { return f.name + f.size; });
    Array.from(newFiles).forEach(function (f) {
      if (existing.indexOf(f.name + f.size) === -1) {
        this.files.push(f);
      }
    }, this);
  };

  FileCollection.prototype.remove = function (index) {
    this.files.splice(index, 1);
  };

  FileCollection.prototype.clear = function () {
    this.files = [];
  };

  FileCollection.prototype.count = function () {
    return this.files.length;
  };

  // ── File drop zone ───────────────────────────────────────────────────────────

  function initFileDrop(dropEl, inputEl, collection, listEl) {
    if (!dropEl || !inputEl) return;

    inputEl.addEventListener('change', function () {
      collection.add(inputEl.files);
      renderFileList(collection, listEl);
      inputEl.value = ''; // reset so same file can be added again
    });

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
      if (e.dataTransfer && e.dataTransfer.files.length > 0) {
        collection.add(e.dataTransfer.files);
        renderFileList(collection, listEl);
      }
    });
  }

  function renderFileList(collection, listEl) {
    if (!listEl) return;
    listEl.innerHTML = '';
    collection.files.forEach(function (f, i) {
      var li = document.createElement('li');
      li.style.cssText = 'display:flex;align-items:center;gap:8px;padding:8px 12px;background:#f8f8f6;border-radius:6px;margin-top:6px;font-size:13px;';
      li.innerHTML =
        '<span>📄</span>' +
        '<span style="flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">' + escHtml(f.name) + '</span>' +
        '<span style="color:#888;flex-shrink:0">' + formatSize(f.size) + '</span>' +
        '<button type="button" style="background:none;border:none;cursor:pointer;color:#aaa;font-size:16px;padding:0 4px;flex-shrink:0" data-idx="' + i + '" title="Remove">×</button>';
      li.querySelector('button').addEventListener('click', function () {
        collection.remove(parseInt(this.dataset.idx));
        renderFileList(collection, listEl);
      });
      listEl.appendChild(li);
    });
  }

  // ── Send page ────────────────────────────────────────────────────────────────

  function initSendPage() {
    var form = document.getElementById('send-form');
    if (!form) return;

    var dropEl   = document.getElementById('file-drop');
    var inputEl  = document.getElementById('file-input');
    var listEl   = document.getElementById('file-list');
    var progWrap = document.getElementById('progress-wrap');
    var progFill = document.getElementById('progress-fill');
    var progLbl  = document.getElementById('progress-label');
    var resultEl = document.getElementById('result-msg');
    var collection = new FileCollection();

    initFileDrop(dropEl, inputEl, collection, listEl);

    form.addEventListener('submit', async function (e) {
      e.preventDefault();

      if (collection.count() === 0) {
        showResult(resultEl, 'error', 'Please select at least one file.');
        return;
      }

      setSubmitState(form, true);
      if (progWrap) progWrap.style.display = '';
      updateProgress(progFill, progLbl, 0, 'Creating transfer…');

      var data = new FormData(form);
      collection.files.forEach(function (f) {
        data.append('filenames[]', f.name);
        data.append('sizes[]', f.size.toString());
      });
      data.delete('files');

      var transferId;
      var downloadUrl;
      try {
        var resp = await fetch('/send', { method: 'POST', body: data });
        var json = await resp.json();
        if (!resp.ok) {
          showResult(resultEl, 'error', json.error || 'Failed to create transfer.');
          setSubmitState(form, false);
          if (progWrap) progWrap.style.display = 'none';
          return;
        }
        transferId = json.transfer_id;
        downloadUrl = json.download_url || null;
      } catch (err) {
        showResult(resultEl, 'error', 'Network error. Please try again.');
        setSubmitState(form, false);
        if (progWrap) progWrap.style.display = 'none';
        return;
      }

      try {
        await uploadFiles(collection.files, { transfer_id: transferId },
          function (pct, label) { updateProgress(progFill, progLbl, pct, label); });
      } catch (err) {
        showResult(resultEl, 'error', 'Upload failed: ' + err.message);
        setSubmitState(form, false);
        if (progWrap) progWrap.style.display = 'none';
        return;
      }

      if (progWrap) progWrap.style.display = 'none';

      if (downloadUrl) {
        showDownloadLink(resultEl, downloadUrl);
      } else {
        showResult(resultEl, 'success',
          collection.count() + ' file(s) uploaded. Recipients will receive a download link by email.');
      }

      form.reset();
      collection.clear();
      renderFileList(collection, listEl);
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
    var doneArea = document.getElementById('done-area');
    var resultEl = document.getElementById('result-msg');
    var collection = new FileCollection();

    if (!startBtn) return;

    initFileDrop(dropEl, inputEl, collection, listEl);

    startBtn.addEventListener('click', async function () {
      if (collection.count() === 0) {
        showResult(resultEl, 'error', 'Please select at least one file.');
        return;
      }

      startBtn.disabled = true;
      if (progWrap) progWrap.style.display = '';
      updateProgress(progFill, progLbl, 0, 'Starting upload…');

      try {
        await uploadFiles(collection.files,
          { upload_request_token: window.FERRI_UPLOAD_TOKEN },
          function (pct, label) { updateProgress(progFill, progLbl, pct, label); });
      } catch (err) {
        showResult(resultEl, 'error', 'Upload failed: ' + err.message);
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

  function uploadFiles(files, extraMeta, onProgress) {
    return new Promise(function (resolve, reject) {
      var index = 0;

      function uploadNext() {
        if (index >= files.length) { resolve(); return; }

        var file = files[index];
        var fileNum = index + 1;
        index++;

        var upload = new tus.Upload(file, {
          endpoint: '/tus/',
          retryDelays: [0, 3000, 5000, 10000, 20000],
          chunkSize: 50 * 1024 * 1024,
          storeFingerprintForResuming: false,
          metadata: Object.assign({ filename: file.name }, extraMeta),

          onProgress: function (bytesUploaded, bytesTotal) {
            var pct = bytesTotal > 0
              ? Math.round(((fileNum - 1 + bytesUploaded / bytesTotal) / files.length) * 100)
              : 0;
            if (onProgress) onProgress(pct,
              'Uploading ' + file.name + ': ' +
              Math.round(bytesUploaded / bytesTotal * 100) + '% (' + fileNum + '/' + files.length + ')');
          },

          onSuccess: function () { uploadNext(); },

          onError: function (error) {
            reject(new Error(file.name + ': ' + (error.message || error)));
          },
        });

        upload.findPreviousUploads().then(function (prev) {
          if (prev.length > 0) upload.resumeFromPreviousUpload(prev[0]);
          upload.start();
        });
      }

      uploadNext();
    });
  }

  // ── UI helpers ───────────────────────────────────────────────────────────────

  function updateProgress(fillEl, labelEl, pct, label) {
    if (fillEl) fillEl.style.width = pct + '%';
    if (labelEl) labelEl.textContent = label;
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
    return str.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
  }

})();
