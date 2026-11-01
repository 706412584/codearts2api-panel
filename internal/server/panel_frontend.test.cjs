const fs = require('node:fs');
const vm = require('node:vm');
const assert = require('node:assert/strict');
const base = __dirname + '/';
const html = fs.readFileSync(base + 'panel.html', 'utf8');
const script = fs.readFileSync(base + 'panel.js', 'utf8');
const all = [], ids = new Map(), events = {}, requests = [];
class Element {
  constructor(attrs = '') { this.attrs = attrs; this.id = /\bid="([^"]+)"/.exec(attrs)?.[1] || ''; this.value = ''; this.textContent = ''; this.innerHTML = ''; this.hidden = /\bhidden\b/.test(attrs); this.open = false; this.disabled = false; this.dataset = {}; this.handlers = {}; this.classes = new Set((/\bclass="([^"]+)"/.exec(attrs)?.[1] || '').split(' ')); for (const a of attrs.matchAll(/data-([\w-]+)="([^"]*)"/g)) this.dataset[a[1].replace(/-([a-z])/g, (_, c) => c.toUpperCase())] = a[2]; this.classList = { add: x => this.classes.add(x), remove: x => this.classes.delete(x), contains: x => this.classes.has(x), toggle: (x, enabled) => { if (enabled === undefined) enabled = !this.classes.has(x); enabled ? this.classes.add(x) : this.classes.delete(x); return enabled; } }; }
  addEventListener(name, cb) { (this.handlers[name] ||= []).push(cb); }
  setAttribute(k, v) { this[k] = v; }
  removeAttribute(k) { delete this[k]; }
  showModal() { this.open = true; }
  close() { this.open = false; for (const cb of this.handlers.close || []) cb(); }
  focus() {}
  append() {}
  remove() {}
  reset() { this.value = ''; }
  querySelector() { return new Element(); }
}
for (const match of html.matchAll(/<\w+\b([^>]+)>/g)) { const el = new Element(match[1]); all.push(el); if (el.id) { assert(!ids.has(el.id), 'duplicate id ' + el.id); ids.set(el.id, el); } }
const selector = s => s === '.view' ? all.filter(e => e.classes.has('view')) : /^\[data-([\w-]+)\]$/.test(s) ? all.filter(e => Object.hasOwn(e.dataset, s.slice(6, -1).replace(/-([a-z])/g, (_, c) => c.toUpperCase()))) : [];
const storage = () => { const values = new Map(); return { getItem: k => values.get(k), setItem: (k, v) => values.set(k, v), removeItem: k => values.delete(k) }; };
const account = { uid: 'u1', nickname: '<img src=x onerror=alert(1)>', credit_balance: null, active_concurrent: 0, max_concurrent: 1, expires_at: '2027-01-01T00:00:00Z' };
const overview = { stats: { total: 1, healthy: 1, cooling: 0, disabled: 0 }, accounts: [account] };
const usage = { completed: 2, succeeded: 1, failed: 1, avg_duration_ms: 500, recent: [{ time: new Date().toISOString(), model: 'test-model', ok: true, status: 200, total_tokens: 25, duration_ms: 500 }] };
const document = { getElementById: id => { assert(ids.has(id), 'missing id ' + id); return ids.get(id); }, querySelectorAll: selector, createElement: () => new Element(), documentElement: { dataset: {} } };
const context = vm.createContext({ document, console, sessionStorage: storage(), localStorage: storage(), matchMedia: () => ({ matches: false }), addEventListener: (n, cb) => { events[n] = cb; }, location: { hash: '#accounts' }, setTimeout: () => 1, clearTimeout() {}, setInterval: () => 1, clearInterval() {}, Headers, URL, navigator: { clipboard: { writeText: async () => {} } }, fetch: async (url, opts) => { requests.push({ url, opts }); let data; const path = url.replace('/admin/api/', ''); if (path === 'overview') data = overview; else if (path.startsWith('models')) data = { data: [{ id: '<model>', model_name: 'Kimi <K3>', ratio_display: '0.7x', context_length: 1000 }] }; else if (path === 'usage') data = usage; else if (path === 'logs') data = { entries: [{ ts: new Date().toISOString(), ch: 'sys', text: '<script>alert(1)</script>' }] }; else if (path === 'config') data = opts.method === 'PUT' ? { ok: true, config: JSON.parse(opts.body), restart_required: true } : { config: { listen: ':7866', admin_key: 'server-secret', api_key: 'api-secret' }, env_overrides: [] }; else data = { ok: true }; return { ok: true, status: 200, json: async () => data }; } });
const tick = () => new Promise(resolve => setImmediate(resolve));
(async () => {
 vm.runInContext(script, context);
 assert(ids.get('loginDialog').open, 'login dialog must gate first visit');
 ids.get('loginKey').value = 'test-admin-secret';
 await ids.get('loginForm').onsubmit({ preventDefault() {} }); await tick();
 assert(!ids.get('loginDialog').open); assert.equal(ids.get('loginKey').value, '');
 assert(ids.get('accountsBody').innerHTML.includes('&lt;img')); assert(!ids.get('accountsBody').innerHTML.includes('<img')); assert(ids.get('accountsBody').innerHTML.includes('未知'));
 for (const page of ['models', 'usage', 'logs', 'config']) { context.location.hash = '#' + page; events.hashchange(); await tick(); await tick(); assert.equal(ids.get('pageError').hidden, true, 'page failed: ' + page); }
 assert(ids.get('modelsBody').innerHTML.includes('&lt;model&gt;'));
 assert(ids.get('modelsBody').innerHTML.includes('Kimi &lt;K3&gt;'));
 assert(!ids.get('modelsBody').innerHTML.includes('<K3>'));
 assert(ids.get('usageChart').innerHTML.includes('<svg'));
 assert(ids.get('runtimeLogs').innerHTML.includes('&lt;script&gt;'));
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
 const beforeDelete = requests.length;
 await ids.get('accountsBody').onclick({ target: { closest: () => ({ dataset: { accountAction: 'delete', uid: 'u1' } }) } });
 assert(ids.get('deleteDialog').open); assert.equal(requests.length, beforeDelete, 'opening delete dialog must not delete');
 await ids.get('confirmDelete').onclick(); assert.equal(requests.filter(r => r.url.endsWith('/accounts/delete')).length, 1);
 ids.get('logoutButton').onclick(); assert(ids.get('loginDialog').open); assert.equal(context.sessionStorage.getItem('codearts2api-panel.admin-key'), undefined);
 console.log('PASS: five views, escaped API content, session-key login, secret redaction, restart notice and explicit deletion gate.');
})().catch(error => { console.error(error); process.exitCode = 1; });

