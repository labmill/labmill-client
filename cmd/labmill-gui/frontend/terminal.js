// Labmill Terminal view (M62.2). Plain JS, no build step; xterm.js and its fit
// addon are vendored in vendor/xterm (no CDN). A full-page view (#terminal)
// with tabs: each tab is an xterm.js terminal wired to a shell on a cluster's
// login node through Go (PageAPI.OpenTerminal/TerminalInput/TerminalResize/
// CloseTerminal and the events Labmill:term-state, Labmill:term-out,
// Labmill:term-exit; the data is base64 of the raw bytes). It is the user's own
// shell: nothing typed or shown here goes to the chat, Claude or any log.
// Going back to the chat only hides the view; the shells keep running until
// their tab is closed. Everything from Go is written with textContent or to
// the terminal, never as HTML.
(function () {
  'use strict';

  var $ = function (id) { return document.getElementById(id); };
  function page() { return window.go && window.go.main && window.go.main.PageAPI; }
  function errText(e) { return (e && e.message) ? e.message : String(e); }
  function el(tag, cls, text) {
    var n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text !== undefined && text !== null) n.textContent = text;
    return n;
  }

  var hooks = { opened: 0, outputBytes: 0, exits: 0 };
  window.__labmillTerminalHooks = hooks;

  var state = { enabled: false, open: false, returnFocus: null, clusters: [], systemOK: false };
  var tabs = [];          // { n, id, cluster, label, term, fit, view, tab, labelEl, pending, ended, sel }
  var byId = {};          // terminal ID -> tab
  var early = {};         // events that arrived before OpenTerminal returned the ID
  var closedIds = {};
  var counter = 0, current = null;

  // ---- bytes <-> base64 -------------------------------------------------
  function encodeText(s) { return new TextEncoder().encode(s); }
  function encodeBinary(s) { // xterm's onBinary: one byte per char
    var b = new Uint8Array(s.length);
    for (var i = 0; i < s.length; i++) b[i] = s.charCodeAt(i) & 0xff;
    return b;
  }
  function toBase64(b) {
    var bin = '';
    for (var i = 0; i < b.length; i += 0x8000) bin += String.fromCharCode.apply(null, b.subarray(i, i + 0x8000));
    return btoa(bin);
  }
  function decode(b64) {
    var bin = atob(b64), out = new Uint8Array(bin.length);
    for (var i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
    return out;
  }

  // ---- show / hide ------------------------------------------------------
  function enable() {
    state.enabled = true;
    $('terminal-open').hidden = false;
    var p = page();
    if (p && p.SystemTerminalAvailable) {
      p.SystemTerminalAvailable().then(function (ok) { state.systemOK = !!ok; syncSystem(); }, function () {});
    }
  }

  function showError(msg) {
    $('terminal-error').textContent = msg || '';
    $('terminal-error').hidden = !msg;
  }

  function loadClusters() {
    var p = page();
    if (!p || !p.TerminalClusters) return Promise.resolve();
    return p.TerminalClusters().then(function (list) {
      state.clusters = list || [];
      var sel = $('terminal-cluster'), keep = sel.value;
      sel.textContent = '';
      state.clusters.forEach(function (c) {
        var o = el('option', '', c.name);
        o.value = c.name;
        sel.appendChild(o);
      });
      if (keep) sel.value = keep;
      updateNew();
      if (!state.clusters.length) showError('No cluster with a login host is set up yet. Add one in Settings.');
      syncSystem();
    }, function (e) { showError(errText(e)); });
  }

  // The New terminal button is off while a terminal is connecting (a sign-in
  // dialog may be open: one at a time) and when there is no cluster.
  var connecting = 0;
  function updateNew() {
    $('terminal-new').disabled = state.clusters.length === 0 || connecting > 0;
  }
  function setConnecting(t, on) {
    if (!!t.connecting === on) return;
    t.connecting = on;
    connecting += on ? 1 : -1;
    updateNew();
  }

  function syncSystem() {
    $('terminal-system').hidden = !(state.systemOK && state.clusters.length);
  }

  // showPanel shows the view; the first time it also loads the clusters.
  function showPanel() {
    if (!state.enabled) return Promise.resolve();
    if (!state.open) {
      state.returnFocus = document.activeElement;
      if (window.labmillSettings && window.labmillSettings.isOpen()) window.labmillSettings.close();
      $('main').hidden = true;
      $('terminal').hidden = false;
      state.open = true;
      $('terminal-open').setAttribute('aria-expanded', 'true');
    }
    showError('');
    return loadClusters().then(function () {
      if (current) fitSoon(current);
    });
  }

  // hidePanel goes back to the chat; the shells keep running.
  function hidePanel() {
    if (!state.open) return;
    state.open = false;
    $('terminal').hidden = true;
    $('main').hidden = false;
    $('terminal-open').setAttribute('aria-expanded', 'false');
    var f = $('input');
    if (f) f.focus();
  }

  // ---- tabs --------------------------------------------------------------
  function themeColors() {
    var cs = getComputedStyle(document.documentElement);
    var bg = cs.getPropertyValue('--code').trim() || '#12151a', fg = cs.getPropertyValue('--fg').trim() || '#e6e9ee';
    return { background: bg, foreground: fg, cursor: fg, selectionBackground: 'rgba(107,148,245,.45)' };
  }

  function fitSoon(t) {
    requestAnimationFrame(function () { fit(t); });
  }
  function fit(t) {
    if (!t || t.closed || t.view.clientWidth === 0 || t.view.clientHeight === 0) return;
    try { t.fit.fit(); } catch (e) { /* hidden or not laid out yet */ }
  }

  function newTab(cluster) {
    var t = { n: ++counter, id: null, cluster: cluster, ended: false, closed: false,
      inQueue: [], inSeq: 0, inFlight: false, rsSeq: 0, rsFlight: false, rsWant: null, ackN: 0, ackTimer: null };
    var same = tabs.filter(function (x) { return x.cluster === cluster; }).length;
    t.label = cluster + (same ? ' (' + (same + 1) + ')' : '');
    t.view = el('div', 'term-view');
    t.view.id = 'term-view-' + t.n;
    t.view.setAttribute('role', 'tabpanel');
    t.view.hidden = true;
    $('terminal-views').appendChild(t.view);
    t.term = new window.Terminal({
      cursorBlink: true, scrollback: 5000, fontSize: 14,
      fontFamily: 'ui-monospace, "Cascadia Mono", Consolas, "SF Mono", Menlo, monospace',
      theme: themeColors(), convertEol: false
    });
    t.fit = new window.FitAddon.FitAddon();
    t.term.loadAddon(t.fit);
    t.term.open(t.view);
    t.term.onData(function (s) { send(t, encodeText(s)); });
    t.term.onBinary(function (s) { send(t, encodeBinary(s)); });
    t.term.onResize(function (sz) { resizeTo(t, sz.cols, sz.rows); });
    // Ctrl+Shift+C / Ctrl+Shift+V copy and paste (the keys of Linux terminals).
    t.term.attachCustomKeyEventHandler(function (e) {
      if (e.type === 'keydown' && e.ctrlKey && e.shiftKey && !e.altKey && !e.metaKey) {
        if (e.code === 'KeyC') {
          var sel = t.term.getSelection();
          if (sel && navigator.clipboard) navigator.clipboard.writeText(sel).catch(function () {});
          return false;
        }
        if (e.code === 'KeyV') {
          if (navigator.clipboard && navigator.clipboard.readText) {
            navigator.clipboard.readText().then(function (x) { if (x) t.term.paste(x); }, function () {});
          }
          return false;
        }
      }
      return true;
    });
    // The tab button and its close button.
    t.tab = el('div', 'term-tab');
    t.tab.dataset.term = String(t.n);
    t.labelEl = el('button', 'term-label', t.label);
    t.labelEl.type = 'button';
    t.labelEl.id = 'term-tab-' + t.n;
    t.labelEl.setAttribute('role', 'tab');
    t.labelEl.setAttribute('aria-controls', t.view.id);
    t.labelEl.addEventListener('click', function () { select(t, true); });
    t.labelEl.addEventListener('keydown', function (e) { tabKey(e, t); });
    var x = el('button', 'term-x', '×');
    x.type = 'button';
    x.title = 'Close this terminal';
    x.setAttribute('aria-label', 'Close terminal ' + t.label);
    x.addEventListener('click', function () { closeTab(t); });
    t.tab.appendChild(t.labelEl);
    t.tab.appendChild(x);
    $('terminal-tabs').appendChild(t.tab);
    tabs.push(t);
    t.view.setAttribute('aria-labelledby', t.labelEl.id);
    select(t, false);
    return t;
  }

  function select(t, focusTerm) {
    current = t;
    tabs.forEach(function (x) {
      var on = x === t;
      x.view.hidden = !on;
      x.tab.classList.toggle('term-ended', x.ended);
      x.labelEl.setAttribute('aria-selected', on ? 'true' : 'false');
      x.labelEl.tabIndex = on ? 0 : -1;
    });
    fitSoon(t);
    if (focusTerm !== false) setTimeout(function () { t.term.focus(); }, 0);
  }

  function tabKey(e, t) {
    var i = tabs.indexOf(t), n = tabs.length, to = -1;
    if (e.key === 'ArrowRight') to = (i + 1) % n;
    else if (e.key === 'ArrowLeft') to = (i - 1 + n) % n;
    else if (e.key === 'Home') to = 0;
    else if (e.key === 'End') to = n - 1;
    if (to < 0) return;
    e.preventDefault();
    select(tabs[to], false);
    tabs[to].labelEl.focus();
  }

  function closeTab(t) {
    if (t.closed) return;
    t.closed = true;
    var p = page();
    setConnecting(t, false);
    clearTimeout(t.ackTimer);
    if (t.id) {
      closedIds[t.id] = true;
      delete byId[t.id];
      if (p && p.CloseTerminal) p.CloseTerminal(t.id).catch(function () {});
    }
    var i = tabs.indexOf(t);
    tabs.splice(i, 1);
    try { t.term.dispose(); } catch (e) { /* already gone */ }
    t.view.remove();
    t.tab.remove();
    if (current === t) {
      current = null;
      if (tabs.length) {
        var next = tabs[Math.min(i, tabs.length - 1)];
        select(next, true);
      } else {
        $('terminal-new').focus();
      }
    }
  }

  // Bound calls run in their own goroutines on the Go side, so they may
  // arrive in any order: input goes one call at a time (what is typed while a
  // call is in flight is joined into the next one) and carries a sequence
  // number; resizes go one at a time too, the newest size wins.
  var MAX_INPUT_CALL = 256 * 1024;
  function send(t, bytes) {
    if (t.ended || t.closed || !bytes.length) return;
    t.inQueue.push(bytes);
    flushInput(t);
  }

  function flushInput(t) {
    var p = page();
    if (t.inFlight || !t.id || !t.inQueue.length || !p || t.closed || t.ended) return;
    var total = 0, take = 0;
    while (take < t.inQueue.length && (take === 0 || total + t.inQueue[take].length <= MAX_INPUT_CALL)) total += t.inQueue[take++].length;
    var buf = new Uint8Array(total), at = 0;
    t.inQueue.splice(0, take).forEach(function (b) { buf.set(b, at); at += b.length; });
    t.inFlight = true;
    p.TerminalInput(t.id, t.inSeq++, toBase64(buf)).catch(function (e) {
      if (/too large/.test(errText(e))) {
        // Drop the rest of the paste too, so it is cut off rather than sent with a hole.
        t.inQueue.length = 0;
        showError('That paste was too large for the terminal.');
      }
    }).then(function () {
      t.inFlight = false;
      flushInput(t);
    });
  }

  function resizeTo(t, cols, rows) {
    t.rsWant = [cols, rows];
    flushResize(t);
  }

  function flushResize(t) {
    var p = page();
    if (t.rsFlight || !t.id || !t.rsWant || !p || t.ended || t.closed) return;
    var w = t.rsWant;
    t.rsWant = null;
    t.rsFlight = true;
    p.TerminalResize(t.id, ++t.rsSeq, w[0], w[1]).catch(function () {}).then(function () {
      t.rsFlight = false;
      flushResize(t);
    });
  }

  // Output flow control: tell Go how many bytes xterm.js has written, so it
  // can pause the shell's output while the page is behind.
  function ackLater(t, n) {
    t.ackN += n;
    if (t.ackTimer || t.closed) return;
    t.ackTimer = setTimeout(function () {
      t.ackTimer = null;
      var m = t.ackN, p = page();
      t.ackN = 0;
      if (p && t.id && !t.closed && m) p.TerminalAck(t.id, m).catch(function () {});
    }, 10);
  }

  // ---- opening ------------------------------------------------------------
  // start opens a terminal tab on cluster; dir is "" for the home folder, or
  // jobId names the job whose folder to start in (Go reads it from the store).
  function start(cluster, dir, jobId, label) {
    if (!page() || !window.Terminal) { showError('The terminal isn\'t available.'); return Promise.resolve(); }
    return showPanel().then(function () {
      showError('');
      if (connecting > 0) { showError('Wait for the terminal that is connecting (a sign-in may be waiting for you).'); return; }
      var t = newTab(cluster);
      setConnecting(t, true);
      if (label) { t.label += label; t.labelEl.textContent = t.label; }
      t.term.write('Connecting to ' + cluster + '…\r\n');
      fit(t);
      var p = page(), cols = t.term.cols, rows = t.term.rows;
      var call = jobId ? p.OpenJobTerminal(jobId, cols, rows) : p.OpenTerminal(cluster, dir || '', cols, rows);
      return call.then(function (id) {
        if (t.closed) {
          closedIds[id] = true;
          delete early[id];
          p.CloseTerminal(id).catch(function () {});
          return;
        }
        t.id = id;
        byId[id] = t;
        hooks.opened++;
        (early[id] || []).forEach(handle);
        delete early[id];
        flushInput(t); // what was typed while connecting
        // The size may have changed while connecting.
        if (t.term.cols !== cols || t.term.rows !== rows) resizeTo(t, t.term.cols, t.term.rows);
      }, function (e) {
        setConnecting(t, false);
        t.ended = true;
        t.term.write('\r\n\x1b[31m' + errText(e).replace(/[\x00-\x08\x0b-\x1f\x7f]/g, ' ') + '\x1b[0m\r\n');
        t.tab.classList.add('term-ended');
      });
    });
  }

  function handle(ev) {
    var t = byId[ev.id];
    if (!t) {
      if (closedIds[ev.id]) return;
      (early[ev.id] = early[ev.id] || []).push(ev);
      return;
    }
    if (ev.kind === 'out') {
      var bytes = decode(ev.data);
      hooks.outputBytes += bytes.length;
      t.term.write(bytes, function () { ackLater(t, bytes.length); });
    } else if (ev.kind === 'state') {
      t.opened = true;
      setConnecting(t, false);
    } else if (ev.kind === 'exit') {
      hooks.exits++;
      setConnecting(t, false);
      t.ended = true;
      var msg = ev.message ? ev.message : ('Session ended (exit status ' + ev.code + ').');
      t.term.write('\r\n\x1b[2m[' + msg.replace(/[\x00-\x1f\x7f]/g, ' ') + ' Close this tab with ×.]\x1b[0m\r\n');
      t.tab.classList.add('term-ended');
      t.labelEl.textContent = t.label + ' (ended)';
    }
  }

  function wireEvents() {
    var on = window.runtime && window.runtime.EventsOn;
    if (!on) return false;
    on('Labmill:term-out', function (e) { e.kind = 'out'; handle(e); });
    on('Labmill:term-state', function (e) { e.kind = 'state'; handle(e); });
    on('Labmill:term-exit', function (e) { e.kind = 'exit'; handle(e); });
    return true;
  }
  (function waitRuntime() { if (!wireEvents()) setTimeout(waitRuntime, 50); })();

  // ---- buttons ------------------------------------------------------------
  $('terminal-open').addEventListener('click', function () {
    if (state.open) { hidePanel(); return; }
    showPanel().then(function () {
      // One cluster and nothing open yet: open it at once.
      if (!tabs.length && state.clusters.length === 1) start(state.clusters[0].name, '', 0);
      else if (current) current.term.focus();
      else $('terminal-cluster').focus();
    });
  });
  $('terminal-back').addEventListener('click', hidePanel);
  $('terminal-new').addEventListener('click', function () {
    var c = $('terminal-cluster').value;
    if (c) start(c, '', 0);
  });
  $('terminal-system').addEventListener('click', function () {
    var c = $('terminal-cluster').value, p = page();
    if (!c || !p) return;
    showError('');
    p.OpenSystemTerminal(c).catch(function (e) { showError(errText(e)); });
  });
  // The Settings gear shows its own view: leave this one (shells keep running).
  $('settings-open').addEventListener('click', function () {
    if (state.open) { state.open = false; $('terminal').hidden = true; $('main').hidden = false; $('terminal-open').setAttribute('aria-expanded', 'false'); }
  }, true);
  window.addEventListener('resize', function () { if (current && state.open) fitSoon(current); });
  if (window.ResizeObserver) new ResizeObserver(function () { if (current && state.open) fitSoon(current); }).observe($('terminal-views'));

  window.labmillTerminal = {
    enable: enable,
    isOpen: function () { return state.open; },
    // openOn opens a terminal on cluster in the home folder (Settings > Clusters).
    openOn: function (cluster) { return start(cluster, '', 0); },
    // openJob opens one in the folder of the job (the jobs panel).
    openJob: function (id, cluster) { return start(cluster, '', id, ' #' + id); },
    count: function () { return tabs.length; },
    // current is the selected tab's xterm.js Terminal (the self-test types into it).
    current: function () { return current ? current.term : null; }
  };
})();
