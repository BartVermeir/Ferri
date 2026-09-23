/**
 * upload-init.js — exposes the upload request token to upload.js via a data attribute
 */

(function () {
  'use strict';
  window.FERRI_UPLOAD_TOKEN = document.body.dataset.uploadToken;
})();
