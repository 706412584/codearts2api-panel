(() => {
  'use strict';
  const $ = id => document.getElementById(id);
  const KEY_STORAGE = 'codearts2api-panel.admin-key';
  const THEME_STORAGE = 'codearts2api-panel.theme';
  const pages = {
    accounts: ['账号池', '管理凭据、查看账户状态与额度。'],
    models: ['模型目录', '查看模型通道与能力，复制 ID 接入客户端。'],
    usage: ['请求用量', '当前进程的请求指标与近期用量。'],
    config: ['服务配置', '管理网关配置，保存后重启服务生效。'],
    logs: ['运行日志', '检查最近请求、错误与服务运行状态。']
  };
  const readStorage = (storage, name) => { try { return storage.getItem(name) || ''; } catch { return ''; } };
  const writeStorage = (storage, name, value) => { try { value ? storage.setItem(name, value) : storage.removeItem(name); } catch { /* Session can still run in memory. */ } };
  let adminKey = readStorage(sessionStorage, KEY_STORAGE);
  let authenticated = false, authEpoch = 0, refreshID = 0;
  let currentPage = 'accounts', accounts = [], models = [], runtimeLogs = [];
  let oauthSession = '', oauthTimer = null, oauthBusy = false, deleteUID = '';
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
    $('themeButton').textContent = theme === 'dark' ? '☀' : '☾';
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
      const expiry = a.permanent === true || a.credential_type === 'permanent' ? '永久凭据' : date(a.expires_at);
      return '<tr><td><span class="account-name">' + esc(a.nickname || a.user_name || a.name || '未命名账户') + '</span><span class="account-id mono">' + esc(uid) + '</span>' + (a.last_error ? '<span class="account-error" title="' + esc(a.last_error) + '">' + esc(a.last_error) + '</span>' : '') + '</td><td><span class="pill ' + state[0] + '">' + state[1] + '</span></td><td>' + balanceCell(a.credit_balance, 'total', a.credit_balance_error) + '</td><td>' + balanceCell(a.benefit_balance, 'total_balance', a.benefit_balance_error) + (a.benefit_balance ? '<span class="balance-detail">总额 ' + esc(number(a.benefit_balance.total_quota)) + '</span>' : '') + '</td><td><span class="muted">' + esc(expiry) + '</span></td><td class="number">' + esc(number(a.active_concurrent)) + ' / ' + esc(number(a.max_concurrent)) + '</td><td><div class="row-actions">' + button('credits', '查额度') + button('checkin', '签到') + button('keepalive', '保活') + button(a.disabled ? 'enable' : 'disable', a.disabled ? '启用' : '禁用') + (a.cooling ? button('clear-cooldown', '清冷却') : '') + button('delete', '删除', true) + '</div></td></tr>';
    }).join('') || emptyRow(7, query ? '没有匹配的账户。' : '还没有账户。点击“添加账户”完成首次接入。');
  }
  function actionMessage(data, fallback) {
    const failed = Array.isArray(data.results) ? data.results.filter(r => r.ok === false) : [];
    const details = failed.map(r => (r.uid || '') + '：' + (r.message || r.credit_balance_error || r.benefit_balance_error || '操作失败')).join('；');
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
    const query = $('modelSearch').value.toLowerCase().trim(), channel = $('modelChannel').value;
    const filtered = models.filter(m => String(m.id).toLowerCase().includes(query) && (!channel || (m.benefit || m.channel === 'benefit' ? 'benefit' : 'agent') === channel));
    $('modelCount').textContent = filtered.length;
    $('modelsBody').innerHTML = filtered.map(m => {
      const benefit = m.benefit || m.channel === 'benefit';
      const vision = m.supports_images ?? m.vision ?? m.supports_vision;
      return '<tr><td class="mono">' + esc(m.id) + '</td><td><span class="pill ' + (benefit ? 'info' : '') + '">' + (benefit ? '免费额度' : '常规模型') + '</span></td><td class="number">' + esc(number(m.context_length ?? m.context_window)) + '</td><td class="number">' + esc(number(m.max_output_tokens ?? m.max_tokens)) + '</td><td>' + (vision === undefined ? '<span class="muted">未知</span>' : vision ? '支持' : '不支持') + '</td><td><button class="button small" data-copy-model="' + esc(m.id) + '">复制 ID</button></td></tr>';
    }).join('') || emptyRow(6, query || channel ? '没有符合条件的模型。' : '模型目录为空，请添加账户后刷新。');
  }
  $('modelSearch').oninput = renderModels; $('modelChannel').onchange = renderModels;
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
  function renderUsage(data) {
    const recent = Array.isArray(data.recent) ? data.recent : [];
    $('usageCompleted').textContent = number(data.completed);
    $('usageSuccess').textContent = data.completed > 0 ? (Number(data.succeeded || 0) / data.completed * 100).toFixed(1) + '%' : '—';
    $('usageOutcomes').textContent = '成功 ' + number(data.succeeded) + ' · 失败 ' + number(data.failed);
    $('usageDuration').textContent = duration(data.avg_duration_ms);
    $('usageTokens').textContent = recent.length ? number(recent.reduce((sum, row) => sum + tokenTotal(row), 0)) : '—';
    $('usageChartNote').textContent = '基于最近 ' + recent.length + ' 条记录，按小时聚合；进程重启后指标重新累计。';
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
    const recent = Array.isArray(data.recent) ? [...data.recent].sort((a, b) => new Date(b.time) - new Date(a.time)) : [];
    $('requestLogsBody').innerHTML = recent.map(row => '<tr><td class="muted">' + esc(date(row.time)) + '</td><td class="mono">' + esc(row.model || '—') + '</td><td>' + esc(row.account || '—') + '</td><td><span class="pill ' + (eventOK(row) ? 'ok' : 'bad') + '">' + esc(eventOK(row) ? '成功' : row.outcome || '失败') + ' ' + esc(row.status ?? '') + '</span></td><td>' + esc(duration(row.duration_ms)) + '</td><td>' + esc(number(tokenTotal(row))) + '</td></tr>').join('') || emptyRow(6, '当前进程尚无请求记录。');
  }
  function renderRuntimeLogs() {
    const channel = $('logChannel').value;
    const rows = runtimeLogs.filter(row => !channel || row.ch === channel);
    $('runtimeLogs').innerHTML = rows.map(row => '<span class="log-line"><span class="log-time">' + esc(date(row.ts)) + '</span><span class="log-channel">' + esc({ sys: '系统', chat: '请求', task: '任务' }[row.ch] || row.ch || '') + '</span>' + esc(row.text) + '</span>').join('') || '<span class="muted">暂无日志。</span>';
  }
  $('logChannel').onchange = renderRuntimeLogs;
  function configForEditor(config) {
    const copy = { ...config, api_key: '', admin_key: '' };
    return JSON.stringify(copy, null, 2);
  }
  async function loadConfig() {
    const data = await api('config');
    $('configEditor').value = configForEditor(data.config || {});
    $('configEditor').dataset.dirty = 'false';
    $('newAdminKey').value = ''; $('newAPIKey').value = '';
    const overrides = Array.isArray(data.env_overrides) ? data.env_overrides : [];
    $('envOverrides').hidden = !overrides.length;
    $('envOverrides').textContent = overrides.length ? '以下字段由环境变量覆盖：' + overrides.join('、') + '。请同时检查启动环境。' : '';
  }
  $('configEditor').oninput = () => { $('configEditor').dataset.dirty = 'true'; };
  $('saveConfigButton').onclick = async () => {
    let config;
    try { config = JSON.parse($('configEditor').value); if (!config || Array.isArray(config) || typeof config !== 'object') throw new Error('请填写配置对象'); }
    catch (error) { toast('配置 JSON 无效：' + error.message, true); return; }
    config.api_key = $('newAPIKey').value || config.api_key || '';
    config.admin_key = $('newAdminKey').value || config.admin_key || '';
    $('newAPIKey').value = ''; $('newAdminKey').value = '';
    $('configEditor').value = configForEditor(config);
    $('saveConfigButton').disabled = true;
    try {
      const data = await api('config', { method: 'PUT', body: JSON.stringify(config) });
      if (data.ok === false) throw new Error(data.message || '保存失败');
      $('configEditor').value = configForEditor(data.config || config); $('configEditor').dataset.dirty = 'false';
      $('configNotice').textContent = '配置已保存，重启服务后生效。若修改管理密钥，请在重启后使用新密钥连接。';
      toast('配置已保存，请重启服务以应用修改。');
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
      else if (page === 'logs') { const [logs, usage] = await Promise.all([api('logs'), api('usage')]); runtimeLogs = logs.entries || []; renderRuntimeLogs(); renderRequestLogs(usage); }
      if (id === refreshID) $('updatedAt').textContent = '更新于 ' + new Date().toLocaleTimeString('zh-CN', { hour12: false });
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
    accounts = []; models = []; runtimeLogs = [];
    $('configEditor').value = ''; $('configEditor').dataset.dirty = 'false'; $('newAPIKey').value = ''; $('newAdminKey').value = '';
    ['statTotal', 'statHealthy', 'statCooling', 'statDisabled', 'usageCompleted', 'usageSuccess', 'usageDuration', 'usageTokens'].forEach(id => { $(id).textContent = '—'; });
    renderAccounts(); renderModels(); renderRuntimeLogs(); renderRequestLogs({}); renderUsage({});
    $('pageLoading').hidden = true; $('pageError').hidden = true; $('refreshButton').disabled = false; showLogin();
  };
  document.querySelectorAll('[data-close]').forEach(button => { button.onclick = () => $(button.dataset.close).close(); });
  function stopOAuth() { if (oauthTimer) clearInterval(oauthTimer); oauthTimer = null; oauthSession = ''; }
  function clearAdd() { stopOAuth(); $('callbackURL').value = ''; $('importJSON').value = ''; $('importFile').value = ''; $('akskForm').reset(); $('oauthLink').removeAttribute('href'); $('oauthReady').hidden = true; fieldError('addError', ''); }
  $('addDialog').addEventListener('close', clearAdd);
  $('addAccountButton').onclick = () => { clearAdd(); $('addDialog').showModal(); };
  document.querySelectorAll('[data-add-mode]').forEach(button => { button.onclick = () => { const mode = button.dataset.addMode; document.querySelectorAll('[data-add-mode]').forEach(b => { b.classList.toggle('active', b === button); }); ['oauth', 'json', 'aksk'].forEach(name => { $(name === 'aksk' ? 'akskForm' : 'add-' + name).hidden = name !== mode; }); fieldError('addError', ''); }; });
  function setAuthURL(value) {
    const url = new URL(value);
    if (url.protocol !== 'https:' || !url.hostname.endsWith('.huaweicloud.com')) throw new Error('服务返回了不受支持的授权地址。');
    $('oauthLink').href = url.href;
  }
  $('oauthStart').onclick = async () => {
    $('oauthStart').disabled = true; fieldError('addError', ''); stopOAuth();
    try { const data = await post('oauth/start'); if (data.ok === false) throw new Error(data.message || '无法发起授权'); setAuthURL(data.auth_url); oauthSession = data.session_id; $('oauthReady').hidden = false; $('oauthStatus').textContent = '等待在浏览器完成授权…'; oauthTimer = setInterval(() => pollOAuth(), 3500); }
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
    catch (error) { stopOAuth(); fieldError('addError', error.message); }
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
  navigate();
  if (adminKey) {
    api('overview').then(data => { authenticated = true; renderOverview(data); navigate(); }).catch(error => showLogin(error.message));
  } else showLogin();
})();
