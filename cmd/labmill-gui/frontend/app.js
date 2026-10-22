// Labmill desktop frontend. Plain JS, no build step. Everything that comes
// from Go is shown with textContent (never innerHTML); assistant Markdown is
// rendered by md.js with DOM nodes only.
(function () {
  'use strict';

  var GRACE_MS = 500;       // input grace after a dialog opens
  var JOBS_EVERY_MS = 5000;
  var RENDER_MS = 50;       // streamed Markdown is re-rendered at most this often
  var ELAPSED_MS = 5000;    // the "Working…" row shows the elapsed seconds after this
  var STILL_WAITING_MS = 10000; // the startup splash says what it waits for after this

  var $ = function (id) { return document.getElementById(id); };
  var appEl = $('app'), logEl = $('log'), inputEl = $('input');
  var lastStatus = null; // the latest Labmill:status text, kept even before ready (it can arrive before or after Labmill:ready)
  var busy = false, ready = false, chatUp = false, settled = false, assistantEl = null, notifyText = '';
  var pendingTool = null;   // the tool item waiting for its result
  // Counters the self-test reads (exposed on window only in self-test mode).
  var testHooks = { replies: 0, done: 0, renders: 0, notices: 0, xferShown: 0, xferEnds: 0, folderRenders: 0, workingShown: 0,
    keyCopies: 0,
    elapsedAfter: 0, // the self-test shortens ELAPSED_MS with it
    reply: function (raw) { appendAssistant(raw); flushAssistant(); },
    isReady: function () { return ready; },
    drop: function (x, y, paths) { return onFolderDrop(x, y, paths); } };

  // The startup splash as this script found it: it is in the initial HTML,
  // visible and animated before any script ran (the self-test checks this).
  (function () {
    var sp = $('starting'), hg = sp && sp.querySelector('.hourglass');
    testHooks.splashAtLoad = !!hg && !sp.hidden && sp.getAttribute('data-splash') === '1' &&
      $('starting-text').textContent === 'Starting labmill…';
    testHooks.splashAnimation = hg ? getComputedStyle(hg).animationName : '';
  })();

  function goApp() { return window.go && window.go.main && window.go.main.PageAPI; }
  // mark reports a startup step to Go's timing (M46.1); it never matters if it fails.
  function mark(step) { try { var a = goApp(); if (a && a.StartupMark) a.StartupMark(step); } catch (e) {} }
  function goSetup() { return window.go && window.go.main && window.go.main.Setup; }
  function errText(e) { return (e && e.message) ? e.message : String(e); }

  // ---- chat ----------------------------------------------------------
  function scrollDown() { logEl.scrollTop = logEl.scrollHeight; }
  function nearBottom() { return logEl.scrollHeight - logEl.scrollTop - logEl.clientHeight < 40; }

  // ---- assistant text: Markdown (md.js), re-rendered while it streams ----
  // The raw text accumulates on the element; a timer re-renders it at most
  // every RENDER_MS, and flushAssistant renders at once (before a tool note,
  // at the end of the turn), so the final text is always rendered.
  function renderAssistant(m) {
    if (!m) return;
    if (m.timer) { clearTimeout(m.timer); m.timer = null; }
    var stick = nearBottom();
    try {
      window.labmillMarkdown.render(m.el, m.raw);
    } catch (e) {
      m.el.textContent = m.raw; // a reply the renderer can't handle shows as plain text
    }
    testHooks.renders++;
    if (stick) scrollDown();
  }
  function flushAssistant() {
    var m = assistantEl;
    assistantEl = null; // cleared first: a failing render can never leave a stale reply behind
    renderAssistant(m);
  }
  function appendAssistant(t) {
    if (!assistantEl) {
      var d = document.createElement('div');
      d.className = 'msg assistant md';
      logEl.appendChild(d);
      assistantEl = { el: d, raw: '', timer: null };
      scrollDown();
    }
    var m = assistantEl;
    m.raw += t;
    if (!m.timer) m.timer = setTimeout(function () { m.timer = null; renderAssistant(m); }, RENDER_MS);
  }

  // ---- tool notes: one collapsed line per tool call -------------------
  function fmtSize(t) {
    var lines = t.replace(/\n+$/, '').split('\n').length;
    if (lines >= 10) return lines + ' lines';
    var n = new TextEncoder().encode(t).length;
    if (n < 1024) return n + ' B';
    if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
    return (n / 1024 / 1024).toFixed(1) + ' MB';
  }
  function firstLine(t) {
    var l = (t || '').split('\n')[0].trim();
    return l.length > 200 ? l.slice(0, 199) + '…' : l;
  }
  // toolItem appends a <details> line for a tool; its body (the full text,
  // monospace) is filled in by finishTool.
  function toolItem(tool) {
    flushAssistant();
    var d = document.createElement('details');
    d.className = 'tool pending';
    var s = document.createElement('summary');
    s.textContent = tool + ' …';
    d.appendChild(s);
    var stick = nearBottom();
    logEl.appendChild(d);
    if (stick) scrollDown();
    return { el: d, sum: s, tool: tool, declined: false };
  }
  function finishTool(item, label, body, isError) {
    item.el.classList.remove('pending');
    item.sum.textContent = label;
    if (isError) item.el.classList.add('err');
    if (body) {
      var pre = document.createElement('pre');
      pre.textContent = body;
      item.el.appendChild(pre);
    } else {
      item.el.classList.add('empty');
    }
  }

  function addMsg(cls, text) {
    var d = document.createElement('div');
    d.className = 'msg ' + cls;
    d.textContent = text;
    logEl.appendChild(d);
    scrollDown();
    return d;
  }

  // addSystem shows a background job notice. While a reply streams, it goes
  // above that reply, so the reply stays one piece.
  function addSystem(text) {
    var d = document.createElement('div');
    d.className = 'msg system';
    d.setAttribute('role', 'status');
    d.textContent = text;
    var stick = nearBottom();
    if (assistantEl && assistantEl.el.parentNode === logEl) logEl.insertBefore(d, assistantEl.el);
    else logEl.appendChild(d);
    if (stick) scrollDown();
    return d;
  }

  // ---- to-do list (M28) -----------------------------------------------
  // update_todos sends the whole list (Labmill:todos). It is kept per page
  // session, rendered in place with textContent only, and hidden once every
  // item is done and a following turn has ended too.
  var todoGlyph = { pending: '\u2610', in_progress: '\u25D0', completed: '\u2611' };
  var todoLabel = { pending: 'pending', in_progress: 'in progress', completed: 'done' };
  var todoState = { items: [], stale: 0, updated: false };
  function todoAllDone() {
    return todoState.items.length > 0 && todoState.items.every(function (t) { return t.status === 'completed'; });
  }
  function renderTodos() {
    var panel = $('todos'), ul = $('todos-list'), items = todoState.items;
    panel.hidden = items.length === 0;
    while (ul.children.length > items.length) ul.removeChild(ul.lastChild);
    items.forEach(function (t, i) {
      var li = ul.children[i];
      if (!li) {
        li = document.createElement('li');
        var g = document.createElement('span'); g.className = 'todo-glyph'; g.setAttribute('aria-hidden', 'true');
        var x = document.createElement('span'); x.className = 'todo-text';
        var sr = document.createElement('span'); sr.className = 'sr-only';
        li.appendChild(g); li.appendChild(x); li.appendChild(sr);
        ul.appendChild(li);
      }
      var st = todoGlyph[t.status] ? t.status : 'pending';
      li.setAttribute('data-status', st);
      li.children[0].textContent = todoGlyph[st];
      li.children[1].textContent = (st === 'in_progress' && t.activeForm) ? t.activeForm : t.content;
      li.children[2].textContent = ' (' + todoLabel[st] + ')';
    });
    var done = items.filter(function (t) { return t.status === 'completed'; }).length;
    $('todos-title').textContent = 'To-do ' + done + '/' + items.length;
    // Polite live region: the count and the item in progress.
    var cur = items.filter(function (t) { return t.status === 'in_progress'; })[0];
    $('todos-live').textContent = items.length === 0 ? '' :
      'To-do ' + done + ' of ' + items.length + ' done' + (cur ? '. Now: ' + (cur.activeForm || cur.content) : '');
  }
  function setTodos(items, fromClaude) {
    todoState.items = items || [];
    todoState.stale = 0;
    todoState.updated = !!fromClaude;
    renderTodos();
  }
  // A turn ended. The list is hidden when the turn was canceled or failed (a
  // plan that stopped must not look like work in progress), when it is all
  // done and a later turn ended (it stays through the turn that finished
  // it), and after two turns without an update_todos call.
  function todosTurnDone(failed) {
    if (todoState.items.length === 0) return;
    if (failed) { setTodos([]); return; }
    if (todoState.updated) { todoState.updated = false; todoState.stale = 0; return; }
    todoState.stale++;
    if (todoState.stale >= 2 || (todoAllDone() && todoState.stale >= 1)) setTodos([]);
  }
  $('todos-toggle').addEventListener('click', function () {
    var open = $('todos-toggle').getAttribute('aria-expanded') !== 'true';
    $('todos-toggle').setAttribute('aria-expanded', open ? 'true' : 'false');
    $('todos-list').hidden = !open;
    $('todos-toggle').querySelector('.todos-caret').textContent = open ? '\u25BE' : '\u25B8';
  });

  // The message waiting for the running reply (Go keeps it; Labmill:queued
  // tells the page). "" hides the line.
  function showQueued(text) {
    $('queued-text').textContent = text || '';
    $('queued-text').title = text || '';
    $('queued').hidden = !text;
  }

  // showTransfer shows a transfer notice: "start" adds the rclone-lib line to
  // the chat, "progress" fills the progress row (bar and rclone-style text) and
  // the tool line, "end" removes the row.
  function showTransfer(e) {
    var row = $('xfer');
    if (e.kind === 'end') {
      row.hidden = true;
      $('xfer-text').textContent = '';
      $('xfer-text').title = '';
      testHooks.xferEnds++;
      return;
    }
    if (e.kind === 'start') {
      var d = document.createElement('div');
      d.className = 'msg xfercmd';
      d.textContent = e.text;
      var stick = nearBottom();
      if (assistantEl && assistantEl.el.parentNode === logEl) logEl.insertBefore(d, assistantEl.el);
      else logEl.appendChild(d);
      if (stick) scrollDown();
      return;
    }
    var pct = e.totalBytes > 0 ? Math.min(100, Math.floor(e.bytes * 100 / e.totalBytes)) : 0;
    $('xfer-bar').value = pct;
    $('xfer-text').textContent = e.text;
    $('xfer-text').title = e.text;
    row.hidden = false;
    testHooks.xferShown++;
  }

  // showProgress shows the latest status line from Go (Labmill:notify, e.g.
  // "approve the sign-in request"); the current step is in the "Working…" row.
  function showProgress() {
    $('progress').textContent = notifyText || '';
  }

  // ---- the "Working…" row (M26) ----------------------------------------
  // Shown at once after Send (before Go answers) and until the reply text
  // starts, with what the turn is doing now (Labmill:step: "Waiting for
  // Claude…", "Connecting to bigcluster…"; "Running <tool>…" for a tool call).
  // A later step or tool call of the same turn shows it again. After
  // ELAPSED_MS it also shows the seconds since it appeared, so a long step
  // never looks frozen. Gone when the text starts or the turn ends.
  var working = { on: false, since: 0, timer: null, last: '' };
  function tickWorking() {
    if (!working.on) return;
    var ms = performance.now() - working.since;
    $('working-time').textContent = ms >= (testHooks.elapsedAfter || ELAPSED_MS) ? Math.floor(ms / 1000) + ' s' : '';
  }
  function showWorking(text) {
    working.last = text || '';
    // While a dialog waits for the user, the row says so (the elapsed
    // counter keeps running) instead of naming the step that is blocked.
    var t = dlg.current ? 'Waiting for your answer…' : (text || 'Working…');
    if ($('working-text').textContent !== t) $('working-text').textContent = t;
    if (!working.on) {
      working.on = true;
      working.since = performance.now();
      $('working').hidden = false;
      testHooks.workingShown++;
      if (!working.timer) working.timer = setInterval(tickWorking, 250);
    }
    tickWorking();
  }
  function hideWorking() {
    working.on = false;
    $('working').hidden = true;
    $('working-time').textContent = '';
    if (working.timer) { clearInterval(working.timer); working.timer = null; }
  }

  // setBusy shows whether a reply is running. The input stays enabled: a
  // message sent while busy is queued by Go and sent when the reply ends.
  function setBusy(b) {
    busy = b;
    $('send').textContent = b ? 'Queue' : 'Send';
    $('send').title = b ? 'Sent when the current reply ends' : '';
    $('cancel').hidden = !b;
    $('spinner').hidden = !b;
    if (!b) { $('xfer').hidden = true; notifyText = ''; hideWorking(); flushAssistant(); pendingTool = null; showProgress(); if (!dlg.current) inputEl.focus(); }
  }

  // sendText hands text to Go, which starts a turn (Labmill:turn shows the
  // message) or queues it while one runs (Labmill:queued). onFail gets the
  // error after it is shown.
  function sendText(text, onFail) {
    var app = goApp();
    if (!app) { addMsg('err', 'Labmill is not connected to its backend.'); return; }
    app.Send(text).catch(function (e) { addMsg('err', errText(e)); if (onFail) onFail(); });
  }

  function send() {
    var text = inputEl.value;
    // The chat takes input as soon as the bindings are up (M46.2); Go holds
    // the message until the runtime exists.
    if (!chatUp || !text.trim()) return;
    inputEl.value = '';
    // Feedback before Go answers: a new turn shows "Working…" at once (a
    // message sent while busy is only queued).
    if (!busy) showWorking();
    sendText(text, function () { if (!inputEl.value) inputEl.value = text; if (!busy) hideWorking(); });
  }

  $('chat-form').addEventListener('submit', function (e) { e.preventDefault(); send(); });
  inputEl.addEventListener('keydown', function (e) {
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) { e.preventDefault(); send(); }
  });
  $('cancel').addEventListener('click', function () {
    var app = goApp();
    if (app) app.Cancel();
  });
  $('queued-remove').addEventListener('click', function () {
    var app = goApp();
    if (app) app.Unqueue();
    inputEl.focus();
  });

  // ---- jobs ----------------------------------------------------------
  // fmtTime is short for the table (time today, else date and time); the
  // full form goes in the tooltip.
  function fmtTime(s) {
    var d = new Date(s);
    if (isNaN(d)) return ['', ''];
    var today = new Date().toDateString() === d.toDateString();
    var short = today ? d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
      : d.toLocaleDateString([], { month: 'short', day: 'numeric' }) + ' ' + d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
    return [short, d.toLocaleString()];
  }

  // Job buttons: each sends a plain chat message for the user through
  // PageAPI.JobAction (queued while a reply runs), so everything goes
  // through the assistant and its approvals (Cancel still asks).
  var ACTIONS = {
    status: ['Status', 'Show the status of job #'],
    logs: ['Logs', 'Show the last lines of the log of job #'],
    fetch: ['Fetch', 'Fetch the results of job #'],
    cancel: ['Cancel', 'Cancel job #'],
    open: ['Open folder', 'Open the downloaded results of job #']
  };
  var lastJobs = null;

  function jobAction(id, act) {
    var app = goApp();
    if (!app) return;
    var p = act === 'open' ? app.OpenJobFolder(id) : app.JobAction(id, act);
    p.catch(function (e) { addMsg('err', errText(e)); });
  }

  function actionsRow(j) {
    var tr = document.createElement('tr');
    tr.className = 'job-actions';
    var td = document.createElement('td');
    td.colSpan = 4;
    (j.actions || []).forEach(function (act) {
      var a = ACTIONS[act];
      if (!a) return;
      var b = document.createElement('button');
      b.type = 'button';
      b.textContent = a[0];
      b.title = a[1] + j.id;
      b.setAttribute('aria-label', a[0] + ' of job #' + j.id + ' ' + j.name);
      b.dataset.job = String(j.id);
      b.dataset.act = act;
      b.addEventListener('click', function () { jobAction(j.id, act); });
      td.appendChild(b);
    });
    // "Open terminal here" (M62.2): a shell on the login node in the job's
    // folder; Go takes the folder from the job store.
    if (j.terminal && window.labmillTerminal) {
      var tb = document.createElement('button');
      tb.type = 'button';
      tb.textContent = 'Terminal here';
      tb.title = 'Open a terminal on the cluster in the folder of job #' + j.id;
      tb.setAttribute('aria-label', 'Open terminal here for job #' + j.id + ' ' + j.name);
      tb.dataset.job = String(j.id);
      tb.dataset.act = 'terminal';
      tb.addEventListener('click', function () { window.labmillTerminal.openJob(j.id, j.cluster); });
      td.appendChild(tb);
    }
    tr.appendChild(td);
    return tr;
  }

  function refreshJobs() {
    var app = goApp();
    if (!ready || !app) return;
    app.Jobs().then(function (jobs) {
      jobs = jobs || [];
      $('jobs-error').hidden = true;
      $('jobs-empty').hidden = jobs.length > 0;
      $('jobs-table').hidden = jobs.length === 0;
      // Rebuild only when something changed, and keep the keyboard focus on
      // the same job button across a rebuild.
      var key = JSON.stringify(jobs);
      if (key === lastJobs) return;
      lastJobs = key;
      var fa = document.activeElement, keep = (fa && fa.dataset && fa.dataset.job) ? [fa.dataset.job, fa.dataset.act] : null;
      var body = $('jobs-body');
      body.textContent = '';
      jobs.forEach(function (j) {
        var tr = document.createElement('tr');
        var t = fmtTime(j.updated);
        // Cells never wrap: long values end in an ellipsis and show in full
        // in the tooltip.
        var name = document.createElement('td');
        var id = document.createElement('span');
        id.className = 'mono';
        id.textContent = '#' + j.id + ' ';
        name.appendChild(id);
        name.appendChild(document.createTextNode(j.name));
        name.title = '#' + j.id + ' ' + j.name + (j.message ? '\n' + j.message : '');
        tr.appendChild(name);
        [[j.cluster, j.cluster], [j.state, j.state + (j.message ? '\n' + j.message : '')], [t[0], t[1]]].forEach(function (v) {
          var td = document.createElement('td');
          td.textContent = v[0];
          td.title = v[1];
          tr.appendChild(td);
        });
        body.appendChild(tr);
        if ((j.actions && j.actions.length) || j.terminal) {
          tr.className = 'has-actions';
          body.appendChild(actionsRow(j));
        }
      });
      if (keep) {
        var bs = body.querySelectorAll('button');
        for (var i = 0; i < bs.length; i++) {
          if (bs[i].dataset.job === keep[0] && bs[i].dataset.act === keep[1]) { bs[i].focus(); break; }
        }
      }
    }).catch(function (e) {
      $('jobs-error').textContent = errText(e);
      $('jobs-error').hidden = false;
    });
  }
  setInterval(refreshJobs, JOBS_EVERY_MS);

  // ---- folders (data and code) -----------------------------------------
  // The + button at the upper left of the chat input box opens a menu: "Add
  // data folder…" (first: many users only have data and want the code
  // written for them), "Add code folder…", up to 5 recent folders and, when
  // set, "Remove …". The "Add" items open the native folder dialog; Go checks
  // and records the folder (only its path) and returns the new state, which
  // shows as chips above the text ("Data: …", "Code: …", data first). A
  // folder dropped onto the input box offers "Add as data folder / Add as
  // code folder"; drops anywhere else are ignored.
  var FOLDER_KINDS = ['data', 'code'];  // display order: data first
  var KIND_LABEL = { data: 'Data', code: 'Code' };
  var MAX_RECENT = 5;
  var lastFolders = null, droppedPath = '';
  var addBtn = $('folder-add'), menuEl = $('folder-menu');

  function showFolderError(msg) {
    $('folder-error').textContent = msg || '';
    $('folder-error').hidden = !msg;
  }

  function recentOf(st, k) { return (st && st[k === 'code' ? 'recentCode' : 'recentData']) || []; }

  // chip builds one chosen-folder chip: "Data: ~/…/x" (full path in the
  // tooltip) and an × that clears it.
  function chip(k, f) {
    var c = document.createElement('span');
    c.className = 'chip';
    c.dataset.kind = k;
    var t = document.createElement('span');
    t.className = 'chip-text';
    t.title = KIND_LABEL[k] + ' folder: ' + f.path;
    var kl = document.createElement('span');
    kl.className = 'chip-kind';
    kl.textContent = KIND_LABEL[k] + ':';
    var pl = document.createElement('span');
    pl.className = 'chip-path';
    pl.textContent = f.short;
    t.appendChild(kl);
    t.appendChild(document.createTextNode(' '));
    t.appendChild(pl);
    var x = document.createElement('button');
    x.type = 'button';
    x.className = 'chip-x';
    x.textContent = '×';
    x.setAttribute('aria-label', 'Remove the ' + k + ' folder');
    x.title = 'Remove the ' + k + ' folder';
    x.addEventListener('click', function () { clearFolder(k); });
    c.appendChild(t);
    c.appendChild(x);
    return c;
  }

  function renderFolders(st) {
    if (!st) return;
    lastFolders = st;
    var box = $('folder-chips');
    box.textContent = '';
    FOLDER_KINDS.forEach(function (k) {
      var f = st[k] || {};
      if (f.path) box.appendChild(chip(k, f));
    });
    box.hidden = !box.firstChild;
    testHooks.folderRenders++;
  }

  // folderCall runs a PageAPI folder call and shows its state; an error
  // (a refused folder) is shown above the input box and the state stays as
  // Go reports it.
  function folderCall(p) {
    return p.then(function (st) { showFolderError(''); renderFolders(st); },
      function (e) { showFolderError(errText(e)); var app = goApp(); if (app) app.Folders().then(renderFolders, function () {}); });
  }

  function refreshFolders() {
    var app = goApp();
    if (!app) return;
    app.Folders().then(renderFolders, function () {});
  }
  // The Settings page (settings.js) changes folders too: it refreshes the chips.
  // ---- "Approve everything automatically" (M43.2) ------------------------
  // The checkbox beside Send, the banner above the chat and the Settings
  // page all show the one saved setting (Go reads it at every approval).
  function settingsApi() { return window.go && window.go.main && window.go.main.SettingsAPI; }
  function showApproveAll(on) {
    $('autoapprove').checked = !!on;
    $('autoapprove-banner').hidden = !on;
  }
  function setApproveAll(want) {
    var a = settingsApi();
    if (!a) return Promise.resolve();
    return a.SetApproveAll(want).then(function (r) {
      if (r.ok) { showApproveAll(want); return; }
      showApproveAll(!want);
      addMsg('err', r.error || 'That didn\'t work.');
    }, function (e) { showApproveAll(!want); addMsg('err', errText(e)); });
  }
  function refreshApproveAll() {
    var a = settingsApi();
    if (a && a.ApproveAll) a.ApproveAll().then(showApproveAll, function () {});
  }
  // ---- "Download results automatically" (M60.1) -------------------------
  // The checkbox beside "Approve everything automatically" and the Settings
  // page show the one saved setting (the job monitor reads it when a job ends).
  function showAutoDownload(on) { $('autodownload').checked = !!on; }
  function setAutoDownload(want) {
    var a = settingsApi();
    if (!a) return Promise.resolve();
    return a.SetAutoDownload(want).then(function (r) {
      if (r.ok) { showAutoDownload(want); return; }
      showAutoDownload(!want);
      addMsg('err', r.error || 'That didn\'t work.');
    }, function (e) { showAutoDownload(!want); addMsg('err', errText(e)); });
  }
  function refreshAutoDownload() {
    var a = settingsApi();
    if (a && a.AutoDownload) a.AutoDownload().then(showAutoDownload, function () {});
  }
  $('autodownload').addEventListener('change', function () { setAutoDownload($('autodownload').checked); });
  $('autoapprove').addEventListener('change', function () { setApproveAll($('autoapprove').checked); });
  $('autoapprove-off').addEventListener('click', function () { setApproveAll(false); });
  // The terminal or another window may change the saved setting: look again
  // when this window comes back.
  window.addEventListener('focus', function () { if (ready) { refreshApproveAll(); refreshAutoDownload(); } });
  document.addEventListener('visibilitychange', function () { if (!document.hidden && ready) { refreshApproveAll(); refreshAutoDownload(); } });

  // The Settings page (settings.js) changes folders too: it refreshes the chips.
  window.labmillApp = { refreshFolders: refreshFolders, showApproveAll: showApproveAll, refreshApproveAll: refreshApproveAll, showAutoDownload: showAutoDownload };

  function pickFolder(k) {
    var app = goApp();
    if (!app) return;
    folderCall(app.PickFolder(k)).then(function () { if (!dlg.current) inputEl.focus(); });
  }

  function clearFolder(k) {
    var app = goApp();
    if (!app) return;
    folderCall(app.SetFolder(k, '')).then(function () { if (!dlg.current) addBtn.focus(); });
  }

  // recentItems are up to MAX_RECENT recent folders that are not chosen now,
  // taken from the data and code lists in turn (data first).
  function recentItems(st) {
    var lists = FOLDER_KINDS.map(function (k) {
      var cur = (st[k] || {}).path;
      return recentOf(st, k).filter(function (f) { return f.path !== cur; }).map(function (f) { return { kind: k, f: f }; });
    });
    var out = [];
    for (var i = 0; out.length < MAX_RECENT && (i < lists[0].length || i < lists[1].length); i++) {
      for (var j = 0; j < lists.length && out.length < MAX_RECENT; j++) if (i < lists[j].length) out.push(lists[j][i]);
    }
    return out;
  }

  function menuItem(text, title, fn, data) {
    var b = document.createElement('button');
    b.type = 'button';
    b.tabIndex = -1;
    b.setAttribute('role', 'menuitem');
    b.textContent = text;
    if (title) b.title = title;
    if (data) Object.keys(data).forEach(function (key) { b.dataset[key] = data[key]; });
    b.addEventListener('click', function () { closeFolderMenu(false); fn(); });
    menuEl.appendChild(b);
    return b;
  }
  function menuSeparator() {
    var s = document.createElement('div');
    s.setAttribute('role', 'separator');
    s.className = 'menu-sep';
    menuEl.appendChild(s);
  }

  function menuOpen() { return !menuEl.hidden; }

  function closeFolderMenu(focusButton) {
    if (!menuOpen()) return;
    menuEl.hidden = true;
    addBtn.setAttribute('aria-expanded', 'false');
    if (focusButton) addBtn.focus();
  }

  // openFolderMenu builds and shows the menu and focuses its first item
  // (the last one with last).
  function openFolderMenu(last) {
    var st = lastFolders || {};
    menuEl.textContent = '';
    menuItem('Add data folder…', 'Choose the folder with your data', function () { pickFolder('data'); }, { action: 'add', kind: 'data' });
    menuItem('Add code folder…', 'Choose the folder with your code (optional: Labmill can write the code for you)', function () { pickFolder('code'); }, { action: 'add', kind: 'code' });
    var recent = recentItems(st), removable = FOLDER_KINDS.filter(function (k) { return (st[k] || {}).path; });
    if (recent.length) {
      menuSeparator();
      recent.forEach(function (r) {
        menuItem(KIND_LABEL[r.kind] + ': ' + r.f.short, r.f.path, function () {
          var app = goApp();
          if (app) folderCall(app.SetFolder(r.kind, r.f.path)).then(function () { if (!dlg.current) inputEl.focus(); });
        }, { action: 'recent', kind: r.kind, path: r.f.path });
      });
    }
    if (removable.length) {
      menuSeparator();
      removable.forEach(function (k) {
        menuItem('Remove ' + k + ' folder', st[k].path, function () { clearFolder(k); }, { action: 'remove', kind: k });
      });
    }
    menuEl.hidden = false;
    addBtn.setAttribute('aria-expanded', 'true');
    var items = menuItems();
    items[last ? items.length - 1 : 0].focus();
  }

  function menuItems() { return Array.prototype.slice.call(menuEl.querySelectorAll('[role=menuitem]')); }

  addBtn.addEventListener('click', function () {
    if (menuOpen()) closeFolderMenu(true); else openFolderMenu(false);
  });
  // The menu button pattern: Enter, Space and ArrowDown open the menu at the
  // first item, ArrowUp at the last. Handled on keydown (default prevented),
  // so the button's own click doesn't toggle it again.
  addBtn.addEventListener('keydown', function (e) {
    if (e.key === 'Enter' || e.key === ' ' || e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault();
      if (e.repeat) return;
      openFolderMenu(e.key === 'ArrowUp');
    } else if (e.key === 'Escape' && menuOpen()) {
      e.preventDefault();
      closeFolderMenu(true);
    }
  });
  addBtn.addEventListener('keyup', function (e) { if (e.key === ' ') e.preventDefault(); });
  menuEl.addEventListener('keydown', function (e) {
    var items = menuItems(), i = items.indexOf(document.activeElement), n = items.length;
    if (e.key === 'Escape') { e.preventDefault(); closeFolderMenu(true); }
    else if (e.key === 'Tab') { closeFolderMenu(false); }
    else if (!n) { return; }
    else if (e.key === 'ArrowDown') { e.preventDefault(); items[(i + 1) % n].focus(); }
    else if (e.key === 'ArrowUp') { e.preventDefault(); items[(i + n - 1) % n].focus(); }
    else if (e.key === 'Home') { e.preventDefault(); items[0].focus(); }
    else if (e.key === 'End') { e.preventDefault(); items[n - 1].focus(); }
    else if ((e.key === 'Enter' || e.key === ' ') && i >= 0) {
      e.preventDefault();
      if (!e.repeat) items[i].click();
    }
  });
  document.addEventListener('click', function (e) {
    if (menuOpen() && (!e.target.closest || !e.target.closest('.plus-wrap'))) closeFolderMenu(false);
  });

  // ---- a folder dropped onto the input box ----
  function hideDropChoice() {
    droppedPath = '';
    $('drop-choice').hidden = true;
    $('drop-choice-name').textContent = '';
    $('drop-choice-name').title = '';
  }
  function showDropChoice(path) {
    droppedPath = path;
    var name = path.replace(/[\\/]+$/, '');
    name = name.slice(Math.max(name.lastIndexOf('/'), name.lastIndexOf('\\')) + 1) || path;
    $('drop-choice-name').textContent = name;
    $('drop-choice-name').title = path;
    $('drop-choice').hidden = false;
    $('drop-data').focus();
  }
  ['data', 'code'].forEach(function (k) {
    $('drop-' + k).addEventListener('click', function () {
      var app = goApp(), p = droppedPath;
      hideDropChoice();
      if (!app || !p) return;
      folderCall(app.DropFolder(k, [p])).then(function () { if (!dlg.current) inputEl.focus(); });
    });
  });
  $('drop-cancel').addEventListener('click', function () { hideDropChoice(); inputEl.focus(); });
  $('drop-choice').addEventListener('keydown', function (e) {
    if (e.key === 'Escape') { e.preventDefault(); hideDropChoice(); inputEl.focus(); }
  });

  // onFolderDrop handles a Wails file drop: only a drop onto the chat input
  // box counts, and only the first path is kept. It shows the choice (data
  // or code) and returns true; Go checks the path when one is picked.
  // Nothing is read or uploaded.
  function onFolderDrop(x, y, paths) {
    var t = document.elementFromPoint(x, y);
    var box = t && t.closest ? t.closest('#composer') : null;
    if (!box || !goApp() || !ready || !paths || !paths.length) return null;
    closeFolderMenu(false);
    showDropChoice(String(paths[0]));
    return true;
  }

  // openFolderButton adds an "Open folder" button after a successful
  // fetch_results note; Go opens the job's recorded folder.
  function openFolderButton(jobId) {
    var b = document.createElement('button');
    b.type = 'button';
    b.className = 'open-folder';
    b.textContent = 'Open folder';
    b.title = 'Open the downloaded results of job #' + jobId;
    b.dataset.job = String(jobId);
    b.addEventListener('click', function () {
      var app = goApp();
      if (app) app.OpenJobFolder(jobId).catch(function (e) { addMsg('err', errText(e)); });
    });
    var stick = nearBottom();
    logEl.appendChild(b);
    if (stick) scrollDown();
  }

  // ---- dialogs (approval and login) ----------------------------------
  // One dialog at a time; later requests wait in order. Each dialog sends
  // exactly one reply. Input is ignored for GRACE_MS after it opens.
  var dlg = { queue: [], current: null };

  function armed() { return !!dlg.current && (performance.now() - dlg.current.openedAt) >= GRACE_MS; }

  // Re-arm the grace period when the window regains focus or becomes visible,
  // so a keystroke meant for another window cannot land on a dialog.
  function rearm() { if (dlg.current) dlg.current.openedAt = performance.now(); }
  window.addEventListener('focus', rearm);
  document.addEventListener('visibilitychange', function () { if (!document.hidden) rearm(); });

  function enqueue(kind, ev) {
    dlg.queue.push({ kind: kind, ev: ev });
    if (!dlg.current) showNext();
  }

  function closeCurrent() {
    var cur = dlg.current;
    dlg.current = null;
    if (working.on) showWorking(working.last);
    $('overlay').hidden = true;
    $('dialog').textContent = '';
    appEl.inert = false;
    if (cur && cur.returnFocus && document.contains(cur.returnFocus)) cur.returnFocus.focus();
    showNext();
  }

  function el(tag, cls, text) {
    var n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text !== undefined) n.textContent = text;
    return n;
  }

  function showNext() {
    if (dlg.current || dlg.queue.length === 0) return;
    var item = dlg.queue.shift();
    var box = $('dialog');
    box.textContent = '';
    var cur = {
      id: item.ev.id, kind: item.kind, answered: false, openedAt: performance.now(),
      returnFocus: document.activeElement, buttons: [], focusEl: null, decline: null
    };
    dlg.current = cur;
    if (working.on) showWorking(working.last);

    // Title and buttons stay put; only the body scrolls.
    var title = el('h2'); title.id = 'dlg-title';
    var body = el('div', 'dlg-body');
    var desc = el('p', 'pre'); desc.id = 'dlg-desc';
    body.appendChild(desc);
    box.appendChild(title); box.appendChild(body);
    var buttons = el('div', 'buttons');

    // guard makes a click on b run fn only after the grace period, and only
    // when its mousedown was after it too.
    function guard(b, fn) {
      b.type = 'button';
      b.addEventListener('mousedown', function () { cur.downArmed = armed(); });
      b.addEventListener('click', function () {
        // A click whose mousedown happened during the grace period is ignored.
        var blocked = cur.downArmed === false;
        cur.downArmed = undefined;
        if (armed() && !blocked) fn();
      });
      cur.buttons.push(b);
      return b;
    }
    function button(label, primary, fn) {
      var b = guard(el('button', primary ? 'primary' : '', label), fn);
      buttons.appendChild(b);
      return b;
    }

    if (item.kind === 'approval') {
      var ev = item.ev, edit = null;
      title.textContent = ev.tool ? 'Approve: ' + ev.tool : 'Approval needed';
      desc.textContent = ev.summary;
      if (ev.altered) {
        var warn = el('p', 'warn', 'The text below was cleaned for display; if you approve without editing, the original runs.');
        warn.setAttribute('role', 'alert');
        body.appendChild(warn);
      }
      if (ev.editable) {
        edit = el('textarea'); edit.value = ev.details; edit.spellcheck = false;
        edit.setAttribute('aria-label', 'Script to run (editable)');
        body.appendChild(edit);
        body.appendChild(el('p', 'muted', 'You can edit this. What you see here is exactly what will run.'));
      } else if (ev.details) {
        var pre = el('pre', '', ev.details);
        pre.tabIndex = 0; pre.setAttribute('aria-label', 'Details');
        body.appendChild(pre);
      }
      // Optional free-text reason for a decline. It never steals the initial
      // focus (Decline keeps it) and Enter in it triggers nothing.
      // Keep the dialog free of <form> elements: a form would give Enter in
      // this input a default (submit) action.
      var reasonLabel = el('label', 'muted', 'Tell Labmill why (optional)');
      reasonLabel.htmlFor = 'dlg-reason';
      var why = el('input'); why.id = 'dlg-reason'; why.type = 'text';
      why.maxLength = 500; why.autocomplete = 'off'; why.spellcheck = false;
      why.placeholder = 'Decline or Esc sends this';
      body.appendChild(reasonLabel); body.appendChild(why);
      cur.decline = function () {
        var reason = why.value;
        reply(function () { return goApp().Decide(ev.id, false, '', reason); });
      };
      cur.focusEl = button('Decline', false, cur.decline);
      var approveBtn = button('Approve', true, function () {
        if (edit && !edit.value.trim()) return;
        reply(function () { return goApp().Decide(ev.id, true, edit ? edit.value : '', ''); });
      });
      if (edit) {
        // An empty script cannot be approved.
        var sync = function () { approveBtn.disabled = cur.answered || !edit.value.trim(); };
        edit.addEventListener('input', sync);
        cur.syncApprove = sync;
        sync();
      }
    } else if (item.kind === 'choice') {
      // A question with fixed answers (ask_user). It changes nothing by
      // itself, so the first option has the initial focus (approvals keep
      // Decline first); the grace period still applies.
      var c = item.ev;
      title.textContent = 'Labmill asks';
      desc.textContent = c.question;
      var list = el('div', 'choices');
      list.setAttribute('role', 'group');
      list.setAttribute('aria-labelledby', 'dlg-desc');
      body.appendChild(list);
      cur.options = [];
      cur.pick = function (i) {
        reply(function () { return goApp().Choose(c.id, i, '', false); });
      };
      (c.options || []).forEach(function (o, i) {
        var b = el('button', 'choice');
        var num = el('span', 'choice-num', String(i + 1));
        var txt = el('span', 'choice-text');
        txt.appendChild(el('span', 'choice-label', o.label));
        if (o.description) txt.appendChild(el('span', 'choice-desc', o.description));
        b.appendChild(num); b.appendChild(txt);
        guard(b, function () { cur.pick(i); });
        list.appendChild(b);
        cur.options.push(b);
      });
      cur.nav = cur.options.slice();
      if (c.allowOther) {
        var otherLabel = el('label', 'muted', 'Or type your own answer');
        otherLabel.htmlFor = 'dlg-other';
        var other = el('input'); other.id = 'dlg-other'; other.type = 'text';
        other.maxLength = 500; other.autocomplete = 'off'; other.spellcheck = false;
        other.placeholder = 'Other… (Enter sends)';
        body.appendChild(otherLabel); body.appendChild(other);
        other.addEventListener('keydown', function (e) {
          if (e.key !== 'Enter' || e.isComposing) return;
          e.preventDefault();
          var v = other.value;
          if (!armed() || !v.trim()) return;
          reply(function () { return goApp().Choose(c.id, -1, v, false); });
        });
        cur.other = other;
        cur.nav.push(other);
      }
      cur.decline = function () { reply(function () { return goApp().Choose(c.id, -1, '', true); }); };
      button('Skip', false, cur.decline);
      cur.focusEl = cur.options[0] || cur.buttons[0];
    } else {
      var p = item.ev, input = null;
      title.textContent = 'Sign in to ' + (p.host || 'the cluster');
      desc.textContent = p.prompt;
      if (p.kind === 'keyupload') {
        // SSH key setup (M38.6): the new public key in its own code field
        // (text only) with a Copy button; Continue says it is uploaded.
        title.textContent = 'Register your SSH key with ' + (p.host || 'the cluster');
        var kRow = el('div', 'login-url-row');
        var keyEl = el('code', 'login-url', p.data || '');
        keyEl.id = 'dlg-pubkey';
        keyEl.setAttribute('aria-label', 'Public key');
        var kStatus = el('p', 'login-status muted'); kStatus.id = 'dlg-copy-status';
        kStatus.setAttribute('aria-live', 'polite');
        var kCopy = el('button', 'copy', 'Copy');
        kCopy.type = 'button';
        kCopy.id = 'dlg-copy';
        kCopy.setAttribute('aria-label', 'Copy the public key');
        kCopy.addEventListener('click', function () {
          window.labmillMarkdown.copyText(p.data || '', keyEl).then(function (ok) {
            testHooks.keyCopies++;
            kStatus.textContent = ok ? 'Public key copied.' : 'The key is selected: press Ctrl+C (or Cmd+C) to copy it.';
          });
        });
        kRow.appendChild(keyEl); kRow.appendChild(kCopy);
        body.appendChild(kRow); body.appendChild(kStatus);
        cur.decline = function () { reply(function () { return goApp().Answer(p.id, '', true); }); };
        button('Cancel', false, cur.decline);
        button("I've added it, try again", true, function () {
          reply(function () { return goApp().Answer(p.id, 'yes', false); });
        });
        cur.focusEl = kCopy;
      } else if (p.kind === 'hostkey' || p.kind === 'keycreate' || p.kind === 'keycopy') {
        var yesLabel = p.kind === 'keycreate' ? 'Create key' : (p.kind === 'keycopy' ? 'Copy key' : 'Yes');
        var noLabel = p.kind === 'keycreate' ? 'Not now' : (p.kind === 'keycopy' ? 'No, create a new key' : 'No');
        if (p.kind === 'keycreate') title.textContent = 'SSH key needed for ' + (p.host || 'the cluster');
        if (p.kind === 'keycopy') title.textContent = 'Copy a private key to this computer?';
        cur.decline = function () { reply(function () { return goApp().Answer(p.id, '', true); }); };
        cur.focusEl = button(noLabel, false, function () {
          reply(function () { return goApp().Answer(p.id, 'no', false); });
        });
        button(yesLabel, true, function () {
          reply(function () { return goApp().Answer(p.id, 'yes', false); });
        });
      } else {
        input = el('input');
        input.type = p.echo ? 'text' : 'password';
        input.autocomplete = 'off'; input.spellcheck = false;
        input.setAttribute('aria-labelledby', 'dlg-desc');
        body.appendChild(input);
        var submit = function () {
          var v = input.value;
          input.value = '';
          reply(function () { return goApp().Answer(p.id, v, false); });
        };
        cur.decline = function () { input.value = ''; reply(function () { return goApp().Answer(p.id, '', true); }); };
        input.addEventListener('keydown', function (e) {
          if (e.key === 'Enter' && armed()) { e.preventDefault(); submit(); }
        });
        cur.focusEl = input;
        button('Cancel', false, cur.decline);
        button('Submit', true, submit);
      }
    }
    box.appendChild(buttons);

    function reply(call) {
      if (cur.answered || dlg.current !== cur) return;
      cur.answered = true;
      testHooks.replies++;
      cur.buttons.forEach(function (b) { b.disabled = true; });
      if (cur.other) cur.other.disabled = true;
      var done = function () { if (dlg.current === cur) closeCurrent(); };
      var p;
      try { p = call(); } catch (e) { p = Promise.reject(e); }
      p.then(done, function (e) { addMsg('err', errText(e)); done(); });
    }

    appEl.inert = true;
    $('overlay').hidden = false;
    cur.focusEl.focus();
  }

  // Keyboard handling while a dialog is open: grace period, Esc declines,
  // Tab stays inside the dialog.
  document.addEventListener('keydown', function (e) {
    var cur = dlg.current;
    if (!cur) return;
    // During the grace period nothing counts; afterwards only held
    // activation keys are ignored (a held Backspace or arrow must still work
    // in the editable script and the password field).
    var activation = e.key === 'Enter' || e.key === ' ' || e.key === 'Escape';
    if (!armed() || (e.repeat && activation)) { e.preventDefault(); e.stopPropagation(); return; }
    if (cur.answered) { e.preventDefault(); return; }
    if (e.key === 'Escape') { e.preventDefault(); cur.decline(); return; }
    if (cur.kind === 'choice' && choiceKey(cur, e)) { e.preventDefault(); e.stopPropagation(); return; }
    if (e.key === 'Tab') {
      var f = Array.prototype.slice.call($('dialog').querySelectorAll('button:not(:disabled), input, textarea, pre[tabindex]'));
      // (the dialog body scrolls; focus moving into it scrolls it into view)
      if (f.length === 0) return;
      var i = f.indexOf(document.activeElement);
      e.preventDefault();
      f[(i + (e.shiftKey ? f.length - 1 : 1)) % f.length].focus();
    }
  }, true);

  // choiceKey handles the selection keys of a choice dialog (after the grace
  // period) and reports whether it used the key: Up/Down move between the
  // options and the "Other" input, Enter picks the focused option (the input
  // handles its own Enter), number keys pick an option directly unless the
  // user is typing in the input.
  function choiceKey(cur, e) {
    var a = document.activeElement, inOther = !!cur.other && a === cur.other;
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      var n = cur.nav.length, i = cur.nav.indexOf(a);
      if (n === 0) return false;
      if (i < 0) i = e.key === 'ArrowDown' ? -1 : 0;
      cur.nav[(i + (e.key === 'ArrowDown' ? 1 : n - 1)) % n].focus();
      return true;
    }
    if (e.key === 'Enter' && !e.isComposing) {
      var k = cur.options.indexOf(a);
      if (k < 0) return false;
      cur.pick(k);
      return true;
    }
    if (!inOther && !e.ctrlKey && !e.metaKey && !e.altKey && /^[1-9]$/.test(e.key)) {
      var idx = parseInt(e.key, 10) - 1;
      if (idx < cur.options.length) cur.pick(idx);
      return true;
    }
    return false;
  }

  // Ignore pointer input during the grace period too.
  ['mousedown', 'mouseup', 'click', 'dblclick'].forEach(function (t) {
    $('overlay').addEventListener(t, function (e) {
      if (dlg.current && !armed()) {
        // Remember that this press began during the grace period: the
        // button never sees it (we stop it here), so mark it on the dialog
        // so a click completing after the grace period is still ignored.
        if (t === 'mousedown') dlg.current.downArmed = false;
        e.preventDefault(); e.stopPropagation();
      }
    }, true);
  });

  function dismiss(ids) {
    var set = {};
    (ids || []).forEach(function (i) { set[i] = true; });
    dlg.queue = dlg.queue.filter(function (q) { return !set[q.ev.id]; });
    if (dlg.current && set[dlg.current.id]) { dlg.current.answered = true; closeCurrent(); }
  }

  // ---- events from Go ------------------------------------------------
  function on(name, fn) { window.runtime.EventsOn(name, fn); }

  function wireEvents() {
    on('Labmill:text', function (e) { hideWorking(); appendAssistant(e.text); });
    // What the running turn is doing now (the "Working…" row).
    on('Labmill:step', function (e) { if (busy) showWorking(e.text); });
    // Tool notes: a call opens a collapsed line, its result fills it in
    // ("load_skill · 4.2 KB", expandable to the full text). Errors show
    // their first line; a decline says so and keeps that label.
    on('Labmill:tool', function (e) {
      flushAssistant();
      if (e.kind === 'call') {
        if (busy) showWorking('Running ' + e.tool + '…');
        pendingTool = toolItem(e.tool);
      } else if (e.kind === 'declined') {
        var di = (pendingTool && pendingTool.tool === e.tool) ? pendingTool : toolItem(e.tool);
        finishTool(di, 'Declined: ' + e.tool + (e.text ? ' (' + firstLine(e.text) + ')' : ''), '', false);
        di.declined = true;
        pendingTool = di;
      } else {
        var it = (pendingTool && pendingTool.tool === e.tool) ? pendingTool : null;
        pendingTool = null;
        if (it && it.declined) { showProgress(); return; }
        if (!it) it = toolItem(e.tool);
        var t = e.text || '';
        if (e.isError) {
          var fl = firstLine(t) || 'failed';
          finishTool(it, '✗ ' + e.tool + ': ' + fl, t.trim() !== fl ? t : '', true);
        } else {
          finishTool(it, e.tool + (t ? ' · ' + fmtSize(t) : ' · done'), t, false);
          if (e.tool === 'fetch_results' && e.jobId) openFolderButton(e.jobId);
        }
      }
      showProgress();
    });
    on('Labmill:progress', showTransfer);
    on('Labmill:todos', function (e) { setTodos(e.items, true); });
    on('Labmill:notify', function (e) { notifyText = e.text; showProgress(); });
    on('Labmill:approval', function (e) { enqueue('approval', e); });
    on('Labmill:prompt', function (e) { enqueue('prompt', e); });
    on('Labmill:choice', function (e) { enqueue('choice', e); });
    on('Labmill:dismiss', function (e) { dismiss(e.ids); });
    // A turn started (typed, queued or from a job button): show the message.
    on('Labmill:turn', function (e) {
      flushAssistant();
      addMsg('user', e.text);
      notifyText = '';
      showProgress();
      setBusy(true);
      hideWorking(); // restarts the elapsed time for this turn
      showWorking();
    });
    on('Labmill:queued', function (e) { showQueued(e.text); });
    // A background job notice: a system line, and the job list at once.
    on('Labmill:job', function (e) {
      addSystem(e.text);
      if (e.jobId) openFolderButton(e.jobId); // results downloaded by themselves (M60.1)
      testHooks.notices++;
      refreshJobs();
    });
    // An app notice (e.g. "still running in the tray"): a system line.
    on('Labmill:notice', function (e) {
      addSystem(e.text);
      // An automatic approval proves the setting is on, even if it was
      // turned on from another process (the terminal).
      if (typeof e.text === 'string' && e.text.indexOf('Auto-approved: ') === 0) showApproveAll(true);
    });
    on('Labmill:done', function (e) {
      flushAssistant();
      if (e.error) addMsg('err', e.error);
      testHooks.done++;
      todosTurnDone(!!e.error);
      setBusy(false);
      refreshJobs();
    });
    on('Labmill:ready', function (e) { showReady(e.status); });
    on('Labmill:status', function (e) { lastStatus = e.status || ''; if (ready) $('status').textContent = lastStatus; });
    on('Labmill:fatal', function (e) { showFatal(e.error); });
    on('Labmill:startup', function (e) { setStartStep(e.step); });
  }

  // ---- startup -------------------------------------------------------
  // The splash (hourglass, "Starting labmill…") is in the initial HTML. The
  // text follows Go's startup steps (Labmill:startup, State.step); after
  // STILL_WAITING_MS a second line says which step it is still waiting for.
  var startStep = '', startSince = performance.now();
  var startTimer = setInterval(tickStart, 1000);

  function setStartStep(step) {
    if (!step || settled) return;
    startStep = step;
    $('starting-text').textContent = step;
    if (chatUp && !ready) $('status').textContent = step; // the chat is shown while the runtime starts
    tickStart();
  }

  function tickStart() {
    var wait = $('starting-wait');
    var ms = performance.now() - startSince;
    if (settled || $('starting').hidden || ms < STILL_WAITING_MS) { wait.hidden = true; return; }
    var what = startStep ? startStep.replace(/…$/, '').toLowerCase() : 'Labmill to start';
    wait.textContent = 'This is taking a while. Still waiting for: ' + what + ' (' + Math.floor(ms / 1000) + ' s).';
    wait.hidden = false;
  }

  function show(id) {
    if (id === 'starting' && $('starting').hidden) startSince = performance.now(); // e.g. after the setup form
    ['starting', 'fatal', 'setup', 'main'].forEach(function (s) { $(s).hidden = (s !== id); });
    tickStart();
  }

  function stopStartTimer() {
    if (startTimer) { clearInterval(startTimer); startTimer = null; }
  }

  // The divider between chat and jobs panel (M51.1). The jobs panel's width
  // is a fraction of the main area (CSS variable --jobs-w). jobsWant is what
  // the user chose (or the saved value); jobsFrac is what is shown, which
  // the window size can clamp. Resizing the window re-applies jobsWant, so a
  // narrow moment never shrinks the choice. The saved value comes from Go
  // (SettingsAPI.JobsWidth, 0 = default); a key change is saved after 400 ms
  // of quiet, the end of a drag at once.
  var JOBS_DEF = 0.40, JOBS_LO = 0.15, JOBS_HI = 0.60, JOBS_STEP = 0.02;
  var JOBS_MIN_REM = 14, CHAT_MIN_REM = 24, DIV_PX = 10;
  var mainEl = $('main'), divEl = $('divider'), jobsFrac = JOBS_DEF, jobsWant = JOBS_DEF, jobsTimer = null;
  function absClamp(f) { return Math.min(JOBS_HI, Math.max(JOBS_LO, f)); }
  function jobsLimits() {
    var w = mainEl.clientWidth || window.innerWidth;
    var rem = parseFloat(getComputedStyle(document.documentElement).fontSize) || 16;
    var lo = Math.max(JOBS_LO, JOBS_MIN_REM * rem / w);
    var hi = Math.min(JOBS_HI, 1 - (CHAT_MIN_REM * rem + DIV_PX) / w);
    return { lo: lo, hi: Math.max(lo, hi) };
  }
  // applyJobsWidth shows f (clamped to what fits now) and returns it.
  function applyJobsWidth(f) {
    var l = jobsLimits();
    jobsFrac = Math.min(l.hi, Math.max(l.lo, f));
    mainEl.style.setProperty('--jobs-w', (jobsFrac * 100).toFixed(2) + '%');
    divEl.setAttribute('aria-valuenow', String(Math.round(jobsFrac * 100)));
    divEl.setAttribute('aria-valuemin', String(Math.round(l.lo * 100)));
    divEl.setAttribute('aria-valuemax', String(Math.round(l.hi * 100)));
    return jobsFrac;
  }
  var jobsSaving = Promise.resolve(), jobsMoved = false;
  function saveJobsWidth(now) {
    clearTimeout(jobsTimer);
    var f = jobsWant === JOBS_DEF ? 0 : jobsWant;
    // Bound calls run concurrently on the Go side: chain the saves so a
    // later one (a double-click reset) can't be overtaken by an earlier one.
    var go = function () {
      jobsSaving = jobsSaving.then(function () {
        var a = settingsApi();
        if (a && a.SetJobsWidth) return a.SetJobsWidth(f);
      }).then(null, function () {});
    };
    if (now) go(); else jobsTimer = setTimeout(go, 400);
  }
  // chooseJobsWidth is the user's choice: shown, remembered and saved. A
  // choice at either end (Home, End, a drag past the end) stays at that end
  // for a larger window too.
  function chooseJobsWidth(f, now) {
    var shown = applyJobsWidth(f);
    jobsWant = (f <= JOBS_LO || f >= JOBS_HI) ? absClamp(f) : shown;
    saveJobsWidth(now);
  }
  divEl.addEventListener('keydown', function (e) {
    var f = null;
    if (e.key === 'ArrowLeft') f = jobsFrac + JOBS_STEP; // the divider moves left: the panel grows
    else if (e.key === 'ArrowRight') f = jobsFrac - JOBS_STEP;
    else if (e.key === 'Home') f = 0; // narrowest panel
    else if (e.key === 'End') f = 1;
    if (f === null) return;
    e.preventDefault();
    chooseJobsWidth(f, false);
  });
  divEl.addEventListener('pointerdown', function (e) {
    if (e.button !== 0) return;
    e.preventDefault();
    try { divEl.setPointerCapture(e.pointerId); } catch (x) {} // a synthetic pointer (self-test) has none
    divEl.classList.add('dragging');
    jobsMoved = false;
    divEl.focus();
  });
  divEl.addEventListener('pointermove', function (e) {
    if (!divEl.classList.contains('dragging')) return;
    if ((e.buttons & 1) === 0) { endDrag(e); return; }
    var r = mainEl.getBoundingClientRect();
    jobsMoved = true;
    chooseJobsWidth((r.right - e.clientX - DIV_PX / 2) / r.width, false);
  });
  // endDrag ends a drag (pointer up or cancel, lost capture, button already
  // released) and saves the width at once when the pointer moved (a plain
  // click or the clicks of a double-click save nothing).
  function endDrag(e) {
    if (!divEl.classList.contains('dragging')) return;
    divEl.classList.remove('dragging');
    try { if (divEl.hasPointerCapture(e.pointerId)) divEl.releasePointerCapture(e.pointerId); } catch (x) {}
    if (jobsMoved) saveJobsWidth(true);
  }
  divEl.addEventListener('pointerup', endDrag);
  divEl.addEventListener('pointercancel', endDrag);
  divEl.addEventListener('lostpointercapture', endDrag);
  divEl.addEventListener('dblclick', function () { chooseJobsWidth(JOBS_DEF, true); });
  window.addEventListener('resize', function () { applyJobsWidth(jobsWant); });
  applyJobsWidth(jobsWant);
  (function () {
    var a = settingsApi();
    if (a && a.JobsWidth) a.JobsWidth().then(function (f) { if (f > 0) { jobsWant = absClamp(f); applyJobsWidth(jobsWant); } }, function () {});
  })();
  testHooks.jobsWidth = function () { return jobsFrac; };
  testHooks.jobsWant = function () { return jobsWant; };

  // showChat shows the chat before the runtime is ready (M46.2): the input
  // works at once and a message typed now is sent when Labmill has started.
  function showChat() {
    if (chatUp || settled) return;
    chatUp = true;
    $('status').textContent = startStep || 'Starting labmill\u2026';
    show('main');
    applyJobsWidth(jobsWant); // the area has its real width now
    inputEl.focus();
    mark('chat');
  }

  function showReady(status) {
    if (settled) return;
    settled = true;
    stopStartTimer();
    ready = true;
    $('status').textContent = lastStatus !== null ? lastStatus : (status || ''); // a status event seen earlier is newer
    show('main');
    applyJobsWidth(jobsWant); // the area has its real width now
    if (!chatUp) { chatUp = true; inputEl.focus(); mark('chat'); }
    refreshJobs();
    refreshFolders();
    refreshApproveAll();
    refreshAutoDownload();
    if (window.labmillSettings) window.labmillSettings.enable(); // the gear button (M29)
    if (window.labmillTerminal) window.labmillTerminal.enable(); // the Terminal button (M62.2)
  }

  function showFatal(msg) {
    if (settled) return;
    settled = true;
    stopStartTimer();
    ready = false;
    chatUp = false;
    $('fatal-text').textContent = msg || 'unknown error';
    show('fatal');
    if (window.labmillSettings) window.labmillSettings.enable(); // Settings helps fix what failed
  }

  var setupSites = [];
  function siteNeedsHosts() {
    var d = $('setup-site').value;
    for (var i = 0; i < setupSites.length; i++) {
      if (setupSites[i].domain === d) return !!setupSites[i].needsHosts;
    }
    return false;
  }
  function syncSetupHosts() { $('setup-hosts').hidden = setupOnly || !siteNeedsHosts(); }

  var setupOnly = false;
  function showSetup(note, only) {
    show('setup');
    setupOnly = !!only; // only the institution is asked: clusters exist already
    $('setup-user-block').hidden = setupOnly;
    $('setup-user').required = !setupOnly;
    var noteEl = $('setup-note');
    noteEl.textContent = note || '';
    noteEl.hidden = !note;
    $(setupOnly ? 'setup-site' : 'setup-user').focus();
    goSetup().Sites().then(function (list) {
      setupSites = list || [];
      var sel = $('setup-site');
      sel.textContent = '';
      setupSites.forEach(function (c) {
        var o = document.createElement('option');
        o.value = c.domain;
        o.textContent = c.domain === 'generic' ? c.name : c.name + ' (' + c.domain + ')';
        if (c.default) o.selected = true;
        sel.appendChild(o);
      });
      syncSetupHosts();
    }, function (err) { $('setup-error').textContent = errText(err); });
  }
  $('setup-site').addEventListener('change', syncSetupHosts);

  $('setup-form').addEventListener('submit', function (e) {
    e.preventDefault();
    var name = $('setup-user').value.trim();
    var errEl = $('setup-error');
    errEl.textContent = '';
    $('setup-submit').disabled = true;
    var hosts = siteNeedsHosts() && !setupOnly;
    goSetup().SubmitSetup($('setup-site').value, name,
      hosts ? $('setup-login').value.trim() : '').then(function () {
      if (!settled) show('starting');
    }, function (err) {
      errEl.textContent = errText(err);
      $('setup-submit').disabled = false;
      $('setup-user').focus();
    });
  });

  function applyState(st) {
    if (!st || settled) return;
    if (st.step && !startStep) setStartStep(st.step); // a live step event is newer than the State snapshot
    if (st.phase === 'ready') showReady(st.status);
    else if (st.phase === 'failed') showFatal(st.error);
    else if (st.phase === 'setup') showSetup(st.note, st.onlyInstitution);
    else showChat();
  }

  function init() {
    if (!window.runtime || !window.go || !goSetup() || !goApp()) { setTimeout(init, 50); return; }
    mark('bindings');
    wireEvents();
    // Wails file drop, limited to the drop target (the chat input box).
    if (window.runtime.OnFileDrop) window.runtime.OnFileDrop(onFolderDrop, true);
    // Registered after Wails' own dragover handler, so this one decides the
    // cursor: a copy cursor over the input box, "not allowed" elsewhere.
    window.addEventListener('dragover', function (e) {
      var onBox = e.target && e.target.closest && e.target.closest('#composer');
      if (e.dataTransfer) e.dataTransfer.dropEffect = onBox ? 'copy' : 'none';
    });
    goSetup().State().then(applyState, function (e) { showFatal(errText(e)); });
    // Self-test mode: load the scripted checks (a separate same-origin file).
    goApp().SelfTest().then(function (on) {
      if (!on) return;
      window.__labmillTest = testHooks;
      var sc = document.createElement('script');
      sc.src = 'selftest.js';
      document.body.appendChild(sc);
    }, function () {});
  }

  // Never let a dropped file navigate the webview away (or be opened by it):
  // only the chat input box takes drops, through Wails, as a path.
  ['dragover', 'drop'].forEach(function (t) {
    window.addEventListener(t, function (e) { e.preventDefault(); });
  });
  document.addEventListener('DOMContentLoaded', init);
})();
