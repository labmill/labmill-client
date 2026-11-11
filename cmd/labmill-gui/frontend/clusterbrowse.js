// The Cluster tab of the folder picker: a lazy tree of folders on a cluster, over the open SSH session. One level
// loads when the user opens it. "Use this folder" puts a sentence with the path into the chat box; nothing is read
// or sent until the user sends the message.
(function () {
  'use strict';
  var $ = function (id) { return document.getElementById(id); };
  var panel = $('cluster-browser'), sel = $('cb-cluster'), crumb = $('cb-path'), list = $('cb-list'), err = $('cb-error');
  var cluster = '', cur = '', seq = 0, use = $('cb-use');
  function setCur(p) { cur = p; use.disabled = !p; }
  function api() { return window.go && window.go.main && window.go.main.PageAPI; }
  function errText(e) { return (e && e.message) ? e.message : String(e); }
  function join(dir, name) { return dir === '/' ? '/' + name : dir + '/' + name; }
  function parent(dir) { var i = dir.lastIndexOf('/'); return i <= 0 ? '/' : dir.slice(0, i); }

  function item(label, fn, cls) {
    var li = document.createElement('li'), b = document.createElement('button');
    b.type = 'button';
    b.textContent = label;
    if (cls) b.className = cls;
    b.addEventListener('click', fn);
    li.appendChild(b);
    list.appendChild(li);
    return b;
  }

  function load(path) {
    var a = api();
    if (!a || !cluster) return;
    var mine = ++seq, forCluster = cluster;
    setCur('');
    err.textContent = '';
    a.BrowseCluster(forCluster, path).then(function (r) {
      if (mine !== seq) return;
      setCur(r.path);
      crumb.textContent = r.path + (r.writable ? '' : ' (read only for you)');
      list.textContent = '';
      if (r.path !== '/') item('.. (up)', function () { load(parent(r.path)); }, 'cb-up');
      r.dirs.forEach(function (d) { item(d, function () { load(join(r.path, d)); }, 'cb-dir'); });
      if (!r.dirs.length) { var li = document.createElement('li'); li.textContent = 'No sub-folders.'; list.appendChild(li); }
    }, function (e) { if (mine === seq) err.textContent = errText(e); });
  }

  function open() {
    var a = api();
    if (!a) return;
    seq++;
    cluster = '';
    setCur('');
    a.ClusterNames().then(function (names) {
      sel.textContent = '';
      names.forEach(function (n) { var o = document.createElement('option'); o.value = o.textContent = n; sel.appendChild(o); });
      panel.hidden = false;
      if (!names.length) { err.textContent = 'No cluster is set up yet.'; return; }
      cluster = names[0];
      load('');
    }, function (e) { panel.hidden = false; err.textContent = errText(e); });
  }
  function close() { panel.hidden = true; }

  sel.addEventListener('change', function () { cluster = sel.value; load(''); });
  use.addEventListener('click', function () {
    var inp = $('input');
    if (cur) inp.value = (inp.value ? inp.value + ' ' : '') + 'Use the folder `' + cur + '` on ' + cluster + '.';
    close();
    inp.focus();
  });
  $('cb-close').addEventListener('click', close);
  panel.addEventListener('keydown', function (e) { if (e.key === 'Escape') { e.preventDefault(); close(); } });
  window.labmillClusterBrowse = { open: open, close: close };
})();
