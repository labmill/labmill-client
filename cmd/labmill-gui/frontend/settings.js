// Labmill Settings page (M29). Plain JS, no build step. Everything from Go is
// shown with textContent (never innerHTML); links (the institution's
// documentation) are shown as text, never as clickable links.
//
// Layout: a full-page view (#settings) with a section list on the left
// (role=tablist: ArrowUp/ArrowDown/Home/End move and open a section, Tab
// goes on into it) and the section on the right; "Back to chat" (or Esc
// outside a form) returns to the chat. It reads everything with
// SettingsAPI.Get (local files only: no network, no login when it opens)
// and saves per section; field errors from Go show under their fields.
(function () {
  'use strict';

  var $ = function (id) { return document.getElementById(id); };
  function api() { return window.go && window.go.main && window.go.main.SettingsAPI; }
  function page() { return window.go && window.go.main && window.go.main.PageAPI; }
  function errText(e) { return (e && e.message) ? e.message : String(e); }

  function el(tag, cls, text) {
    var n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text !== undefined && text !== null) n.textContent = text;
    return n;
  }
  function btn(label, cls, fn) {
    var b = el('button', cls || '', label);
    b.type = 'button';
    if (fn) b.addEventListener('click', fn);
    return b;
  }

  var SECTIONS = [
    { id: 'institution', label: 'Institution' },
    { id: 'clusters', label: 'Clusters' },
    { id: 'ssh', label: 'SSH connections' },
    { id: 'folders', label: 'Folders' },
    { id: 'app', label: 'App' },
    { id: 'account', label: 'Labmill account' },
    { id: 'about', label: 'About & diagnostics' }
  ];

  var state = { open: false, enabled: false, section: 'institution', view: null, returnTo: 'main', editing: null, status: {} };
  // Counters the self-test reads (window.__labmillSettings in self-test mode).
  var hooks = { renders: 0, gets: 0 };
  window.__labmillSettingsHooks = hooks;

  // ---- open / close ----------------------------------------------------
  function enable() {
    state.enabled = true;
    $('settings-open').hidden = false;
  }

  function open(section) {
    if (!state.enabled || !api()) return;
    if (!state.open) {
      state.returnTo = !$('main').hidden ? 'main' : (!$('fatal').hidden ? 'fatal' : 'main');
      state.returnFocus = document.activeElement;
      $(state.returnTo).hidden = true;
      $('settings').hidden = false;
      state.open = true;
      $('settings-open').setAttribute('aria-expanded', 'true');
    }
    if (section) state.section = section;
    state.editing = null;
    refresh(true);
  }

  function close() {
    if (!state.open) return;
    state.open = false;
    state.editing = null;
    $('settings').hidden = true;
    $(state.returnTo).hidden = false;
    $('settings-open').setAttribute('aria-expanded', 'false');
    var f = state.returnTo === 'main' ? $('input') : $('settings-open');
    if (f) f.focus();
  }

  // refresh reads the view from Go and renders; focusTab moves the focus to
  // the selected section's tab.
  function refresh(focusTab) {
    var a = api();
    if (!a) return Promise.resolve();
    hooks.gets++;
    return a.Get().then(function (v) {
      state.view = v;
      render(focusTab);
    }, function (e) {
      $('settings-pane').textContent = '';
      $('settings-pane').appendChild(el('p', 'error', errText(e)));
    });
  }

  // ---- the section list (tabs) -----------------------------------------
  function renderNav(focusTab) {
    var nav = $('settings-nav');
    nav.textContent = '';
    SECTIONS.forEach(function (s) {
      var b = el('button', 'settings-tab', s.label);
      b.type = 'button';
      b.id = 'settings-tab-' + s.id;
      b.setAttribute('role', 'tab');
      b.setAttribute('aria-controls', 'settings-pane');
      var sel = s.id === state.section;
      b.setAttribute('aria-selected', sel ? 'true' : 'false');
      b.tabIndex = sel ? 0 : -1;
      b.dataset.section = s.id;
      b.addEventListener('click', function () { select(s.id, true); });
      nav.appendChild(b);
    });
    $('settings-pane').setAttribute('aria-labelledby', 'settings-tab-' + state.section);
    if (focusTab) $('settings-tab-' + state.section).focus();
  }

  function select(id, focusTab) {
    if (state.section === id && state.view) { if (focusTab) $('settings-tab-' + id).focus(); return; }
    state.section = id;
    state.editing = null;
    render(focusTab);
  }

  function onNavKey(e) {
    var ids = SECTIONS.map(function (s) { return s.id; });
    var i = ids.indexOf(state.section), n = ids.length, next = -1;
    if (e.key === 'ArrowDown' || e.key === 'ArrowRight') next = (i + 1) % n;
    else if (e.key === 'ArrowUp' || e.key === 'ArrowLeft') next = (i + n - 1) % n;
    else if (e.key === 'Home') next = 0;
    else if (e.key === 'End') next = n - 1;
    if (next < 0) return;
    e.preventDefault();
    select(ids[next], true);
  }

  // ---- rendering ---------------------------------------------------------
  function render(focusTab) {
    var v = state.view;
    if (!v) return;
    // A re-render keeps the focus on the element with the same id.
    var a = document.activeElement, keepId = a && a.id && $('settings').contains(a) ? a.id : '';
    renderNav(focusTab);
    var banner = $('settings-restart');
    banner.hidden = !v.restart;
    banner.textContent = v.restart ? 'Some changes take effect after Labmill restarts: quit it (tray menu > Quit Labmill) and open it again.' : '';
    var pane = $('settings-pane');
    pane.textContent = '';
    var s = SECTIONS.filter(function (x) { return x.id === state.section; })[0];
    pane.appendChild(el('h3', 'settings-h', s.label));
    ({ institution: renderInstitution, clusters: renderClusters, ssh: renderSSH, folders: renderFolders,
      app: renderApp, account: renderAccount, about: renderAbout })[s.id](pane, v);
    if (!focusTab && keepId && $(keepId)) $(keepId).focus();
    hooks.renders++;
  }

  // statusLine is the section's status line (role=status): the last save's
  // message or error.
  function statusLine(pane) {
    var p = el('p', 'settings-status');
    p.id = 'settings-status';
    p.setAttribute('role', 'status');
    var st = state.status[state.section];
    if (st) { p.textContent = st.text; if (st.error) p.classList.add('error'); }
    pane.appendChild(p);
    return p;
  }
  function setStatus(text, isError) {
    state.status[state.section] = text ? { text: text, error: !!isError } : null;
    var p = $('settings-status');
    if (p) { p.textContent = text || ''; p.classList.toggle('error', !!isError); }
  }

  // field adds a labeled input (or select) with an error line under it.
  // opts: {label, key (the Go field name for errors), value, type, hint,
  // options [[value, label]], disabled, list, placeholder}.
  function field(form, opts) {
    var id = 'set-f-' + opts.key.replace(/[^a-zA-Z0-9]/g, '-');
    var wrap = el('div', 'field');
    wrap.dataset.field = opts.key;
    var lab = el('label', '', opts.label);
    lab.htmlFor = id;
    var inp;
    if (opts.options) {
      inp = el('select');
      opts.options.forEach(function (o) {
        var op = el('option', '', o[1]);
        op.value = o[0];
        if (o[0] === (opts.value || '')) op.selected = true;
        inp.appendChild(op);
      });
    } else {
      inp = el('input');
      inp.type = opts.type || 'text';
      inp.value = opts.value === undefined || opts.value === null ? '' : String(opts.value);
      inp.spellcheck = false;
      inp.autocomplete = 'off';
      inp.setAttribute('autocapitalize', 'none');
      if (opts.placeholder) inp.placeholder = opts.placeholder;
      if (opts.list) inp.setAttribute('list', opts.list);
      if (opts.type === 'number') { inp.min = '1'; inp.max = '65535'; }
    }
    inp.id = id;
    inp.dataset.key = opts.key;
    if (opts.disabled) inp.disabled = true;
    var err = el('p', 'field-error');
    err.id = id + '-err';
    err.setAttribute('aria-live', 'polite');
    var describedBy = [err.id];
    wrap.appendChild(lab);
    wrap.appendChild(inp);
    if (opts.hint) {
      var h = el('p', 'muted field-hint', opts.hint);
      h.id = id + '-hint';
      wrap.appendChild(h);
      describedBy.unshift(h.id);
    }
    wrap.appendChild(err);
    inp.setAttribute('aria-describedby', describedBy.join(' '));
    form.appendChild(wrap);
    return inp;
  }

  // showErrors puts each field error under its field (aria-invalid) and
  // focuses the first; errors for fields not on the form go to the status.
  function showErrors(form, errors) {
    Array.prototype.forEach.call(form.querySelectorAll('.field'), function (w) {
      var i = w.querySelector('input, select');
      w.querySelector('.field-error').textContent = '';
      if (i) i.removeAttribute('aria-invalid');
    });
    var first = null, rest = [];
    (errors || []).forEach(function (fe) {
      var w = form.querySelector('.field[data-field="' + fe.field + '"]');
      if (!w) { rest.push(fe.message); return; }
      var e = w.querySelector('.field-error');
      e.textContent = e.textContent ? e.textContent + ' ' + fe.message : fe.message;
      var i = w.querySelector('input, select');
      if (i) { i.setAttribute('aria-invalid', 'true'); if (!first) first = i; }
    });
    if (rest.length || (errors && errors.length)) setStatus(rest.length ? rest.join(' ') : 'Please fix the marked fields.', true);
    if (first) first.focus();
  }

  // handle shows a settings.Result: errors inline, or the message, then
  // re-reads the view (onOK runs first on success).
  function handle(form, r, onOK) {
    if (!r) return;
    if (r.ok) {
      setStatus(r.message || 'Saved.', false);
      if (onOK) onOK();
      return refresh(false).then(function () {
        // A form that closed took the focus with it: the status line gets it.
        var s = $('settings-status');
        if (s && (!document.activeElement || document.activeElement === document.body)) { s.tabIndex = -1; s.focus(); }
      });
    }
    if (r.errors && r.errors.length && form) { showErrors(form, r.errors); return; }
    setStatus(r.error || 'That didn\'t work.', true);
  }

  function formButtons(form, saveLabel, onSave, onCancel) {
    var row = el('div', 'buttons');
    if (onCancel) row.appendChild(btn('Cancel', '', onCancel));
    var save = btn(saveLabel, 'primary', onSave);
    save.dataset.role = 'save';
    row.appendChild(save);
    form.appendChild(row);
    return save;
  }

  // ---- Institution --------------------------------------------------------
  function renderInstitution(pane, v) {
    pane.appendChild(el('p', '', 'The institution decides which help and skills Labmill uses. Your clusters and connections stay as they are when you change it.'));
    var cur = el('p');
    cur.appendChild(el('strong', '', 'In use: '));
    cur.appendChild(document.createTextNode(v.site.name + ' (' + v.site.domain + ')'));
    pane.appendChild(cur);
    if (v.site.docsUrl) {
      var d = el('p', 'muted');
      d.appendChild(document.createTextNode('Documentation: '));
      d.appendChild(el('code', 'settings-url', v.site.docsUrl));
      pane.appendChild(d);
    }
    if (v.configError) { pane.appendChild(el('p', 'error', 'clusters.yaml can\'t be read, so this can\'t be changed here: ' + v.configError)); return; }
    var form = el('div', 'settings-form');
    var sel = field(form, { label: 'Your institution', key: 'site', value: v.site.domain,
      options: (v.sites || []).map(function (s) { return [s.domain, s.domain === 'generic' ? s.name : s.name + ' (' + s.domain + ')']; }) });
    formButtons(form, 'Save', function () {
      api().SaveSite(sel.value).then(function (r) { handle(form, r); }, function (e) { setStatus(errText(e), true); });
    });
    pane.appendChild(form);
    statusLine(pane);
  }

  // ---- Clusters -------------------------------------------------------------
  function typeLabel(t) { return t === 'slurm' ? 'Slurm' : t; }

  function renderClusters(pane, v) {
    if (v.configError) { pane.appendChild(el('p', 'error', 'clusters.yaml can\'t be read, so clusters can\'t be edited here: ' + v.configError)); return; }
    if (state.editing && state.editing.kind === 'cluster') { clusterForm(pane, v, state.editing); return; }
    pane.appendChild(el('p', 'muted', 'Saved in clusters.yaml (a backup is made before the first change; your comments are kept).'));
    var list = el('ul', 'settings-list');
    list.setAttribute('aria-label', 'Clusters');
    if (!v.clusters.length) list.appendChild(el('li', 'muted', 'No clusters yet.'));
    v.clusters.forEach(function (c) {
      var li = el('li', 'settings-item');
      li.dataset.cluster = c.name;
      var t = el('div', 'settings-item-text');
      t.appendChild(el('strong', '', c.name));
      t.appendChild(document.createTextNode(' · ' + typeLabel(c.type)));
      var bits = [];
      if (c.sshHost) bits.push('SSH host ' + c.sshHost);
      if (c.filesHost) bits.push('files host ' + c.filesHost);
      bits.push('transfer ' + (c.transfer || 'sftp'));
      if (c.s3 && c.s3.bucket) bits.push('S3 ' + c.s3.bucket);
      t.appendChild(el('div', 'muted', bits.join(' · ')));
      li.appendChild(t);
      var b = el('div', 'settings-item-buttons');
      var edit = btn('Edit', '', function () { state.editing = { kind: 'cluster', name: c.name }; render(false); focusFirst(); });
      edit.setAttribute('aria-label', 'Edit cluster ' + c.name);
      edit.dataset.action = 'edit';
      var rm = btn('Remove', '', function () { confirmRemove(li, 'cluster ' + c.name, function () { return api().RemoveCluster(c.name); }); });
      rm.setAttribute('aria-label', 'Remove cluster ' + c.name);
      // "Terminal" (M62.2): your own shell on this cluster's login node.
      if (c.sshHost && window.labmillTerminal) {
        var term = btn('Terminal', '', function () { close(); window.labmillTerminal.openOn(c.name); });
        term.setAttribute('aria-label', 'Open a terminal on cluster ' + c.name);
        term.title = 'Open a terminal on the login node of ' + c.name;
        term.dataset.action = 'terminal';
        b.appendChild(term);
      }
      b.appendChild(edit); b.appendChild(rm);
      li.appendChild(b);
      list.appendChild(li);
    });
    pane.appendChild(list);
    var add = btn('Add cluster', '', function () { state.editing = { kind: 'cluster', name: '' }; render(false); focusFirst(); });
    add.id = 'settings-add-cluster';
    pane.appendChild(add);
    renderMoreClusters(pane, v);
    statusLine(pane);
  }

  // "More clusters at your institution" (M57.2): clusters of the institution's
  // profile that this install's clusters.yaml doesn't have. Add shows what
  // will be written first; the second click ("Add cluster") is the approval.
  function renderMoreClusters(pane, v) {
    var more = v.moreClusters || [];
    if (!more.length) return;
    var box = el('div', 'settings-more');
    box.id = 'settings-more';
    box.appendChild(el('h4', 'settings-h2', 'More clusters at your institution'));
    box.appendChild(el('p', 'muted', 'These clusters exist at your institution but aren\'t set up in Labmill yet. Adding one writes its SSH settings and a new entry in clusters.yaml; nothing is sent to or run on any cluster.'));
    var list = el('ul', 'settings-list');
    list.setAttribute('aria-label', 'More clusters at your institution');
    more.forEach(function (m) {
      var li = el('li', 'settings-item');
      li.dataset.more = m.name;
      var t = el('div', 'settings-item-text');
      t.appendChild(el('strong', '', m.name));
      t.appendChild(document.createTextNode(' · ' + typeLabel(m.type)));
      li.appendChild(t);
      var b = el('div', 'settings-item-buttons');
      var add = btn('Add', '', function () { showMorePlan(li, m.name); });
      add.dataset.action = 'add-more';
      add.setAttribute('aria-label', 'Add cluster ' + m.name);
      b.appendChild(add);
      li.appendChild(b);
      list.appendChild(li);
    });
    box.appendChild(list);
    pane.appendChild(box);
  }

  function showMorePlan(li, name) {
    var old = li.querySelector('.settings-more-plan');
    if (old) old.remove();
    api().MoreClusterPlan(name).then(function (p) {
      var plan = el('div', 'settings-more-plan');
      plan.style.flexBasis = '100%';
      plan.appendChild(el('p', '', 'Adding ' + p.cluster + ' (' + typeLabel(p.type) + ') will write:'));
      var ul = el('ul');
      (p.hosts || []).forEach(function (h) { ul.appendChild(el('li', '', h)); });
      (p.files || []).forEach(function (f) { ul.appendChild(el('li', '', f)); });
      plan.appendChild(ul);
      var lab = el('label', '', 'Your account name there');
      var inp = el('input');
      inp.type = 'text'; inp.id = 'settings-more-user'; inp.value = p.username || '';
      inp.spellcheck = false; inp.autocomplete = 'off';
      lab.htmlFor = inp.id;
      plan.appendChild(lab); plan.appendChild(inp);
      var go = btn('Add cluster', '', function () {
        api().AddMoreCluster(name, inp.value).then(function () {
          setStatus('Adding ' + name + '… (see the chat)');
          plan.remove();
        }, function (e) { setStatus(errText(e), true); });
      });
      go.dataset.action = 'confirm-more';
      var cancel = btn('Cancel', '', function () { plan.remove(); });
      var row = el('div', 'settings-item-buttons');
      row.appendChild(go); row.appendChild(cancel);
      plan.appendChild(row);
      li.appendChild(plan);
      inp.focus();
    }, function (e) { setStatus(errText(e), true); });
  }

  function focusFirst() {
    var f = $('settings-pane').querySelector('input:not([disabled]), select:not([disabled])');
    if (f) f.focus();
  }

  // confirmRemove asks inline (no window.confirm) before removing.
  function confirmRemove(li, what, call) {
    if (li.querySelector('.settings-confirm')) return;
    var box = el('div', 'settings-confirm');
    box.setAttribute('role', 'group');
    box.appendChild(el('span', '', 'Remove ' + what + '?'));
    var keep = btn('Keep', '', function () { box.remove(); });
    var yes = btn('Remove', 'danger', function () {
      call().then(function (r) { handle(null, r); }, function (e) { setStatus(errText(e), true); });
    });
    box.appendChild(keep); box.appendChild(yes);
    li.appendChild(box);
    keep.focus();
  }

  function hostDatalist(pane, v) {
    var dl = el('datalist');
    dl.id = 'settings-hosts-list';
    (v.hosts || []).forEach(function (h) { var o = el('option'); o.value = h.alias; dl.appendChild(o); });
    pane.appendChild(dl);
  }

  function clusterForm(pane, v, ed) {
    var c = null;
    v.clusters.forEach(function (x) { if (x.name === ed.name) c = x; });
    c = c || { name: '', type: 'slurm', transfer: 'sftp' };
    var s3 = c.s3 || {};
    pane.appendChild(el('p', '', ed.name ? 'Edit cluster ' + ed.name : 'Add a cluster'));
    hostDatalist(pane, v);
    var form = el('div', 'settings-form');
    form.id = 'settings-cluster-form';
    var f = {};
    f.name = field(form, { label: 'Name', key: 'name', value: c.name, hint: 'How you and Labmill refer to it, e.g. bigcluster.' });
    f.type = field(form, { label: 'Type', key: 'type', value: c.type, options: [['slurm', 'Slurm']] });
    f.sshHost = field(form, { label: 'SSH host', key: 'ssh_host', value: c.sshHost, list: 'settings-hosts-list',
      hint: 'A connection from SSH connections.' });
    f.filesHost = field(form, { label: 'Files host (optional)', key: 'files_host', value: c.filesHost, list: 'settings-hosts-list',
      hint: 'A dedicated transfer host, used for all file transfers when set.' });
    f.timezone = field(form, { label: 'Time zone (optional)', key: 'timezone', value: c.timezone, placeholder: 'America/Los_Angeles',
      hint: 'Rarely needed: only if the cluster\'s Slurm ignores TZ.' });
    f.transfer = field(form, { label: 'Transfer', key: 'transfer', value: c.transfer || 'sftp', options: [['sftp', 'SFTP (no setup)'], ['s3', 'S3 bucket']] });
    var fs = el('fieldset', 'settings-s3');
    fs.appendChild(el('legend', '', 'S3 staging (optional)'));
    fs.appendChild(el('p', 'muted', 'Keys never go here: Labmill uses the AWS profile below. Leave all empty for no S3 block.'));
    form.appendChild(fs);
    var s = {};
    s.bucket = field(fs, { label: 'Bucket', key: 's3.bucket', value: s3.bucket });
    s.prefix = field(fs, { label: 'Prefix', key: 's3.prefix', value: s3.prefix });
    s.region = field(fs, { label: 'Region', key: 's3.region', value: s3.region, placeholder: 'us-west-2' });
    s.profile = field(fs, { label: 'AWS profile', key: 's3.profile', value: s3.profile });
    s.endpoint = field(fs, { label: 'Endpoint (S3-compatible storage)', key: 's3.endpoint', value: s3.endpoint, type: 'url' });
    s.sse = field(fs, { label: 'Server-side encryption', key: 's3.sse', value: s3.sse || '', options: [['', 'Bucket default'], ['AES256', 'AES256 (SSE-S3)'], ['aws:kms', 'aws:kms (SSE-KMS)']] });
    s.kms = field(fs, { label: 'KMS key (with aws:kms)', key: 's3.kms_key_id', value: s3.kmsKeyId });
    function save() {
      var cl = { name: f.name.value, type: f.type.value, sshHost: f.sshHost.value,
        transfer: f.transfer.value, filesHost: f.filesHost.value, timezone: f.timezone.value, site: c.site || '',
        s3: { bucket: s.bucket.value, prefix: s.prefix.value, region: s.region.value, profile: s.profile.value,
          endpoint: s.endpoint.value, sse: s.sse.value, kmsKeyId: s.kms.value } };
      api().SaveCluster(ed.name, cl).then(function (r) {
        handle(form, r, function () { state.editing = null; });
      }, function (e) { setStatus(errText(e), true); });
    }
    formButtons(form, 'Save', save, function () { state.editing = null; setStatus(''); render(false); var a = $('settings-add-cluster'); if (a) a.focus(); });
    form.addEventListener('keydown', function (e) {
      if (e.key === 'Enter' && e.target.tagName === 'INPUT') { e.preventDefault(); save(); }
    });
    pane.appendChild(form);
    statusLine(pane);
  }

  // ---- SSH connections --------------------------------------------------------
  function renderSSH(pane, v) {
    if (state.editing && state.editing.kind === 'host') { hostForm(pane, v, state.editing); return; }
    pane.appendChild(el('p', 'muted', 'labmill\'s connections are in its own SSH file and can be edited here. Your own entries in ~/.ssh/config are shown read-only: Labmill never changes them.'));
    if (v.sshError) pane.appendChild(el('p', 'error', v.sshError));
    var list = el('ul', 'settings-list');
    list.setAttribute('aria-label', 'SSH connections');
    if (!v.hosts.length) list.appendChild(el('li', 'muted', 'No connections yet.'));
    v.hosts.forEach(function (h) {
      var li = el('li', 'settings-item');
      li.dataset.host = h.alias;
      var t = el('div', 'settings-item-text');
      t.appendChild(el('strong', '', h.alias));
      var target = (h.user ? h.user + '@' : '') + (h.hostName || h.alias) + (h.port ? ':' + h.port : '');
      t.appendChild(document.createTextNode(' → ' + target + (h.proxyJump ? ' via ' + h.proxyJump : '')));
      var meta = [h.inLabmill ? 'labmill\'s' : 'yours (read-only)', h.loggedIn ? 'logged in' : 'not logged in'];
      if (h.usedBy && h.usedBy.length) meta.push('used by ' + h.usedBy.join(', '));
      var m = el('div', 'muted', meta.join(' · '));
      t.appendChild(m);
      li.appendChild(t);
      if (h.inLabmill) {
        var b = el('div', 'settings-item-buttons');
        var edit = btn('Edit', '', function () { state.editing = { kind: 'host', alias: h.alias }; render(false); focusFirst(); });
        edit.setAttribute('aria-label', 'Edit connection ' + h.alias);
        var rm = btn('Remove', '', function () { confirmRemove(li, 'connection ' + h.alias, function () { return api().RemoveHost(h.alias); }); });
        rm.setAttribute('aria-label', 'Remove connection ' + h.alias);
        b.appendChild(edit); b.appendChild(rm);
        li.appendChild(b);
      }
      list.appendChild(li);
    });
    pane.appendChild(list);
    var add = btn('Add connection', '', function () { state.editing = { kind: 'host', alias: '' }; render(false); focusFirst(); });
    add.id = 'settings-add-host';
    pane.appendChild(add);
    pane.appendChild(el('p', 'muted', 'Logins end by themselves 10 minutes after Labmill quits.'));
    statusLine(pane);
  }

  function hostForm(pane, v, ed) {
    var h = null;
    v.hosts.forEach(function (x) { if (x.alias === ed.alias && x.inLabmill) h = x; });
    h = h || { alias: '', hostName: '', user: '', port: 0, proxyJump: '' };
    pane.appendChild(el('p', '', ed.alias ? 'Edit connection ' + ed.alias : 'Add a connection'));
    hostDatalist(pane, v);
    var form = el('div', 'settings-form');
    form.id = 'settings-host-form';
    var f = {};
    f.alias = field(form, { label: 'Name (alias)', key: 'alias', value: h.alias, disabled: !!ed.alias,
      hint: ed.alias ? 'The name can\'t change: clusters refer to it.' : 'What you would type after ssh, e.g. login.' });
    f.hostName = field(form, { label: 'Host name', key: 'hostName', value: h.hostName, placeholder: 'login.example.edu' });
    f.user = field(form, { label: 'User name', key: 'user', value: h.user });
    f.port = field(form, { label: 'Port (empty: 22)', key: 'port', value: h.port || '', type: 'number' });
    f.proxyJump = field(form, { label: 'Jump host (optional)', key: 'proxyJump', value: h.proxyJump, list: 'settings-hosts-list',
      hint: 'Another connection to go through first (ProxyJump).' });
    function save() {
      var port = f.port.value.trim() === '' ? 0 : Number(f.port.value);
      if (!isFinite(port) || Math.floor(port) !== port) port = -1;
      api().SaveHost(ed.alias, { alias: f.alias.value, hostName: f.hostName.value, user: f.user.value, port: port, proxyJump: f.proxyJump.value })
        .then(function (r) { handle(form, r, function () { state.editing = null; }); }, function (e) { setStatus(errText(e), true); });
    }
    formButtons(form, 'Save', save, function () { state.editing = null; setStatus(''); render(false); var a = $('settings-add-host'); if (a) a.focus(); });
    form.addEventListener('keydown', function (e) {
      if (e.key === 'Enter' && e.target.tagName === 'INPUT') { e.preventDefault(); save(); }
    });
    pane.appendChild(form);
    statusLine(pane);
  }

  // ---- Folders ---------------------------------------------------------------
  // The same folders as the chat box's + menu (PageAPI, the same checks):
  // data first.
  function renderFolders(pane) {
    pane.appendChild(el('p', 'muted', 'Only the folder names are kept: nothing is read or sent until you ask in the chat and approve.'));
    var box = el('div', 'settings-folders');
    box.id = 'settings-folders';
    pane.appendChild(box);
    var st = statusLine(pane);
    var p = page();
    if (!p) return;
    p.Folders().then(function (fs) { fillFolders(box, fs); }, function (e) { st.textContent = errText(e); st.classList.add('error'); });
  }

  function fillFolders(box, fs) {
    box.textContent = '';
    [['data', 'Data folder', fs.data, fs.recentData], ['code', 'Code folder (optional)', fs.code, fs.recentCode]].forEach(function (k) {
      var sec = el('div', 'settings-folder');
      sec.dataset.kind = k[0];
      sec.appendChild(el('h4', '', k[1]));
      var cur = el('p');
      if (k[2] && k[2].path) { var c = el('code', '', k[2].short); c.title = k[2].path; cur.appendChild(c); }
      else cur.appendChild(el('span', 'muted', 'none chosen'));
      sec.appendChild(cur);
      var row = el('div', 'buttons left');
      var choose = btn('Choose…', '', function () { folderCall(page().PickFolder(k[0])); });
      choose.setAttribute('aria-label', 'Choose the ' + k[0] + ' folder');
      row.appendChild(choose);
      if (k[2] && k[2].path) {
        var rm = btn('Remove', '', function () { folderCall(page().SetFolder(k[0], '')); });
        rm.setAttribute('aria-label', 'Remove the ' + k[0] + ' folder');
        row.appendChild(rm);
      }
      sec.appendChild(row);
      var recent = (k[3] || []).filter(function (r) { return !k[2] || r.path !== k[2].path; });
      if (recent.length) {
        sec.appendChild(el('p', 'muted', 'Recent:'));
        var ul = el('ul', 'settings-list compact');
        recent.forEach(function (r) {
          var li = el('li', 'settings-item');
          var c = el('code', '', r.short); c.title = r.path;
          li.appendChild(c);
          var use = btn('Use', '', function () { folderCall(page().SetFolder(k[0], r.path)); });
          use.setAttribute('aria-label', 'Use ' + r.path + ' as the ' + k[0] + ' folder');
          li.appendChild(use);
          ul.appendChild(li);
        });
        sec.appendChild(ul);
      }
      box.appendChild(sec);
    });
  }

  function folderCall(p) {
    p.then(function (fs) {
      setStatus('');
      fillFolders($('settings-folders'), fs);
      // The chat box's chips follow at their next refresh.
      if (window.labmillApp) window.labmillApp.refreshFolders();
    }, function (e) { setStatus(errText(e), true); });
  }

  // ---- App -------------------------------------------------------------------
  // ---- Labmill account ---------------------------------------------------
  // The token is typed into a password field and goes only to Go
  // (AccountConnect); the page never shows a saved token.
  function renderAccount(pane) {
    var box = el('div');
    box.id = 'settings-account';
    pane.appendChild(box);
    statusLine(pane);
    api().AccountStatus().then(function (a) {
      box.textContent = '';
      if (a.error) box.appendChild(el('p', 'error', a.error));
      if (a.linked) {
        box.appendChild(el('p', '', 'Connected to ' + a.url + ' since ' + a.since + '. When a job ends, Labmill sends its record to your account: the cluster, the state, the times, the resources asked for, the partition and the software modules. It never sends job scripts, data files, program code, folder or job names, or log lines.'));
        var row = el('div', 'buttons');
        var check = btn('Check connection', '', function () {
          api().AccountCheck().then(function (r) { setStatus(r.ok ? r.message : r.error, !r.ok); }, function (e) { setStatus(errText(e), true); });
        });
        check.id = 'settings-account-check';
        var off = btn('Disconnect', 'danger', function () {
          api().AccountDisconnect().then(function (r) { setStatus(r.ok ? r.message : r.error, !r.ok); render(false); }, function (e) { setStatus(errText(e), true); });
        });
        off.id = 'settings-account-disconnect';
        row.appendChild(check); row.appendChild(off);
        box.appendChild(row);
        return;
      }
      box.appendChild(el('p', '', 'Not connected. Labmill sends nothing to a Labmill account. To connect, make a program token at ' + a.url + ' (Settings > Program tokens) and paste it here.'));
      var form = el('form');
      form.id = 'settings-account-form';
      form.addEventListener('submit', function (e) { e.preventDefault(); });
      var inp = field(form, { label: 'Program token', key: 'token', type: 'password', placeholder: 'lmt_…',
        hint: 'Kept on this computer only, in a file that only you can read.' });
      formButtons(form, 'Connect', function () {
        api().AccountConnect(inp.value).then(function (r) {
          if (r.ok) { inp.value = ''; setStatus(r.message, false); render(false); return; }
          showErrors(form, r.errors);
        }, function (e) { setStatus(errText(e), true); });
      });
      box.appendChild(form);
    }, function (e) { setStatus(errText(e), true); });
  }

  function renderApp(pane, v) {
    var row = el('div', 'settings-check');
    var cb = el('input');
    cb.type = 'checkbox';
    cb.id = 'settings-start-tray';
    cb.checked = !!v.app.startInTray;
    var lab = el('label', '', 'Start Labmill in the tray when I log in');
    lab.htmlFor = cb.id;
    row.appendChild(cb); row.appendChild(lab);
    pane.appendChild(row);
    pane.appendChild(el('p', 'muted', 'The same switch as in the tray menu. Off by default.'));
    cb.addEventListener('change', function () {
      var want = cb.checked;
      // No re-render: the checkbox keeps its state and the focus.
      api().SetStartInTray(want).then(function (r) {
        if (!r.ok) cb.checked = !want;
        setStatus(r.ok ? r.message : (r.error || 'That didn\'t work.'), !r.ok);
      }, function (e) { cb.checked = !want; setStatus(errText(e), true); });
    });
    pane.appendChild(el('h4', 'settings-h2', 'Commands on the cluster'));
    var arow = el('div', 'settings-check');
    var acb = el('input');
    acb.type = 'checkbox';
    acb.id = 'settings-run-without-asking';
    acb.checked = !!v.app.runWithoutAsking;
    var alab = el('label', '', 'Run commands on the cluster without asking');
    alab.htmlFor = acb.id;
    arow.appendChild(acb); arow.appendChild(alab);
    pane.appendChild(arow);
    pane.appendChild(el('p', 'muted', 'Labmill will run commands on your cluster account without showing them first; reads of key or token files still ask. Off by default. It applies to the next command.'));
    acb.addEventListener('change', function () {
      var want = acb.checked;
      api().SetRunWithoutAsking(want).then(function (r) {
        if (!r.ok) acb.checked = !want;
        setStatus(r.ok ? r.message : (r.error || 'That didn\'t work.'), !r.ok);
      }, function (e) { acb.checked = !want; setStatus(errText(e), true); });
    });
    pane.appendChild(el('h4', 'settings-h2', 'Approvals'));
    var prow = el('div', 'settings-check');
    var pcb = el('input');
    pcb.type = 'checkbox';
    pcb.id = 'settings-approve-all';
    pcb.checked = !!v.app.approveAll;
    var plab = el('label', '', 'Approve everything automatically');
    plab.htmlFor = pcb.id;
    prow.appendChild(pcb); prow.appendChild(plab);
    pane.appendChild(prow);
    pane.appendChild(el('p', 'muted', 'Labmill approves every step for you without showing it first: uploads, downloads, job submissions and commands. Each one appears in the chat as "Auto-approved: …", and a banner shows while this is on. Reads of key or token files, questions from Labmill and logins still ask. This includes everything "Run commands on the cluster without asking" does, and more. Off by default. It applies to the next step.'));
    pcb.addEventListener('change', function () {
      var want = pcb.checked;
      api().SetApproveAll(want).then(function (r) {
        if (!r.ok) pcb.checked = !want;
        else if (window.labmillApp) window.labmillApp.showApproveAll(want);
        setStatus(r.ok ? r.message : (r.error || 'That didn\'t work.'), !r.ok);
      }, function (e) { pcb.checked = !want; setStatus(errText(e), true); });
    });
    var drow = el('div', 'settings-check');
    var dcb = el('input');
    dcb.type = 'checkbox';
    dcb.id = 'settings-auto-download';
    dcb.checked = !!v.app.autoDownload;
    var dlab = el('label', '', 'Download results automatically');
    dlab.htmlFor = dcb.id;
    drow.appendChild(dcb); drow.appendChild(dlab);
    pane.appendChild(drow);
    pane.appendChild(el('p', 'muted', 'When a job you submitted finishes (completed, failed or timed out; not when you cancel it), Labmill brings its results to your results folder by itself, checked against the cluster. Results over 2 GB, or more than your disk can hold, are left for you to ask for. Nothing leaves your computer. Off by default. It applies to the next job that finishes.'));
    dcb.addEventListener('change', function () {
      var want = dcb.checked;
      api().SetAutoDownload(want).then(function (r) {
        if (!r.ok) dcb.checked = !want;
        else if (window.labmillApp) window.labmillApp.showAutoDownload(want);
        setStatus(r.ok ? r.message : (r.error || 'That didn\'t work.'), !r.ok);
      }, function (e) { dcb.checked = !want; setStatus(errText(e), true); });
    });
    [
      ['settings-notify-job-end', 'Notify me when a job ends', !!v.app.notifyJobEnd, 'SetNotifyJobEnd',
        'When a job you submitted ends (completed, failed, timed out or canceled by the scheduler; usually not when you cancel it yourself), Labmill shows a desktop notification. A job that Labmill will submit again by itself has not ended yet. On by default.'],
      ['settings-email-job-end', 'Email me when a Slurm job ends', !!v.app.emailOnJobEnd, 'SetEmailOnJobEnd',
        'New Slurm scripts ask Slurm to email you when the job ends or fails (#SBATCH --mail-type=END,FAIL). Slurm sends it to the address your site has for you. The line is visible in the review, and you can edit it. Off by default. It applies to the next script.']
    ].forEach(function (d) {
      var row = el('div', 'settings-check');
      var cb = el('input');
      cb.type = 'checkbox';
      cb.id = d[0];
      cb.checked = d[2];
      var lab = el('label', '', d[1]);
      lab.htmlFor = cb.id;
      row.appendChild(cb); row.appendChild(lab);
      pane.appendChild(row);
      pane.appendChild(el('p', 'muted', d[4]));
      cb.addEventListener('change', function () {
        var want = cb.checked;
        api()[d[3]](want).then(function (r) {
          if (!r.ok) cb.checked = !want;
          setStatus(r.ok ? r.message : (r.error || 'That didn\'t work.'), !r.ok);
        }, function (e) { cb.checked = !want; setStatus(errText(e), true); });
      });
    });
    var reset = btn('Show the one-time tips again', '', function () {
      api().ResetNotices().then(function (r) { setStatus(r.ok ? r.message : (r.error || 'That didn\'t work.'), !r.ok); }, function (e) { setStatus(errText(e), true); });
    });
    reset.id = 'settings-reset-notices';
    pane.appendChild(reset);
    pane.appendChild(el('p', 'muted', v.app.noticesShown ? 'For example the note that Labmill keeps running in the tray when you close its window.' : 'No one-time tip has been shown yet.'));
    statusLine(pane);
  }

  // ---- About & diagnostics ------------------------------------------------------
  function dl(pane, rows) {
    var d = el('dl', 'settings-dl');
    rows.forEach(function (r) {
      d.appendChild(el('dt', '', r[0]));
      var dd = el('dd');
      if (r[2]) dd.appendChild(el('code', '', r[1])); else dd.textContent = r[1];
      d.appendChild(dd);
    });
    pane.appendChild(d);
  }

  function renderAbout(pane, v) {
    var a = v.about;
    dl(pane, [['Version', (a.version || 'unknown') + ' (' + a.platform + ')'], ['Assistant address', a.brokerUrl || '(none)', !!a.brokerUrl]]);
    pane.appendChild(el('h4', '', 'Files'));
    dl(pane, a.files.map(function (f) { return [f.label, f.path, true]; }));
    var row = el('div', 'buttons left');
    var openLogs = btn('Open logs folder', '', function () {
      api().OpenLogsFolder().then(function () { setStatus('Opened ' + a.logDir + '.'); }, function (e) { setStatus(errText(e), true); });
    });
    openLogs.id = 'settings-open-logs';
    var copy = btn('Copy diagnostics', 'primary', function () {
      api().Diagnostics().then(function (t) {
        pre.textContent = t;
        return window.labmillMarkdown.copyText(t, pre).then(function (ok) {
          setStatus(ok ? 'Diagnostics copied. They hold no passwords, keys or sign-in codes.' : 'The text is selected: press Ctrl+C (or Cmd+C) to copy it.');
        });
      }, function (e) { setStatus(errText(e), true); });
    });
    copy.id = 'settings-copy-diag';
    row.appendChild(openLogs); row.appendChild(copy);
    pane.appendChild(row);
    statusLine(pane);
    pane.appendChild(el('h4', '', 'What Copy diagnostics copies'));
    var pre = el('pre', 'settings-diag');
    pre.id = 'settings-diag';
    pre.tabIndex = 0;
    pre.setAttribute('aria-label', 'Diagnostics text');
    pane.appendChild(pre);
    api().Diagnostics().then(function (t) { pre.textContent = t; }, function () {});
  }

  // ---- wiring ------------------------------------------------------------------
  function init() {
    if (!window.runtime || !api()) { setTimeout(init, 50); return; }
    $('settings-open').addEventListener('click', function () { if (state.open) close(); else open(); });
    $('settings-back').addEventListener('click', close);
    $('settings-nav').addEventListener('keydown', onNavKey);
    $('settings').addEventListener('keydown', function (e) {
      if (e.key !== 'Escape' || e.defaultPrevented) return;
      var t = e.target.tagName;
      if (t === 'INPUT' || t === 'SELECT' || t === 'TEXTAREA') {
        if (state.editing) { e.preventDefault(); state.editing = null; setStatus(''); render(false); }
        return;
      }
      e.preventDefault();
      close();
    });
    window.runtime.EventsOn('Labmill:open-settings', function () { open(); });
    window.runtime.EventsOn('Labmill:settings-changed', function () { if (state.open && !state.editing) refresh(false); });
  }

  window.labmillSettings = { enable: enable, open: open, close: close, isOpen: function () { return state.open; } };
  document.addEventListener('DOMContentLoaded', init);
})();
