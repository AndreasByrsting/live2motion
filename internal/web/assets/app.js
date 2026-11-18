/* immich2motion 面板前端：SSE 实时日志 + 状态徽章 + 轻量交互 */
(function () {
  'use strict';

  var state = { level: 'all', autoscroll: true };

  /* ---------- 主题：按本地时间自动切换 ---------- */
  function currentTheme() {
    var forced = null;
    try { forced = new URLSearchParams(location.search).get('theme'); } catch (e) {}
    if (forced === 'light' || forced === 'dark') return forced;
    var h = new Date().getHours();
    return (h >= 6 && h < 18) ? 'light' : 'dark';
  }

  function applyTheme() {
    var t = currentTheme();
    if (document.documentElement.getAttribute('data-theme') !== t) {
      document.documentElement.setAttribute('data-theme', t);
    }
    var label = document.getElementById('theme-label');
    if (label) label.textContent = (t === 'light' ? '日间' : '夜间');
    var meta = document.querySelector('meta[name="theme-color"]');
    if (meta) meta.setAttribute('content', t === 'light' ? '#f0f4fa' : '#070a12');
  }

  // 页面停留期间跨过 06:00 / 18:00 时自动换主题
  setInterval(applyTheme, 5 * 60 * 1000);

  function esc(s) {
    return String(s == null ? '' : s)
      .replace(/&/g, '&amp;').replace(/</g, '&lt;')
      .replace(/>/g, '&gt;').replace(/"/g, '&quot;');
  }

  function pad(n) { return n < 10 ? '0' + n : '' + n; }

  function fmtTime(ts) {
    if (!ts) return '-';
    var d = new Date(ts * 1000);
    return pad(d.getMonth() + 1) + '-' + pad(d.getDate()) + ' ' +
      pad(d.getHours()) + ':' + pad(d.getMinutes()) + ':' + pad(d.getSeconds());
  }

  /* ---------- toast ---------- */
  function scheduleToasts(root) {
    (root || document).querySelectorAll('.toast').forEach(function (el) {
      if (el.dataset.timer) return;
      el.dataset.timer = '1';
      setTimeout(function () {
        el.style.opacity = '0';
        setTimeout(function () { el.remove(); }, 320);
      }, 3800);
    });
  }
  document.addEventListener('DOMContentLoaded', function () {
    applyTheme();
    scheduleToasts(document);
    connect();
  });

  document.body.addEventListener('htmx:afterSwap', function () { scheduleToasts(document); });

  /* ---------- 日志过滤 ---------- */
  // 级别匹配：选 warn 时 error 也要显示（错误比警告更严重，过滤掉反而漏信息）
  function matchLevel(lv) {
    if (state.level === 'all') return true;
    if (state.level === 'warn') return lv === 'warn' || lv === 'error';
    if (state.level === 'error') return lv === 'error';
    if (state.level === 'info') return lv === 'info';
    return true;
  }

  function applyFilter() {
    document.querySelectorAll('.log-line').forEach(function (el) {
      el.style.display = matchLevel(el.dataset.level) ? '' : 'none';
    });
  }

  document.addEventListener('click', function (e) {
    var btn = e.target.closest && e.target.closest('#level-filter .seg');
    if (!btn) return;
    document.querySelectorAll('#level-filter .seg').forEach(function (b) {
      b.classList.remove('seg-active');
    });
    btn.classList.add('seg-active');
    state.level = btn.dataset.level || 'all';
    applyFilter();
  });

  var autoBox = document.getElementById('autoscroll');
  if (autoBox) {
    autoBox.addEventListener('change', function () { state.autoscroll = autoBox.checked; });
  }

  var logBox = document.getElementById('log-box');
  if (logBox) {
    logBox.addEventListener('scroll', function () {
      var atBottom = logBox.scrollHeight - logBox.scrollTop - logBox.clientHeight < 48;
      if (!autoBox) return;
      if (atBottom !== autoBox.checked) autoBox.checked = atBottom;
      state.autoscroll = autoBox.checked;
    });
  }

  function appendLog(entry) {
    var box = document.getElementById('log-box');
    if (!box) return;
    // 不符合当前筛选的日志直接丢弃，不在 DOM 里堆不可见的行
    if (!matchLevel(entry.level)) return;

    var empty = document.getElementById('log-empty');
    if (empty) empty.remove();

    var color = entry.level === 'error' ? 'text-rose-400'
      : entry.level === 'warn' ? 'text-amber-400' : 'text-sky-400';

    var row = document.createElement('div');
    row.className = 'log-line flex gap-2 px-3 py-[3px] hover:bg-slate-900/50 border-b border-slate-900/60';
    row.dataset.level = entry.level;
    row.innerHTML =
      '<span class="shrink-0 text-slate-600">' + esc(fmtTime(entry.ts)) + '</span>' +
      '<span class="shrink-0 w-11 font-semibold ' + color + '">' + esc(entry.level) + '</span>' +
      '<span class="shrink-0 w-20 text-slate-500 truncate">' + esc(entry.scope || '') + '</span>' +
      '<span class="min-w-0 break-all text-slate-300">' + esc(entry.message) + '</span>';
    box.appendChild(row);

    applyFilter();
    if (state.autoscroll) box.scrollTop = box.scrollHeight;

    var lines = box.querySelectorAll('.log-line');
    if (lines.length > 2000) {
      for (var i = 0; i < lines.length - 2000; i++) lines[i].remove();
    }
  }

  /* ---------- 状态徽章 ---------- */
  function toggleBadge(id, on) {
    var el = document.getElementById(id);
    if (!el) return;
    el.classList.toggle('hidden', !on);
  }

  function applySnapshot(d) {
    if (!d) return;
    toggleBadge('hdr-paused', !!d.paused);
    toggleBadge('hdr-dry', !!d.dry_run);
    toggleBadge('hdr-running', !!(d.running && d.running.length));
    toggleBadge('hdr-scan', !!d.scanning);
  }

  /* ---------- SSE ---------- */
  var dashTimer = null;
  function refreshDashboard() {
    if (dashTimer) return;
    dashTimer = setTimeout(function () {
      dashTimer = null;
      var el = document.getElementById('dashboard-body');
      if (el && window.htmx) window.htmx.trigger(el, 'refresh');
    }, 600);
  }

  function setSSE(online) {
    var dot = document.getElementById('sse-dot');
    var txt = document.getElementById('sse-text');
    if (!dot || !txt) return;
    dot.className = 'dot ' + (online ? 'bg-emerald-400' : 'bg-rose-500');
    txt.textContent = online ? '实时连接已建立' : '连接断开，重试中…';
  }

  function connect() {
    if (!window.EventSource) { setSSE(false); return; }
    var es = new EventSource('/api/events');

    es.addEventListener('open', function () { setSSE(true); });
    es.addEventListener('error', function () { setSSE(false); });

    es.addEventListener('snapshot', function (e) {
      try { applySnapshot(JSON.parse(e.data)); } catch (_) {}
    });
    es.addEventListener('stats', function () { refreshDashboard(); });
    es.addEventListener('job', function (e) {
      refreshDashboard();
      try { applySnapshot({ running: [JSON.parse(e.data)] }); } catch (_) {}
    });
    es.addEventListener('scan', function () { refreshDashboard(); });
    es.addEventListener('log', function (e) {
      try { appendLog(JSON.parse(e.data)); } catch (_) {}
    });
  }
})();
