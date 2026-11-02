(() => {
  'use strict';
  const $ = id => document.getElementById(id);
  const KEY_STORAGE = 'codearts2api-panel.admin-key';
  const THEME_STORAGE = 'codearts2api-panel.theme';
  const pages = {
    accounts: ['账号池', '管理凭据、查看账户状态与额度。'],
    models: ['模型目录', '查看模型通道与能力，复制 ID 接入客户端。'],
    usage: ['请求用量', '当前进程的请求指标与近期用量。'],
    config: ['服务配置', '管理网关配置，保存即热生效；装配期字段需重启。'],
    logs: ['运行日志', '检查最近请求、错误与服务运行状态。']
  };
  const readStorage = (storage, name) => { try { return storage.getItem(name) || ''; } catch { return ''; } };
  const writeStorage = (storage, name, value) => { try { value ? storage.setItem(name, value) : storage.removeItem(name); } catch { /* Session can still run in memory. */ } };
  let adminKey = readStorage(sessionStorage, KEY_STORAGE);
  let authenticated = false, authEpoch = 0, refreshID = 0;
  let currentPage = 'accounts', accounts = [], models = [], runtimeLogs = [], requestRows = [], requestRowsFromArchive = false;
  let requestAttemptErrors = {}; // request_id → 最后一次上游错误（内存态，仅用于 tooltip）
  let oauthSession = '', oauthTimer = null, oauthBusy = false, deleteUID = '', logsLoading = false;
  let serverStartedAt = 0; // 由 overview 的 uptime_sec 换算，本地时钟每 30 秒刷新顶栏
  let theme = readStorage(localStorage, THEME_STORAGE) || (matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark');

  function esc(value) {
    return String(value ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
  }
  function number(value) {
    return value !== null && value !== undefined && Number.isFinite(Number(value)) ? Number(value).toLocaleString('zh-CN', { maximumFractionDigits: 3 }) : '未知';
  }
  function duration(value) {
    if (value === null || value === undefined || !Number.isFinite(Number(value))) return '—';
    return Number(value) < 1000 ? Math.round(Number(value)) + ' ms' : (Number(value) / 1000).toFixed(1) + ' s';
  }
  function date(value) {
    if (!value) return '未知';
    const parsed = new Date(value);
    return Number.isNaN(parsed.getTime()) ? '未知' : parsed.toLocaleString('zh-CN', { hour12: false });
  }
  // 顶栏运行时长，格式与 WorkBuddy 面板一致：运行 3 天 5 时 12 分。
  function uptimeText(ms) {
    const up = Math.max(0, Math.floor(ms / 1000));
    return '运行 ' + (up >= 86400 ? Math.floor(up / 86400) + ' 天 ' : '') + Math.floor(up % 86400 / 3600) + ' 时 ' + Math.floor(up % 3600 / 60) + ' 分';
  }
  function renderUptime() {
    if (serverStartedAt) $('uptime').textContent = uptimeText(Date.now() - serverStartedAt);
  }
  function emptyRow(columns, text) { return '<tr><td class="empty" colspan="' + columns + '">' + esc(text) + '</td></tr>'; }
  function errorText(data, status) {
    if (typeof data.error === 'string') return data.error;
    return data.error?.message || data.message || '请求失败（HTTP ' + status + '）';
  }
  function toast(message, failed = false) {
    const el = document.createElement('div');
    el.className = 'toast' + (failed ? ' error' : '');
    el.textContent = message;
    $('toasts').append(el);
    setTimeout(() => el.remove(), 6000);
  }
  function fieldError(id, message) { $(id).textContent = message; $(id).hidden = !message; }
  function showLogin(message = '') {
    authenticated = false;
    $('connection').textContent = '尚未连接';
    $('connection').classList.remove('online');
    ['addDialog', 'deleteDialog'].forEach(id => { if ($(id).open) $(id).close(); });
    fieldError('loginError', message);
    if (!$('loginDialog').open) $('loginDialog').showModal();
    $('loginKey').focus();
  }
  async function api(path, options = {}, keyOverride) {
    const epoch = authEpoch;
    const headers = new Headers(options.headers || {});
    const key = keyOverride === undefined ? adminKey : keyOverride;
    if (key) headers.set('Authorization', 'Bearer ' + key);
    if (options.body !== undefined) headers.set('Content-Type', 'application/json');
    const response = await fetch('/admin/api/' + path, { ...options, headers, cache: 'no-store', credentials: 'same-origin' });
    if (epoch !== authEpoch) throw new Error('会话已结束');
    const data = await response.json().catch(() => ({}));
    if (response.status === 401 || response.status === 403) {
      if (keyOverride === undefined) { adminKey = ''; writeStorage(sessionStorage, KEY_STORAGE, ''); showLogin('管理密钥无效，请重新连接。'); }
      throw new Error('管理密钥无效或权限不足。');
    }
    if (!response.ok) throw new Error(errorText(data, response.status));
    return data;
  }
  const post = (path, body = {}) => api(path, { method: 'POST', body: JSON.stringify(body) });
  function applyTheme() {
    document.documentElement.dataset.theme = theme;
    // 图标指示「当前已是哪个主题」：深色显示太阳，浅色显示月亮。
    $('themeIconSun').hidden = theme !== 'dark';
    $('themeIconMoon').hidden = theme !== 'light';
    $('themeButton').setAttribute('aria-label', theme === 'dark' ? '切换浅色主题' : '切换深色主题');
  }
  $('themeButton').onclick = () => { theme = theme === 'dark' ? 'light' : 'dark'; writeStorage(localStorage, THEME_STORAGE, theme); applyTheme(); };
  applyTheme();
  function menu(open) {
    $('sidebar').classList.toggle('open', open);
    $('navBackdrop').hidden = !open;
    $('menuButton').setAttribute('aria-expanded', String(open));
  }
  $('menuButton').onclick = () => menu(!$('sidebar').classList.contains('open'));
  $('navBackdrop').onclick = () => menu(false);
  function navigate() {
    const requested = location.hash.slice(1);
    currentPage = Object.hasOwn(pages, requested) ? requested : 'accounts';
    const [title, description] = pages[currentPage];
    $('pageTitle').textContent = title; $('pageDescription').textContent = description; $('breadcrumb').textContent = '工作空间 / ' + title;
    document.querySelectorAll('.view').forEach(el => { el.hidden = el.id !== 'view-' + currentPage; });
    document.querySelectorAll('[data-page]').forEach(el => { const active = el.dataset.page === currentPage; el.classList.toggle('active', active); active ? el.setAttribute('aria-current', 'page') : el.removeAttribute('aria-current'); });
    menu(false);
    if (authenticated) refresh();
  }
  addEventListener('hashchange', navigate);
  function renderOverview(data) {
    const uptimeSec = Number(data.uptime_sec);
    if (Number.isFinite(uptimeSec)) {
      serverStartedAt = Date.now() - uptimeSec * 1000;
      $('uptime').title = '服务启动于 ' + date(data.started_at);
      renderUptime();
    }
    accounts = data.accounts || [];
    const stats = data.stats || {};
    [['statTotal', 'total'], ['statHealthy', 'healthy'], ['statCooling', 'cooling'], ['statDisabled', 'disabled']].forEach(([id, name]) => { $(id).textContent = number(stats[name]); });
    $('accountCount').textContent = number(accounts.length);
    $('connection').textContent = stats.healthy > 0 ? '服务正常' : accounts.length ? '暂无可用账户' : '等待添加账户';
    $('connection').classList.add('online');
    renderAccounts();
  }
  function balanceCell(balance, field, error) {
    if (!balance || balance[field] === null || balance[field] === undefined) return '<span class="muted">未知</span>' + (error ? '<small class="account-error" title="' + esc(error) + '">查询失败</small>' : '');
    return '<span class="number">' + esc(number(balance[field])) + '</span>';
  }
  function renderAccounts() {
    const query = $('accountSearch').value.toLowerCase().trim();
    const filtered = accounts.filter(a => [a.nickname, a.user_name, a.uid, a.name].join(' ').toLowerCase().includes(query));
    $('accountsBody').innerHTML = filtered.map(a => {
      const uid = String(a.uid || a.name || '');
      const state = a.disabled ? ['bad', '已禁用'] : a.cooling ? ['warn', '冷却中'] : ['ok', '可用'];
      const button = (action, label, danger = false) => '<button class="button small' + (danger ? ' danger' : '') + '" data-account-action="' + action + '" data-uid="' + esc(uid) + '">' + label + '</button>';
      // 签到按自然日只领一次：了结后按钮变「已签」，点击只是回放本地结果，不再打上游。
      const checkinButton = a.checkin_done
        ? '<button class="button small done" data-account-action="checkin" data-uid="' + esc(uid) + '" title="' + esc((a.checkin && a.checkin.message ? a.checkin.message + ' · ' : '') + '今日已签到，每天只领一次') + '">已签</button>'
        : button('checkin', '签到');
      const expiry = a.permanent === true || a.credential_type === 'permanent' ? '永久凭据' : date(a.expires_at);
      return '<tr><td><span class="account-name">' + esc(a.nickname || a.user_name || a.name || '未命名账户') + '</span><span class="account-id mono">' + esc(uid) + '</span>' + (a.last_error ? '<span class="account-error" title="' + esc(a.last_error) + '">' + esc(a.last_error) + '</span>' : '') + '</td><td><span class="pill ' + state[0] + '">' + state[1] + '</span></td><td>' + balanceCell(a.credit_balance, 'total', a.credit_balance_error) + '</td><td><span class="muted">' + esc(expiry) + '</span></td><td class="number">' + esc(number(a.active_concurrent)) + ' / ' + esc(number(a.max_concurrent)) + '</td><td><div class="row-actions">' + button('credits', '查额度') + checkinButton + button('keepalive', '保活') + button(a.disabled ? 'enable' : 'disable', a.disabled ? '启用' : '禁用') + (a.cooling ? button('clear-cooldown', '清冷却') : '') + button('delete', '删除', true) + '</div></td></tr>';
    }).join('') || emptyRow(6, query ? '没有匹配的账户。' : '还没有账户。点击“添加账户”完成首次接入。');
  }
  function actionMessage(data, fallback) {
    const failed = Array.isArray(data.results) ? data.results.filter(r => r.ok === false) : [];
    const details = failed.map(r => (r.uid || '') + '：' + (r.message || r.credit_balance_error || '操作失败')).join('；');
    return [data.message || fallback, details].filter(Boolean).join(' · ');
  }
  async function accountAction(action, uid, button) {
    if (action === 'delete') {
      deleteUID = uid;
      const account = accounts.find(a => String(a.uid || a.name) === uid);
      $('deleteAccountName').textContent = (account?.nickname || account?.user_name || '账户') + ' · ' + uid;
      $('deleteDialog').showModal();
      return;
    }
    button.disabled = true;
    try {
      const route = ['enable', 'disable', 'clear-cooldown'].includes(action) ? 'accounts/' + action : action;
      const data = await post(route, { uid, ...(action === 'disable' ? { reason: '面板手动禁用' } : {}) });
      toast(actionMessage(data, '操作完成'), data.ok === false);
      renderOverview(await api('overview'));
    } catch (error) { toast(error.message, true); }
    finally { button.disabled = false; }
  }
  $('accountsBody').onclick = event => { const button = event.target.closest('[data-account-action]'); if (button) accountAction(button.dataset.accountAction, button.dataset.uid, button); };
  $('accountSearch').oninput = renderAccounts;
  document.querySelectorAll('[data-bulk]').forEach(button => { button.onclick = async () => {
    button.disabled = true;
    try { const data = await post(button.dataset.bulk); toast(actionMessage(data, '操作完成'), data.ok === false); renderOverview(await api('overview')); }
    catch (error) { toast(error.message, true); }
    finally { button.disabled = false; }
  }; });
  $('confirmDelete').onclick = async () => {
    if (!deleteUID) return;
    $('confirmDelete').disabled = true;
    try { const data = await post('accounts/delete', { uid: deleteUID }); if (data.ok === false) throw new Error(data.message || '删除失败'); $('deleteDialog').close(); toast('账户已删除'); renderOverview(await api('overview')); }
    catch (error) { toast(error.message, true); }
    finally { $('confirmDelete').disabled = false; }
  };
  $('deleteDialog').addEventListener('close', () => { deleteUID = ''; $('deleteAccountName').textContent = ''; });

  function renderModels() {
    const query = $('modelSearch').value.toLowerCase().trim();
    const filtered = models.filter(m => (String(m.id) + ' ' + (m.model_name || '')).toLowerCase().includes(query));
    $('modelCount').textContent = filtered.length;
    $('modelsBody').innerHTML = filtered.map(m => {
      const name = m.model_name ? '<small class="model-subtitle">' + esc(m.model_name) + '</small>' : '';
      // 模型 ID 列的原生 tooltip：模型描述，有 notice 时追加一行。
      const tip = [m.model_desc, m.notice].filter(Boolean).join('\n');
      const title = tip ? ' title="' + esc(tip).replace(/\n/g, '&#10;') + '"' : '';
      // 徽标行：上游标签（紫）、被隐藏的模型（橘）、输入模态（绿＝视觉理解 / 灰＝纯文本）。
      // 上游只在支持图片时才回 supports_images，因此「不是 true」就是纯文本；
      // 这样每个模型都能一眼看出能不能贴图，不必点开。
      const badges = [
        m.model_tag ? '<span class="pill tag">' + esc(m.model_tag) + '</span>' : '',
        m.display_enabled === false ? '<span class="pill warn">辅助模型</span>' : '',
        m.supports_images === true ? '<span class="pill ok">视觉理解</span>' : '<span class="pill">纯文本</span>'
      ].filter(Boolean).join('');
      const badgeRow = badges ? '<div class="model-badges">' + badges + '</div>' : '';
      return '<tr><td class="mono"' + title + '>' + esc(m.id) + name + badgeRow + '</td><td>' + (m.ratio_display ? esc(m.ratio_display) : '<span class="muted">未知</span>') + '</td><td><span class="pill">常规模型</span></td><td class="number">' + esc(number(m.context_length ?? m.context_window)) + '</td><td class="number">' + esc(number(m.max_output_tokens ?? m.max_tokens)) + '</td><td><button class="button small" data-copy-model="' + esc(m.id) + '">复制 ID</button></td></tr>';
    }).join('') || emptyRow(6, query ? '没有符合条件的模型。' : '模型目录为空，请添加账户后刷新。');
  }
  $('modelSearch').oninput = renderModels;
  $('modelsBody').onclick = async event => {
    const button = event.target.closest('[data-copy-model]');
    if (!button) return;
    try { await navigator.clipboard.writeText(button.dataset.copyModel); toast('模型 ID 已复制'); } catch { toast('浏览器无法访问剪贴板，请选中模型 ID 复制。', true); }
  };
  function eventOK(event) { return event.ok === true || (event.ok === undefined && event.status >= 200 && event.status < 300 && event.outcome !== 'stream_error'); }
  function tokenTotal(event) {
    if (event.total_tokens !== undefined && event.total_tokens !== null) return Number(event.total_tokens) || 0;
    return (Number(event.prompt_tokens) || 0) + (Number(event.completion_tokens) || 0);
  }
  // 进程指标随重启清零，归档（data/requests）才是跨重启的账本：两者合并去重后展示，
  // 归档里还没落盘的新请求也照样出现在列表里。没有归档时才退回纯进程数据。
  function usageTotals(recent) {
    let succeeded = 0, duration = 0;
    for (const row of recent) { succeeded += Number(eventOK(row)); duration += Number(row.duration_ms) || 0; }
    return { completed: recent.length, succeeded, failed: recent.length - succeeded, avg_duration_ms: recent.length ? duration / recent.length : 0 };
  }
  // 首字时间只对「真的吐出过首字」的请求有意义：失败在首字之前、非流式没记录的请求
  // 不参与平均，否则一律当成 0 会把两个数字拉低成没意义的乐观值。
  function ttftAverage(recent) {
    let sum = 0, count = 0;
    for (const row of recent) { const ms = Number(row.ttfb_ms); if (ms > 0) { sum += ms; count++; } }
    return count ? sum / count : null;
  }
  // 缓存命中率按 token 加权（总命中 / 总 prompt 缓存 token），不是逐条命中率的平均：
  // 一条 10 万 token 全命中的请求不该和一条 10 token 的请求等权。
  // 老归档只有命中数，未命中按 prompt 差值补；两个字段都没有＝上游没报缓存，直接跳过。
  // 同时把两个绝对数带回给卡片：没有样本时返回 null，让界面显示「—」而不是 0%。
  function cacheTotals(recent) {
    let hit = 0, miss = 0, seen = false;
    for (const row of recent) {
      if (row.cache_hit_tokens === undefined && row.cache_miss_tokens === undefined) continue;
      const hits = Number(row.cache_hit_tokens) || 0;
      hit += hits;
      miss += row.cache_miss_tokens !== undefined ? Number(row.cache_miss_tokens) || 0 : Math.max(0, (Number(row.prompt_tokens) || 0) - hits);
      seen = true;
    }
    if (!seen) return null;
    return { hit, miss, rate: hit + miss > 0 ? hit / (hit + miss) * 100 : null };
  }
  // 输入/输出 Token 只累加真的上报了该字段的请求：整批都没上报时给 null（显示「—」），
  // 而不是把「没有数据」画成 0。
  function sumTokens(recent, key) {
    let sum = 0, seen = false;
    for (const row of recent) {
      if (row[key] === undefined || row[key] === null) continue;
      sum += Number(row[key]) || 0; seen = true;
    }
    return seen ? sum : null;
  }
  function usageView(data) {
    const live = Array.isArray(data.recent) ? data.recent : [];
    const archived = data.history && Array.isArray(data.history.recent) ? data.history.recent : [];
    if (!archived.length) return { fromArchive: false, recent: live, totals: data };
    const seen = new Set(), merged = [];
    for (const row of [...live, ...archived]) {
      const key = row.request_id || [row.time, row.model, row.status, row.duration_ms].join('|');
      if (seen.has(key)) continue;
      seen.add(key); merged.push(row);
    }
    merged.sort((a, b) => new Date(b.time) - new Date(a.time));
    return { fromArchive: true, recent: merged, totals: usageTotals(merged) };
  }
  function renderUsage(data) {
    const view = usageView(data), recent = view.recent, totals = view.totals;
    $('usageCompleted').textContent = number(totals.completed);
    $('usageSuccess').textContent = totals.completed > 0 ? (Number(totals.succeeded || 0) / totals.completed * 100).toFixed(1) + '%' : '—';
    $('usageOutcomes').textContent = '成功 ' + number(totals.succeeded) + ' · 失败 ' + number(totals.failed);
    $('usageDuration').textContent = duration(totals.avg_duration_ms);
    const ttft = ttftAverage(recent), cache = cacheTotals(recent);
    $('usageTTFT').textContent = ttft === null ? '—' : duration(ttft);
    $('usageCacheHit').textContent = cache && cache.rate !== null ? cache.rate.toFixed(1) + '%' : '—';
    $('usageCacheNote').textContent = cache ? '命中 ' + number(cache.hit) + ' · 未命中 ' + number(cache.miss) : '上游未上报缓存用量';
    const promptTokens = sumTokens(recent, 'prompt_tokens'), completionTokens = sumTokens(recent, 'completion_tokens');
    $('usagePromptTokens').textContent = promptTokens === null ? '—' : number(promptTokens);
    $('usageCompletionTokens').textContent = completionTokens === null ? '—' : number(completionTokens);
    $('usageTokens').textContent = recent.length ? number(recent.reduce((sum, row) => sum + tokenTotal(row), 0)) : '—';
    $('usageCompletedNote').textContent = view.fromArchive ? '本地归档 data/requests（含重启前的记录）' : '当前服务进程累计';
    $('usageChartNote').textContent = view.fromArchive
      ? '本地归档 data/requests 最近 ' + recent.length + ' 条记录按小时聚合，新请求落盘后自动计入。'
      : '基于最近 ' + recent.length + ' 条记录，按小时聚合；进程重启后指标重新累计。';
    const grouped = new Map();
    recent.forEach(row => { const id = row.model || '未知模型'; const g = grouped.get(id) || { requests: 0, succeeded: 0, tokens: 0, duration: 0 }; g.requests++; g.succeeded += Number(eventOK(row)); g.tokens += tokenTotal(row); g.duration += Number(row.duration_ms) || 0; grouped.set(id, g); });
    $('usageModelsBody').innerHTML = [...grouped].sort((a, b) => b[1].tokens - a[1].tokens).map(([id, g]) => '<tr><td class="mono">' + esc(id) + '</td><td>' + number(g.requests) + '</td><td>' + number(g.succeeded) + '</td><td>' + number(g.tokens) + '</td><td>' + duration(g.duration / g.requests) + '</td></tr>').join('') || emptyRow(5, '暂无请求。通过 /v1/chat/completions 发起对话后可查看用量。');
    renderChart(recent);
  }
  function renderChart(recent) {
    const hours = new Map();
    recent.forEach(row => { const t = new Date(row.time || row.ts).getTime(); if (Number.isFinite(t)) { const h = Math.floor(t / 3600000) * 3600000; hours.set(h, (hours.get(h) || 0) + 1); } });
    const points = [...hours].sort((a, b) => a[0] - b[0]);
    if (!points.length) { $('usageChart').textContent = '暂无请求数据。完成一次模型调用后刷新此页。'; return; }
    const W = 1000, H = 220, left = 38, top = 12, bottom = 36, usableH = H - top - bottom;
    const max = Math.max(4, Math.ceil(Math.max(...points.map(p => p[1])) / 4) * 4);
    const slot = (W - left - 12) / points.length, width = Math.min(64, slot * .62);
    let svg = '<svg viewBox="0 0 ' + W + ' ' + H + '" role="img" aria-label="按小时统计的近期请求数"><title>近期请求数，每柱代表一个有请求的小时</title>';
    for (let i = 0; i <= 4; i++) { const y = top + usableH * i / 4; svg += '<line class="chart-grid" x1="' + left + '" y1="' + y + '" x2="990" y2="' + y + '"/><text x="30" y="' + (y + 4) + '" text-anchor="end">' + Math.round(max * (4 - i) / 4) + '</text>'; }
    points.forEach(([t, count], i) => { const x = left + slot * (i + .5), height = count / max * usableH, label = new Date(t).toLocaleString('zh-CN', { month: 'numeric', day: 'numeric', hour: '2-digit', hour12: false }); svg += '<rect class="chart-bar" x="' + (x - width / 2) + '" y="' + (top + usableH - height) + '" width="' + width + '" height="' + height + '" rx="3"><title>' + esc(label) + '：' + count + ' 次请求</title></rect>'; if (i % Math.max(1, Math.ceil(points.length / 8)) === 0) svg += '<text x="' + x + '" y="207" text-anchor="middle">' + esc(label) + '</text>'; });
    $('usageChart').innerHTML = svg + '</svg>';
  }
  function renderRequestLogs(data) {
    const view = usageView(data);
    requestRows = [...view.recent].sort((a, b) => new Date(b.time) - new Date(a.time));
    requestRowsFromArchive = view.fromArchive;
    requestAttemptErrors = (data && data.attempt_errors) || {};
    paintRequestLogs();
  }
  // 尝试次数：重试过（>1）才把最后一次上游错误挂到单元格的原生 tooltip 上；
  // 没有错误就不给 title，鼠标移上去不会弹出空提示。
  function attemptsCell(row) {
    const attempts = Number(row.attempts);
    const shown = Number.isFinite(attempts) && attempts > 0 ? String(attempts) : '—';
    const failure = attempts > 1 ? requestAttemptErrors[row.request_id] : '';
    return '<td class="number"' + (failure ? ' title="' + esc(failure) + '"' : '') + '>' + esc(shown) + '</td>';
  }
  // 筛选只作用于已取回的这一批记录（进程 100 条 + 归档 200 条），不额外请求后端。
  function paintRequestLogs() {
    const filter = $('requestOutcome').value;
    const rows = filter ? requestRows.filter(row => eventOK(row) === (filter === 'ok')) : requestRows;
    $('requestLogsBody').innerHTML = rows.map(row => '<tr><td class="muted">' + esc(date(row.time)) + '</td><td class="mono">' + esc(row.model || '—') + '</td><td>' + esc(row.account || '—') + '</td><td><span class="pill ' + (eventOK(row) ? 'ok' : 'bad') + '">' + esc(eventOK(row) ? '成功' : row.outcome || '失败') + ' ' + esc(row.status ?? '') + '</span>' + (!eventOK(row) && row.error ? '<small class="request-error" title="' + esc(row.error) + '">' + esc(row.error) + '</small>' : '') + '</td>' + attemptsCell(row) + '<td>' + esc(duration(row.duration_ms)) + '</td><td>' + esc(number(tokenTotal(row))) + '</td></tr>').join('') || emptyRow(7, filter ? '没有符合条件的请求。' : requestRowsFromArchive ? '本地归档中暂无请求记录。' : '当前进程尚无请求记录。');
  }
  function renderRuntimeLogs() {
    const box = $('runtimeLogs');
    const channel = $('logChannel').value;
    const rows = runtimeLogs.filter(row => !channel || row.ch === channel);
    const atEnd = box.scrollTop + box.clientHeight >= box.scrollHeight - 24;
    box.innerHTML = rows.map(row => '<span class="log-line"><span class="log-time">' + esc(date(row.ts)) + '</span><span class="log-channel">' + esc({ sys: '系统', chat: '请求', task: '任务' }[row.ch] || row.ch || '') + '</span>' + esc(row.text) + '</span>').join('') || '<span class="muted">暂无日志。</span>';
    if (atEnd) box.scrollTop = box.scrollHeight;
  }
  $('logChannel').onchange = renderRuntimeLogs;
  $('requestOutcome').onchange = paintRequestLogs;
  async function loadLogs() {
    if (logsLoading) return;
    logsLoading = true;
    try {
      const [logs, usage] = await Promise.all([api('logs'), api('usage')]);
      runtimeLogs = logs.entries || [];
      renderRuntimeLogs();
      renderRequestLogs(usage);
      $('updatedAt').textContent = '刷新于 ' + new Date().toLocaleTimeString('zh-CN', { hour12: false });
    } finally { logsLoading = false; }
  }
  function configForEditor(config) {
    const copy = { ...config, api_key: '', admin_key: '' };
    return JSON.stringify(copy, null, 2);
  }
  // 表单字段 ↔ 配置点路径。填充与读取共用这张表，新增字段只改这里和 panel.html。
  // 未列出的字段（含未知扩展字段）在表单模式下原样保留，要用就用 Raw JSON。
  const CONFIG_FIELDS = [
    ['cfgListen', 'listen', 'text'],
    ['cfgDefaultModel', 'default_model', 'text'],
    ['cfgAuthDir', 'auth_dir', 'text'],
    ['cfgStateFile', 'state_file', 'text'],
    ['cfgOAuthCallbackHost', 'oauth_callback_host', 'text'],
    ['cfgLoginClientID', 'login_client_id', 'text'],
    ['cfgMaxConcurrent', 'max_concurrent', 'int'],
    ['cfgKeepaliveWindow', 'keepalive_window', 'text'],
    ['cfgQueueRetrySeconds', 'queue_retry_seconds', 'int'],
    ['cfgQueueMaxAttempts', 'queue_max_attempts', 'int'],
    ['cfgSoftRate', 'cooldown.soft_rate', 'text'],
    ['cfgErrThreshold', 'cooldown.err_threshold', 'int'],
    ['cfgErrCooldown', 'cooldown.err_cooldown', 'text'],
    ['cfgTimeoutSeconds', 'upstream.timeout_seconds', 'int'],
    ['cfgWatchEnabled', 'watch.enabled', 'bool'],
    ['cfgCreditsAutoClaim', 'credits_auto_claim', 'bool'],
    ['cfgPollMinutes', 'watch.poll_minutes', 'int'],
    ['cfgRefreshSkew', 'watch.refresh_skew_minutes', 'int'],
    ['cfgKeepaliveInterval', 'watch.keepalive_interval_minutes', 'int'],
    ['cfgCheckinHours', 'checkin_hours', 'hours'],
    ['cfgRequestLogRetentionDays', 'request_log_retention_days', 'int'],
  ];
  const configPathGet = (source, path) => path.split('.').reduce((value, key) => (value === null || value === undefined ? undefined : value[key]), source);
  function configPathSet(target, path, value) {
    const keys = path.split('.');
    let cursor = target;
    for (const key of keys.slice(0, -1)) { if (!cursor[key] || typeof cursor[key] !== 'object') cursor[key] = {}; cursor = cursor[key]; }
    cursor[keys[keys.length - 1]] = value;
  }
  function markConfigDirty() { $('configEditor').dataset.dirty = 'true'; }
  function markConfigClean() { $('configEditor').dataset.dirty = 'false'; }
  function fillConfigForm(config) {
    for (const [id, path, kind] of CONFIG_FIELDS) {
      const input = $(id), value = configPathGet(config, path);
      if (kind === 'bool') input.checked = value === true;
      else if (kind === 'hours') input.value = Array.isArray(value) ? value.join(', ') : '';
      else input.value = value === null || value === undefined ? '' : String(value);
      input.oninput = markConfigDirty; input.onchange = markConfigDirty;
    }
  }
  // 以最近读到的配置为底只覆盖表单字段：表单没覆盖到的键（含未知字段）不会丢。
  function readConfigForm(config) {
    const next = JSON.parse(JSON.stringify(config || {}));
    for (const [id, path, kind] of CONFIG_FIELDS) {
      const input = $(id), raw = String(input.value === undefined ? '' : input.value).trim();
      if (kind === 'bool') { configPathSet(next, path, input.checked === true); continue; }
      if (kind === 'hours') {
        const hours = raw === '' ? [] : raw.split(',').map(part => part.trim()).filter(Boolean).map(Number);
        if (hours.some(hour => !Number.isInteger(hour) || hour < 0 || hour > 23)) throw new Error('签到时点必须是 0-23 的整数，逗号分隔');
        configPathSet(next, path, hours);
        continue;
      }
      if (kind === 'int') {
        if (raw === '') continue; // 留空＝沿用原值，免得把必填项清成 0
        const number = Number(raw);
        if (!Number.isFinite(number)) throw new Error(path + ' 必须是数字');
        configPathSet(next, path, Math.trunc(number));
        continue;
      }
      configPathSet(next, path, raw);
    }
    return next;
  }
  let configMode = 'form';
  function renderConfigMode() {
    const form = configMode === 'form';
    $('configForm').hidden = !form;
    $('configRaw').hidden = form;
    for (const [id, active] of [['configModeForm', form], ['configModeRaw', !form]]) {
      $(id).classList.toggle('active', active);
      $(id).setAttribute('aria-selected', String(active));
    }
    $('configHelp').textContent = form ? '表单只覆盖以上字段；其余字段请切到 Raw JSON 编辑。保存不会自动重启服务。' : '请使用合法 JSON。保存不会自动重启服务。';
  }
  let loadedConfig = {};
  async function loadConfig() {
    const data = await api('config');
    // 服务端已把密钥脱敏成空串；这里再显式清一次，保证表单克隆出的 patch 永不带密钥。
    loadedConfig = { ...(data.config || {}), api_key: '', admin_key: '' };
    $('configEditor').value = configForEditor(loadedConfig);
    fillConfigForm(loadedConfig);
    markConfigClean();
    $('newAdminKey').value = ''; $('newAPIKey').value = '';
    const overrides = Array.isArray(data.env_overrides) ? data.env_overrides : [];
    $('envOverrides').hidden = !overrides.length;
    $('envOverrides').textContent = overrides.length ? '以下字段由环境变量覆盖：' + overrides.join('、') + '。请同时检查启动环境。' : '';
    renderConfigMode();
  }
  // 切换模式一律重新读取配置文件（未保存的修改随之丢弃，这是页面上写明的行为）。
  async function switchConfigMode(mode) {
    if (mode === configMode) return;
    configMode = mode;
    renderConfigMode();
    try { await loadConfig(); toast('已重新读取配置文件'); }
    catch (error) { toast(error.message, true); }
  }
  $('configModeForm').onclick = () => switchConfigMode('form');
  $('configModeRaw').onclick = () => switchConfigMode('raw');
  $('configEditor').oninput = markConfigDirty;
  $('saveConfigButton').onclick = async () => {
    let config;
    // 以当前界面模式为准：Raw 模式解析编辑框，表单模式从输入控件重建。
    if (configMode === 'raw') {
      try { config = JSON.parse($('configEditor').value); if (!config || Array.isArray(config) || typeof config !== 'object') throw new Error('请填写配置对象'); }
      catch (error) { toast('配置 JSON 无效：' + error.message, true); return; }
    } else {
      try { config = readConfigForm(loadedConfig); }
      catch (error) { toast('表单有无效值：' + error.message, true); return; }
    }
    const newAdminKey = $('newAdminKey').value.trim();
    config.api_key = $('newAPIKey').value || config.api_key || '';
    config.admin_key = newAdminKey || config.admin_key || '';
    $('newAPIKey').value = ''; $('newAdminKey').value = '';
    $('saveConfigButton').disabled = true;
    try {
      const data = await api('config', { method: 'PUT', body: JSON.stringify(config) });
      if (data.ok === false) throw new Error(data.message || '保存失败');
      // 热生效后旧密钥立即失效：本次会话改用新管理密钥，避免下一次轮询被 401。
      if (newAdminKey) { adminKey = newAdminKey; writeStorage(sessionStorage, KEY_STORAGE, adminKey); }
      loadedConfig = { ...(data.config || config), api_key: '', admin_key: '' };
      $('configEditor').value = configForEditor(loadedConfig);
      fillConfigForm(loadedConfig);
      markConfigClean();
      const restart = Array.isArray(data.restart_required) ? data.restart_required.filter(Boolean) : [];
      $('configNotice').textContent = restart.length
        ? '配置已保存并热生效；标有「需重启」的字段需重启服务后生效。'
        : '配置已保存并立即生效。';
      toast(restart.length ? '配置已保存并热生效，标有「需重启」的字段待重启服务后生效' : '配置已保存并立即生效');
    } catch (error) { toast(error.message, true); }
    finally { $('saveConfigButton').disabled = false; }
  };
  async function refresh(forceModels = false) {
    if (!authenticated) return;
    const id = ++refreshID, page = currentPage;
    $('pageError').hidden = true; $('pageLoading').hidden = false; $('refreshButton').disabled = true;
    if (page === 'models' && forceModels) { $('refreshModelsButton').disabled = true; $('refreshModelsButton').textContent = '正在获取…'; }
    try {
      if (page === 'accounts') renderOverview(await api('overview'));
      else if (page === 'models') { const data = await api('models' + (forceModels ? '?refresh=1' : '')); models = data.data || []; renderModels(); if (forceModels) toast('模型目录已重新获取'); }
      else if (page === 'usage') renderUsage(await api('usage'));
      else if (page === 'config') { if ($('configEditor').dataset.dirty !== 'true') await loadConfig(); }
      else if (page === 'logs') await loadLogs();
      if (id === refreshID) $('updatedAt').textContent = '刷新于 ' + new Date().toLocaleTimeString('zh-CN', { hour12: false });
    } catch (error) { if (id === refreshID && authenticated) { $('pageError').textContent = error.message + '。点击右上角刷新重试。'; $('pageError').hidden = false; } }
    finally { if (id === refreshID) { $('pageLoading').hidden = true; $('refreshButton').disabled = false; $('refreshModelsButton').disabled = false; $('refreshModelsButton').textContent = '重新获取'; } }
  }
  $('refreshButton').onclick = () => { if (currentPage === 'config' && $('configEditor').dataset.dirty === 'true') { toast('配置含未保存的编辑，请先保存；切换页面不会丢失编辑。'); return; } refresh(); };
  $('refreshModelsButton').onclick = () => refresh(true);
  $('loginDialog').addEventListener('cancel', event => event.preventDefault());
  $('loginForm').onsubmit = async event => {
    event.preventDefault();
    const candidate = $('loginKey').value.trim();
    $('loginKey').value = ''; $('loginSubmit').disabled = true; fieldError('loginError', '');
    try { const data = await api('overview', {}, candidate); adminKey = candidate; writeStorage(sessionStorage, KEY_STORAGE, adminKey); authenticated = true; $('loginDialog').close(); renderOverview(data); navigate(); }
    catch (error) { fieldError('loginError', error.message); $('loginKey').focus(); }
    finally { $('loginSubmit').disabled = false; }
  };
  $('logoutButton').onclick = () => {
    authEpoch++; refreshID++; adminKey = ''; authenticated = false; writeStorage(sessionStorage, KEY_STORAGE, ''); stopOAuth();
    accounts = []; models = []; runtimeLogs = []; serverStartedAt = 0; $('uptime').textContent = '—';
    $('configEditor').value = ''; $('configEditor').dataset.dirty = 'false'; $('newAPIKey').value = ''; $('newAdminKey').value = '';
    ['statTotal', 'statHealthy', 'statCooling', 'statDisabled', 'usageCompleted', 'usageSuccess', 'usageDuration', 'usageTTFT', 'usageCacheHit', 'usagePromptTokens', 'usageCompletionTokens', 'usageTokens'].forEach(id => { $(id).textContent = '—'; });
    renderAccounts(); renderModels(); renderRuntimeLogs(); renderRequestLogs({}); renderUsage({});
    $('pageLoading').hidden = true; $('pageError').hidden = true; $('refreshButton').disabled = false; showLogin();
  };
  document.querySelectorAll('[data-close]').forEach(button => { button.onclick = () => $(button.dataset.close).close(); });
  function stopOAuth() { if (oauthTimer) clearInterval(oauthTimer); oauthTimer = null; oauthSession = ''; }
  function clearAdd() { stopOAuth(); $('callbackURL').value = ''; $('importJSON').value = ''; $('importFile').value = ''; $('akskForm').reset(); $('oauthStart').textContent = '生成授权链接'; $('oauthCopy').hidden = true; $('oauthLink').hidden = true; $('oauthLink').removeAttribute('href'); $('oauthReady').hidden = true; fieldError('addError', ''); }
  $('addDialog').addEventListener('close', clearAdd);
  $('addAccountButton').onclick = () => { clearAdd(); $('addDialog').showModal(); };
  document.querySelectorAll('[data-add-mode]').forEach(button => { button.onclick = () => { const mode = button.dataset.addMode; document.querySelectorAll('[data-add-mode]').forEach(b => { b.classList.toggle('active', b === button); }); ['oauth', 'json', 'aksk'].forEach(name => { $(name === 'aksk' ? 'akskForm' : 'add-' + name).hidden = name !== mode; }); fieldError('addError', ''); }; });
  function setAuthURL(value) {
    const url = new URL(value);
    if (url.protocol !== 'https:' || !url.hostname.endsWith('.huaweicloud.com')) throw new Error('服务返回了不受支持的授权地址。');
    $('oauthLink').href = url.href;
    $('oauthCopy').hidden = false;
    $('oauthLink').hidden = false;
  }
  $('oauthCopy').onclick = async () => {
    const url = $('oauthLink').href;
    if (!url) return;
    try { await navigator.clipboard.writeText(url); toast('授权链接已复制'); }
    catch { toast('浏览器无法访问剪贴板，请右键“在新标签页打开授权页”复制链接。', true); }
  };
  $('oauthStart').onclick = async () => {
    $('oauthStart').disabled = true; fieldError('addError', ''); stopOAuth();
    $('oauthCopy').hidden = true; $('oauthLink').hidden = true; $('oauthLink').removeAttribute('href'); $('oauthReady').hidden = true;
    try { const data = await post('oauth/start'); if (data.ok === false) throw new Error(data.message || '无法发起授权'); setAuthURL(data.auth_url); oauthSession = data.session_id; $('oauthStart').textContent = '重新生成授权链接'; $('oauthReady').hidden = false; $('oauthStatus').textContent = '等待在浏览器完成授权…'; oauthTimer = setInterval(() => pollOAuth(), 3500); }
    catch (error) { fieldError('addError', error.message); }
    finally { $('oauthStart').disabled = false; }
  };
  async function oauthResult(data) {
    if (data.status === 'done') { stopOAuth(); $('addDialog').close(); toast(data.message || '账户已添加'); renderOverview(await api('overview')); }
    else if (data.ok === false || data.status === 'error') { stopOAuth(); throw new Error(data.message || '授权失败'); }
    else { $('oauthStatus').textContent = data.message || '等待浏览器完成授权…'; if (data.next_url) setAuthURL(data.next_url); }
  }
  async function pollOAuth() {
    if (!oauthSession || oauthBusy) return;
    const session = oauthSession;
    oauthBusy = true;
    try { const data = await post('oauth/poll', { session_id: session }); if (session === oauthSession) await oauthResult(data); }
    catch (error) { if (session === oauthSession) { stopOAuth(); fieldError('addError', error.message); } }
    finally { oauthBusy = false; }
  }
  $('oauthPoll').onclick = pollOAuth;
  $('oauthCallback').onclick = async () => {
    if (!oauthSession) { fieldError('addError', '请先生成授权链接。'); return; }
    const value = $('callbackURL').value.trim();
    if (!value) { fieldError('addError', '请粘贴浏览器的完整回调地址。'); return; }
    $('oauthCallback').disabled = true;
    try { const data = await post('oauth/import-callback', { session_id: oauthSession, callback_url: value }); $('callbackURL').value = ''; await oauthResult(data); }
    catch (error) { fieldError('addError', error.message); }
    finally { $('oauthCallback').disabled = false; }
  };
  async function importCredentials(payload) {
    const data = await post('accounts/import', payload);
    if (data.ok === false) throw new Error(data.message || '导入失败');
    $('addDialog').close(); toast('已导入 ' + (data.imported ?? data.accounts?.length ?? 1) + ' 个账户'); renderOverview(await api('overview'));
  }
  $('importFile').onchange = async () => {
    const file = $('importFile').files[0];
    if (!file) return;
    if (file.size > 1024 * 1024) { fieldError('addError', 'JSON 文件不能超过 1 MB。'); $('importFile').value = ''; return; }
    try { $('importJSON').value = await file.text(); fieldError('addError', ''); } catch { fieldError('addError', '无法读取文件。'); }
  };
  $('importJSONButton').onclick = async () => {
    $('importJSONButton').disabled = true;
    try { const payload = JSON.parse($('importJSON').value); if (!payload || typeof payload !== 'object') throw new Error('请提供凭据对象或数组'); await importCredentials(payload); }
    catch (error) { fieldError('addError', error.message); }
    finally { $('importJSONButton').disabled = false; }
  };
  $('akskForm').onsubmit = async event => {
    event.preventDefault(); const button = $('akskForm').querySelector('button[type=submit]'); button.disabled = true;
    const payload = Object.fromEntries(new FormData($('akskForm')));
    try { await importCredentials(payload); } catch (error) { fieldError('addError', error.message); }
    finally { button.disabled = false; }
  };
  // 与参考面板一致：只轮询可见日志页，避免后台标签页持续请求。
  setInterval(() => {
    if (authenticated && currentPage === 'logs' && !document.hidden) loadLogs().catch(() => {});
  }, 5000);
  // 运行时长是本地推算的，30 秒跳一次即可，不必为它增加请求。
  setInterval(() => { if (authenticated) renderUptime(); }, 30000);
  navigate();
  if (adminKey) {
    api('overview').then(data => { authenticated = true; renderOverview(data); navigate(); }).catch(error => showLogin(error.message));
  } else showLogin();
})();
