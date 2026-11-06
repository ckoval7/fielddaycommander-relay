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
