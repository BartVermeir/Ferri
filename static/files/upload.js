/**
 * upload.js — Ferri TUS upload client
 *
 * Handles two contexts:
 *   1. Send page (GET /) — POST /send first, then TUS uploads with transfer_id
 *   2. Upload request page (GET /ul/:token) — TUS uploads directly with upload_request_token
 *
 * Requires tus-js-client loaded separately (via CDN or bundled).
 * window.FERRI_UPLOAD_TOKEN is set by the upload request page template.
 *
 * TUS metadata sent per upload:
 *   Send context:    { transfer_id, filename }
 *   Request context: { upload_request_token, filename }
 */

(function () {
  'use strict';

  // ── Detect context ───────────────────────────────────────────────────────────

  const isUploadRequest = typeof window.FERRI_UPLOAD_TOKEN === 'string';

  if (isUploadRequest) {
    initUploadRequestPage();
  } else {
    initSendPage();
  }

  // ── Send page ────────────────────────────────────────────────────────────────

  function initSendPage() {
    const form = document.getElementById('send-form');
    if (!form) return;

    form.addEventListener('submit', async function (e) {
      e.preventDefault();

      const fileInput = form.querySelector('input[type="file"]');
      const files = Array.from(fileInput.files);

      if (files.length === 0) {
        showError('Please select at least one file.');
        return;
      }

      setSubmitState(form, true);
      showProgress('Creating transfer...');

      // Build form data for POST /send
      // Sends filenames[] and sizes[] — actual file data goes via TUS
      const data = new FormData(form);
      files.forEach(f => {
        data.append('filenames[]', f.name);
        data.append('sizes[]', f.size.toString());
      });
      // Remove the actual file objects — we don't want to POST the file data here
      data.delete('files');

      let transferId;
      try {
        const resp = await fetch('/send', { method: 'POST', body: data });
        const json = await resp.json();
        if (!resp.ok) {
          showError(json.error || 'Failed to create transfer.');
          setSubmitState(form, false);
          return;
        }
        transferId = json.transfer_id;
      } catch (err) {
        showError('Network error. Please try again.');
        setSubmitState(form, false);
        return;
      }

      // Start TUS uploads
      showProgress(`Uploading ${files.length} file(s)...`);
      try {
        await uploadFiles(files, { transfer_id: transferId });
      } catch (err) {
        showError('Upload failed: ' + err.message);
        setSubmitState(form, false);
        return;
      }

      showSuccess(`${files.length} file(s) uploaded successfully. Recipients will receive a download link by email.`);
    });
  }

  // ── Upload request page ──────────────────────────────────────────────────────

  function initUploadRequestPage() {
    const fileInput = document.getElementById('file-input');
    const uploadBtn = document.getElementById('upload-btn');
    const completeForm = document.getElementById('complete-form');
    if (!fileInput || !uploadBtn) return;

    uploadBtn.addEventListener('click', async function () {
      const files = Array.from(fileInput.files);
      if (files.length === 0) {
        showError('Please select at least one file.');
        return;
      }

      uploadBtn.disabled = true;
      showProgress(`Uploading ${files.length} file(s)...`);

      try {
        await uploadFiles(files, { upload_request_token: window.FERRI_UPLOAD_TOKEN });
      } catch (err) {
        showError('Upload failed: ' + err.message);
        uploadBtn.disabled = false;
        return;
      }

      showProgress('All files uploaded.');

      // Show the "I\'m done" button
      if (completeForm) {
        completeForm.style.display = '';
      }
    });
  }

  // ── TUS upload engine ────────────────────────────────────────────────────────

  /**
   * Upload all files sequentially via TUS.
   * Returns a promise that resolves when all uploads complete,
   * or rejects on the first failure.
   *
   * @param {File[]} files
   * @param {Object} extraMeta - added to TUS metadata (transfer_id or upload_request_token)
   */
  function uploadFiles(files, extraMeta) {
    return new Promise(function (resolve, reject) {
      let index = 0;

      function uploadNext() {
        if (index >= files.length) {
          resolve();
          return;
        }

        const file = files[index];
        index++;

        showProgress(`Uploading ${file.name} (${index}/${files.length})...`);

        const upload = new tus.Upload(file, {
          endpoint: '/tus/',
          retryDelays: [0, 3000, 5000, 10000, 20000],
          chunkSize: 50 * 1024 * 1024, // 50 MB chunks — suitable for large files
          metadata: Object.assign({ filename: file.name }, extraMeta),

          onProgress: function (bytesUploaded, bytesTotal) {
            const pct = bytesTotal > 0
              ? Math.round((bytesUploaded / bytesTotal) * 100)
              : 0;
            showProgress(`Uploading ${file.name}: ${pct}% (${index}/${files.length})`);
          },

          onSuccess: function () {
            uploadNext();
          },

          onError: function (error) {
            reject(new Error(`${file.name}: ${error.message || error}`));
          },
        });

        // Check for a resumable upload before starting
        upload.findPreviousUploads().then(function (previousUploads) {
          if (previousUploads.length > 0) {
            upload.resumeFromPreviousUpload(previousUploads[0]);
          }
          upload.start();
        });
      }

      uploadNext();
    });
  }

  // ── UI helpers ───────────────────────────────────────────────────────────────

  function getOrCreateStatus() {
    let el = document.getElementById('ferri-status');
    if (!el) {
      el = document.createElement('div');
      el.id = 'ferri-status';
      el.style.cssText = 'margin: 1rem 0; padding: 0.75rem; border-radius: 4px;';
      const form = document.getElementById('send-form') ||
                   document.getElementById('upload-area') ||
                   document.body;
      form.parentNode.insertBefore(el, form.nextSibling);
    }
    return el;
  }

  function showProgress(msg) {
    const el = getOrCreateStatus();
    el.style.background = '#f0f0f0';
    el.style.color = '#333';
    el.textContent = msg;
  }

  function showError(msg) {
    const el = getOrCreateStatus();
    el.style.background = '#fee';
    el.style.color = '#c00';
    el.textContent = msg;
  }

  function showSuccess(msg) {
    const el = getOrCreateStatus();
    el.style.background = '#efe';
    el.style.color = '#060';
    el.textContent = msg;
  }

  function setSubmitState(form, disabled) {
    const btn = form.querySelector('button[type="submit"]');
    if (btn) btn.disabled = disabled;
  }

})();
