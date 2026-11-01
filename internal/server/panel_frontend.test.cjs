const fs = require('node:fs');
const vm = require('node:vm');
const assert = require('node:assert/strict');
const base = __dirname + '/';
const html = fs.readFileSync(base + 'panel.html', 'utf8');
const script = fs.readFileSync(base + 'panel.js', 'utf8');
const all = [], ids = new Map(), events = {}, requests = [], copiedURLs = [], intervals = [];
let oauthStarts = 0, oauthURLOverride = '', clipboardError = false;
class Element {
  constructor(attrs = '') { this.attrs = attrs; this.id = /\bid="([^"]+)"/.exec(attrs)?.[1] || ''; this.value = ''; this.textContent = ''; this.innerHTML = ''; this.hidden = /\bhidden\b/.test(attrs); this.open = false; this.disabled = false; this.dataset = {}; this.handlers = {}; this.classes = new Set((/\bclass="([^"]+)"/.exec(attrs)?.[1] || '').split(' ')); for (const a of attrs.matchAll(/data-([\w-]+)="([^"]*)"/g)) this.dataset[a[1].replace(/-([a-z])/g, (_, c) => c.toUpperCase())] = a[2]; this.classList = { add: x => this.classes.add(x), remove: x => this.classes.delete(x), contains: x => this.classes.has(x), toggle: (x, enabled) => { if (enabled === undefined) enabled = !this.classes.has(x); enabled ? this.classes.add(x) : this.classes.delete(x); return enabled; } }; }
  addEventListener(name, cb) { (this.handlers[name] ||= []).push(cb); }
  setAttribute(k, v) { this[k] = v; }
  removeAttribute(k) { delete this[k]; }
  showModal() { this.open = true; }
  close() { this.open = false; for (const cb of this.handlers.close || []) cb(); }
  focus() {}
  append(...children) { (this.children ||= []).push(...children); }
  remove() {}
  reset() { this.value = ''; }
  querySelector() { return new Element(); }
}
const ancestors = [];
for (const match of html.matchAll(/<(\/?)([\w-]+)\b([^>]*)>/g)) {
  const [, closing, tag, attrs] = match;
  if (closing) { const index = ancestors.findLastIndex(el => el.tagName === tag); if (index >= 0) ancestors.length = index; continue; }
  const el = new Element(attrs); el.tagName = tag; el.parentElement = ancestors.at(-1);
  for (const name of ['href', 'target', 'rel']) { const value = new RegExp('\\b' + name + '="([^"]*)"').exec(attrs)?.[1]; if (value !== undefined) el[name] = value; }
  all.push(el); if (el.id) { assert(!ids.has(el.id), 'duplicate id ' + el.id); ids.set(el.id, el); }
  if (!/^(area|base|br|col|embed|hr|img|input|link|meta|param|source|track|wbr)$/.test(tag)) ancestors.push(el);
}
const selector = s => s === '.view' ? all.filter(e => e.classes.has('view')) : /^\[data-([\w-]+)\]$/.test(s) ? all.filter(e => Object.hasOwn(e.dataset, s.slice(6, -1).replace(/-([a-z])/g, (_, c) => c.toUpperCase()))) : [];
const storage = () => { const values = new Map(); return { getItem: k => values.get(k), setItem: (k, v) => values.set(k, v), removeItem: k => values.delete(k) }; };
const account = { uid: 'u1', nickname: '<img src=x onerror=alert(1)>', credit_balance: null, active_concurrent: 0, max_concurrent: 1, expires_at: '2027-01-01T00:00:00Z' };
const overview = { stats: { total: 1, healthy: 1, cooling: 0, disabled: 0 }, accounts: [account] };
const usage = { completed: 2, succeeded: 1, failed: 1, avg_duration_ms: 500, recent: [{ time: new Date().toISOString(), model: 'test-model', ok: true, status: 200, total_tokens: 25, duration_ms: 500 }, { time: new Date().toISOString(), model: 'failed-model', ok: false, outcome: 'http_error', status: 503, error: 'upstream <failed> & unavailable', duration_ms: 100 }] };
const document = { getElementById: id => { assert(ids.has(id), 'missing id ' + id); return ids.get(id); }, querySelectorAll: selector, createElement: () => new Element(), documentElement: { dataset: {} }, hidden: false };
const context = vm.createContext({ document, console, sessionStorage: storage(), localStorage: storage(), matchMedia: () => ({ matches: false }), addEventListener: (n, cb) => { events[n] = cb; }, location: { hash: '#accounts' }, setTimeout: () => 1, clearTimeout() {}, setInterval: (cb, ms) => { intervals.push({ cb, ms }); return intervals.length; }, clearInterval() {}, Headers, URL, navigator: { clipboard: { writeText: async value => { if (clipboardError) throw new Error("clipboard blocked"); copiedURLs.push(value); } } }, fetch: async (url, opts) => { requests.push({ url, opts }); let data; const path = url.replace('/admin/api/', ''); if (path === 'overview') data = overview; else if (path.startsWith('models')) data = { data: [{ id: '<model>', model_name: 'Kimi <K3>', model_desc: '描述 <D>', notice: '注意 & 提示', model_tag: '闲时 <T>', display_enabled: false, supports_images: true, ratio_display: '0.7x', context_length: 1000 }, { id: 'plain-model' }] }; else if (path === "oauth/start") { oauthStarts++; data = { ok: true, session_id: "session-" + oauthStarts, auth_url: oauthURLOverride || "https://codearts.huaweicloud.com/portal/authorize?ticket_id=test-" + oauthStarts }; } else if (path === 'usage') data = usage; else if (path === 'logs') data = { entries: [{ ts: new Date().toISOString(), ch: 'sys', text: '<script>alert(1)</script>' }] }; else if (path === 'config') data = opts.method === 'PUT' ? { ok: true, config: JSON.parse(opts.body), restart_required: true } : { config: { listen: ':7866', admin_key: 'server-secret', api_key: 'api-secret' }, env_overrides: [] }; else data = { ok: true }; return { ok: true, status: 200, json: async () => data }; } });
const tick = () => new Promise(resolve => setImmediate(resolve));
(async () => {
 vm.runInContext(script, context);
 assert(ids.get('loginDialog').open, 'login dialog must gate first visit');
 ids.get('loginKey').value = 'test-admin-secret';
 await ids.get('loginForm').onsubmit({ preventDefault() {} }); await tick();
 assert(!ids.get('loginDialog').open); assert.equal(ids.get('loginKey').value, '');
 assert(ids.get('accountsBody').innerHTML.includes('&lt;img')); assert(!ids.get('accountsBody').innerHTML.includes('<img')); assert(ids.get('accountsBody').innerHTML.includes('未知'));
 // 签到按钮按「今日是否已签」切换：未签显示「签到」，已签显示「已签」并带本地结果提示。
 const checkinButton = () => /<button[^>]*data-account-action="checkin"[^>]*>(签到|已签)<\/button>/.exec(ids.get('accountsBody').innerHTML);
 assert.equal(checkinButton()[1], '签到', 'not signed today must show 签到');
 account.checkin_done = true; account.checkin = { ok: true, message: '领取成功（100 积分）' };
 ids.get('accountSearch').oninput();
 const signed = checkinButton();
 assert.equal(signed[1], '已签', 'signed today must show 已签');
 assert(signed[0].includes('class="button small done"'), '已签 must carry the done style');
 assert(signed[0].includes('title="领取成功（100 积分） · 今日已签到，每天只领一次"'), '已签 must explain itself: ' + signed[0]);
 account.checkin_done = false; account.checkin = undefined; ids.get('accountSearch').oninput();
 assert.equal(checkinButton()[1], '签到');
 for (const page of ['models', 'usage', 'logs', 'config']) { context.location.hash = '#' + page; events.hashchange(); await tick(); await tick(); assert.equal(ids.get('pageError').hidden, true, 'page failed: ' + page); }
 assert(ids.get('modelsBody').innerHTML.includes('&lt;model&gt;'));
 assert(ids.get('modelsBody').innerHTML.includes('Kimi &lt;K3&gt;'));
 assert(!ids.get('modelsBody').innerHTML.includes('<K3>'));
 assert(ids.get('modelsBody').innerHTML.includes('title="描述 &lt;D&gt;&#10;注意 &amp; 提示"'));
 assert(ids.get('modelsBody').innerHTML.includes('<div class="model-badges"><span class="pill tag">闲时 &lt;T&gt;</span><span class="pill warn">隐藏模型</span><span class="pill ok">视觉理解</span></div>'));
 assert(ids.get('modelsBody').innerHTML.includes('<td class="mono">plain-model</td>'));
 assert.equal(ids.get('themeIconSun').hidden, false); assert.equal(ids.get('themeIconMoon').hidden, true);
 ids.get('themeButton').onclick();
 assert.equal(ids.get('themeIconSun').hidden, true); assert.equal(ids.get('themeIconMoon').hidden, false);
 ids.get('themeButton').onclick();
 assert.equal(ids.get('themeIconSun').hidden, false); assert.equal(ids.get('themeIconMoon').hidden, true);
 assert(ids.get('usageChart').innerHTML.includes('<svg'));
 // 用量页：本地归档（data/requests）与进程内记录合并去重 —— 重启后要有历史，
 // 本进程跑过请求之后也不能塌成一条。
 const liveUsage = { ...usage, recent: [...usage.recent] };
 const archivedRow = (i, model, ok) => ({ time: new Date(Date.now() - (i + 10) * 60e3).toISOString(), request_id: 'arch-' + i, model, ok, status: ok ? 200 : 500, total_tokens: 10, duration_ms: 100 });
 const archivedRows = [archivedRow(1, 'archived-a', true), archivedRow(2, 'archived-b', false)];
 usage.completed = 0; usage.succeeded = 0; usage.failed = 0; usage.recent = [];
 usage.history = { recent: archivedRows };
 context.location.hash = '#accounts'; events.hashchange(); await tick();
 context.location.hash = '#usage'; events.hashchange(); await tick(); await tick();
 assert.equal(ids.get('usageCompleted').textContent, '2', 'archive rows must feed the cards');
 assert.equal(ids.get('usageSuccess').textContent, '50.0%');
 assert(ids.get('usageCompletedNote').textContent.includes('归档'));
 assert(ids.get('usageChartNote').textContent.includes('归档'));
 assert(ids.get('usageModelsBody').innerHTML.includes('archived-a'), 'archive rows must feed the model table');
 // 本进程跑过一条请求：归档仍然要在，页面上是「进程 + 归档」的并集。
 usage.recent = [liveUsage.recent[0]];
 context.location.hash = '#accounts'; events.hashchange(); await tick();
 context.location.hash = '#usage'; events.hashchange(); await tick(); await tick();
 assert.equal(ids.get('usageCompleted').textContent, '3', 'live + archived rows must merge, not replace');
 assert(ids.get('usageModelsBody').innerHTML.includes('archived-a') && ids.get('usageModelsBody').innerHTML.includes('test-model'), 'merged rows must feed the model table');
 // 同一条请求既在进程里又在归档里：只算一次。
 usage.history = { recent: [usage.recent[0], ...archivedRows] };
 context.location.hash = '#accounts'; events.hashchange(); await tick();
 context.location.hash = '#usage'; events.hashchange(); await tick(); await tick();
 assert.equal(ids.get('usageCompleted').textContent, '3', 'duplicate rows must be dropped');
 context.location.hash = '#logs'; events.hashchange(); await tick(); await tick();
 assert(ids.get('requestLogsBody').innerHTML.includes('archived-a'), 'archive rows must feed the request log');
 Object.assign(usage, liveUsage); delete usage.history;
 context.location.hash = '#logs'; events.hashchange(); await tick(); await tick();
 assert(ids.get('runtimeLogs').innerHTML.includes('&lt;script&gt;'));
 assert(ids.get('requestLogsBody').innerHTML.includes('upstream &lt;failed&gt; &amp; unavailable'));
 assert(!ids.get('requestLogsBody').innerHTML.includes('upstream <failed>'));
 const logPoll = intervals.find(timer => timer.ms === 5000);
 assert(logPoll, 'logs must have a five-second refresh timer');
 context.location.hash = '#accounts'; events.hashchange(); await tick(); await tick();
 let beforePoll = requests.length;
 logPoll.cb(); await tick();
 assert.equal(requests.length, beforePoll, 'polling must not run outside the logs page');
 usage.recent.push({ time: new Date().toISOString(), model: 'live-failure', ok: false, outcome: 'stream_error', status: 200, error: 'live stream failed' });
 context.location.hash = '#logs'; events.hashchange(); await tick(); await tick();
 beforePoll = requests.length;
 logPoll.cb(); await tick(); await tick();
 assert.equal(requests.length, beforePoll + 2, 'each visible log poll fetches logs and usage once');
 assert(ids.get('requestLogsBody').innerHTML.includes('live stream failed'));
 // 最近请求的结果筛选只过滤已取回的这一批记录，不发新请求。
 const outcome = ids.get('requestOutcome'), beforeFilter = requests.length;
 outcome.value = 'ok'; outcome.onchange();
 assert(ids.get('requestLogsBody').innerHTML.includes('test-model'), '成功筛选必须保留成功记录');
 assert(!ids.get('requestLogsBody').innerHTML.includes('live stream failed'), '成功筛选必须隐藏失败记录');
 outcome.value = 'fail'; outcome.onchange();
 assert(ids.get('requestLogsBody').innerHTML.includes('live stream failed') && !ids.get('requestLogsBody').innerHTML.includes('test-model'), '失败筛选只显示失败记录');
 outcome.value = ''; outcome.onchange();
 assert(ids.get('requestLogsBody').innerHTML.includes('test-model') && ids.get('requestLogsBody').innerHTML.includes('live stream failed'), '全部筛选恢复所有记录');
 assert.equal(requests.length, beforeFilter, '筛选不得触发额外请求');
 document.hidden = true; beforePoll = requests.length; logPoll.cb(); await tick();
 assert.equal(requests.length, beforePoll, 'hidden tabs must not poll');
 document.hidden = false;
 // 配置页：默认表单模式；切到 Raw 会重新读取配置文件；保存按当前模式取数据。
 assert.equal(ids.get('configForm').hidden, false, 'config page must default to the form');
 assert.equal(ids.get('configRaw').hidden, true);
 assert.equal(ids.get('cfgListen').value, ':7866');
 const beforeMode = requests.length;
 ids.get('configModeRaw').onclick(); await tick(); await tick();
 assert(requests.length > beforeMode, 'switching mode must re-read the config file');
 assert.equal(ids.get('configForm').hidden, true); assert.equal(ids.get('configRaw').hidden, false);
 assert(ids.get('configEditor').value.includes('"listen": ":7866"'));
 ids.get('configEditor').value = JSON.stringify({ listen: ':9000', custom_keep: true });
 await ids.get('saveConfigButton').onclick();
 const rawSave = JSON.parse(requests.filter(r => r.opts.method === 'PUT').at(-1).opts.body);
 assert.equal(rawSave.listen, ':9000', 'raw mode must save the editor content');
 assert.equal(rawSave.custom_keep, true, 'raw mode must keep unknown fields');
 ids.get('configModeForm').onclick(); await tick(); await tick();
 assert.equal(ids.get('configForm').hidden, false);
 assert.equal(ids.get('cfgListen').value, ':7866', 'switching back must re-read the file, not the raw edit');
 ids.get('cfgPollMinutes').value = '15';
 ids.get('cfgCheckinHours').value = '8, 20';
 await ids.get('saveConfigButton').onclick();
 const formSave = JSON.parse(requests.filter(r => r.opts.method === 'PUT').at(-1).opts.body);
 assert.equal(formSave.watch.poll_minutes, 15, 'form mode must save the form values');
 assert.deepEqual(formSave.checkin_hours, [8, 20]);
 assert.equal(formSave.listen, ':7866', 'untouched form fields keep the loaded value');
 assert.equal(ids.get('configEditor').dataset.dirty, 'false', 'saving must clear the dirty flag');
 assert(!ids.get('configEditor').value.includes('server-secret')); assert(!ids.get('configEditor').value.includes('api-secret'));
 ids.get('newAdminKey').value = 'rotated-secret'; ids.get('newAPIKey').value = 'new-api-secret';
 await ids.get('saveConfigButton').onclick();
 assert.equal(ids.get('newAdminKey').value, ''); assert.equal(ids.get('newAPIKey').value, ''); assert(!ids.get('configEditor').value.includes('rotated-secret')); assert(ids.get('configNotice').textContent.includes('重启'));
 for (const req of requests) { assert(!req.url.includes('test-admin-secret')); assert.equal(req.opts.headers.get('Authorization'), 'Bearer test-admin-secret'); }
 context.location.hash = '#models'; events.hashchange(); await tick(); await tick();
 const beforeModelRefresh = requests.length;
 await ids.get('refreshModelsButton').onclick();
 assert.equal(requests.length, beforeModelRefresh + 1); assert(requests.at(-1).url.endsWith('/models?refresh=1'));
 assert.equal(ids.get('refreshModelsButton').disabled, false); assert.equal(ids.get('refreshModelsButton').textContent, '重新获取');
 assert(ids.get('modelsBody').innerHTML.includes('&lt;model&gt;'));
 ids.get('addAccountButton').onclick();
 assert(ids.get('addDialog').open);
 assert.equal(ids.get('oauthStart').textContent, '生成授权链接');
 assert.equal(ids.get('oauthCopy').hidden, true); assert.equal(ids.get('oauthLink').hidden, true);
 for (let parent = ids.get('callbackURL'); parent && parent.id !== 'addDialog'; parent = parent.parentElement) {
   assert.equal(parent.hidden, false, 'callback field must remain visible before generating a URL');
   assert.notEqual(parent.tagName, 'details', 'callback field must not require expanding a disclosure');
   assert.notEqual(parent.id, 'oauthReady', 'callback field must not depend on generated-link visibility');
 }
 await ids.get('oauthStart').onclick();
 const firstAuthURL = 'https://codearts.huaweicloud.com/portal/authorize?ticket_id=test-1';
 assert.equal(ids.get('oauthStart').textContent, '重新生成授权链接');
 assert.equal(ids.get('oauthStart').disabled, false);
 assert.equal(ids.get('oauthCopy').hidden, false); assert.equal(ids.get('oauthLink').hidden, false);
 assert.equal(ids.get('oauthLink').href, firstAuthURL);
 assert.equal(ids.get('oauthLink').target, '_blank'); assert(ids.get('oauthLink').rel.split(/\s+/).includes('noopener'));
 await ids.get('oauthCopy').onclick(); assert.equal(copiedURLs.at(-1), firstAuthURL);
 await ids.get('oauthStart').onclick();
 const secondAuthURL = 'https://codearts.huaweicloud.com/portal/authorize?ticket_id=test-2';
 assert.equal(ids.get('oauthLink').href, secondAuthURL);
 await ids.get('oauthCopy').onclick(); assert.equal(copiedURLs.at(-1), secondAuthURL);
 const originalFetch = context.fetch;
 let rejectOldPoll;
 context.fetch = (url, options) => url.endsWith('/oauth/poll') ? new Promise((resolve, reject) => { rejectOldPoll = reject; }) : originalFetch(url, options);
 const oldPoll = ids.get('oauthPoll').onclick();
 await tick();
 await ids.get('oauthStart').onclick();
 rejectOldPoll(new Error('old session expired')); await oldPoll;
 assert.equal(ids.get('addError').hidden, true, 'an old failed poll must not invalidate the regenerated session');
 context.fetch = originalFetch;
 await ids.get('oauthPoll').onclick();
 assert.equal(JSON.parse(requests.at(-1).opts.body).session_id, 'session-3');
 clipboardError = true;
 const beforeClipboardError = ids.get('toasts').children.length;
 await ids.get('oauthCopy').onclick();
 assert.equal(copiedURLs.length, 2, 'clipboard failure must not report another copied URL');
 assert.equal(ids.get('toasts').children.length, beforeClipboardError + 1);
 assert(ids.get('toasts').children.at(-1).className.includes('error'));
 assert(ids.get('toasts').children.at(-1).textContent.includes('复制'), 'clipboard failure needs visible feedback');
 clipboardError = false;
 oauthURLOverride = 'javascript:alert(1)';
 await ids.get('oauthStart').onclick();
 assert(ids.get('addError').textContent.length > 0, 'unsafe authorization URL must be rejected');
 assert(!String(ids.get('oauthLink').href).startsWith('javascript:'), 'unsafe authorization URL reached the link');
 oauthURLOverride = '';
 ids.get('addDialog').close(); ids.get('addAccountButton').onclick();
 assert.equal(ids.get('oauthStart').textContent, '生成授权链接');
 assert.equal(ids.get('oauthCopy').hidden, true); assert.equal(ids.get('oauthLink').hidden, true);
 assert.equal(ids.get('oauthLink').href, undefined);
 ids.get('addDialog').close();
 const beforeDelete = requests.length;
 await ids.get('accountsBody').onclick({ target: { closest: () => ({ dataset: { accountAction: 'delete', uid: 'u1' } }) } });
 assert(ids.get('deleteDialog').open); assert.equal(requests.length, beforeDelete, 'opening delete dialog must not delete');
 await ids.get('confirmDelete').onclick(); assert.equal(requests.filter(r => r.url.endsWith('/accounts/delete')).length, 1);
 // 零账号：模型目录返回空列表时，面板显示空态而不是残留旧数据。
 context.fetch = (url, opts) => url.replace('/admin/api/', '').startsWith('models') ? { ok: true, status: 200, json: async () => ({ data: [] }) } : originalFetch(url, opts);
 context.location.hash = '#accounts'; events.hashchange(); await tick(); await tick();
 context.location.hash = '#models'; events.hashchange(); await tick(); await tick();
 assert.equal(ids.get('modelCount').textContent, 0);
 assert(ids.get('modelsBody').innerHTML.includes('模型目录为空'));
 assert(!ids.get('modelsBody').innerHTML.includes('&lt;model&gt;'));
 context.fetch = originalFetch;
 ids.get('logoutButton').onclick(); assert(ids.get('loginDialog').open); assert.equal(context.sessionStorage.getItem('codearts2api-panel.admin-key'), undefined);
 console.log('PASS: five views, live log polling, escaped recent failures, OAuth controls, session-key login, secret redaction and deletion gate.');
})().catch(error => { console.error(error); process.exitCode = 1; });

