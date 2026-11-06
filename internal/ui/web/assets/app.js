// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

// Refreshes the status while the page is showing, without touching the forms.
const refreshSeconds = 5;

async function refresh() {
  if (document.hidden) {
    return;
  }
  try {
    const response = await fetch('/status');
    if (response.ok) {
      document.getElementById('status').innerHTML = await response.text();
    }
  } catch {
    // The relay has stopped. The page keeps showing what it last knew.
  }
}

setInterval(refresh, refreshSeconds * 1000);

// Show only the settings for the chosen role. Other PCs can only connect to a
// relay that's the N3FJP server.
const listen = document.querySelector('input[name="listen_on_network"]');
for (const role of document.querySelectorAll('input[name="role"]')) {
  role.addEventListener('change', () => {
    for (const group of document.querySelectorAll('[data-role]')) {
      group.hidden = group.dataset.role !== role.value;
    }
    listen.disabled = role.value !== 'server';
    if (listen.disabled) {
      listen.checked = false;
    }
  });
}
