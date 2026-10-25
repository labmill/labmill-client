// In-webview self-test (LABMILL_GUI_SELFTEST=1). app.js loads this file only
// when Go says it is in self-test mode. It drives the real DOM through the
// scripted conversation in selftest.go and reports with
// PageAPI.SelfTestReport. Never loaded in normal runs.
(function () {
  'use strict';
  var log = [], passed = true;
  var hooks = window.__labmillTest;
  var api = window.go.main.PageAPI;

  function sleep(ms) { return new Promise(function (r) { setTimeout(r, ms); }); }
  function check(ok, msg) {
    log.push((ok ? 'PASS ' : 'FAIL ') + msg);
    if (!ok) passed = false;
  }
  function waitFor(what, fn, ms) {
    var end = performance.now() + (ms || 8000);
    return new Promise(function (resolve, reject) {
      (function poll() {
        var v = fn();
        if (v) return resolve(v);
        if (performance.now() > end) return reject(new Error('timed out waiting for ' + what));
        setTimeout(poll, 20);
      })();
    });
  }
  function title() { var t = document.getElementById('dlg-title'); return t ? t.textContent : ''; }
  function dialogOpen() { return !document.getElementById('overlay').hidden; }
  function button(label) {
    var bs = document.querySelectorAll('#dialog button');
    for (var i = 0; i < bs.length; i++) if (bs[i].textContent === label) return bs[i];
    return null;
  }
  function key(target, k) {
    target.dispatchEvent(new KeyboardEvent('keydown', { key: k, bubbles: true, cancelable: true }));
  }

  // mediaRules returns the style rules inside the @media rules whose
  // condition contains media.
  function mediaRules(media) {
    var out = [];
    Array.prototype.forEach.call(document.styleSheets, function (sh) {
      var rules;
      try { rules = sh.cssRules; } catch (e) { return; }
      Array.prototype.forEach.call(rules || [], function (r) {
        if (r.media && r.media.mediaText.indexOf(media) !== -1) {
          Array.prototype.forEach.call(r.cssRules, function (x) { out.push(x); });
        }
      });
    });
    return out;
  }

  async function run() {
    // 0. The startup splash (M26): in the initial HTML (before any script),
    // visible and animated when app.js first ran; a static hourglass when
    // the OS asks for reduced motion.
    var reduced = window.matchMedia('(prefers-reduced-motion: reduce)').matches;
    check(hooks.splashAtLoad === true, 'the startup splash (hourglass, "Starting labmill…") was visible when app.js first ran');
    check(reduced ? hooks.splashAnimation === 'none' : (!!hooks.splashAnimation && hooks.splashAnimation !== 'none'),
      'the splash hourglass was animated at load (animation ' + hooks.splashAnimation + ', reduced motion ' + reduced + ')');
    try {
      var html = await (await fetch('index.html')).text();
      var doc = new DOMParser().parseFromString(html, 'text/html');
      var sp = doc.getElementById('starting'), stx = doc.getElementById('starting-text');
      check(!!sp && !sp.hasAttribute('hidden') && !!sp.querySelector('.hourglass[aria-hidden="true"]') && !!stx &&
        stx.textContent === 'Starting labmill…' && stx.getAttribute('aria-live') === 'polite' && doc.getElementById('main').hasAttribute('hidden'),
        'index.html itself shows the splash first: a visible hourglass and "Starting labmill…" (aria-live polite), the chat hidden');
      check(doc.querySelectorAll('script:not([src]), [style]').length === 0, 'index.html has no inline script or style');
    } catch (e) {
      check(false, 'reading index.html failed: ' + e);
    }
    var still = mediaRules('prefers-reduced-motion').filter(function (r) {
      return r.selectorText && r.selectorText.indexOf('.hourglass') !== -1 && r.style && r.style.animationName === 'none';
    });
    check(still.length > 0, 'app.css stops the hourglass animation for prefers-reduced-motion (' + (still[0] ? still[0].selectorText : 'no rule') + ')');
    // M46.2: the chat is shown as soon as the bindings are up, while Go is
    // still in its "starting" phase (the self-test holds it there until it
    // calls SelfTestRelease); the status line names the startup step and
    // the input takes text.
    await waitFor('the chat before the runtime is ready', function () { return !document.getElementById('main').hidden; });
    var inp = document.getElementById('input');
    check(document.getElementById('starting').hidden, 'the chat replaced the splash before the runtime was ready');
    check(hooks.isReady() === false, 'the page is not marked ready yet while the chat is shown');
    check(!inp.disabled && document.activeElement === inp, 'the chat input is enabled and focused before ready');
    check(document.getElementById('status').textContent === 'Opening the job database…',
      'the status line shows the startup step before ready (got "' + document.getElementById('status').textContent + '")');
    inp.value = 'typed before ready';
    check(inp.value === 'typed before ready', 'the chat input takes text before ready');
    inp.value = '';
    await api.SelfTestRelease();
    await waitFor('ready', function () { return hooks.isReady() === true; });
    check(document.getElementById('status').textContent === 'self-test (status event first)', 'a Labmill:status seen before Labmill:ready wins over the ready text (got "' + document.getElementById('status').textContent + '")');

    await waitFor('the main view', function () { return !document.getElementById('main').hidden; });
    await api.Send('self-test');

    // 1. Approval dialog: hostile markup stays text, Decline is focused, the
    // grace period swallows early input, Approve works after it.
    await waitFor('the approval dialog', function () { return dialogOpen() && title().indexOf('Approve') === 0; });
    var opened = performance.now();
    var wrow = document.getElementById('working');
    check(!wrow.hidden && document.getElementById('working-text').textContent === 'Waiting for your answer…',
      'while the approval dialog waits, the Working row says "Waiting for your answer…" (got "' + document.getElementById('working-text').textContent + '")');
    var desc = document.getElementById('dlg-desc');
    check(desc.textContent.indexOf('<img') !== -1, 'summary shows the literal "<img" as text');
    check(document.querySelector('#dialog img') === null && document.querySelector('img') === null, 'no <img> element exists in the page');
    check(window.__pwned === undefined, 'window.__pwned is undefined');
    var pre = document.querySelector('#dialog pre');
    check(!!pre && pre.textContent.indexOf('\n') !== -1, 'multi-line details are shown');
    var early = button('Approve');
    early.dispatchEvent(new MouseEvent('mousedown', { bubbles: true, cancelable: true }));
    check(performance.now() - opened < 500, 'a mousedown on Approve was dispatched inside the grace period');
    var ae = document.activeElement;
    check(!!ae && ae.textContent === 'Decline', 'Decline is focused (got ' + (ae ? ae.textContent : 'nothing') + ')');
    key(document.activeElement, 'Escape');
    await sleep(50);
    check(dialogOpen() && hooks.replies === 0, 'Escape within the grace period sent no reply');
    key(document.activeElement, 'Enter');
    var dec = button('Decline'); if (dec) dec.click();
    await sleep(100);
    check(dialogOpen() && title().indexOf('Approve') === 0 && hooks.replies === 0, 'Enter and a click within the grace period sent no decision');
    await sleep(Math.max(0, 600 - (performance.now() - opened)));
    var ap = button('Approve');
    check(!!ap && !ap.disabled, 'Approve is available after the grace period');
    var notCancelled = document.activeElement.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowDown', repeat: true, bubbles: true, cancelable: true }));
    check(notCancelled, 'a held (repeat) ArrowDown in the dialog is not cancelled');
    ap.click();  // its mousedown happened during the grace period
    await sleep(150);
    check(hooks.replies === 0, 'a click whose mousedown was in the grace period is ignored (replies=' + hooks.replies + ')');
    if (hooks.replies === 0) ap.click();
    await waitFor('the approval dialog to close', function () { return !dialogOpen() || title().indexOf('Approve') !== 0; });
    check(true, 'the approval dialog closed after Approve');

    // 1b. A second approval that the test declines with a typed reason.
    await waitFor('the second approval dialog', function () { return dialogOpen() && title() === 'Approval needed'; });
    var opened1b = performance.now();
    var why = document.getElementById('dlg-reason');
    check(!!why && why.type === 'text' && why.maxLength === 500, 'the reason input exists (single line, maxlength 500)');
    check(document.activeElement !== why && document.activeElement.textContent === 'Decline', 'Decline, not the reason input, is focused on open');
    await sleep(Math.max(0, 600 - (performance.now() - opened1b)));
    why.focus();
    why.value = 'use the gpu partition';
    why.dispatchEvent(new Event('input', { bubbles: true }));
    key(why, 'Enter');
    await sleep(150);
    check(dialogOpen() && title() === 'Approval needed' && hooks.replies === 1, 'Enter inside the reason input sent no decision (replies=' + hooks.replies + ')');
    button('Decline').click();
    await waitFor('the second approval dialog to close', function () { return !dialogOpen() || title() !== 'Approval needed'; });
    check(hooks.replies === 2, 'Decline sent exactly one more decision (replies=' + hooks.replies + ')');

    // 2. Login dialog: password field, cleared after submit.
    await waitFor('the login dialog', function () { return dialogOpen() && title().indexOf('Sign in') === 0; });
    var opened2 = performance.now();
    var input = document.querySelector('#dialog input');
    check(!!input && input.type === 'password', 'the login input is type=password');
    await sleep(Math.max(0, 600 - (performance.now() - opened2)));
    input.value = 'pw';
    key(input, 'Enter');
    check(input.value === '', 'the password field is empty after submitting');

    // 2b. Choice dialog (ask_user): the first option is focused, a number
    // key and a click inside the grace period pick nothing, ArrowDown +
    // Enter after it picks option 2.
    await waitFor('the choice dialog', function () { return dialogOpen() && title() === 'Labmill asks'; });
    var opened3 = performance.now(), before = hooks.replies;
    var opts = document.querySelectorAll('#dialog button.choice');
    check(opts.length === 3, 'the choice dialog shows 3 options (got ' + opts.length + ')');
    check(document.getElementById('dlg-desc').textContent.indexOf('<b>Which</b>') !== -1 && document.querySelector('#dialog b') === null,
      'the question shows "<b>" as text and creates no <b> element');
    check(opts.length > 1 && opts[1].textContent.indexOf('<img') !== -1 && document.querySelector('img') === null,
      'an option description shows "<img" as text and creates no <img> element');
    check(!!document.getElementById('dlg-other'), 'the Other input exists');
    check(opts.length > 0 && document.activeElement === opts[0], 'the first option is focused on open');
    key(document.activeElement, '2');
    if (opts.length > 2) opts[2].click();
    await sleep(50);
    check(performance.now() - opened3 < 500, 'the number key and click were sent inside the grace period');
    check(dialogOpen() && title() === 'Labmill asks' && hooks.replies === before, 'a number key and a click within the grace period picked nothing');
    await sleep(Math.max(0, 600 - (performance.now() - opened3)));
    key(document.activeElement, 'ArrowDown');
    check(opts.length > 1 && document.activeElement === opts[1], 'ArrowDown moves the focus to the second option');
    key(document.activeElement, 'Enter');
    await waitFor('the choice dialog to close', function () { return !dialogOpen() || title() !== 'Labmill asks'; });
    check(hooks.replies === before + 1, 'Enter sent exactly one choice (replies=' + hooks.replies + ')');

    // 3. The turn ends.
    await waitFor('Labmill:done', function () { return hooks.done >= 1; });
    check(!dialogOpen(), 'no dialog is open after the turn ended');
    var logEl = document.getElementById('log');
    check(logEl.textContent.indexOf('Self-test: hello.') !== -1, 'the streamed text reached the chat log');

    // 4. Markdown: normal Markdown becomes elements, hostile Markdown stays text.
    ['strong', 'em', 'code', 'pre', 'h4', 'ul li', 'blockquote'].forEach(function (sel) {
      check(logEl.querySelector('.msg.assistant ' + sel) !== null, 'Markdown rendered a <' + sel + '> element');
    });
    check(document.querySelector('a') === null, 'no <a> element exists in the page');
    check(document.querySelector('img') === null, 'no <img> element exists in the page');
    check(logEl.querySelector('script') === null, 'no <script> element exists in the chat');
    check(window.__pwned === undefined, 'window.__pwned is still undefined');
    var lt = logEl.textContent;
    check(lt.indexOf('x (javascript:alert(1))') !== -1, 'a javascript: link is shown as "text (url)" plain text');
    check(lt.indexOf('<img src=x') !== -1 && lt.indexOf('<a href=') !== -1, 'raw HTML inside Markdown is shown as literal text');
    var pres = logEl.querySelectorAll('.msg.assistant pre');
    check(pres.length >= 2 && pres[pres.length - 1].textContent.indexOf('unterminated <script>') !== -1, 'an unterminated code fence renders as a code block');
    check(pres.length > 0 && pres[0].textContent.indexOf('<b>not bold</b>') !== -1, 'HTML inside a code block stays text');
    var am = logEl.querySelector('.msg.assistant');
    var fs = am ? parseFloat(getComputedStyle(am).fontSize) : 0;
    check(fs >= 15, 'the chat font is at least 15px (got ' + fs + ')');
    var codeFont = logEl.querySelector('.msg.assistant code');
    check(!!codeFont && /monospace/.test(getComputedStyle(codeFont).fontFamily), 'code uses a monospace font');

    // 4b. Pathological Markdown renders fast and never throws or leaves the
    // chat busy (ReDoS and recursion-depth regressions).
    var rep = function (s, n) { return new Array(n + 1).join(s); };
    var evil = {
      'heading with 4000 spaces': '# a' + rep(' ', 4000) + 'x',
      '4000 quote markers': rep('>', 4000),
      '5000 nested brackets': rep('[[', 5000) + 'x' + rep('](u)]', 5000),
      '40000 open brackets': rep('[', 40000),
      '5000 x *b': rep('x *b', 5000)
    };
    Object.keys(evil).forEach(function (name) {
      var box = document.createElement('div'), err = '', t0 = performance.now();
      try { window.labmillMarkdown.render(box, evil[name]); } catch (e) { err = String(e); }
      var ms = performance.now() - t0;
      check(!err && ms < 200, 'pathological Markdown (' + name + ') renders in under 200 ms without throwing (' + Math.round(ms) + ' ms' + (err ? ', ' + err : '') + ')');
      var t1 = performance.now(), before = hooks.renders;
      hooks.reply(evil[name]);
      check(hooks.renders === before + 1 && performance.now() - t1 < 200, 'the chat renders the same reply (' + name + ') in under 200 ms');
    });
    check(!document.getElementById('send').disabled && !document.getElementById('input').disabled && document.getElementById('cancel').hidden,
      'the chat is not left busy after the pathological replies');

    // 5. Tool output is collapsed to one line.
    var tool = logEl.querySelector('details.tool');
    var sum = tool && tool.querySelector('summary');
    check(!!tool && !tool.open, 'the tool output is collapsed');
    check(!!sum && sum.textContent === 'load_skill · 40 lines', 'the tool summary is "load_skill · 40 lines" (got ' + (sum ? sum.textContent : 'none') + ')');
    var tpre = tool && tool.querySelector('pre');
    check(!!tpre && tpre.textContent.indexOf('reference text line') !== -1, 'the full tool output is inside the collapsed item');

    // 6. The job table never wraps its cells.
    var th = document.querySelector('#jobs-table th');
    check(!!th && getComputedStyle(th).whiteSpace === 'nowrap', 'job table cells do not wrap');

    // 6b. The divider between chat and jobs (M51.1): role separator, the
    // saved width (30 %, seeded by Go) applied, keys and a drag change the
    // jobs column, a double-click resets, the value round-trips through
    // SettingsAPI, and the narrow layout hides it.
    var dv = document.getElementById('divider'), jobsEl = document.querySelector('aside.jobs');
    check(!!dv && dv.getAttribute('role') === 'separator' && dv.getAttribute('aria-orientation') === 'vertical' && dv.tabIndex === 0 &&
      dv.previousElementSibling === document.querySelector('.chat') && dv.nextElementSibling === jobsEl, 'the divider is a focusable vertical separator between chat and jobs');
    var mw = document.getElementById('main').getBoundingClientRect().width;
    await waitFor('the saved jobs width', function () { return Math.abs(hooks.jobsWidth() - 0.30) < 0.001; });
    check(dv.getAttribute('aria-valuenow') === '30' && Math.abs(jobsEl.getBoundingClientRect().width / mw - 0.30) < 0.02,
      'the saved jobs width (30 %) was applied at start (aria-valuenow ' + dv.getAttribute('aria-valuenow') + ', ' + Math.round(jobsEl.getBoundingClientRect().width) + ' of ' + Math.round(mw) + ' px)');
    var w0 = jobsEl.getBoundingClientRect().width;
    key(dv, 'ArrowLeft');
    var w1 = jobsEl.getBoundingClientRect().width;
    check(w1 > w0 + 4 && dv.getAttribute('aria-valuenow') === '32', 'Left widens the jobs panel by 2 % (' + Math.round(w0) + ' -> ' + Math.round(w1) + ' px)');
    var mr = document.getElementById('main').getBoundingClientRect();
    function ptr(type, x) { dv.dispatchEvent(new PointerEvent(type, { clientX: x, clientY: mr.top + 50, button: 0, buttons: type === 'pointerup' ? 0 : 1, pointerId: 77, bubbles: true, cancelable: true })); }
    ptr('pointerdown', mr.right - 0.3 * mr.width);
    ptr('pointermove', mr.right - 0.5 * mr.width);
    ptr('pointerup', mr.right - 0.5 * mr.width);
    var w2 = jobsEl.getBoundingClientRect().width;
    check(w2 > w1 + 20 && Math.abs(w2 / mr.width - 0.5) < 0.03, 'dragging the divider left widened the jobs panel to about 50 % (' + Math.round(w2) + ' px)');
    key(dv, 'Home');
    check(dv.getAttribute('aria-valuenow') === dv.getAttribute('aria-valuemin') && jobsEl.getBoundingClientRect().width < w2 - 20 && jobsEl.getBoundingClientRect().width >= 14 * 16 - 2,
      'Home narrows the jobs panel to its minimum (aria-valuenow ' + dv.getAttribute('aria-valuenow') + ' = valuemin), not below 14rem');
    var wHome = jobsEl.getBoundingClientRect().width;
    key(dv, 'End');
    check(dv.getAttribute('aria-valuenow') === dv.getAttribute('aria-valuemax') && jobsEl.getBoundingClientRect().width > wHome + 20 && mr.width - jobsEl.getBoundingClientRect().width >= 24 * 16 - 2,
      'End widens it to the maximum (aria-valuenow ' + dv.getAttribute('aria-valuenow') + ' = valuemax), the chat keeps its minimum');
    // A narrow moment must not shrink the choice: squeeze the main area,
    // then give it back; the panel returns to the width the user chose.
    var wEnd = jobsEl.getBoundingClientRect().width, wantEnd = hooks.jobsWant();
    var mainNode = document.getElementById('main');
    mainNode.style.maxWidth = '500px';
    window.dispatchEvent(new Event('resize'));
    var wSq = jobsEl.getBoundingClientRect().width;
    mainNode.style.maxWidth = '';
    window.dispatchEvent(new Event('resize'));
    check(wSq < wEnd - 20 && Math.abs(jobsEl.getBoundingClientRect().width - wEnd) < 2 && hooks.jobsWant() === wantEnd,
      'squeezing the window and restoring it brought the panel back to the chosen width (' + Math.round(wEnd) + ' -> ' + Math.round(wSq) + ' -> ' + Math.round(jobsEl.getBoundingClientRect().width) + ' px)');
    key(dv, 'ArrowLeft'); key(dv, 'ArrowRight');
    await sleep(700); // the save is debounced
    var sapi = window.go.main.SettingsAPI;
    var saved = await sapi.JobsWidth();
    check(Math.abs(saved - hooks.jobsWant()) < 0.001 && saved > 0.4, 'the width round-tripped through SettingsAPI (' + saved + ')');
    dv.dispatchEvent(new MouseEvent('dblclick', { bubbles: true }));
    check(Math.abs(hooks.jobsWidth() - 0.40) < 0.001 && dv.getAttribute('aria-valuenow') === '40', 'a double-click resets the width to 40 %');
    await sleep(700);
    check(await sapi.JobsWidth() === 0, 'the reset saved 0 (the default)');
    var narrow = mediaRules('max-width: 760px').filter(function (r) { return r.selectorText && r.selectorText.indexOf('.divider') !== -1 && r.style.display === 'none'; });
    check(narrow.length === 1, 'the narrow layout (max-width: 760px) hides the divider');

    // 7. The job notice sent during the first turn: a system line, markup
    // as text, above the reply that was still open.
    var sys = logEl.querySelector('.msg.system');
    check(hooks.notices === 1 && !!sys, 'the job notice rendered as a system line (notices=' + hooks.notices + ')');
    check(!!sys && sys.textContent.indexOf('Job #12 (selftest-train) on selftest-cluster finished.') === 0 && sys.textContent.indexOf('<img') !== -1,
      'the notice text is shown literally');
    check(document.querySelector('img') === null && window.__pwned === undefined, 'the notice created no element');
    var after = sys && sys.nextElementSibling;
    check(!!after && after.classList.contains('assistant') && after.textContent.indexOf('Final words') !== -1,
      'the notice went above the reply that was still streaming, which stayed whole');

    // 8. The prompt stays available while a reply runs: Send queues one
    // message (a newer one replaces it), Cancel keeps it, and it is sent when
    // the reply ends.
    var input = document.getElementById('input'), sendBtn = document.getElementById('send');
    function typeSend(text, viaEnter) {
      input.value = text;
      input.dispatchEvent(new Event('input', { bubbles: true }));
      if (viaEnter) key(input, 'Enter'); else sendBtn.click();
    }
    var doneBefore = hooks.done;
    typeSend('slow', false);
    await waitFor('the slow turn to start', function () { return !document.getElementById('cancel').hidden; });
    check(!input.disabled && !sendBtn.disabled, 'the input and Send stay enabled while a reply runs');
    check(sendBtn.textContent === 'Queue', 'Send reads "Queue" while a reply runs (got ' + sendBtn.textContent + ')');
    await waitFor('the progress row', function () { return !document.getElementById('xfer').hidden; });
    check(document.getElementById('xfer-text').textContent.indexOf('Transferred: 1.0 MiB / 4.0 MiB, 25%, 45.0 MiB/s, ETA 1m02s') === 0 &&
      document.getElementById('xfer-bar').value === 25, 'the progress row shows the rclone-style line and a 25% bar');
    var xc = logEl.querySelector('.msg.xfercmd');
    check(!!xc && xc.textContent.indexOf('rclone-lib: copy') === 0 && xc.textContent.indexOf('<b>x</b>') !== -1 && !xc.querySelector('b'),
      'the rclone-lib line is shown as text at the start of the transfer');
    typeSend('queued one', false);
    await waitFor('the queued line', function () { return !document.getElementById('queued').hidden; });
    check(document.getElementById('queued-text').textContent === 'queued one' && input.value === '', 'Send while busy queues the message and clears the input');
    typeSend('queued two', true);
    await waitFor('the queued line to change', function () { return document.getElementById('queued-text').textContent === 'queued two'; });
    check(true, 'a second message (Enter) replaces the queued one');
    function userMsgs(t) {
      return Array.prototype.filter.call(logEl.querySelectorAll('.msg.user'), function (m) { return m.textContent === t; }).length;
    }
    check(userMsgs('queued one') === 0 && userMsgs('queued two') === 0, 'queued messages are not in the chat yet');
    // A job button while a message is queued keeps that message and shows a notice.
    await waitFor('a job button', function () { return !!document.querySelector('#jobs-body button[data-job="12"][data-act="status"]'); });
    document.querySelector('#jobs-body button[data-job="12"][data-act="status"]').click();
    await waitFor('the queued-message notice', function () { return logEl.textContent.indexOf('Your queued message was kept; try the button again when Labmill is free') !== -1; });
    check(document.getElementById('queued-text').textContent === 'queued two', 'a job button keeps the queued message (and says so)');
    document.getElementById('cancel').click();
    await waitFor('the progress row to go', function () { return document.getElementById('xfer').hidden; });
    check(hooks.xferShown >= 1 && hooks.xferEnds >= 1, 'the progress row disappeared when the transfer ended');
    await waitFor('the queued message to be sent', function () { return userMsgs('queued two') === 1; });
    check(document.getElementById('queued').hidden, 'the queued line is gone once its message was sent');
    await waitFor('the queued turn to end', function () { return hooks.done >= doneBefore + 2 && document.getElementById('cancel').hidden; });
    check(userMsgs('queued one') === 0, 'the replaced message was never sent');
    check(logEl.textContent.indexOf('Got: queued two') !== -1, 'the queued turn ran');

    // 9. Job buttons: the right ones per job, real labelled buttons, and a
    // click sends the plain chat message.
    await waitFor('the job buttons', function () { return document.querySelectorAll('#jobs-body button').length > 0; });
    function jobButtons(id) {
      return Array.prototype.filter.call(document.querySelectorAll('#jobs-body button'), function (b) { return b.dataset.job === String(id); })
        .map(function (b) { return b.textContent; }).join(',');
    }
    check(jobButtons(12) === 'Status,Logs,Cancel,Terminal here', 'the running job has Status, Logs, Cancel, Terminal here (got ' + jobButtons(12) + ')');
    check(jobButtons(13) === 'Status,Logs,Fetch,Terminal here', 'the completed job has Status, Logs, Fetch, Terminal here (got ' + jobButtons(13) + ')');
    check(jobButtons(14) === '', 'the draft has no buttons (got ' + jobButtons(14) + ')');
    var stBtn = document.querySelector('#jobs-body button[data-job="12"][data-act="status"]');
    check(!!stBtn && stBtn.type === 'button' && stBtn.tabIndex === 0 && /Status of job #12/.test(stBtn.getAttribute('aria-label') || ''),
      'job buttons are keyboard-reachable buttons with a label naming the job');
    doneBefore = hooks.done;
    stBtn.click();
    await waitFor('the job button message', function () { return userMsgs('Show the status of job #12') === 1; });
    await waitFor('the job button turn to end', function () { return hooks.done >= doneBefore + 1; });
    check(true, 'the Status button sent "Show the status of job #12"');

    // 10. Folders (M20, M24): the + button at the upper left of the input
    // box opens the folder menu by keyboard (data first, then code); "Add
    // data folder…" opens the (stub) folder dialog and a Data chip appears
    // (shortened, full path in the tooltip); the next message carries it
    // with the data-only sentence (checked in Go); then a code folder; the
    // × on a chip removes it; the menu offers recent folders and "Remove";
    // a folder dropped onto the input box offers "Add as data / code
    // folder" and is added as data; a drop elsewhere or of a hidden folder
    // does not set anything.
    var composer = document.getElementById('composer'), plus = document.getElementById('folder-add'), menu = document.getElementById('folder-menu');
    function chipEl(kind) { return document.querySelector('#folder-chips .chip[data-kind="' + kind + '"]'); }
    function chipText(kind) { var c = chipEl(kind); return c ? c.querySelector('.chip-text').textContent : ''; }
    function menuLabels() { return Array.prototype.map.call(menu.querySelectorAll('[role=menuitem]'), function (b) { return b.textContent; }); }
    function menuItemBy(fn) { return Array.prototype.filter.call(menu.querySelectorAll('[role=menuitem]'), fn)[0] || null; }
    check(!document.getElementById('folder-code') && !document.getElementById('folder-data') && !document.getElementById('folders'),
      'the old folder buttons above the chat are gone');
    check(!!plus && composer.contains(plus) && plus.getAttribute('aria-label') === 'Add folder' && plus.getAttribute('aria-haspopup') === 'menu',
      'the + button is inside the input box with aria-label "Add folder" and a menu popup');
    var pr = plus.getBoundingClientRect(), cr = composer.getBoundingClientRect(), ir = document.getElementById('input').getBoundingClientRect();
    check(pr.left - cr.left < 20 && pr.top - cr.top < 20 && pr.right <= ir.left + 1,
      'the + button sits at the upper left of the input box, left of the text');
    check(getComputedStyle(composer).getPropertyValue('--wails-drop-target').trim() === 'drop' &&
      getComputedStyle(document.getElementById('log')).getPropertyValue('--wails-drop-target').trim() === '',
      'only the input box is a drop target');
    check(document.getElementById('folder-chips').hidden, 'no folder chips before a folder is chosen');

    key(plus, 'Enter');
    check(!menu.hidden && plus.getAttribute('aria-expanded') === 'true', 'Enter on the + button opens the menu');
    var labels = menuLabels();
    check(labels[0] === 'Add data folder…' && labels[1] === 'Add code folder…' && labels.length === 2,
      'the menu lists "Add data folder…" first, then "Add code folder…" (got ' + JSON.stringify(labels) + ')');
    check(document.activeElement === menu.querySelectorAll('[role=menuitem]')[0], 'the first menu item has the focus');
    key(document.activeElement, 'ArrowDown');
    check(document.activeElement.textContent === 'Add code folder…', 'ArrowDown moves to "Add code folder…"');
    key(document.activeElement, 'ArrowDown');
    check(document.activeElement.textContent === 'Add data folder…', 'ArrowDown wraps to the first item');
    key(document.activeElement, 'Escape');
    check(menu.hidden && document.activeElement === plus && plus.getAttribute('aria-expanded') === 'false', 'Escape closes the menu and returns the focus to +');

    var rendersBefore = hooks.folderRenders;
    key(plus, ' ');
    check(!menu.hidden && document.activeElement.textContent === 'Add data folder…', 'Space on the + button opens the menu');
    key(document.activeElement, 'Enter');
    await waitFor('the Data chip', function () { return !!chipEl('data'); });
    check(menu.hidden, 'choosing a menu item closes the menu');
    var st = await api.Folders();
    var dataChip = chipEl('data');
    check(hooks.folderRenders > rendersBefore && st.data.path !== '' && chipText('data') === 'Data: ' + st.data.short && /data$/.test(st.data.short) &&
      st.data.short.length < st.data.path.length, 'the chosen data folder shows as a chip "' + chipText('data') + '"');
    check(dataChip.querySelector('.chip-text').title.indexOf(st.data.path) !== -1, 'the chip tooltip has the full path');
    var dataX = dataChip.querySelector('button.chip-x');
    check(!!dataX && dataX.getAttribute('aria-label') === 'Remove the data folder' && !chipEl('code'), 'the Data chip has an × labelled "Remove the data folder"; no Code chip');
    doneBefore = hooks.done;
    typeSend('data only check', false);
    await waitFor('the data-only turn to end', function () { return hooks.done >= doneBefore + 1 && document.getElementById('cancel').hidden; });

    key(plus, 'ArrowDown');
    var addCode = menuItemBy(function (b) { return b.textContent === 'Add code folder…'; });
    check(!!addCode && !!menuItemBy(function (b) { return b.textContent === 'Remove data folder'; }), 'with a data folder set the menu offers "Remove data folder"');
    addCode.focus();
    key(addCode, 'Enter');
    await waitFor('the Code chip', function () { return !!chipEl('code'); });
    st = await api.Folders();
    var chips = document.querySelectorAll('#folder-chips .chip');
    check(chips.length === 2 && chips[0].dataset.kind === 'data' && chips[1].dataset.kind === 'code' && chipText('code') === 'Code: ' + st.code.short,
      'the chips show data first, then code (' + chipText('data') + ' | ' + chipText('code') + ')');
    var ar = document.getElementById('folder-chips').getBoundingClientRect();
    check(composer.contains(document.getElementById('folder-chips')) && ar.bottom <= document.getElementById('input').getBoundingClientRect().top + 1,
      'the chips are inside the input box, above the text');
    doneBefore = hooks.done;
    typeSend('workspace check', false);
    await waitFor('the workspace turn to end', function () { return hooks.done >= doneBefore + 1 && document.getElementById('cancel').hidden; });

    chipEl('code').querySelector('button.chip-x').click();
    await waitFor('the Code chip to go', function () { return !chipEl('code'); });
    check(!!chipEl('data'), 'the × on the Code chip removed only the code folder');
    plus.click();
    var recentCode = menuItemBy(function (b) { return b.dataset.action === 'recent' && b.dataset.kind === 'code'; });
    check(!menu.hidden && !!recentCode && recentCode.textContent === 'Code: ' + st.code.short && recentCode.title === st.code.path &&
      !menuItemBy(function (b) { return b.dataset.action === 'recent' && b.dataset.kind === 'data'; }),
      'the menu offers the recent code folder labelled with its kind (and not the data folder in use)');
    check(!!menu.querySelector('[role=separator]'), 'a separator divides the Add items from the recent folders');
    recentCode.click();
    await waitFor('the recent code folder to be set', function () { return chipText('code') === 'Code: ' + st.code.short; });
    check(menu.hidden, 'choosing a recent folder sets it and closes the menu');
    key(plus, 'Enter');
    var rmCode = menuItemBy(function (b) { return b.textContent === 'Remove code folder'; });
    check(!!rmCode, 'the menu offers "Remove code folder"');
    if (rmCode) rmCode.click();
    await waitFor('the code folder to clear again', function () { return !chipEl('code'); });
    chipEl('data').querySelector('button.chip-x').click();
    await waitFor('the Data chip to go', function () { return !chipEl('data'); });
    check(document.getElementById('folder-chips').hidden && document.activeElement === plus, 'with both removed no chips remain and the focus is on +');

    function centre(el) { var r = el.getBoundingClientRect(); return [Math.round(r.left + r.width / 2), Math.round(r.top + r.height / 2)]; }
    var dataPath = st.data.path, hiddenPath = st.data.path.replace(/data$/, '.hidden');
    var choice = document.getElementById('drop-choice');
    var lc = centre(document.getElementById('log'));
    check(hooks.drop(lc[0], lc[1], [dataPath]) === null && choice.hidden, 'a folder dropped on the conversation is ignored');
    var ic = centre(document.getElementById('input'));
    check(hooks.drop(ic[0], ic[1], [hiddenPath]) === true && !choice.hidden, 'a folder dropped on the input box offers a choice');
    var cbs = Array.prototype.map.call(choice.querySelectorAll('button[data-drop-kind]'), function (b) { return b.textContent; });
    check(cbs[0] === 'Add as data folder' && cbs[1] === 'Add as code folder' && document.activeElement === document.getElementById('drop-data'),
      'the choice is "Add as data folder" (focused) / "Add as code folder" (got ' + JSON.stringify(cbs) + ')');
    check(document.getElementById('drop-choice-name').textContent === '.hidden' && document.querySelector('img') === null, 'the choice names the dropped folder as text');
    document.getElementById('drop-data').click();
    await waitFor('the refusal', function () { return !document.getElementById('folder-error').hidden; });
    check(choice.hidden && !chipEl('data') && /private|hidden/.test(document.getElementById('folder-error').textContent),
      'a hidden folder dropped and added as data is refused with a message (' + document.getElementById('folder-error').textContent + ')');
    hooks.drop(ic[0], ic[1], [dataPath]);
    key(document.activeElement, 'Escape');
    check(choice.hidden && !chipEl('data'), 'Escape cancels the drop choice and adds nothing');
    hooks.drop(ic[0], ic[1], [dataPath]);
    document.getElementById('drop-data').click();
    await waitFor('the dropped data folder', function () { return !!chipEl('data'); });
    check(chipText('data') === 'Data: ' + st.data.short && document.getElementById('folder-error').hidden && choice.hidden,
      'a folder dropped on the input box and added as data sets the data folder');

    // 11. Open folder: after a successful fetch_results the chat offers it,
    // and so does the fetched job in the job list (Go checks the opener).
    doneBefore = hooks.done;
    typeSend('fetch check', false);
    await waitFor('the fetch turn to end', function () { return hooks.done >= doneBefore + 1 && document.getElementById('cancel').hidden; });
    var ob = logEl.querySelector('button.open-folder');
    check(!!ob && ob.textContent === 'Open folder' && ob.dataset.job === '15', 'the chat shows an "Open folder" button after fetch_results');
    if (ob) ob.click();
    await waitFor('the fetched job button', function () { return document.querySelector('#jobs-body button[data-job="15"][data-act="open"]'); });
    check(jobButtons(15) === 'Status,Logs,Open folder,Terminal here', 'the fetched job has Status, Logs, Open folder, Terminal here (got ' + jobButtons(15) + ')');
    document.querySelector('#jobs-body button[data-job="15"][data-act="open"]').click();
    await sleep(200);
    check(hooks.done === doneBefore + 1, 'Open folder sent no chat message');

    // 11b. The to-do list (M28): rendered from Labmill:todos with the right
    // state per item (textContent only), updated in place, collapsible with
    // the keyboard, hidden once all done and one more turn ended.
    var tdp = document.getElementById('todos'), tdl = document.getElementById('todos-list'), tdt = document.getElementById('todos-toggle');
    function tdItems() { return Array.prototype.map.call(tdl.children, function (li) { return li.getAttribute('data-status') + '|' + li.children[0].textContent + '|' + li.children[1].textContent; }); }
    check(tdp.hidden, 'the to-do panel is hidden while the list is empty');
    doneBefore = hooks.done;
    typeSend('todos check', false);
    await waitFor('the to-do panel', function () { return !tdp.hidden && hooks.done >= doneBefore + 1; });
    var tdFirst = tdl.children[1];
    var tdNow = tdItems();
    check(tdNow.length === 3 && tdNow[0] === 'completed|\u2611|Scan the code' && tdNow[2] === 'pending|\u2610|Submit the job' &&
      tdNow[1].indexOf('in_progress|\u25D0|Uploading the data') === 0, 'the to-do panel shows ☑ done, ◐ in progress (with its active form) and ☐ pending (got ' + tdNow.join(' ; ') + ')');
    check(document.getElementById('todos-title').textContent === 'To-do 1/3' && !tdl.querySelector('img') && window.__pwned !== 1,
      'the to-do title counts the done items and markup stays text');
    // Updated in place: the same <li> elements are reused.
    doneBefore = hooks.done;
    typeSend('todos check 2', false);
    await waitFor('the to-do update', function () { return hooks.done >= doneBefore + 1 && tdl.children[2].getAttribute('data-status') === 'in_progress'; });
    tdNow = tdItems();
    check(tdl.children[1] === tdFirst && tdNow[1] === 'completed|\u2611|<img src=x onerror="window.__pwned=1">Upload the data' && !tdl.querySelector('img') && tdNow[2] === 'in_progress|\u25D0|Submitting the job',
      'the to-do list updates in place (got ' + tdNow.join(' ; ') + ')');
    // Collapse and expand with the keyboard (a button: Space/Enter click it).
    tdt.focus();
    check(document.activeElement === tdt && tdt.tagName === 'BUTTON' && tdt.getAttribute('aria-expanded') === 'true' && !tdl.hidden, 'the to-do toggle is a focusable button, expanded');
    tdt.click();
    check(tdt.getAttribute('aria-expanded') === 'false' && tdl.hidden && !tdp.hidden, 'collapsing hides the items but keeps the title');
    tdt.click();
    check(tdt.getAttribute('aria-expanded') === 'true' && !tdl.hidden, 'expanding shows the items again');
    // All done stays for the turn that finished it, goes after the next.
    doneBefore = hooks.done;
    typeSend('todos check 3', false);
    await waitFor('the finished list', function () { return hooks.done >= doneBefore + 1 && tdl.children[2].getAttribute('data-status') === 'completed'; });
    check(!tdp.hidden, 'a finished list is still shown right after the turn that finished it');
    doneBefore = hooks.done;
    typeSend('plain turn', false);
    await waitFor('the next turn to end', function () { return hooks.done >= doneBefore + 1; });
    check(tdp.hidden, 'the finished list is hidden after the next turn');
    check(document.getElementById('todos-live').textContent === '', 'the live region is empty once the list is gone');
    // A list with an item in progress: announced politely, and hidden once
    // two turns ended without an update_todos call.
    doneBefore = hooks.done;
    typeSend('todos check', false);
    await waitFor('a new list', function () { return !tdp.hidden && hooks.done >= doneBefore + 1; });
    var tdLive = document.getElementById('todos-live');
    check(tdLive.getAttribute('role') === 'status' && tdLive.getAttribute('aria-live') === 'polite' &&
      tdLive.textContent === 'To-do 1 of 3 done. Now: Uploading the data', 'a polite live region announces the item in progress (got ' + tdLive.textContent + ')');
    for (var tn = 0; tn < 2; tn++) {
      check(!tdp.hidden, 'the list is still shown after ' + tn + ' turn(s) without update_todos');
      doneBefore = hooks.done;
      typeSend('plain turn', false);
      await waitFor('a turn without update_todos', function () { return hooks.done >= doneBefore + 1; });
    }
    check(tdp.hidden && !tdl.querySelector('[data-status=in_progress]'), 'two turns without update_todos hide the list');
    // A turn that ends with an error hides the list at once.
    doneBefore = hooks.done;
    typeSend('todos error', false);
    await waitFor('the failing turn', function () { return hooks.done >= doneBefore + 1; });
    check(tdp.hidden && !tdl.querySelector('[data-status=in_progress]'), 'a failed turn hides the list (no stale in-progress item)');

    // 12. The "Working…" row (M26): within 100 ms of Send, before Go says
    // anything; then the steps from Go, the elapsed time, Cancel available;
    // gone when the reply text starts and after the turn.
    var w = document.getElementById('working'), wt = document.getElementById('working-text'), wtime = document.getElementById('working-time');
    check(w.hidden, 'no "Working…" row while idle');
    check(!!w.querySelector('.hourglass[aria-hidden="true"]') && wt.getAttribute('aria-live') === 'polite',
      'the row has an hourglass and an aria-live="polite" status text');
    hooks.elapsedAfter = 1000;
    doneBefore = hooks.done;
    var t0 = performance.now();
    typeSend('working check', true);
    var shownAfter = performance.now() - t0;
    check(!w.hidden && wt.textContent === 'Working…' && shownAfter < 100, '"Working…" shows right after Send (' + Math.round(shownAfter) + ' ms)');
    var wa = w.querySelector('.hourglass'), anim = wa ? getComputedStyle(wa).animationName : '';
    check(reduced ? anim === 'none' : anim !== 'none', 'the row hourglass is animated (or still with reduced motion): ' + anim);
    await waitFor('the connecting step', function () { return wt.textContent === 'Connecting to selftest.example…'; });
    check(!w.hidden && !document.getElementById('cancel').hidden, 'the step from Go shows, and Cancel is available');
    check(logEl.textContent.indexOf('Working check done.') === -1, 'the row shows before the first token');
    await waitFor('the elapsed time', function () { return /^\d+ s$/.test(wtime.textContent); });
    check(true, 'the elapsed time shows after a while (' + wtime.textContent + ')');
    await waitFor('the Claude step', function () { return wt.textContent === 'Waiting for Claude…'; });
    check(!w.hidden && logEl.textContent.indexOf('Working check done.') === -1, '"Waiting for Claude…" shows before the first token');
    await waitFor('the reply text', function () { return logEl.textContent.indexOf('Working check done.') !== -1; });
    check(w.hidden, 'the row is gone once the reply text starts');
    await waitFor('the working turn to end', function () { return hooks.done >= doneBefore + 1 && document.getElementById('cancel').hidden; });
    check(w.hidden && wtime.textContent === '', 'the row is gone after the turn');
    hooks.elapsedAfter = 0;

    // 13. Copy buttons (M25): every code block in the chat has one, and it
    // copies the block's text through the clipboard API (stubbed here).
    var copied = [];
    var hadClip = Object.prototype.hasOwnProperty.call(navigator, 'clipboard');
    var ownClip = hadClip ? Object.getOwnPropertyDescriptor(navigator, 'clipboard') : null;
    Object.defineProperty(navigator, 'clipboard', { configurable: true,
      value: { writeText: function (t) { copied.push(t); return Promise.resolve(); } } });
    var codeBlocks = logEl.querySelectorAll('.msg.assistant pre.md-code');
    var copyBtns = logEl.querySelectorAll('.msg.assistant .md-codewrap > button.copy');
    check(codeBlocks.length > 0 && copyBtns.length === codeBlocks.length,
      'every code block has a Copy button (' + copyBtns.length + ' buttons, ' + codeBlocks.length + ' blocks)');
    var cb0 = copyBtns[0];
    check(!!cb0 && cb0.type === 'button' && cb0.getAttribute('aria-label') === 'Copy' && cb0.textContent === 'Copy' && !cb0.hasAttribute('onclick'),
      'the code block Copy button is a real button with aria-label "Copy" and no inline handler');
    check(!!cb0 && cb0.parentNode.querySelector('pre').textContent.indexOf('Copy') === -1, 'the button is not part of the copied block text');
    if (cb0) cb0.click();
    await waitFor('the code block copy', function () { return copied.length >= 1; });
    check(copied[0] === 'print("<b>not bold</b>")', 'Copy put the code block text on the clipboard (got ' + JSON.stringify(copied[0]) + ')');

    // 14b. SSH key setup of a key-only host (M38.6): a create question with
    // its own buttons, a masked passphrase input, then the public key as text
    // with Copy and Continue. The passphrase reaches Go (checked there) and
    // never the page.
    var KEY_LINE = 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAISelfTestOnlyNotARealKey000000000000000000 labmill-selftest';
    doneBefore = hooks.done;
    typeSend('key setup check', false);
    await waitFor('the key create dialog', function () { return dialogOpen() && title() === 'SSH key needed for login.selftest.example'; });
    check(!!button('Create key') && !!button('Not now') && document.getElementById('dlg-desc').textContent.indexOf('accepts only SSH keys') !== -1,
      'the key dialog offers Create key and Not now and says the host accepts only keys');
    await sleep(600);
    button('Create key').click();
    await waitFor('the passphrase dialog', function () { return dialogOpen() && title() === 'Sign in to login.selftest.example' && !!document.querySelector('#dialog input'); });
    var passIn = document.querySelector('#dialog input');
    check(passIn.type === 'password' && passIn.autocomplete === 'off', 'the new-key passphrase input is masked');
    await sleep(600);
    passIn.value = 'SELFPASSPHRASE';
    passIn.dispatchEvent(new Event('input', { bubbles: true }));
    key(passIn, 'Enter');
    await waitFor('the key upload dialog', function () { return dialogOpen() && title() === 'Register your SSH key with login.selftest.example'; });
    var pubEl = document.getElementById('dlg-pubkey'), copyK = document.getElementById('dlg-copy');
    check(!!pubEl && pubEl.tagName === 'CODE' && pubEl.textContent === KEY_LINE, 'the public key is shown as text in its own field');
    check(document.getElementById('dlg-desc').textContent.indexOf('https://registry.selftest.example/keys') !== -1, 'the site\'s upload instructions are shown');
    check(!!copyK && copyK.type === 'button' && copyK.textContent === 'Copy' && document.activeElement === copyK && !!button("I've added it, try again") && !!button('Cancel'),
      'the upload dialog has Copy (focused), Continue and Cancel');
    await sleep(600);
    var copiedBefore = copied.length;
    copyK.click();
    await waitFor('the key copy', function () { return copied.length > copiedBefore && hooks.keyCopies >= 1; });
    check(copied[copied.length - 1] === KEY_LINE, 'Copy put the public key on the clipboard');
    button("I've added it, try again").click();
    await waitFor('the key copy dialog', function () { return dialogOpen() && title() === 'Copy a private key to this computer?'; });
    check(!!button('Copy key') && !!button('No, create a new key') && document.getElementById('dlg-desc').textContent.indexOf('copy of a private key') !== -1,
      'the key copy dialog names the risk and offers Copy key and No');
    await sleep(600);
    button('No, create a new key').click();
    await waitFor('the key setup turn to end', function () { return hooks.done >= doneBefore + 1 && document.getElementById('cancel').hidden; });
    check(!dialogOpen() && logEl.textContent.indexOf('Key setup done.') !== -1, 'the reply after the key setup reached the chat');
    check(document.body.textContent.indexOf('SELFPASSPHRASE') === -1 && logEl.textContent.indexOf('SELFPASSPHRASE') === -1, 'the passphrase never appears in the page');
    if (ownClip) Object.defineProperty(navigator, 'clipboard', ownClip); else delete navigator.clipboard;

    // 14c. The 2FA notice goes away (M43.1): it shows while the login waits and
    // is gone as soon as the login ends, while the turn is still running.
    var progEl = document.getElementById('progress');
    doneBefore = hooks.done;
    typeSend('login notice check', false);
    await waitFor('the 2FA notice', function () { return progEl.textContent.indexOf('Approve the sign-in request') !== -1; });
    check(!document.getElementById('cancel').hidden, 'the 2FA notice shows while the login waits');
    await waitFor('the 2FA notice to clear', function () { return progEl.textContent === ''; });
    check(!document.getElementById('cancel').hidden && logEl.textContent.indexOf('Login notice done.') === -1,
      'the 2FA notice was cleared when the login ended, before the turn did');
    await waitFor('the login notice turn to end', function () { return hooks.done >= doneBefore + 1 && document.getElementById('cancel').hidden; });
    check(progEl.textContent === '', 'no 2FA notice is left after the turn');

    // 14d. "Approve everything automatically" (M43.2): the checkbox beside Send
    // shows the banner; a plain proposal is approved at once with an
    // "Auto-approved: ..." chat line; a read of a secret file still asks.
    var aaBox = document.getElementById('autoapprove'), aaBanner = document.getElementById('autoapprove-banner');
    check(!!aaBox && !aaBox.checked && aaBanner.hidden, 'Approve everything automatically is off by default and the banner is hidden');
    check(!!aaBox && document.querySelector('label[for=autoapprove]').textContent.trim() === 'Approve everything automatically', 'the checkbox has its label');
    aaBox.click();
    await waitFor('the auto-approve banner', function () { return !aaBanner.hidden; });
    check(aaBanner.textContent.indexOf('Auto-approve is on') !== -1 && !!document.getElementById('autoapprove-off') && aaBox.checked,
      'the banner says auto-approve is on and offers Turn off');
    doneBefore = hooks.done;
    typeSend('auto approve check', false);
    await waitFor('the secret read dialog', function () { return dialogOpen() && title().indexOf('Approve') === 0; });
    check(logEl.textContent.indexOf('Auto-approved: Upload 2 files to selftest:/scratch') !== -1, 'the plain proposal was approved at once and the chat says so');
    check(document.getElementById('dlg-desc').textContent.indexOf('id_ed25519') !== -1 || document.getElementById('dialog').textContent.indexOf('id_ed25519') !== -1,
      'the secret file read still shows its approval dialog');
    await sleep(600);
    button('Approve').click();
    await waitFor('the auto-approve turn to end', function () { return hooks.done >= doneBefore + 1 && document.getElementById('cancel').hidden; });
    check(logEl.textContent.indexOf('Auto-approved: Run on selftest-hpc') === -1, 'the secret read was not auto-approved');
    document.getElementById('autoapprove-off').click();
    await waitFor('the banner to go', function () { return aaBanner.hidden; });
    check(!aaBox.checked, 'Turn off hides the banner and clears the checkbox');

    // 14e. "Download results automatically" (M60.1): the checkbox beside the
    // approve-all one, off by default, saved when clicked, and a job notice
    // that carries a job ID offers "Open folder".
    var adBox = document.getElementById('autodownload');
    check(!!adBox && !adBox.checked && document.querySelector('label[for=autodownload]').textContent.trim() === 'Download results automatically',
      'Download results automatically is off by default and has its label');
    check(!!adBox && adBox.parentNode.parentNode === aaBox.parentNode.parentNode, 'it sits next to Approve everything automatically');
    adBox.click();
    await sleep(300);
    check(adBox.checked, 'the checkbox stays on after the click (saved)');
    adBox.click();
    await sleep(300);
    check(!adBox.checked, 'and goes off again');

    // 15. The Settings page (M29): the gear opens it, the section list is
    // keyboard navigable, a cluster field with an invalid value shows an
    // inline error, the fixed value saves (Go checks the file, its comments
    // and the backup), folders list data first, nothing on the network until
    // Check is clicked, and the diagnostics hold nothing secret-looking.
    var sh = window.__labmillSettingsHooks;
    var gear = document.getElementById('settings-open');
    check(!!gear && !gear.hidden && gear.getAttribute('aria-label') === 'Settings' && gear.type === 'button', 'the header has a Settings (gear) button');
    gear.click();
    await waitFor('the Settings page', function () { return !document.getElementById('settings').hidden && document.querySelectorAll('#settings-nav [role=tab]').length === 7; });
    check(document.getElementById('main').hidden, 'the chat is hidden while Settings is open');
    var tabNames = Array.prototype.map.call(document.querySelectorAll('#settings-nav [role=tab]'), function (b) { return b.textContent; });
    check(tabNames.join('|') === 'Institution|Clusters|SSH connections|Folders|App|Labmill account|About & diagnostics', 'the section list has the seven sections in order (' + tabNames.join(', ') + ')');
    function selTab() { return document.querySelector('#settings-nav [aria-selected=true]'); }
    function paneTitle() { var h = document.querySelector('#settings-pane .settings-h'); return h ? h.textContent : ''; }
    function statusText() { var s = document.getElementById('settings-status'); return s ? s.textContent : ''; }
    check(document.activeElement === selTab() && selTab().textContent === 'Institution' && paneTitle() === 'Institution', 'the Institution section is open and its tab has the focus');
    var tabsInOrder = document.querySelectorAll('#settings-nav [role=tab]');
    check(Array.prototype.filter.call(tabsInOrder, function (b) { return b.tabIndex === 0; }).length === 1, 'only the selected tab is in the Tab order');
    key(document.activeElement, 'ArrowDown');
    await waitFor('ArrowDown to Clusters', function () { return paneTitle() === 'Clusters' && document.activeElement === selTab() && selTab().textContent === 'Clusters'; });
    key(document.activeElement, 'End');
    await waitFor('End to About', function () { return paneTitle() === 'About & diagnostics' && document.activeElement === selTab(); });
    key(document.activeElement, 'ArrowDown');
    await waitFor('ArrowDown wraps to Institution', function () { return paneTitle() === 'Institution' && document.activeElement === selTab(); });
    key(document.activeElement, 'ArrowUp');
    await waitFor('ArrowUp wraps to About', function () { return paneTitle() === 'About & diagnostics'; });
    key(document.activeElement, 'Home');
    await waitFor('Home to Institution', function () { return paneTitle() === 'Institution'; });
    check(true, 'the section list moves with ArrowDown/ArrowUp/Home/End (wrapping) and opens each section');
    check(document.querySelector('#settings a, #settings [href]') === null, 'the Settings page has no links');
    check(document.getElementById('set-f-site') && document.getElementById('set-f-site').tagName === 'SELECT', 'Institution offers the institutions in a list');

    // Clusters: edit, invalid value, inline error, fix, save.
    key(document.activeElement, 'ArrowDown');
    await waitFor('the Clusters section', function () { return paneTitle() === 'Clusters' && document.querySelector('#settings-pane li[data-cluster="selftest-hpc"]'); });
    document.querySelector('#settings-pane li[data-cluster="selftest-hpc"] button[data-action=edit]').click();
    await waitFor('the cluster form', function () { return document.getElementById('set-f-ssh-host'); });
    var sshIn = document.getElementById('set-f-ssh-host');
    check(sshIn.value === 'selftest-login' && document.activeElement === document.getElementById('set-f-name'), 'the form shows the cluster and focuses its first field');
    check(!document.querySelector('#settings-cluster-form .field[data-field=timezone]').hidden,
      'a Slurm cluster shows the time zone');
    check(!/key|secret/i.test(Array.prototype.map.call(document.querySelectorAll('.settings-s3 label'), function (l) { return l.textContent; }).join(' ').replace('KMS key', '')),
      'the S3 block asks for no access keys');
    sshIn.value = 'bad host';
    var rendersBefore = sh.renders;
    document.querySelector('#settings-cluster-form [data-role=save]').click();
    await waitFor('the inline error', function () { return document.getElementById('set-f-ssh-host-err').textContent !== ''; });
    check(sshIn.getAttribute('aria-invalid') === 'true' && document.activeElement === sshIn && sh.renders === rendersBefore,
      'an invalid ssh_host shows an error under the field, marks it invalid and focuses it (' + document.getElementById('set-f-ssh-host-err').textContent + ')');
    check(document.getElementById('settings-restart').hidden, 'no restart notice before anything was saved');
    sshIn.value = 'selftest-spare';
    document.querySelector('#settings-cluster-form [data-role=save]').click();
    await waitFor('the save', function () { return !document.getElementById('settings-cluster-form') && statusText().indexOf('Saved cluster selftest-hpc') !== -1; });
    check(!document.getElementById('settings-restart').hidden && document.getElementById('settings-restart').textContent.indexOf('restart') !== -1, 'after the save the page says to restart Labmill to apply');
    check(document.querySelector('#settings-pane li[data-cluster="selftest-hpc"]').textContent.indexOf('SSH host selftest-spare') !== -1, 'the list shows the saved value');

    // More clusters at your institution (M57.2): lists only the unconfigured
    // profile cluster; Add shows the preview first, the second click adds.
    var more = document.querySelector('#settings-more');
    check(more && more.textContent.indexOf('More clusters at your institution') !== -1 && more.querySelectorAll('li[data-more]').length === 1 && more.querySelector('li[data-more="selftest-more"]') !== null, 'the Clusters section lists the one cluster of the institution that is not set up');
    check(document.querySelector('#settings-more li[data-more="selftest-hpc"]') === null, 'a configured cluster is not offered');
    more.querySelector('li[data-more="selftest-more"] button[data-action=add-more]').click();
    await waitFor('the preview', function () { return document.getElementById('settings-more-user'); });
    check(document.getElementById('settings-more-user').value === 'selftest' && more.textContent.indexOf('more.selftest.example') !== -1 && more.textContent.indexOf('clusters.yaml') !== -1, 'Add first shows the SSH host, the files and the account name');
    var doneBeforeM = hooks.done;
    more.querySelector('button[data-action=confirm-more]').click();
    await waitFor('the add', function () { return hooks.done > doneBeforeM && logEl.textContent.indexOf('Added the cluster selftest-more (self-test).') !== -1; });
    check(true, 'the second click added the cluster as a turn shown in the chat');

    // SSH connections: Labmill's are editable, the user's own read-only; the
    // logged-in state comes from the helper.
    document.getElementById('settings-tab-ssh').click();
    await waitFor('the SSH section', function () { return document.querySelector('#settings-pane li[data-host="selftest-mine"]'); });
    var mine = document.querySelector('#settings-pane li[data-host="selftest-mine"]'), login = document.querySelector('#settings-pane li[data-host="selftest-login"]');
    check(mine.querySelectorAll('button').length === 0 && mine.textContent.indexOf('read-only') !== -1, "the user's own connection is read-only (no buttons)");
    check(login.textContent.indexOf('logged in') !== -1 && login.textContent.indexOf('not logged in') === -1 && login.querySelectorAll('button').length === 2, "Labmill's connection shows logged in, with Edit and Remove");

    // Folders: data first.
    document.getElementById('settings-tab-folders').click();
    await waitFor('the folders', function () { return document.querySelectorAll('#settings-folders .settings-folder').length === 2; });
    var kinds = Array.prototype.map.call(document.querySelectorAll('#settings-folders .settings-folder'), function (f) { return f.dataset.kind; });
    check(kinds.join(',') === 'data,code', 'the Folders section lists the data folder first (' + kinds.join(',') + ')');

    // App: the start-in-tray switch (a stub in the self-test) and the tips.
    document.getElementById('settings-tab-app').click();
    await waitFor('the App section', function () { return document.getElementById('settings-start-tray'); });
    var cb = document.getElementById('settings-start-tray');
    cb.focus();
    cb.click();
    await waitFor('start in tray on', function () { return statusText().indexOf('starts in the tray') !== -1; });
    check(document.getElementById('settings-start-tray').checked && document.activeElement === document.getElementById('settings-start-tray'), 'the switch turned on and kept the focus');
    document.getElementById('settings-start-tray').click();
    await waitFor('start in tray off', function () { return statusText().indexOf("won't start") !== -1; });
    var aa = document.getElementById('settings-approve-all');
    check(!!aa && !aa.checked, 'Approve everything automatically is on the Settings page and off');
    var ra = document.getElementById('settings-run-without-asking');
    check(ra && !ra.checked, 'Run commands on the cluster without asking is off by default');
    ra.click();
    await waitFor('run without asking on', function () { return statusText().indexOf('without asking') !== -1; });
    check(document.getElementById('settings-run-without-asking').checked, 'the run-without-asking switch turned on');
    document.getElementById('settings-run-without-asking').click();
    await waitFor('run without asking off', function () { return statusText().indexOf('for your OK again') !== -1; });
    document.getElementById('settings-reset-notices').click();
    await waitFor('the tips reset', function () { return statusText().indexOf('will show again') !== -1; });
    check(true, 'Start in the tray went on and off and the one-time tips were reset');

    // Labmill account: not connected at first; a wrong token is refused
    // under the field; the right one connects (a fake account in Go); the
    // token never shows on the page; Disconnect ends the link.
    document.getElementById('settings-tab-account').click();
    await waitFor('the account section', function () { return document.getElementById('set-f-token'); });
    check(document.getElementById('settings-account').textContent.indexOf('Not connected') !== -1 && document.getElementById('set-f-token').type === 'password',
      'the account section says not connected and asks for the token in a password field');
    document.getElementById('set-f-token').value = 'lmt_wrongwrongwrongwrongwrong';
    document.querySelector('#settings-account-form [data-role=save]').click();
    await waitFor('the refused token', function () { return document.getElementById('set-f-token-err').textContent !== ''; });
    check(document.getElementById('set-f-token-err').textContent.indexOf('did not accept') !== -1, 'a wrong token shows an error under the field');
    document.getElementById('set-f-token').value = 'lmt_selftestselftestselftest01';
    document.querySelector('#settings-account-form [data-role=save]').click();
    await waitFor('the connect', function () { return document.getElementById('settings-account-disconnect'); });
    check(statusText().indexOf('Self Test') !== -1 && document.getElementById('settings-pane').textContent.indexOf('lmt_selftest') === -1,
      'the right token connects, and the page never shows it');
    document.getElementById('settings-account-check').click();
    await waitFor('the check', function () { return statusText().indexOf('Connected as Self Test') !== -1; });
    document.getElementById('settings-account-disconnect').click();
    await waitFor('the disconnect', function () { return document.getElementById('set-f-token') && statusText().indexOf('Disconnected') !== -1; });
    check(true, 'Check connection and Disconnect work');

    // About & diagnostics.
    document.getElementById('settings-tab-about').click();
    await waitFor('the About section', function () { return document.getElementById('settings-diag') && document.getElementById('settings-diag').textContent !== ''; });
    var about = document.getElementById('settings-pane').textContent;
    check(about.indexOf('https://broker.selftest.example/v1') !== -1 && about.indexOf('hunter2') === -1 && about.indexOf('SELFTESTSIG') === -1,
      'the assistant address shows without user info or query');
    document.getElementById('settings-open-logs').click();
    await waitFor('the logs folder', function () { return statusText().indexOf('Opened ') === 0; });
    var diagCopies = [];
    var ownClip2 = Object.getOwnPropertyDescriptor(navigator, 'clipboard');
    Object.defineProperty(navigator, 'clipboard', { configurable: true,
      value: { writeText: function (t) { diagCopies.push(t); return Promise.resolve(); } } });
    document.getElementById('settings-copy-diag').click();
    await waitFor('the diagnostics copy', function () { return diagCopies.length === 1; });
    var diag = diagCopies[0] || '';
    var secretLike = /(password|passwd|secret|token|BEGIN [A-Z ]*PRIVATE KEY|AKIA[0-9A-Z]{16}|hunter2|SELFTESTSIG|sig=|:\/\/[^\/\s]*@|[\w.-]+@[\w.-]+)/i;
    check(diag.indexOf('Labmill diagnostics') === 0 && diag.indexOf('selftest-hpc') !== -1, 'Copy diagnostics copied the diagnostics text');
    check(!secretLike.test(diag), 'the diagnostics contain nothing secret-looking (match: ' + JSON.stringify((diag.match(secretLike) || [''])[0]) + ')');
    check(diag.indexOf('User selftest') === -1 && diag.indexOf('selftest@') === -1 && diag.indexOf('via selftest-login:2222') !== -1,
      'the diagnostics name no user (also not in a jump host)');
    if (ownClip2) Object.defineProperty(navigator, 'clipboard', ownClip2); else delete navigator.clipboard;

    // Back to the chat with Esc (outside a form).
    selTab().focus();
    key(document.activeElement, 'Escape');
    await waitFor('back to the chat', function () { return document.getElementById('settings').hidden && !document.getElementById('main').hidden; });
    check(document.activeElement === document.getElementById('input'), 'Esc went back to the chat and focused the message box');

    // 12. The terminal (M62.2): xterm.js is vendored (no CDN), the Terminal
    // button opens the view, a fake login node answers through the real Go
    // bridge (events and base64 in both directions), tabs open and close.
    var tm = window.labmillTerminal, th = window.__labmillTerminalHooks;
    check(typeof window.Terminal === 'function' && !!window.FitAddon && typeof window.FitAddon.FitAddon === 'function', 'xterm.js and its fit addon are loaded from local files');
    for (var vf of ['vendor/xterm/xterm.js', 'vendor/xterm/xterm.css', 'vendor/xterm/addon-fit.js', 'vendor/xterm/LICENSE-xterm', 'vendor/xterm/LICENSE-addon-fit']) {
      try { var vr = await fetch(vf); check(vr.ok, vf + ' is served from the app'); } catch (e) { check(false, vf + ' fetch failed: ' + e); }
    }
    var tbtn = document.getElementById('terminal-open');
    check(!!tbtn && !tbtn.hidden && tbtn.textContent === 'Terminal' && tbtn.type === 'button', 'the header has a Terminal button once Labmill is ready');
    check(!!document.querySelector('#jobs-body button[data-job="12"][data-act="terminal"]'), 'a job with a cluster folder has an "Open terminal here" button');
    tbtn.click();
    await waitFor('the terminal view', function () { return !document.getElementById('terminal').hidden; });
    check(document.getElementById('main').hidden && tbtn.getAttribute('aria-expanded') === 'true', 'the Terminal button shows the terminal view in place of the chat');
    await waitFor('the cluster picker', function () { return document.querySelectorAll('#terminal-cluster option').length === 2; });
    check(!document.getElementById('terminal-system').hidden, '"Open in system terminal" is offered when the Labmill program is found');
    function termText(n) { var v = document.getElementById('term-view-' + n); return v ? (v.querySelector('.xterm-rows') || {}).textContent || '' : ''; }
    document.getElementById('terminal-new').click();
    await waitFor('the first terminal', function () { return termText(1).indexOf('selftest-shell on selftest-cluster in ~') !== -1; });
    var scr = document.querySelector('#term-view-1 .xterm-screen');
    check(tm.count() === 1 && th.opened === 1 && !!scr && scr.clientWidth > 0 && scr.clientHeight > 0, 'a terminal tab opened, rendered by xterm.js with a size');
    var xt = tm.current();
    check(xt.cols > 20 && xt.rows > 5, 'the terminal fitted its view (' + xt.cols + 'x' + xt.rows + ')');
    check(/\(\d+x\d+\)/.test(termText(1)), 'the login node was told the terminal size');
    xt.input('ls é\r');
    await waitFor('the typed line to come back', function () { return termText(1).indexOf('got:ls é') !== -1; });
    check(true, 'typed input went to the shell and its output came back (UTF-8 intact)');
    xt.resize(100, 30);
    await waitFor('the window-change', function () { return termText(1).indexOf('size 100x30') !== -1; });
    check(true, 'resizing the terminal reached the login node');
    check(document.querySelectorAll('#chat-form').length === 1 && document.getElementById('log').textContent.indexOf('selftest-shell') === -1,
      'nothing from the terminal appears in the chat');
    // Back to chat, open a second terminal in a job's folder.
    document.getElementById('terminal-back').click();
    await waitFor('the chat again', function () { return !document.getElementById('main').hidden && document.getElementById('terminal').hidden; });
    document.querySelector('#jobs-body button[data-job="12"][data-act="terminal"]').click();
    await waitFor('the job terminal', function () { return termText(2).indexOf('selftest-shell on selftest-cluster in /scratch/selftest') !== -1; });
    check(tm.count() === 2 && /#12/.test(document.getElementById('term-tab-2').textContent), 'Open terminal here opened a second tab in the job\'s cluster folder');
    check(document.getElementById('term-view-1').hidden && !document.getElementById('term-view-2').hidden, 'only the selected tab is shown');
    document.getElementById('term-tab-1').click();
    check(!document.getElementById('term-view-1').hidden && document.getElementById('term-view-2').hidden && document.getElementById('term-tab-1').getAttribute('aria-selected') === 'true', 'a click on a tab switches to it');
    document.getElementById('terminal-system').click();
    await sleep(150);
    check(document.getElementById('terminal-error').hidden, '"Open in system terminal" started without an error');
    // The shell ends on its own: the tab says so and stays until closed.
    tm.current().input('exit\r');
    await waitFor('the shell to end', function () { return th.exits === 1 && /\(ended\)/.test(document.getElementById('term-tab-1').textContent); });
    check(termText(1).indexOf('Session ended (exit status 5)') !== -1, 'a finished shell shows its exit status in the tab');
    // Closing a tab closes its session; the other one goes on.
    document.querySelector('#terminal-tabs [data-term="2"] .term-x').click();
    await waitFor('the tab to close', function () { return tm.count() === 1; });
    check(!document.getElementById('term-view-2'), 'closing a tab removes its terminal');
    document.querySelector('#terminal-tabs [data-term="1"] .term-x').click();
    await waitFor('the last tab to close', function () { return tm.count() === 0; });
    document.getElementById('terminal-back').click();
    await waitFor('the chat after the terminal', function () { return !document.getElementById('main').hidden; });
    check(true, 'back to the chat after the last terminal closed');
  }

  run().catch(function (e) { check(false, 'script error: ' + (e && e.message ? e.message : e)); })
    .then(function () { return api.SelfTestReport(passed, log.join('\n')); });
})();
