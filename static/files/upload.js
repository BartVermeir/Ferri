/**
 * upload.js — Ferri TUS upload client
 *
 * Folders, and more files than the server's per-transfer limit, are packed
 * into one ZIP in the browser (client-zip, no compression) and uploaded as a
 * single stream. Everything else is uploaded file by file. See DEC-035.
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

  // ── Limits and packing ───────────────────────────────────────────────────────

  // readLimits takes the server limits from data attributes on el.
  function readLimits(el) {
    return {
      maxFiles: parseInt(el && el.dataset.maxFiles, 10) || 50,
      maxBytes: Number(el && el.dataset.maxBytes) || Infinity,
    };
  }

  // shouldPack: a folder, or more files than one transfer may hold, goes up as
  // one ZIP. The ZIP keeps the folder structure; nothing is compressed.
  function shouldPack(collection, limits) {
    return collection.hasFolder() || collection.count() > limits.maxFiles;
  }

  // zipName: one top-level folder and nothing else → "<folder>.zip";
  // otherwise the title (send page) or "files".
  function zipName(collection, title) {
    var tops = {};
    var loose = false;
    collection.items.forEach(function (it) {
      var slash = it.path.indexOf('/');
      if (slash === -1) loose = true;
      else tops[it.path.slice(0, slash)] = true;
    });
    var names = Object.keys(tops);
    var base = (!loose && names.length === 1) ? names[0] : (title || 'files');
    base = base.replace(/[\/\\:*?"<>|\x00-\x1f]/g, '_').trim().slice(0, 100) || 'files';
    return base + '.zip';
  }

  // buildUploads turns the collection into what uploadFiles sends. Packed:
  // one item whose source is a ZIP stream reader of known length (TUS needs
  // the size up front; client-zip predicts it exactly). Otherwise one item per
  // file. Rejects with a readable message when something exceeds the limits.
  async function buildUploads(collection, limits, title) {
    if (!shouldPack(collection, limits)) {
      var tooBig = collection.items.filter(function (it) { return it.file.size > limits.maxBytes; });
      if (tooBig.length > 0) {
        throw new Error(tooBig[0].path + ' is larger than the limit of ' + formatSize(limits.maxBytes) + '.');
      }
      return collection.items.map(function (it) {
        return { source: it.file, name: it.path, size: it.file.size };
      });
    }

    var cz = await import('/static/client-zip.js');
    var entries = collection.items.map(function (it) {
      return { name: it.path, size: it.file.size, lastModified: new Date(it.file.lastModified), input: it.file };
    });
    // Metadata and input must be in the same order for an exact prediction.
    var size = Number(cz.predictLength(entries.map(function (e) {
      return { name: e.name, size: e.size, lastModified: e.lastModified };
    })));
    if (size > limits.maxBytes) {
      throw new Error('Together these files are ' + formatSize(size) + ', more than the limit of ' + formatSize(limits.maxBytes) + '.');
    }
    return [{ source: cz.makeZip(entries).getReader(), name: zipName(collection, title), size: size, packed: collection.count() }];
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
    var noteEl = document.getElementById('pack-note');
    var refresh = function () { renderFileList(collection, listEl, noteEl, limits); };

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

  // Long lists (a folder of 1600 files) show the first rows only.
  var LIST_PREVIEW = 100;

  function renderFileList(collection, listEl, noteEl, limits) {
    if (noteEl) {
      if (collection.count() > 0 && shouldPack(collection, limits)) {
        noteEl.textContent = collection.count() + ' file(s) will be sent as one ZIP file (' +
          formatSize(collection.totalSize()) + '), folders included.';
        noteEl.hidden = false;
      } else {
        noteEl.hidden = true;
      }
    }
    if (!listEl) return;
    listEl.innerHTML = '';
    collection.items.slice(0, LIST_PREVIEW).forEach(function (it, i) {
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
    if (collection.count() > LIST_PREVIEW) {
      var more = document.createElement('li');
      more.style.cssText = 'padding:8px 12px;font-size:13px;color:#888;';
      more.textContent = '… and ' + (collection.count() - LIST_PREVIEW) + ' more';
      listEl.appendChild(more);
    }
  }

  // ── Send page ────────────────────────────────────────────────────────────────

  function initSendPage() {
    var form = document.getElementById('send-form');
    if (!form) return;

    var dropEl   = document.getElementById('file-drop');
    var inputEl  = document.getElementById('file-input');
    var listEl   = document.getElementById('file-list');
    var noteEl   = document.getElementById('pack-note');
    var progWrap = document.getElementById('progress-wrap');
    var progFill = document.getElementById('progress-fill');
    var progLbl  = document.getElementById('progress-label');
    var resultEl = document.getElementById('result-msg');
    var limits   = readLimits(form);
    var collection = new FileCollection();

    initFileDrop(dropEl, inputEl, collection, listEl, limits);

    form.addEventListener('submit', async function (e) {
      e.preventDefault();

      if (collection.count() === 0) {
        showResult(resultEl, 'error', 'Please select at least one file.');
        return;
      }

      var uploads;
      try {
        uploads = await buildUploads(collection, limits, (form.elements.title && form.elements.title.value.trim()) || '');
      } catch (err) {
        showResult(resultEl, 'error', err.message);
        return;
      }

      setSubmitState(form, true);
      if (progWrap) progWrap.style.display = '';
      updateProgress(progFill, progLbl, 0, 'Creating transfer…');

      // One JSON field for the whole list: two form fields per file broke
      // at 500 files on the server's multipart part limit (audit M12).
      var data = new FormData(form);
      data.set('files', JSON.stringify(uploads.map(function (u) { return { name: u.name, size: u.size }; })));

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
        await uploadFiles(uploads, { transfer_id: transferId },
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

    if (!startBtn) return;

    initFileDrop(dropEl, inputEl, collection, listEl, limits);

    startBtn.addEventListener('click', async function () {
      if (collection.count() === 0) {
        showResult(resultEl, 'error', 'Please select at least one file.');
        return;
      }

      var uploads;
      try {
        uploads = await buildUploads(collection, limits, '');
      } catch (err) {
        showResult(resultEl, 'error', err.message);
        return;
      }

      startBtn.disabled = true;
      if (progWrap) progWrap.style.display = '';
      updateProgress(progFill, progLbl, 0, 'Starting upload…');

      try {
        await uploadFiles(uploads,
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

  // uploadFiles sends each item one after another. An item's source is a File,
  // or (packed) a ZIP stream reader with its exact size in item.size: a stream
  // has no size of its own, and TUS needs one before the first byte.
  function uploadFiles(uploads, extraMeta, onProgress) {
    return new Promise(function (resolve, reject) {
      var index = 0;

      function uploadNext() {
        if (index >= uploads.length) { resolve(); return; }

        var item = uploads[index];
        var num = index + 1;
        index++;

        var options = {
          endpoint: '/tus/',
          retryDelays: [0, 3000, 5000, 10000, 20000],
          chunkSize: 50 * 1024 * 1024,
          storeFingerprintForResuming: false,
          metadata: Object.assign({ filename: item.name }, extraMeta),

          onProgress: function (bytesUploaded, bytesTotal) {
            var pct = bytesTotal > 0
              ? Math.round(((num - 1 + bytesUploaded / bytesTotal) / uploads.length) * 100)
              : 0;
            var what = item.packed ? 'Packing and uploading ' + item.packed + ' files as ' + item.name : 'Uploading ' + item.name;
            if (onProgress) onProgress(pct,
              what + ': ' + Math.round(bytesUploaded / bytesTotal * 100) + '% (' + num + '/' + uploads.length + ')');
          },

          onSuccess: function () { uploadNext(); },

          onError: function (error) {
            reject(new Error(item.name + ': ' + (error.message || error)));
          },
        };
        if (!(item.source instanceof Blob)) {
          options.uploadSize = item.size;
        }

        new tus.Upload(item.source, options).start();
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
    return str.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;').replace(/'/g, '&#39;');
  }

})();
