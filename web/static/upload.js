/**
 * upload.js — Ferri TUS upload client
 *
 * Handles two contexts:
 *   1. Send page  — POST /send first, then TUS uploads with transfer_id
 *   2. Upload request page — TUS uploads with upload_request_token
 *
 * window.FERRI_UPLOAD_TOKEN is set by the upload request page template.
 */

(function () {
  'use strict';

  const isUploadRequest = typeof window.FERRI_UPLOAD_TOKEN === 'string';

  if (isUploadRequest) {
    initUploadRequestPage();
  } else {
    initSendPage();
  }

  // ── File drop zone ───────────────────────────────────────────────────────────

  function initFileDrop(dropEl, inputEl, onFilesChanged) {
    if (!dropEl || !inputEl) return;

    // Click to open file picker
    dropEl.addEventListener('click', function (e) {
      if (e.target === inputEl) return;
      inputEl.click();
    });

    // File input change
    inputEl.addEventListener('change', function () {
      onFilesChanged(Array.from(inputEl.files));
    });

    // Drag and drop
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
      const dt = e.dataTransfer;
      if (dt && dt.files.length > 0) {
        // Set files on the input (for form submission compatibility)
        try {
          const container = new DataTransfer();
          Array.from(dt.files).forEach(f => container.items.add(f));
          inputEl.files = container.files;
        } catch (err) { /* Safari fallback — files won't be on input */ }
        onFilesChanged(Array.from(dt.files));
      }
    });
  }

  function renderFileList(files, listEl) {
    if (!listEl) return;
    listEl.innerHTML = '';
    files.forEach(function (f) {
      const li = document.createElement('li');
      li.innerHTML = '<span>📄</span><span>' + escHtml(f.name) + '</span>' +
        '<span class="file-size">' + formatSize(f.size) + '</span>';
      listEl.appendChild(li);
    });
  }

  // ── Send page ────────────────────────────────────────────────────────────────

  function initSendPage() {
    const form = document.getElementById('send-form');
    if (!form) return;

    const dropEl   = document.getElementById('file-drop');
    const inputEl  = document.getElementById('file-input');
    const listEl   = document.getElementById('file-list');
    const progWrap = document.getElementById('progress-wrap');
    const progFill = document.getElementById('progress-fill');
    const progLbl  = document.getElementById('progress-label');
    const resultEl = document.getElementById('result-msg');

    let selectedFiles = [];

    initFileDrop(dropEl, inputEl, function (files) {
      selectedFiles = files;
      renderFileList(files, listEl);
    });

    form.addEventListener('submit', async function (e) {
      e.preventDefault();

      if (selectedFiles.length === 0) {
        showResult(resultEl, 'error', 'Please select at least one file.');
        return;
      }

      setSubmitState(form, true);
      if (progWrap) progWrap.style.display = '';
      updateProgress(progFill, progLbl, 0, 'Creating transfer…');

      // POST /send with file metadata
      const data = new FormData(form);
      selectedFiles.forEach(function (f) {
        data.append('filenames[]', f.name);
        data.append('sizes[]', f.size.toString());
      });
      data.delete('files');

      let transferId;
      try {
        const resp = await fetch('/send', { method: 'POST', body: data });
        const json = await resp.json();
        if (!resp.ok) {
          showResult(resultEl, 'error', json.error || 'Failed to create transfer.');
          setSubmitState(form, false);
          if (progWrap) progWrap.style.display = 'none';
          return;
        }
        transferId = json.transfer_id;
      } catch (err) {
        showResult(resultEl, 'error', 'Network error. Please try again.');
        setSubmitState(form, false);
        if (progWrap) progWrap.style.display = 'none';
        return;
      }

      // TUS uploads
      try {
        await uploadFiles(selectedFiles, { transfer_id: transferId },
          function (pct, label) { updateProgress(progFill, progLbl, pct, label); });
      } catch (err) {
        showResult(resultEl, 'error', 'Upload failed: ' + err.message);
        setSubmitState(form, false);
        if (progWrap) progWrap.style.display = 'none';
        return;
      }

      if (progWrap) progWrap.style.display = 'none';
      showResult(resultEl, 'success',
        selectedFiles.length + ' file(s) uploaded successfully. Recipients will receive a download link by email.');
      form.reset();
      selectedFiles = [];
      if (listEl) listEl.innerHTML = '';
      setSubmitState(form, false);
    });
  }

  // ── Upload request page ──────────────────────────────────────────────────────

  function initUploadRequestPage() {
    const dropEl   = document.getElementById('file-drop');
    const inputEl  = document.getElementById('file-input');
    const listEl   = document.getElementById('file-list');
    const startBtn = document.getElementById('start-btn');
    const progWrap = document.getElementById('progress-wrap');
    const progFill = document.getElementById('progress-fill');
    const progLbl  = document.getElementById('progress-label');
    const doneArea = document.getElementById('done-area');
    const resultEl = document.getElementById('result-msg');

    if (!startBtn) return;

    let selectedFiles = [];

    initFileDrop(dropEl, inputEl, function (files) {
      selectedFiles = files;
      renderFileList(files, listEl);
    });

    startBtn.addEventListener('click', async function () {
      if (selectedFiles.length === 0) {
        showResult(resultEl, 'error', 'Please select at least one file.');
        return;
      }

      startBtn.disabled = true;
      if (progWrap) progWrap.style.display = '';
      updateProgress(progFill, progLbl, 0, 'Starting upload…');

      try {
        await uploadFiles(selectedFiles,
          { upload_request_token: window.FERRI_UPLOAD_TOKEN },
          function (pct, label) { updateProgress(progFill, progLbl, pct, label); });
      } catch (err) {
        showResult(resultEl, 'error', 'Upload failed: ' + err.message);
        startBtn.disabled = false;
        if (progWrap) progWrap.style.display = 'none';
        return;
      }

      if (progWrap) progWrap.style.display = 'none';
      if (doneArea) doneArea.style.display = '';
    });
  }

  // ── TUS upload engine ────────────────────────────────────────────────────────

  function uploadFiles(files, extraMeta, onProgress) {
    return new Promise(function (resolve, reject) {
      let index = 0;

      function uploadNext() {
        if (index >= files.length) { resolve(); return; }

        const file = files[index];
        const fileNum = index + 1;
        index++;

        if (onProgress) onProgress(
          Math.round(((fileNum - 1) / files.length) * 100),
          'Uploading ' + file.name + ' (' + fileNum + '/' + files.length + ')…'
        );

        const upload = new tus.Upload(file, {
          endpoint: '/tus/',
          retryDelays: [0, 3000, 5000, 10000, 20000],
          chunkSize: 50 * 1024 * 1024,
          metadata: Object.assign({ filename: file.name }, extraMeta),

          onProgress: function (bytesUploaded, bytesTotal) {
            const pct = bytesTotal > 0
              ? Math.round(((fileNum - 1 + bytesUploaded / bytesTotal) / files.length) * 100)
              : 0;
            if (onProgress) onProgress(pct,
              'Uploading ' + file.name + ': ' + Math.round(bytesUploaded / bytesTotal * 100) + '% (' + fileNum + '/' + files.length + ')');
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

  function setSubmitState(form, disabled) {
    const btn = form.querySelector('button[type="submit"]');
    if (btn) btn.disabled = disabled;
  }

  function formatSize(bytes) {
    if (bytes < 1024) return bytes + ' B';
    if (bytes < 1024 * 1024) return (bytes / 1024).toFixed(1) + ' KB';
    if (bytes < 1024 * 1024 * 1024) return (bytes / 1024 / 1024).toFixed(1) + ' MB';
    return (bytes / 1024 / 1024 / 1024).toFixed(1) + ' GB';
  }

  function escHtml(str) {
    return str.replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;');
  }

})();
