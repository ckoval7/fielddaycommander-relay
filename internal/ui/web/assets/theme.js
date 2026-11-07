// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

// Loaded before the page paints, so the chosen theme never flashes the other one.
// Follows the system's theme until someone uses the switcher, and then keeps their
// choice. The storage key is the same as Field Day Commander's.
(function () {
  const system = window.matchMedia('(prefers-color-scheme: dark)');

  function chosen() {
    try {
      const theme = localStorage.getItem('theme');
      return theme === 'dark' || theme === 'light' ? theme : null;
    } catch {
      return null;
    }
  }

  function current() {
    return chosen() || (system.matches ? 'dark' : 'light');
  }

  function label(theme) {
    const button = document.getElementById('theme-toggle');
    if (button) {
      const text = theme === 'dark' ? button.dataset.toLight : button.dataset.toDark;
      button.setAttribute('aria-label', text);
      button.title = text;
    }
  }

  function apply(theme) {
    document.documentElement.dataset.theme = theme;
    label(theme);
  }

  apply(current());

  system.addEventListener('change', function () {
    if (!chosen()) {
      apply(current());
    }
  });

  document.addEventListener('DOMContentLoaded', function () {
    label(document.documentElement.dataset.theme);
    document.getElementById('theme-toggle')?.addEventListener('click', function () {
      const next = document.documentElement.dataset.theme === 'dark' ? 'light' : 'dark';
      apply(next);
      try {
        localStorage.setItem('theme', next);
      } catch {
        // The choice lasts for this page only.
      }
    });
  });
})();
