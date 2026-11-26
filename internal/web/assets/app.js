/* live2motion 面板前端：SSE 实时日志 + 状态徽章 + 轻量交互 */
(function () {
  'use strict';

  var state = { level: 'all', autoscroll: true };

  /* ---------- 主题 ---------- */
  // 具体逻辑在 theme.js（<head> 里同步加载，负责首帧配色）。
  // 这里只保证 DOM 就绪后把图标与按钮状态同步一次。
  function applyTheme() {
    if (window.l2mTheme) window.l2mTheme.apply();
  }

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
        el.style.transform = 'translate3d(10px, 0, 0) scale(0.98)';
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

  /* ---------- 设置页：有改动才滑出保存条 ---------- */
  var settingsForm = document.querySelector('form[hx-post="/api/settings"]');
  if (settingsForm) {
    var saveBar = document.querySelector('.save-bar');
    var markDirty = function () {
      if (saveBar) saveBar.classList.add('is-visible');
    };
    settingsForm.addEventListener('input', markDirty);
    settingsForm.addEventListener('change', markDirty);
    document.body.addEventListener('htmx:afterRequest', function (e) {
      if (!saveBar || !e.detail || e.detail.elt !== settingsForm) return;
      if (e.detail.successful) saveBar.classList.remove('is-visible');
    });
  }

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
    document.querySelectorAll('.log-row').forEach(function (el) {
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

    var row = document.createElement('div');
    row.className = 'log-row log-' + (entry.level || 'info');
    row.dataset.level = entry.level;
    row.innerHTML =
      '<span class="log-time">' + esc(fmtTime(entry.ts)) + '</span>' +
      '<span class="log-level">' + esc(entry.level) + '</span>' +
      '<span class="log-scope">' + esc(entry.scope || '') + '</span>' +
      '<span class="log-msg">' + esc(entry.message) + '</span>';
    box.appendChild(row);

    applyFilter();
    if (state.autoscroll) box.scrollTop = box.scrollHeight;

    var lines = box.querySelectorAll('.log-row');
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
  // delay 默认 600ms：SSE 事件很密时靠它合并成一次局部刷新。
  // 扫描进度事件更频繁，调用方传更大的值（1.2s），避免一边扫描一边频繁查库。
  function refreshDashboard(delay) {
    if (dashTimer) return;
    var wait = typeof delay === 'number' ? delay : 600;
    dashTimer = setTimeout(function () {
      dashTimer = null;
      var el = document.getElementById('dashboard-body');
      if (el && window.htmx) window.htmx.trigger(el, 'refresh');
    }, wait);
  }

  function setSSE(online) {
    var dot = document.getElementById('sse-dot');
    var txt = document.getElementById('sse-text');
    if (!dot || !txt) return;
    dot.className = 'dot ' + (online ? 'dot-live bg-emerald-400' : 'bg-rose-500');
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
    es.addEventListener('scan', function (e) {
      var st = null;
      try { st = JSON.parse(e.data); } catch (_) {}
      // start / progress 表示扫描进行中，done / error 表示结束；
      // active 字段优先，避免「扫描结束」那一帧因为 state 仍是 progress 而留下徽章。
      var active = st ? (st.active !== false && (st.state === 'start' || st.state === 'progress')) : false;
      toggleBadge('hdr-scan', active);
      refreshDashboard(st && st.state === 'progress' ? 1200 : 0);
    });
    es.addEventListener('log', function (e) {
      try { appendLog(JSON.parse(e.data)); } catch (_) {}
    });
  }
})();
