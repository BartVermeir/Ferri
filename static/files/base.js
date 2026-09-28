/**
 * base.js — derives --primary-text (black/white) from --primary for contrast
 */

(function () {
  'use strict';

  var p = getComputedStyle(document.documentElement).getPropertyValue('--primary').trim();
  if (!p) return;
  var h = p.replace('#', '');
  if (h.length === 3) h = h[0] + h[0] + h[1] + h[1] + h[2] + h[2];
  var r = parseInt(h.substr(0, 2), 16) / 255, g = parseInt(h.substr(2, 2), 16) / 255, b = parseInt(h.substr(4, 2), 16) / 255;
  document.documentElement.style.setProperty('--primary-text', (0.2126 * r + 0.7152 * g + 0.0722 * b) > 0.5 ? '#000' : '#fff');
})();
