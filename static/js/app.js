/* ──────────────────────────────────────────────────────────────
   kontrolplane/feed — app.js
   The only imperative JavaScript in the app. No build step, no
   modules, no dependencies: this is loaded with a plain
   <script defer> and htmx does everything else.

   It replaces hotkeys.js, which had four structural bugs:

     1. j/k called items[n].click(), which fired the row's htmx
        request AND marked the item read. Scrolling the list with
        the keyboard silently burned through the unread queue.
        Navigation and opening are now separate verbs.
     2. s and m reached into the reader and clicked
        `#reader .actions .rh button` by POSITIONAL INDEX. Reordering
        two buttons rebound "mark read" to "star" with no error.
        Both now target the hx-post URL, which is what the button
        actually is rather than where it happens to sit.
     3. The guard only checked input/textarea/select, so Cmd+K,
        Ctrl+R and every other modifier combination was swallowed as
        a bare-key shortcut, and contenteditable was missed entirely.
     4. Nothing announced htmx swaps, nothing moved focus after one,
        and an HTTP 500 produced absolutely nothing on screen —
        htmx does not swap a non-2xx response, so a failed request
        and a slow one looked identical.

   Everything below is delegated from `document` so it survives
   htmx replacing whole panes. The only per-swap rebinding is the
   reader's scroll listener, and even that is capture-phase.
   ────────────────────────────────────────────────────────────── */

(function () {
  'use strict';

  /* ══════════════════════════════════════════════════════════════
     0 / UTILITIES
     Every selector in this file goes through these. A pane that a
     concurrent template change removed must degrade to a no-op,
     never throw — one exception here would take down the keyboard,
     the theme and the error toast all at once.
     ══════════════════════════════════════════════════════════════ */

  function $(sel, root) {
    try { return (root || document).querySelector(sel); } catch (e) { return null; }
  }
  function $$(sel, root) {
    try { return Array.prototype.slice.call((root || document).querySelectorAll(sel)); }
    catch (e) { return []; }
  }
  function app() { return $('.app'); }

  /* offsetParent is null for anything display:none (and for fixed
     elements, hence the second test). Focusing or announcing a pane
     that is not on screen is worse than doing nothing. */
  function isVisible(el) {
    if (!el) return false;
    if (el.offsetParent !== null) return true;
    var cs = window.getComputedStyle ? window.getComputedStyle(el) : null;
    return !!(cs && cs.position === 'fixed' && cs.display !== 'none');
  }

  /* localStorage throws in private mode and when the quota is full.
     Reading a preference is never important enough to break the page. */
  var ls = {
    get: function (k) { try { return window.localStorage.getItem(k); } catch (e) { return null; } },
    set: function (k, v) { try { window.localStorage.setItem(k, v); } catch (e) { /* ignore */ } },
    del: function (k) { try { window.localStorage.removeItem(k); } catch (e) { /* ignore */ } }
  };

  var KEY_THEME    = 'theme';
  var KEY_DENSITY  = 'density';
  var KEY_SIDEBAR  = 'sidebar-collapsed';
  var KEY_LIST     = 'list-collapsed';
  var KEY_FOLDERS  = 'folders-open';

  var reduceMotion = window.matchMedia
    ? window.matchMedia('(prefers-reduced-motion: reduce)')
    : { matches: false };

  /* The three breakpoints from the design spec, as media queries
     rather than as a width read at call time: a resize past 960px
     must change behaviour without a reload. */
  var mqOnePane = window.matchMedia ? window.matchMedia('(max-width: 959px)') : { matches: false };
  var mqDrawer  = window.matchMedia ? window.matchMedia('(max-width: 1279px)') : { matches: false };

  /* A dialog is modal chrome: while one is open the list shortcuts
     must not fire underneath it. */
  function openDialog() { return $('dialog[open]'); }

  function isEditable(el) {
    if (!el || el.nodeType !== 1) return false;
    var tag = (el.tagName || '').toLowerCase();
    if (tag === 'input' || tag === 'textarea' || tag === 'select') return true;
    /* isContentEditable covers inherited editability, which a bare
       [contenteditable] attribute check misses — the old guard let
       every key through inside a rich-text region. */
    return el.isContentEditable === true;
  }

  /* ══════════════════════════════════════════════════════════════
     1 / THEME
     The plate is decided before first paint by the inline script in
     layout.templ. This module owns everything after that: the flip,
     the persistence, and keeping every aria-pressed in the document
     truthful — the settings page renders its segmented control with
     aria-pressed="false" on both options and relies on us to fix it,
     because the server has no idea which plate this browser chose.
     ══════════════════════════════════════════════════════════════ */

  function currentTheme() {
    return document.documentElement.getAttribute('data-theme') === 'dark' ? 'dark' : 'light';
  }

  function applyTheme(t) {
    var dark = t === 'dark';
    if (dark) document.documentElement.setAttribute('data-theme', 'dark');
    else document.documentElement.removeAttribute('data-theme');
    syncTheme();
  }

  function syncTheme() {
    var t = currentTheme();
    $$('[data-theme-opt], [data-theme-option]').forEach(function (b) {
      var v = b.getAttribute('data-theme-opt') || b.getAttribute('data-theme-option');
      b.setAttribute('aria-pressed', String(v === t));
    });
    var toggle = $('#theme-toggle');
    if (toggle) {
      /* aria-pressed reports the state; the label says what pressing
         it will DO. An icon button whose name is "dark" is ambiguous
         about whether that is the current or the offered plate. */
      toggle.setAttribute('aria-pressed', String(t === 'dark'));
      toggle.setAttribute('aria-label', t === 'dark' ? 'use the light theme' : 'use the dark theme');
    }
  }

  function setTheme(t) {
    t = t === 'dark' ? 'dark' : 'light';
    ls.set(KEY_THEME, t);
    applyTheme(t);
    announce(t === 'dark' ? 'dark theme' : 'light theme');
  }

  function toggleTheme() { setTheme(currentTheme() === 'dark' ? 'light' : 'dark'); }

  function initTheme() {
    var stored = ls.get(KEY_THEME);
    if (stored === 'dark' || stored === 'light') {
      applyTheme(stored);
    } else if (window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches) {
      /* Nothing stored: follow the operating system, but do NOT write
         it down. Persisting here would freeze the browser onto
         whatever the OS happened to be the first time the app was
         opened, and the user would never see it follow again. */
      applyTheme('dark');
    } else {
      syncTheme();
    }

    if (!window.matchMedia) return;
    var os = window.matchMedia('(prefers-color-scheme: dark)');
    var onOS = function (e) {
      var s = ls.get(KEY_THEME);
      if (s === 'dark' || s === 'light') return;  /* an explicit choice wins */
      applyTheme(e.matches ? 'dark' : 'light');
    };
    if (os.addEventListener) os.addEventListener('change', onOS);
    else if (os.addListener) os.addListener(onOS);
  }

  /* ══════════════════════════════════════════════════════════════
     2 / DENSITY
     tokens.css already maps [data-density] on .app to --row-y and
     --row-gap, so this writes one attribute and nothing else. The
     server's default attribute value is not necessarily one of our
     three steps (it is whatever DENSITY was set to), so anything
     unrecognised normalises to "default" rather than leaving a
     segmented control with no pressed option at all.
     ══════════════════════════════════════════════════════════════ */

  var DENSITIES = ['tight', 'default', 'loose'];

  function normaliseDensity(v) {
    return DENSITIES.indexOf(v) === -1 ? 'default' : v;
  }

  function currentDensity() {
    var a = app();
    return normaliseDensity(a ? a.getAttribute('data-density') : 'default');
  }

  function syncDensity() {
    var d = currentDensity();
    /* Two spellings in the wild: the list header emits
       [data-density-option], the settings page [data-density-opt].
       Supporting both costs one selector and avoids a dead control
       on whichever pane loses the coin toss. */
    $$('[data-density-opt], [data-density-option]').forEach(function (b) {
      var v = b.getAttribute('data-density-opt') || b.getAttribute('data-density-option');
      b.setAttribute('aria-pressed', String(normaliseDensity(v) === d));
    });
  }

  function setDensity(v) {
    v = normaliseDensity(v);
    var a = app();
    if (a) a.setAttribute('data-density', v);
    ls.set(KEY_DENSITY, v);
    syncDensity();
  }

  function initDensity() {
    var a = app();
    if (!a) return;
    var stored = ls.get(KEY_DENSITY);
    a.setAttribute('data-density', normaliseDensity(stored || a.getAttribute('data-density')));
    syncDensity();
  }

  /* ══════════════════════════════════════════════════════════════
     3 / PANES
     Two different jobs that used to be one tangled global in
     layout.templ:

     togglePane() collapses a column at ≥1280px, where all three are
     visible and the reader wants the width.

     showPane() switches which single pane is on screen below 960px.
     It is guarded by a media query because above that breakpoint the
     panes sit side by side — flipping [data-pane] there would hide a
     column the user can see, which is what "open an item on a
     laptop" would otherwise do (every row calls showPane('reader')
     from its onclick, on every viewport).
     ══════════════════════════════════════════════════════════════ */

  function togglePane(pane) {
    var a = app();
    if (!a) return;
    var cls, key;
    if (pane === 'sidebar') { cls = 'sidebar-collapsed'; key = KEY_SIDEBAR; }
    else if (pane === 'list') { cls = 'list-collapsed'; key = KEY_LIST; }
    else return;
    var on = a.classList.toggle(cls);
    ls.set(key, on ? '1' : '0');
    announce(pane + (on ? ' hidden' : ' shown'));
  }

  function showPane(p) {
    if (p !== 'list' && p !== 'reader') return;
    if (!mqOnePane.matches) return;
    var a = app();
    if (a) a.setAttribute('data-pane', p);
  }

  /* ══════════════════════════════════════════════════════════════
     4 / DRAWER
     Between 960 and 1279px the sidebar is an overlay. An overlay
     that you can open but that never receives focus is unusable
     from a keyboard — you tab from the toggle straight past it into
     the list underneath — so opening moves focus in and closing
     puts it back on the control that opened it.
     ══════════════════════════════════════════════════════════════ */

  function drawerOpen() {
    var a = app();
    return !!(a && a.classList.contains('drawer-open'));
  }

  function toggleDrawer(open) {
    var a = app();
    if (!a) return;
    var want = (open === undefined || open === null) ? !drawerOpen() : !!open;
    a.classList.toggle('drawer-open', want);

    var toggle = $('#drawer-toggle');
    if (toggle) toggle.setAttribute('aria-expanded', String(want));

    var sidebar = $('#sidebar');
    if (want) {
      if (sidebar) {
        var first = $('a[href], button:not([disabled]), [tabindex]:not([tabindex="-1"])', sidebar);
        if (first) first.focus({ preventScroll: true });
        else {
          sidebar.setAttribute('tabindex', '-1');
          sidebar.focus({ preventScroll: true });
        }
      }
    } else if (toggle) {
      /* Only pull focus back if it is currently inside the thing we
         just hid; otherwise closing the drawer as a side effect of
         navigating would yank focus off the destination. */
      if (!sidebar || sidebar.contains(document.activeElement) || document.activeElement === document.body) {
        toggle.focus({ preventScroll: true });
      }
    }
  }

  /* ══════════════════════════════════════════════════════════════
     5 / DECLARATIVE ACTIONS
     layout.templ marks its three chrome controls with [data-action]
     instead of an inline handler. One delegated listener binds them
     all, and it keeps working if the topbar is ever swapped.
     ══════════════════════════════════════════════════════════════ */

  function searchInput() { return $('#q') || $('input[name="q"]'); }

  function clearSearch(focus) {
    var q = searchInput();
    if (!q) return false;
    var had = q.value !== '';
    q.value = '';
    /* The input's hx-trigger includes `search`, so dispatching it
       re-runs the query with an empty q and restores the full list.
       Clearing the field without this left the results of a query
       the user can no longer see. */
    if (had) q.dispatchEvent(new Event('search', { bubbles: true }));
    if (focus) q.focus();
    return had;
  }

  document.addEventListener('click', function (e) {
    var t = e.target;
    if (!t || !t.closest) return;

    var actor = t.closest('[data-action]');
    if (actor) {
      var action = actor.getAttribute('data-action');
      if (action === 'toggle-drawer') { e.preventDefault(); toggleDrawer(); return; }
      if (action === 'close-drawer') { e.preventDefault(); toggleDrawer(false); return; }
      if (action === 'clear-search') { e.preventDefault(); clearSearch(true); return; }
    }

    /* The settings page renders its theme / density segmented
       controls as plain buttons with no handler — the server cannot
       know which one is pressed, so the behaviour has to live here. */
    var themeBtn = t.closest('[data-theme-opt], [data-theme-option]');
    if (themeBtn) {
      e.preventDefault();
      setTheme(themeBtn.getAttribute('data-theme-opt') || themeBtn.getAttribute('data-theme-option'));
      return;
    }
    var densityBtn = t.closest('[data-density-opt], [data-density-option]');
    if (densityBtn) {
      e.preventDefault();
      setDensity(densityBtn.getAttribute('data-density-opt') || densityBtn.getAttribute('data-density-option'));
      return;
    }

    /* Clicking a row selects it, so the keyboard picks up where the
       mouse left off instead of jumping back to the top of the list. */
    var row = t.closest('.list-body .item');
    if (row) setActive(row, false);
  });

  /* ══════════════════════════════════════════════════════════════
     6 / MODALS
     The modals agent swaps a <dialog class="modal"> into #modal.
     Using the native element buys the backdrop, the inertness of the
     page behind it, Escape-to-close and a real focus trap — all four
     of which the old scrim-div reimplemented badly or not at all.

     What it does NOT buy is focus RESTORATION when the dialog is
     removed from the DOM rather than closed (htmx clearing #modal),
     so we record where focus was before the request and put it back.
     ══════════════════════════════════════════════════════════════ */

  var modalReturnFocus = null;

  function modalHost() { return $('#modal'); }

  function showModal() {
    var host = modalHost();
    if (!host) return;
    var dlg = $('dialog.modal', host) || $('dialog', host);
    if (!dlg || typeof dlg.showModal !== 'function') return;
    if (!dlg.open) {
      try { dlg.showModal(); } catch (e) { return; }
    }
    /* Prefer an explicitly marked field, then the first real control.
       <dialog> otherwise focuses the first focusable child, which is
       usually the close button — landing a keyboard user on "cancel". */
    var focusTarget = $('[autofocus]', dlg) || $('input:not([type="hidden"]), textarea, select', dlg);
    if (focusTarget) {
      try { focusTarget.focus({ preventScroll: true }); } catch (e) { /* ignore */ }
    }
  }

  function closeModal() {
    var host = modalHost();
    if (!host) return;
    var dlg = $('dialog[open]', host);
    if (dlg) dlg.close();
    else host.innerHTML = '';
  }

  document.addEventListener('click', function (e) {
    var t = e.target;
    if (!t || !t.closest) return;

    var closer = t.closest('[data-close-modal]');
    if (closer) {
      e.preventDefault();
      var d = closer.closest('dialog');
      if (d && typeof d.close === 'function') d.close();
      else closeModal();
      return;
    }

    /* A <dialog>'s ::backdrop is painted by the dialog itself, so a
       click on the backdrop arrives with target === the dialog. A
       click on anything inside it targets that child instead, which
       is what makes this one line a correct backdrop test and not
       the stopPropagation dance the old markup used. */
    if (t.tagName === 'DIALOG' && t.classList.contains('modal') && t.open) {
      t.close();
    }
  });

  /* `close` fires for every route out of a dialog — the close button,
     Escape, a form submit — so the teardown lives here once. */
  document.addEventListener('close', function (e) {
    var dlg = e.target;
    if (!dlg || dlg.tagName !== 'DIALOG') return;
    var host = modalHost();
    if (host && host.contains(dlg)) host.innerHTML = '';
    var back = modalReturnFocus;
    modalReturnFocus = null;
    if (back && document.contains(back) && typeof back.focus === 'function') {
      try { back.focus({ preventScroll: true }); } catch (err) { /* ignore */ }
    }
  }, true);  /* `close` does not bubble */

  /* ── folder picker ──────────────────────────────────────────────
     Delegated, because this markup arrives inside a swapped-in modal
     and again inside swapped-in probe results. It was previously a
     200-character inline onchange duplicated per render that poked
     style.display directly — invisible to assistive tech, which was
     still announcing the hidden field as an available form control. */
  document.addEventListener('change', function (e) {
    var sel = e.target;
    if (!sel || !sel.closest || !sel.matches || !sel.matches('.folder-picker select')) return;
    var picker = sel.closest('.folder-picker');
    if (!picker) return;
    var panel = $('.folder-picker-new', picker);
    if (!panel) return;
    var input = $('input', panel);
    if (sel.value === '__new__') {
      panel.hidden = false;
      if (input) input.focus();
    } else {
      panel.hidden = true;
      if (input) input.value = '';
    }
  });

  /* ══════════════════════════════════════════════════════════════
     7 / LIST KEYBOARD NAVIGATION
     The core fix. j/k move a highlight and nothing else; o/Enter is
     the only thing that opens an item. Previously j/k called
     .click(), so holding j marked every article in the queue read
     and fired one request per keypress.
     ══════════════════════════════════════════════════════════════ */

  function rows() { return $$('.list-body .item'); }
  function activeRow() { return $('.list-body .item.active'); }

  function setActive(el, moveFocus) {
    var prev = activeRow();
    if (prev && prev !== el) prev.classList.remove('active');
    if (!el) return;
    el.classList.add('active');
    el.scrollIntoView({ block: 'nearest' });
    if (moveFocus === false) return;
    /* Real focus, not just a class. A highlight that the browser does
       not know about is invisible to a screen reader, which keeps
       reading wherever its own cursor happens to be. An <a href> is
       already focusable, so only give a tabindex to something that
       is not — adding tabindex="-1" to the anchor would pull the row
       out of the tab order entirely. */
    var focusable = /^(a|button|input|select|textarea)$/.test((el.tagName || '').toLowerCase()) && !el.hasAttribute('disabled');
    if (!focusable && !el.hasAttribute('tabindex')) el.setAttribute('tabindex', '-1');
    try { el.focus({ preventScroll: true }); } catch (e) { /* ignore */ }
  }

  function moveActive(delta) {
    var all = rows();
    if (!all.length) return;
    var cur = activeRow();
    var idx = cur ? all.indexOf(cur) : -1;
    var next = idx === -1 ? (delta > 0 ? 0 : all.length - 1)
                          : Math.min(all.length - 1, Math.max(0, idx + delta));
    setActive(all[next], true);
  }

  function openActive() {
    var el = activeRow() || rows()[0];
    if (!el) return;
    setActive(el, false);
    /* A dispatched click is what triggers the row's hx-get. This is
       the ONE place that is allowed to do it. */
    el.click();
    showPane('reader');
  }

  /* The reader identifies its item through the hx-post on its own
     toggles — `/items/{id}/star`. That is a stable contract (the
     route table is fixed) and it is how we re-find the open row
     after the list is swapped out from under us. */
  function readerItemID() {
    var btn = $('#reader [hx-post$="/star"]');
    if (!btn) return null;
    var m = /\/items\/(.+)\/star$/.exec(btn.getAttribute('hx-post') || '');
    return m ? m[1] : null;
  }

  function clickReaderAction(suffix, label) {
    var btn = $('#reader [hx-post$="/' + suffix + '"]');
    if (!btn) { announce('no item open'); return; }
    btn.click();
    announce(label);
  }

  function restoreActive() {
    if (activeRow()) return;
    var id = readerItemID();
    var el = id ? $('#item-' + (window.CSS && CSS.escape ? CSS.escape(id) : id) + ' .item') : null;
    /* Selection follows the reader, not the scroll position: if an
       article is open, j continues from there rather than restarting
       at the top of a list that was just re-sorted. Focus is NOT
       moved — a swap the user did not ask for should not steal it. */
    setActive(el || rows()[0], false);
  }

  var gPending = 0;

  document.addEventListener('keydown', function (e) {
    /* Modifier combinations belong to the browser and the OS. The old
       handler ate Cmd+K, Ctrl+R, Alt+arrow and every other one of
       them by matching on e.key alone. */
    if (e.metaKey || e.ctrlKey || e.altKey) return;

    var key = e.key;
    var target = e.target;

    /* ESCAPE is the one key that must work from inside a field. */
    if (key === 'Escape') {
      if (openDialog()) return;                       /* <dialog> closes itself */
      if (drawerOpen()) { e.preventDefault(); toggleDrawer(false); return; }
      if (isEditable(target)) {
        var q = searchInput();
        if (target === q && q.value !== '') { e.preventDefault(); clearSearch(true); return; }
        target.blur();
        return;
      }
      if (clearSearch(false)) return;
      if (document.activeElement && document.activeElement.blur) document.activeElement.blur();
      return;
    }

    if (isEditable(target)) return;
    if (openDialog()) return;

    /* `g` `g` — a two-key sequence, so the window has to be short
       enough that a stray g does not arm a jump minutes later. */
    if (gPending && Date.now() - gPending < 700) {
      gPending = 0;
      if (key === 'g') {
        e.preventDefault();
        var body = $('#list-body');
        if (body) body.scrollTop = 0;
        var first = rows()[0];
        if (first) setActive(first, true);
        announce('top of list');
        return;
      }
    }
    gPending = 0;

    switch (key) {
      case 'j':
        e.preventDefault();
        moveActive(1);
        return;

      case 'k':
        e.preventDefault();
        moveActive(-1);
        return;

      case 'g':
        gPending = Date.now();
        return;

      case 'o':
        e.preventDefault();
        openActive();
        return;

      case 'Enter':
        /* If a real control has focus — including the active row,
           which we deliberately focus — the browser already activates
           it on Enter. Handling it here too would fire the request
           twice. */
        if (target && target !== document.body && target.matches &&
            target.matches('a[href], button, [role="button"], summary, [tabindex]')) return;
        e.preventDefault();
        openActive();
        return;

      case 's':
        e.preventDefault();
        clickReaderAction('star', 'star toggled');
        return;

      case 'm':
        e.preventDefault();
        clickReaderAction('toggle-read', 'read state toggled');
        return;

      case 'n':
        e.preventDefault();
        if (openDialog()) return;         /* guarded: the old code re-fired on every keypress */
        if (!window.htmx) return;
        /* Opened from a bare keypress, so focus is usually on <body>.
           Hand it back to the selected row instead, which is where the
           user's attention actually was. */
        modalReturnFocus = (document.activeElement && document.activeElement !== document.body)
          ? document.activeElement
          : activeRow();
        window.htmx.ajax('GET', '/feeds/new', { target: '#modal', swap: 'innerHTML' });
        return;

      case '/':
        e.preventDefault();
        var s = searchInput();
        if (s) { s.focus(); s.select(); }
        return;

      case '1':
        e.preventDefault();
        togglePane('sidebar');
        return;

      case '2':
        e.preventDefault();
        togglePane('list');
        return;
    }
  });

  /* ══════════════════════════════════════════════════════════════
     8 / READING PROGRESS
     Writes --read-progress (0→1) on .reader; reader.css scales its
     own hairline with transform: scaleX(var(--read-progress, 0)).

     Bound once, in the capture phase on document: scroll events do
     not bubble, but they DO capture, so this survives every #reader
     swap without a rebind and without caring which descendant ends
     up being the scrollport.
     ══════════════════════════════════════════════════════════════ */

  var progressQueued = false;
  var progressPort = null;

  function paintProgress() {
    progressQueued = false;
    var reader = $('#reader');
    if (!reader || !progressPort || !document.contains(progressPort)) return;
    var span = progressPort.scrollHeight - progressPort.clientHeight;
    var ratio = span > 0 ? progressPort.scrollTop / span : 0;
    reader.style.setProperty('--read-progress', String(Math.min(1, Math.max(0, ratio))));
  }

  document.addEventListener('scroll', function (e) {
    var t = e.target;
    if (!t || t.nodeType !== 1) return;
    var reader = $('#reader');
    if (!reader || !reader.contains(t)) return;
    progressPort = t;
    if (progressQueued) return;
    progressQueued = true;
    window.requestAnimationFrame(paintProgress);
  }, { capture: true, passive: true });

  function resetProgress() {
    progressPort = null;
    var reader = $('#reader');
    if (reader) reader.style.setProperty('--read-progress', '0');
  }

  /* ══════════════════════════════════════════════════════════════
     9 / FOLDER PERSISTENCE
     The sidebar emits <details data-folder="{id}">. State goes into
     ONE json key rather than one key per folder: the sidebar is
     re-rendered and swapped on every subscribe, and a per-folder key
     scheme leaves orphaned entries behind for every folder ever
     deleted, with no way to tell them from current ones.
     ══════════════════════════════════════════════════════════════ */

  function readFolders() {
    try {
      var v = JSON.parse(ls.get(KEY_FOLDERS) || '{}');
      return (v && typeof v === 'object' && !Array.isArray(v)) ? v : {};
    } catch (e) { return {}; }
  }

  function restoreFolders() {
    var state = readFolders();
    $$('details[data-folder]').forEach(function (d) {
      var id = d.getAttribute('data-folder');
      if (Object.prototype.hasOwnProperty.call(state, id)) d.open = !!state[id];
    });
  }

  /* `toggle` does not bubble either — capture, same reasoning as scroll. */
  document.addEventListener('toggle', function (e) {
    var d = e.target;
    if (!d || d.tagName !== 'DETAILS' || !d.hasAttribute('data-folder')) return;
    var state = readFolders();
    state[d.getAttribute('data-folder')] = !!d.open;
    ls.set(KEY_FOLDERS, JSON.stringify(state));
  }, true);

  /* ══════════════════════════════════════════════════════════════
     10 / ANNOUNCEMENTS + TOASTS
     #announcer is a polite live region; .toast-host is the visible
     channel. Both exist because htmx swaps are silent: a screen
     reader got no notification that a pane had been replaced, and a
     non-2xx response produced NOTHING on screen at all, because htmx
     does not swap an error response. A failed delete and a slow
     network were indistinguishable.
     ══════════════════════════════════════════════════════════════ */

  var announceTimer = null;

  function announce(msg) {
    var region = $('#announcer');
    if (!region || !msg) return;
    /* Setting the same string twice is a no-op for most screen
       readers, so clear first — "12 items" after another "12 items"
       still needs to be heard. */
    region.textContent = '';
    if (announceTimer) window.clearTimeout(announceTimer);
    announceTimer = window.setTimeout(function () {
      var r = $('#announcer');
      if (r) r.textContent = msg;
    }, 50);
  }

  function toastHost() {
    var host = $('.toast-host');
    if (host) return host;
    host = document.createElement('div');
    host.className = 'toast-host';
    /* Not a live region itself: each toast carries role="alert", so
       the container announcing would double every message. */
    document.body.appendChild(host);
    return host;
  }

  function toast(msg, kind) {
    var host = toastHost();
    if (!host) return;
    var el = document.createElement('div');
    el.className = 'toast toast-' + (kind || 'error');
    el.setAttribute('role', kind === 'error' || !kind ? 'alert' : 'status');

    /* base.css is explicit that state is never carried by colour
       alone, so the left rule is paired with a glyph. */
    var mark = document.createElement('span');
    mark.setAttribute('aria-hidden', 'true');
    mark.textContent = kind === 'ok' ? '✓' : kind === 'warn' ? '!' : '×';

    var body = document.createElement('span');
    body.className = 'toast-msg';
    body.textContent = msg;

    var close = document.createElement('button');
    close.type = 'button';
    close.className = 'toast-close btn-icon';
    close.setAttribute('aria-label', 'dismiss this message');
    close.textContent = '✕';
    close.addEventListener('click', function () { remove(); });

    el.appendChild(mark);
    el.appendChild(body);
    el.appendChild(close);
    host.appendChild(el);

    var timer = window.setTimeout(remove, 6000);
    function remove() {
      window.clearTimeout(timer);
      if (el.parentNode) el.parentNode.removeChild(el);
    }
  }

  /* ══════════════════════════════════════════════════════════════
     11 / PROGRESS BAR
     A counter, not a boolean. The search field fires a request every
     200ms while typing; with a naive show/hide the first response to
     land cleared the bar while three more requests were still in
     flight, so the bar flickered off mid-query.
     ══════════════════════════════════════════════════════════════ */

  var inFlight = 0;
  var barTimer = null;

  function barStart() {
    inFlight++;
    var bar = $('#progress-bar');
    if (!bar) return;
    if (barTimer) { window.clearTimeout(barTimer); barTimer = null; }
    bar.classList.remove('done');
    bar.classList.add('active');
  }

  function barEnd() {
    inFlight = Math.max(0, inFlight - 1);
    if (inFlight > 0) return;
    var bar = $('#progress-bar');
    if (!bar) return;
    bar.classList.remove('active');
    bar.classList.add('done');
    if (barTimer) window.clearTimeout(barTimer);
    /* Long enough for the width transition to finish, unless the user
       asked for no motion, in which case there is nothing to wait for. */
    barTimer = window.setTimeout(function () {
      var b = $('#progress-bar');
      if (b && inFlight === 0) b.classList.remove('done', 'active');
      barTimer = null;
    }, reduceMotion.matches ? 0 : 320);
  }

  /* ══════════════════════════════════════════════════════════════
     12 / HTMX LIFECYCLE
     Swap bookkeeping: focus, announcements, progress, errors.
     ══════════════════════════════════════════════════════════════ */

  /* Which pane was just replaced. Three fallbacks because htmx does
     not put the target in the same place for every swap style: an
     outerHTML swap leaves detail.target pointing at the element that
     was removed (it still carries the id), while the event itself is
     dispatched on whatever is left standing. */
  function detailTargetID(evt) {
    var d = evt && evt.detail;
    var t = d && (d.target || (d.requestConfig && d.requestConfig.target));
    if (t && t.id) return t.id;
    if (evt && evt.target && evt.target.id) return evt.target.id;
    return '';
  }

  function triggerElt(evt) {
    var d = evt && evt.detail;
    return (d && (d.elt || (d.requestConfig && d.requestConfig.elt))) || null;
  }

  /* The status footer polls itself every 900s. Announcing that would
     interrupt whatever the user is reading, twice an hour, forever. */
  function isBackgroundPoll(evt) {
    var elt = triggerElt(evt);
    if (!elt) return false;
    if (elt.id === 'status') return true;
    var trig = elt.getAttribute && elt.getAttribute('hx-trigger');
    return !!(trig && trig.indexOf('every ') !== -1);
  }

  document.addEventListener('htmx:beforeRequest', function (evt) {
    barStart();
    /* Record where focus was so a modal can hand it back on close.
       Anything already inside the modal is skipped — otherwise the
       "probe" button inside the dialog becomes the restore target,
       and it is removed with the dialog a moment later. */
    var a = document.activeElement;
    var host = modalHost();
    if (a && a !== document.body && (!host || !host.contains(a))) modalReturnFocus = a;
  });

  document.addEventListener('htmx:afterRequest', function (evt) {
    barEnd();
    /* Navigating from inside the overlay drawer should dismiss it —
       otherwise the sidebar stays parked over the list you just asked
       it to show. */
    var elt = triggerElt(evt);
    if (drawerOpen() && elt && elt.closest && elt.closest('#sidebar')) toggleDrawer(false);
  });

  document.addEventListener('htmx:afterSwap', function (evt) {
    var id = detailTargetID(evt);
    var elt = triggerElt(evt);
    var poll = isBackgroundPoll(evt);

    if (id === 'modal') { showModal(); return; }
    if (id === 'sidebar') { restoreFolders(); syncDensity(); syncTheme(); return; }
    if (id === 'status') return;

    /* The whole main pane changed: re-derive every piece of state
       that lived on the old DOM. */
    if (id === 'main') {
      syncDensity();
      syncTheme();
      restoreActive();
      showPane('list');
    }

    if (id === 'reader') {
      resetProgress();
      restoreActive();
    }

    if (!poll) {
      focusAfterSwap(id, elt, evt);
      announceSwap(id);
    }
  });

  /* ── focus after swap ───────────────────────────────────────────
     htmx replaces a pane wholesale, which detaches whatever the
     keyboard was on. The browser then drops focus to <body>, so the
     next Tab restarts at the top of the document — on EVERY
     navigation. Moving focus to the new pane's heading also gives a
     screen reader the title of what it just loaded. */
  function focusAfterSwap(id, elt, evt) {
    if (id !== 'main' && id !== 'reader') return;

    /* Only for navigation the user asked for. A POST is a state
       change (star, mark read) that re-renders the pane in place —
       stealing focus there would throw a reader out of the article
       they are in. */
    var verb = evt.detail && evt.detail.requestConfig && evt.detail.requestConfig.verb;
    if (verb && String(verb).toLowerCase() !== 'get') return;

    /* Never while typing in search: the results pane swaps on every
       keystroke and focus must stay in the field. */
    var q = searchInput();
    if (elt && (elt === q || (elt.closest && elt.closest('.tb-search')))) return;

    var pane = document.getElementById(id);
    if (!pane || !isVisible(pane)) return;

    /* Settings and Manage swap #main and then OOB-swap #reader to an
       empty, hidden <section>. That second swap fires its own
       afterSwap, and without this guard it would immediately steal
       the focus we just put on the settings heading — and hand it to
       an element with no accessible name that is not even on screen. */
    var head = $('.pane-title, .reader-title, .es-title, h1, h2', pane);
    if (!head && id === 'reader') return;
    var mark = head || pane;
    if (!mark.hasAttribute('tabindex')) mark.setAttribute('tabindex', '-1');
    try { mark.focus({ preventScroll: true }); } catch (e) { /* ignore */ }
  }

  /* ── announcements ──────────────────────────────────────────────
     Terse on purpose. A live region that reads a whole pane is worse
     than one that says nothing, because it cannot be interrupted. */
  function announceSwap(id) {
    if (id === 'main') {
      var main = document.getElementById('main');
      if (!main) return;
      var title = $('.pane-title', main);
      var n = $$('.list-body .item', main).length;
      var name = title ? (title.textContent || '').trim() : 'view';
      announce(n ? name + ', ' + n + (n === 1 ? ' item' : ' items') : name + ', empty');
      return;
    }
    if (id === 'reader') {
      var t = $('#reader .reader-title') || $('#reader .es-title');
      /* Same reason as above: an empty OOB reader reset is not an
         event worth interrupting a screen reader for. */
      if (t) announce((t.textContent || '').trim());
      return;
    }
    if (id && id.indexOf('item-') === 0) announce('item updated');
  }

  /* ── failures ───────────────────────────────────────────────────
     The single biggest "is it broken or is it slow?" problem in the
     app: htmx refuses to swap a non-2xx response and, by default,
     says nothing about it. */
  document.addEventListener('htmx:responseError', function (evt) {
    var xhr = evt.detail && evt.detail.xhr;
    var code = xhr ? xhr.status : 0;
    var msg = code === 404 ? 'not found — that item or feed is gone.'
            : code === 409 ? 'conflict — something else changed this first.'
            : code >= 500 ? 'the server failed on that request (' + code + '). nothing was changed.'
            : 'that request was refused (' + code + ').';
    toast(msg, 'error');
    announce(msg);
  });

  document.addEventListener('htmx:sendError', function () {
    var msg = 'could not reach the server. check that the process is still running.';
    toast(msg, 'error');
    announce(msg);
  });

  document.addEventListener('htmx:timeout', function () {
    var msg = 'that request timed out.';
    toast(msg, 'warn');
    announce(msg);
  });

  /* htmx swallows a swap error silently; surfacing it is the only
     way to tell a template bug from a server that never answered. */
  document.addEventListener('htmx:swapError', function () {
    toast('the response could not be displayed.', 'error');
  });

  /* ══════════════════════════════════════════════════════════════
     13 / BOOT
     ══════════════════════════════════════════════════════════════ */

  function init() {
    initTheme();
    initDensity();
    restoreFolders();
    restoreActive();

    /* If the viewport crosses 960px the single-pane attribute has to
       stop being meaningful in one direction and start again in the
       other — otherwise resizing a window while the reader is open
       leaves the list permanently hidden on a desktop. */
    var onBreak = function () {
      var a = app();
      if (!a) return;
      if (!mqOnePane.matches) a.setAttribute('data-pane', 'list');
      if (!mqDrawer.matches && drawerOpen()) toggleDrawer(false);
    };
    if (mqOnePane.addEventListener) mqOnePane.addEventListener('change', onBreak);
    else if (mqOnePane.addListener) mqOnePane.addListener(onBreak);
    if (mqDrawer.addEventListener) mqDrawer.addEventListener('change', onBreak);
    else if (mqDrawer.addListener) mqDrawer.addListener(onBreak);
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }

  /* ══════════════════════════════════════════════════════════════
     14 / GLOBALS
     Exactly six, and every one of them is called from an
     onclick/hx-on attribute in a template. Nothing else leaks.
     ══════════════════════════════════════════════════════════════ */

  window.toggleTheme  = toggleTheme;
  window.setTheme     = setTheme;
  window.setDensity   = setDensity;
  window.showPane     = showPane;
  window.togglePane   = togglePane;
  window.toggleDrawer = toggleDrawer;
})();
