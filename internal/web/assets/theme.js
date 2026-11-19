/* live2motion 主题控制：按本地时间自动切换 + 手动临时覆盖
 *
 * 这个文件在 <head> 里同步加载（先于样式表），首帧就能决定配色，
 * 不会出现「先闪一下深色」。规则：
 *   - 后台开关 auto_theme 打开（html[data-auto-theme=1]）：06:00–18:00 日间，其余夜间；
 *   - 侧边栏按钮手动切换后，只在「当前时段内」生效，跨过 06:00 / 18:00 自动回到自动模式；
 *   - 后台开关关闭：完全听手动选择，默认夜间；
 *   - URL 上的 ?theme=light|dark 优先级最高，便于截图与验收。
 */
(function () {
  'use strict';

  var KEY = 'l2m-theme';

  function readPref() {
    try { return JSON.parse(localStorage.getItem(KEY) || 'null'); } catch (e) { return null; }
  }

  function writePref(p) {
    try {
      if (p) localStorage.setItem(KEY, JSON.stringify(p));
      else localStorage.removeItem(KEY);
    } catch (e) { /* 隐私模式下不可写，忽略即可 */ }
  }

  function forcedFromURL() {
    try {
      var t = new URLSearchParams(location.search).get('theme');
      return (t === 'light' || t === 'dark') ? t : null;
    } catch (e) { return null; }
  }

  function autoEnabled() {
    return document.documentElement.getAttribute('data-auto-theme') !== '0';
  }

  function timeTheme() {
    var h = new Date().getHours();
    return (h >= 6 && h < 18) ? 'light' : 'dark';
  }

  /* 下一次 06:00 或 18:00 的时间戳：手动覆盖的有效期到此为止 */
  function nextBoundary() {
    var now = new Date();
    var t = now.getTime();
    var s = new Date(now), e = new Date(now);
    s.setHours(6, 0, 0, 0);
    e.setHours(18, 0, 0, 0);
    var list = [s.getTime(), e.getTime(), s.getTime() + 86400000, e.getTime() + 86400000];
    list.sort(function (a, b) { return a - b; });
    for (var i = 0; i < list.length; i++) {
      if (list[i] > t) return list[i];
    }
    return t + 86400000;
  }

  function current() {
    var forced = forcedFromURL();
    if (forced) return forced;
    var pref = readPref();
    var manual = pref && (pref.theme === 'light' || pref.theme === 'dark') ? pref : null;
    if (autoEnabled()) {
      if (manual && manual.until && Date.now() < manual.until) return manual.theme;
      return timeTheme();
    }
    return manual ? manual.theme : 'dark';
  }

  function apply() {
    var t = current();
    var root = document.documentElement;
    if (root.getAttribute('data-theme') !== t) root.setAttribute('data-theme', t);

    var sun = document.getElementById('theme-icon-sun');
    var moon = document.getElementById('theme-icon-moon');
    if (sun) sun.classList.toggle('hidden', t !== 'light');
    if (moon) moon.classList.toggle('hidden', t !== 'dark');

    var btn = document.getElementById('theme-toggle');
    if (btn) {
      btn.setAttribute('title', t === 'light' ? '当前：日间配色，点击切换为夜间' : '当前：夜间配色，点击切换为日间');
      btn.setAttribute('aria-label', btn.getAttribute('title'));
    }

    var meta = document.querySelector('meta[name="theme-color"]');
    if (meta) meta.setAttribute('content', t === 'light' ? '#f0f4fa' : '#070a12');
    return t;
  }

  function toggle() {
    var next = current() === 'light' ? 'dark' : 'light';
    // 自动模式下手动选择是「临时」的，跨过日出/日落时间就交还给自动逻辑
    writePref({ theme: next, until: autoEnabled() ? nextBoundary() : 0 });
    return apply();
  }

  window.l2mTheme = { apply: apply, toggle: toggle, current: current };

  apply();

  document.addEventListener('click', function (e) {
    var btn = e.target.closest && e.target.closest('#theme-toggle');
    if (!btn) return;
    e.preventDefault();
    toggle();
  });

  // 页面长时间停留时跨过 06:00 / 18:00，或临时覆盖到期，都要重新计算
  setInterval(apply, 30 * 1000);
})();
