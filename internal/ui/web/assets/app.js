// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

// Refreshes the status while the page is showing, without touching the forms.
const refreshSeconds = 5;

// The parts of the page that change by themselves.
const regions = ['status', 'log-files', 'station'];

async function refresh() {
  if (document.hidden) {
    return;
  }
  try {
    const response = await fetch('/status');
    if (response.ok) {
      const fresh = new DOMParser().parseFromString(await response.text(), 'text/html');
      for (const id of regions) {
        const region = document.getElementById(id);
        const html = fresh.getElementById(id)?.innerHTML;
        // Redrawing would take keyboard focus off a button. A mouse click leaves focus
        // there too, and that shouldn't stop the updates, but it would close an open
        // dropdown. Open help and a new station's name also wait, so they don't vanish.
        if (!region || html === undefined || html === region.innerHTML || region.querySelector(':focus-visible, select:focus, details[open], #new-station:not([hidden])')) {
          continue;
        }
        region.innerHTML = html;
      }
    }
  } catch {
    // The relay has stopped. The page keeps showing what it last knew.
  }
}

setInterval(refresh, refreshSeconds * 1000);
// Coming back from the file window should show the chosen file right away.
window.addEventListener('focus', refresh);

// Choices take effect as soon as they're made, so nobody forgets to save. The page
// comes back showing what the new choice needs.
document.addEventListener('change', (event) => {
  // Choosing to make a new station saves nothing until it has a name.
  if (event.target.selectedOptions?.[0]?.hasAttribute('data-new-station')) {
    const form = document.getElementById('new-station');
    form.hidden = false;
    form.elements.new_station.focus();
  } else if (event.target.matches('[data-autosave]')) {
    event.target.form.submit();
  }
});

// Help closes when someone clicks elsewhere or presses Escape.
function closeHelp(unless) {
  for (const help of document.querySelectorAll('details[data-help][open]')) {
    if (!help.contains(unless)) {
      help.open = false;
    }
  }
}

document.addEventListener('click', (event) => closeHelp(event.target));
document.addEventListener('keydown', (event) => {
  if (event.key === 'Escape') {
    const help = document.activeElement?.closest('details[data-help][open]');
    closeHelp();
    help?.querySelector('summary').focus();
  }
});
