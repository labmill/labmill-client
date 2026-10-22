// Safe Markdown rendering for assistant text. Plain JS, no build step.
//
// Everything is built with DOM nodes: createElement with fixed tag names and
// createTextNode/textContent for every piece of text. No innerHTML, no
// attribute ever takes a value from the text (the only attribute set is an
// ordered list's numeric start), so nothing the model writes can become
// markup, script or a navigable link:
//   - links and images ([t](u), ![t](u)) become the plain text "t (u)";
//   - raw HTML stays literal text;
//   - an unterminated code fence renders as a code block to the end (so a
//     half-received reply never breaks the page).
// Supported: headings (# to ###; deeper ones look like ###), paragraphs,
// **bold**, __bold__, *italic*, _italic_ (only at word boundaries, so
// snake_case stays as is), ~~strike~~, `code`, fenced code blocks, - * +
// and 1. lists (nested by indentation), > quotes, horizontal rules and
// simple | tables. Every fenced code block gets a "Copy" button (aria-label
// "Copy") that copies the block's text (copyText: the clipboard API, else
// the block's text is selected for Ctrl+C).
(function () {
  'use strict';

  function node(tag, cls) {
    var n = document.createElement(tag);
    if (cls) n.className = cls;
    return n;
  }
  function text(s) { return document.createTextNode(s); }

  // ---- inline -------------------------------------------------------
  var PUNCT = '!"#$%&\'()*+,-./:;<=>?@[\\]^_`{|}~';
  function isSpace(c) { return c === undefined || c === '' || /\s/.test(c); }
  function isWordChar(c) { return c !== undefined && /[A-Za-z0-9]/.test(c); }

  // findClose returns the index of the closing delimiter d for an opening at
  // i (content starts at i + d.length), or -1. The content must not start or
  // end with a space and must not be empty.
  // The scan stops after SCAN_MAX characters or at a blank line, so a long
  // run of unmatched delimiters can't make rendering quadratic.
  // budget is the total number of characters all delimiter scans of one
  // render() may look at (about 40 per input character): past it, nothing
  // more is matched and the rest renders as literal text.
  var SCAN_MAX = 1000, DEPTH_MAX = 16, budget = 0;
  function findClose(s, i, d, wordBoundary) {
    var start = i + d.length;
    if (start >= s.length || isSpace(s[start])) return -1;
    var j = start, limit = start + SCAN_MAX;
    while (true) {
      j = s.indexOf(d, j);
      if (j < 0 || j > limit) { budget -= Math.min(SCAN_MAX, s.length - start); return -1; }
      budget -= j - start + 1;
      if (budget < 0) return -1;
      if (s.slice(start, j).indexOf('\n\n') >= 0) return -1;
      if (j > start && !isSpace(s[j - 1]) && s[j - 1] !== '\\' &&
          (!wordBoundary || !isWordChar(s[j + d.length]))) {
        // "**" must not close a "*" run (e.g. "*a**").
        if (d.length === 1 && s[j + 1] === d) { j += 2; continue; }
        return j;
      }
      j += d.length;
    }
  }

  // matchBracket returns the index of the "]" matching the "[" at i, or -1.
  function matchBracket(s, i) {
    var depth = 0, end = Math.min(s.length, i + SCAN_MAX);
    if (budget < 0) return -1;
    budget -= end - i;
    for (var j = i; j < end; j++) {
      var c = s[j];
      if (c === '\\') { j++; continue; }
      if (c === '\n' && s[j + 1] === '\n') return -1;
      if (c === '`') { var e = s.indexOf('`', j + 1); if (e < 0 || e >= end) return -1; j = e; continue; }
      if (c === '[') depth++;
      else if (c === ']') { depth--; if (depth === 0) return j; }
    }
    return -1;
  }

  // matchParen returns the index of the ")" closing the "(" at i, or -1.
  function matchParen(s, i) {
    var depth = 0, end = Math.min(s.length, i + SCAN_MAX);
    if (budget < 0) return -1;
    budget -= end - i;
    for (var j = i; j < end; j++) {
      var c = s[j];
      if (c === '\\') { j++; continue; }
      if (c === '\n') return -1;
      if (c === '(') depth++;
      else if (c === ')') { depth--; if (depth === 0) return j; }
    }
    return -1;
  }

  // dropTitle cuts a link title: from the first whitespace run that is
  // followed by a quote or "(".
  function dropTitle(u) {
    for (var k = 0; k < u.length; k++) {
      if (/\s/.test(u[k])) {
        var n = k;
        while (n < u.length && /\s/.test(u[n])) n++;
        if (n < u.length && '"\'('.indexOf(u[n]) >= 0) return u.slice(0, k);
        k = n - 1;
      }
    }
    return u;
  }

  function inline(parent, s, depth) {
    depth = depth || 0;
    if (depth > DEPTH_MAX) { if (s) parent.appendChild(text(s)); return; }
    var buf = '';
    function flush() { if (buf) { parent.appendChild(text(buf)); buf = ''; } }
    function wrap(tag, inner) { flush(); var n = node(tag); inline(n, inner, depth + 1); parent.appendChild(n); }
    var i = 0;
    while (i < s.length) {
      var c = s[i];
      if (c === '\\' && i + 1 < s.length && PUNCT.indexOf(s[i + 1]) >= 0) { buf += s[i + 1]; i += 2; continue; }
      if (c === '\n') { flush(); parent.appendChild(node('br')); i++; continue; }
      if (c === '`') {
        var run = 1; while (s[i + run] === '`') run++;
        var fence = s.substr(i, run), close = s.indexOf(fence, i + run);
        while (close >= 0 && s[close + run] === '`') close = s.indexOf(fence, close + run + 1);
        if (close < 0) { buf += fence; i += run; continue; }
        var code = s.slice(i + run, close).replace(/\n/g, ' ');
        if (code.length > 2 && code[0] === ' ' && code[code.length - 1] === ' ') code = code.slice(1, -1);
        flush();
        var cn = node('code'); cn.textContent = code; parent.appendChild(cn);
        i = close + run; continue;
      }
      if (c === '[' || (c === '!' && s[i + 1] === '[')) {
        var b0 = c === '!' ? i + 1 : i, b1 = matchBracket(s, b0);
        if (b1 > 0 && s[b1 + 1] === '(') {
          var p1 = matchParen(s, b1 + 1);
          if (p1 > 0) {
            var label = s.slice(b0 + 1, b1), url = s.slice(b1 + 2, p1).trim();
            url = dropTitle(url); // drop a link title
            flush();
            var span = node('span', 'md-link'); inline(span, label, depth + 1); parent.appendChild(span);
            if (url && url !== label) parent.appendChild(text(' (' + url + ')'));
            i = p1 + 1; continue;
          }
        }
      }
      if ((c === '*' || c === '_') && s[i + 1] === c && s[i + 2] === c) {
        var tri = c + c + c, tj = findClose(s, i, tri, c === '_');
        if (tj > 0 && !(c === '_' && isWordChar(s[i - 1]))) {
          flush();
          var st = node('strong'), em = node('em');
          inline(em, s.slice(i + 3, tj), depth + 1);
          st.appendChild(em); parent.appendChild(st);
          i = tj + 3; continue;
        }
      }
      if (c === '*' || c === '_' || c === '~') {
        var dbl = s[i + 1] === c, d = dbl ? c + c : c;
        if (c === '~' && !dbl) { buf += c; i++; continue; }
        var wb = c === '_';
        if (wb && isWordChar(s[i - 1])) { buf += d; i += d.length; continue; }
        var j = findClose(s, i, d, wb);
        if (j > 0) {
          wrap(c === '~' ? 'del' : (dbl ? 'strong' : 'em'), s.slice(i + d.length, j));
          i = j + d.length; continue;
        }
        buf += d; i += d.length; continue;
      }
      buf += c; i++;
    }
    flush();
  }

  // ---- blocks -------------------------------------------------------
  var reFence = /^ {0,3}(`{3,}|~{3,})(.*)$/;
  var reHeading = /^ {0,3}(#{1,6})(?:[ \t]+(.*))?$/;
  var reQuote = /^ {0,3}> ?(.*)$/;
  var reUl = /^( *)([-*+])[ \t]+(.*)$/;
  var reOl = /^( *)(\d{1,9})[.)][ \t]+(.*)$/;
  var reTableCell = /^:?-+:?$/;

  function isWs(c) { return c === ' ' || c === '\t'; }

  // isHr reports a thematic break (three or more -, * or _, spaces allowed).
  // Plain loops, no regexes that can backtrack.
  function isHr(line) {
    var i = 0;
    while (i < line.length && line[i] === ' ') i++;
    if (i > 3 || i >= line.length) return false;
    var ch = line[i], n = 0;
    if (ch !== '-' && ch !== '*' && ch !== '_') return false;
    for (; i < line.length; i++) {
      if (line[i] === ch) n++;
      else if (!isWs(line[i])) return false;
    }
    return n >= 3;
  }

  // isTableSep reports a table delimiter row such as "|---|:-:|".
  function isTableSep(line) {
    var t = line.trim();
    if (t[0] === '|') t = t.slice(1);
    if (t[t.length - 1] === '|') t = t.slice(0, -1);
    var cells = t.split('|');
    for (var i = 0; i < cells.length; i++) if (!reTableCell.test(cells[i].trim())) return false;
    return true;
  }

  // headingText strips trailing whitespace and an optional closing run of #.
  function headingText(c) {
    var end = c.length;
    while (end > 0 && isWs(c[end - 1])) end--;
    var k = end;
    while (k > 0 && c[k - 1] === '#') k--;
    if (k === 0 || isWs(c[k - 1])) end = k;
    while (end > 0 && isWs(c[end - 1])) end--;
    return c.slice(0, end);
  }

  function listMatch(line) {
    var m = reUl.exec(line);
    if (m) return { indent: m[1].length, ordered: false, num: 1, rest: m[3] };
    m = reOl.exec(line);
    if (m) return { indent: m[1].length, ordered: true, num: parseInt(m[2], 10), rest: m[3] };
    return null;
  }

  function splitRow(line) {
    var t = line.trim();
    if (t[0] === '|') t = t.slice(1);
    if (t[t.length - 1] === '|' && t[t.length - 2] !== '\\') t = t.slice(0, -1);
    var cells = [], cur = '';
    for (var i = 0; i < t.length; i++) {
      if (t[i] === '\\' && t[i + 1] === '|') { cur += '|'; i++; continue; }
      if (t[i] === '|') { cells.push(cur.trim()); cur = ''; continue; }
      cur += t[i];
    }
    cells.push(cur.trim());
    return cells;
  }

  function startsBlock(line) {
    return reFence.test(line) || reHeading.test(line) || isHr(line) || reQuote.test(line) || !!listMatch(line);
  }

  function blocks(parent, lines, depth) {
    depth = depth || 0;
    if (depth > DEPTH_MAX) {
      var lit = node('p');
      lit.appendChild(text(lines.join('\n')));
      parent.appendChild(lit);
      return;
    }
    var i = 0;
    while (i < lines.length) {
      var line = lines[i], m;
      if (!line.trim()) { i++; continue; }

      if ((m = reFence.exec(line))) {
        var fence = m[1], body = [];
        i++;
        while (i < lines.length) {
          var cl = lines[i].trim();
          if (cl[0] === fence[0] && cl.length >= fence.length && /^(`+|~+)$/.test(cl)) { i++; break; }
          body.push(lines[i]); i++;
        }
        var wrap = node('div', 'md-codewrap'), pre = node('pre', 'md-code'), code = node('code');
        code.textContent = body.join('\n');
        pre.appendChild(code);
        wrap.appendChild(copyButton(code, 'Copy', 'Copy this block'));
        wrap.appendChild(pre); parent.appendChild(wrap);
        continue;
      }
      if ((m = reHeading.exec(line)) && m[1].length >= 1) {
        var level = Math.min(m[1].length, 3);
        var h = node('h' + (level + 2), 'md-h');
        inline(h, headingText(m[2] || ''), depth + 1);
        parent.appendChild(h); i++;
        continue;
      }
      if (isHr(line)) { parent.appendChild(node('hr')); i++; continue; }
      if (reQuote.test(line)) {
        var inner = [];
        while (i < lines.length && (m = reQuote.exec(lines[i]))) { inner.push(m[1]); i++; }
        var bq = node('blockquote');
        blocks(bq, inner, depth + 1);
        parent.appendChild(bq);
        continue;
      }
      var lm = listMatch(line);
      if (lm) { i = list(parent, lines, i, lm, depth); continue; }
      if (line.indexOf('|') >= 0 && i + 1 < lines.length && isTableSep(lines[i + 1])) {
        i = table(parent, lines, i, depth);
        continue;
      }
      var para = [];
      while (i < lines.length && lines[i].trim() && !(para.length && startsBlock(lines[i]))) {
        para.push(lines[i].trim());
        i++;
      }
      var p = node('p');
      inline(p, para.join('\n'), depth + 1);
      parent.appendChild(p);
    }
  }

  // list renders the list that starts at lines[i] and returns the index
  // after it. Items continue on lines indented past the marker; a deeper
  // marker starts a nested list inside the item.
  function list(parent, lines, i, first, depth) {
    var el = node(first.ordered ? 'ol' : 'ul');
    if (first.ordered && first.num !== 1) el.start = first.num;
    var base = first.indent;
    while (i < lines.length) {
      var lm = listMatch(lines[i]);
      if (!lm || lm.indent !== base || lm.ordered !== first.ordered) break;
      var item = [lm.rest];
      i++;
      while (i < lines.length) {
        var l = lines[i];
        if (!l.trim()) {
          // A blank line continues the item only if indented text follows.
          var nx = lines[i + 1];
          if (nx !== undefined && nx.trim() && /^ +/.exec(nx) && /^ +/.exec(nx)[0].length > base) { item.push(''); i++; continue; }
          break;
        }
        var ind = /^ */.exec(l)[0].length;
        var sub = listMatch(l);
        if (sub && sub.indent <= base) break;
        if (ind > base) { item.push(l.slice(Math.min(ind, base + 2))); i++; continue; }
        if (sub || startsBlock(l)) break;
        item.push(l.trim()); i++; // lazy continuation
      }
      var li = node('li');
      if (item.length === 1) inline(li, item[0], depth + 1);
      else blocks(li, item, depth + 1);
      el.appendChild(li);
      // Skip blank lines between items of the same list.
      var k = i;
      while (k < lines.length && !lines[k].trim()) k++;
      var again = k < lines.length && listMatch(lines[k]);
      if (again && again.indent === base && again.ordered === first.ordered) i = k;
    }
    parent.appendChild(el);
    return i;
  }

  function table(parent, lines, i, depth) {
    var wrapEl = node('div', 'md-table');
    var t = node('table'), thead = node('thead'), tbody = node('tbody'), tr = node('tr');
    var head = splitRow(lines[i]);
    head.forEach(function (c) { var th = node('th'); inline(th, c, depth + 1); tr.appendChild(th); });
    thead.appendChild(tr); t.appendChild(thead);
    i += 2;
    while (i < lines.length && lines[i].trim() && lines[i].indexOf('|') >= 0) {
      var row = node('tr'), cells = splitRow(lines[i]);
      for (var k = 0; k < head.length; k++) {
        var td = node('td'); inline(td, cells[k] || '', depth + 1); row.appendChild(td);
      }
      tbody.appendChild(row); i++;
    }
    t.appendChild(tbody); wrapEl.appendChild(t); parent.appendChild(wrapEl);
    return i;
  }

  // ---- copying -------------------------------------------------------
  // copyText puts text on the clipboard (navigator.clipboard.writeText) and
  // resolves true; where the clipboard API is missing or refuses, it selects
  // the text of fallbackEl instead (so Ctrl+C / Cmd+C copies it) and
  // resolves false. It never rejects.
  function copyText(text, fallbackEl) {
    function select() {
      try {
        if (fallbackEl && fallbackEl.select) { fallbackEl.focus(); fallbackEl.select(); return false; }
        if (fallbackEl) {
          var r = document.createRange();
          r.selectNodeContents(fallbackEl);
          var sel = window.getSelection();
          sel.removeAllRanges();
          sel.addRange(r);
        }
      } catch (e) { /* nothing more to do */ }
      return false;
    }
    try {
      var cb = navigator.clipboard;
      if (cb && typeof cb.writeText === 'function') {
        return Promise.resolve(cb.writeText(text)).then(function () { return true; }, select);
      }
    } catch (e) { /* fall through to the selection */ }
    return Promise.resolve(select());
  }

  // copyButton is a "Copy" button (aria-label label) for the text of el; a
  // click copies it and briefly says what happened. Its handler is attached
  // with addEventListener (the page's CSP allows no inline handlers).
  function copyButton(el, label, title) {
    var b = node('button', 'copy');
    b.type = 'button';
    b.textContent = 'Copy';
    b.title = title || 'Copy';
    b.setAttribute('aria-label', label || 'Copy');
    b.addEventListener('click', function () {
      copyText(el.textContent, el).then(function (ok) {
        b.textContent = ok ? 'Copied' : 'Press Ctrl+C';
        setTimeout(function () { b.textContent = 'Copy'; }, 1500);
      });
    });
    return b;
  }

  // render replaces el's content with the rendered Markdown of src.
  function render(el, src) {
    el.textContent = '';
    src = String(src || '');
    budget = 40 * src.length + 20000;
    blocks(el, src.replace(/\r\n?/g, '\n').split('\n'));
  }

  window.labmillMarkdown = { render: render, copyText: copyText, copyButton: copyButton };
})();
