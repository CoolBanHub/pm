const $ = (selector) => document.querySelector(selector);
const $$ = (selector) => [...document.querySelectorAll(selector)];

const ICON = {
  sun: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M4.93 4.93l1.41 1.41M17.66 17.66l1.41 1.41M2 12h2M20 12h2M6.34 17.66l-1.41 1.41M19.07 4.93l-1.41 1.41"/></svg>',
  moon: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 3a6 6 0 0 0 9 9 9 9 0 1 1-9-9Z"/></svg>',
};

// semantic order for state sorting (healthier first)
const STATE_ORDER = { RUNNING: 0, BACKOFF: 1, STOPPING: 2, STARTING: 3, STOPPED: 4, EXITED: 5, FATAL: 6 };

const state = {
  token: sessionStorage.getItem('pm-token') || '',
  processes: [], events: [],
  filter: 'all', groupFilter: 'all', query: '',
  selected: null, selectedNames: new Set(), editingName: null,
  logController: null, logPaused: false, confirmAction: null,
  logContent: '', logQuery: '', logMatchIndex: -1,
  moreMenuName: null,
  // new
  sortKey: 'name', sortDir: 'asc',
  lastSig: '', lastCount: -1, firstLoad: true,
};

async function api(path, options = {}) {
  const headers = { ...(options.headers || {}) };
  if (state.token) headers.Authorization = `Bearer ${state.token}`;
  if (options.body && !headers['Content-Type']) headers['Content-Type'] = 'application/json';
  const response = await fetch(path, { ...options, headers });
  const contentType = response.headers.get('content-type') || '';
  const body = contentType.includes('application/json') ? await response.json() : null;
  if (!response.ok) throw new Error(body?.error || `请求失败 (${response.status})`);
  return body;
}

/* ============================================================
   Bootstrap + connection
   ============================================================ */
async function bootstrap() {
  initTheme();
  try {
    await api('/api/v1/session');
    showApp();
  } catch (error) {
    if (error.message.includes('token')) showAuth();
    else { showApp(); setConnection(false, error.message); }
  }
}

function showAuth() { $('#auth-view').classList.remove('hidden'); $('#app').classList.add('hidden'); }
function showApp() {
  $('#auth-view').classList.add('hidden'); $('#app').classList.remove('hidden');
  renderSkeleton();
  refreshAll();
  setInterval(loadProcesses, 2000);
  setInterval(loadEvents, 5000);
}

$('#auth-form').addEventListener('submit', async (event) => {
  event.preventDefault(); state.token = $('#token-input').value;
  try {
    await api('/api/v1/session'); sessionStorage.setItem('pm-token', state.token); showApp();
  } catch (error) { $('#auth-error').textContent = error.message; }
});

async function refreshAll() { await Promise.allSettled([loadProcesses(), loadEvents()]); }
async function loadProcesses() {
  try {
    const data = await api('/api/v1/processes');
    state.processes = data.processes || [];
    const currentNames = new Set(state.processes.map(process => process.name));
    state.selectedNames = new Set([...state.selectedNames].filter(name => currentNames.has(name)));
    renderGroupOptions();
    setConnection(true); renderProcesses(); renderSummary();
    $('#updated-at').textContent = `更新于 ${formatTime(data.timestamp)}`;
    if (state.selected) {
      state.selected = state.processes.find(p => p.name === state.selected.name) || state.selected;
      renderDrawer();
    }
    state.firstLoad = false;
  } catch (error) { setConnection(false, error.message); }
}
async function loadEvents() {
  try { const data = await api('/api/v1/events?limit=200'); state.events = data.events || []; renderEvents(); }
  catch (error) { toast(error.message, true); }
}

function setConnection(online, detail = '') {
  const label = $('#connection-label');
  label.className = `connection ${online ? 'online' : 'offline'}`;
  label.innerHTML = `<i></i>${online ? '守护进程在线' : '连接中断'}`;
  label.title = detail;
}

/* ============================================================
   Theme
   ============================================================ */
function initTheme() {
  const stored = localStorage.getItem('pm-theme');
  if (stored === 'light' || stored === 'dark') document.documentElement.setAttribute('data-theme', stored);
  updateThemeToggle();
  matchMedia('(prefers-color-scheme: dark)').addEventListener('change', () => { if (!localStorage.getItem('pm-theme')) updateThemeToggle(); });
}
function effectiveTheme() {
  const attr = document.documentElement.getAttribute('data-theme');
  if (attr) return attr;
  return matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
}
function toggleTheme() {
  const next = effectiveTheme() === 'dark' ? 'light' : 'dark';
  document.documentElement.setAttribute('data-theme', next);
  localStorage.setItem('pm-theme', next);
  updateThemeToggle();
}
function updateThemeToggle() {
  $('#theme-toggle').innerHTML = effectiveTheme() === 'dark' ? ICON.sun : ICON.moon;
  $('#theme-toggle').title = effectiveTheme() === 'dark' ? '切换到浅色' : '切换到暗色';
}
$('#theme-toggle').addEventListener('click', toggleTheme);

/* layout uses the comfortable row height only */

/* ============================================================
   Summary
   ============================================================ */
function renderSummary() {
  const running = state.processes.filter(p => p.state === 'RUNNING').length;
  const failed = state.processes.filter(p => p.state === 'FATAL').length;
  const idle = state.processes.length - running - failed;
  const memory = state.processes.reduce((sum, p) => sum + (p.memory_bytes || 0), 0);
  $('#summary-total').textContent = state.processes.length;
  $('#summary-groups').textContent = new Set(state.processes.map(p => p.group || 'default')).size;
  $('#summary-running').textContent = running;
  $('#summary-idle').textContent = idle;
  $('#summary-failed').textContent = failed;
  $('#summary-memory').textContent = formatBytes(memory);
}
$$('.stat-card[data-state]').forEach(card => card.addEventListener('click', () => setStateFilter(card.dataset.state)));

/* ============================================================
   Process list: filter / sort / render (with anti-flicker)
   ============================================================ */
function visibleProcesses() {
  const query = state.query.toLowerCase();
  return state.processes.filter(process => {
    const searchable = `${process.name} ${process.group || 'default'} ${process.command} ${(process.args || []).join(' ')}`.toLowerCase();
    const category = process.state === 'RUNNING' ? 'running' : process.state === 'FATAL' ? 'failed' : 'stopped';
    return (!query || searchable.includes(query)) && (state.filter === 'all' || state.filter === category) && (state.groupFilter === 'all' || process.group === state.groupFilter);
  });
}
function displayedProcesses() { return sortProcesses(visibleProcesses()); }

function processAgeSec(p) {
  if (!p.started_at) return -1;
  const t = new Date(p.started_at).getTime();
  return Number.isNaN(t) ? -1 : (Date.now() - t) / 1000;
}
function sortProcesses(list) {
  const key = state.sortKey, dir = state.sortDir === 'asc' ? 1 : -1;
  const out = [...list];
  out.sort((a, b) => {
    let va, vb;
    switch (key) {
      case 'state': va = STATE_ORDER[a.state] ?? 9; vb = STATE_ORDER[b.state] ?? 9; break;
      case 'cpu': va = a.cpu_percent || 0; vb = b.cpu_percent || 0; break;
      case 'memory': va = a.memory_bytes || 0; vb = b.memory_bytes || 0; break;
      case 'children': va = a.child_processes || 0; vb = b.child_processes || 0; break;
      case 'goroutines': va = a.goroutines ?? -1; vb = b.goroutines ?? -1; break;
      case 'pid': va = a.pid || 0; vb = b.pid || 0; break;
      case 'restarts': va = a.restarts || 0; vb = b.restarts || 0; break;
      case 'uptime': va = processAgeSec(a); vb = processAgeSec(b); break;
      case 'group': va = (a.group || 'default').toLowerCase(); vb = (b.group || 'default').toLowerCase(); break;
      default: va = a.name.toLowerCase(); vb = b.name.toLowerCase();
    }
    if (va < vb) return -1 * dir;
    if (va > vb) return 1 * dir;
    return a.name.localeCompare(b.name);
  });
  return out;
}
function updateSortIndicators() {
  $$('th[data-sort]').forEach(th => {
    if (th.dataset.sort === state.sortKey) th.dataset.sortDir = state.sortDir;
    else delete th.dataset.sortDir;
  });
}
$$('th[data-sort]').forEach(th => th.addEventListener('click', () => {
  const key = th.dataset.sort;
  if (state.sortKey === key) state.sortDir = state.sortDir === 'asc' ? 'desc' : 'asc';
  else { state.sortKey = key; state.sortDir = (key === 'name' || key === 'group') ? 'asc' : 'desc'; }
  updateSortIndicators();
  state.lastSig = ''; // force rebuild so new order animates
  renderProcesses();
}));

function rowMarkup(process, animate) {
  return `<tr data-name="${escapeAttr(process.name)}" class="${animate ? 'is-entering' : ''}">
    <td class="check-column"><input type="checkbox" data-select="${escapeAttr(process.name)}" aria-label="选择 ${escapeAttr(process.name)}" ${state.selectedNames.has(process.name) ? 'checked' : ''}></td>
    <td><button class="process-name" data-detail="${escapeAttr(process.name)}">${escapeHTML(process.name)}</button><span class="process-command">${escapeHTML(commandText(process))}</span></td>
    <td><button class="group-tag group-button" data-edit-group="${escapeAttr(process.name)}" title="修改分组">${escapeHTML(process.group || 'default')}</button></td>
    <td class="cell-state">${statusBadge(process.state, '', process.paused)}</td>
    <td class="num-col cell-pid"><span class="metric">${process.pid || '-'}</span></td>
    <td class="num-col cell-cpu"><span class="metric">${process.pid ? `${(process.cpu_percent || 0).toFixed(1)}%` : '-'}</span></td>
    <td class="num-col cell-mem"><span class="metric">${process.pid ? formatBytes(process.memory_bytes || 0) : '-'}</span></td>
    <td class="num-col cell-children"><span class="metric">${process.pid ? (process.child_processes || 0) : '-'}</span></td>
    <td class="num-col cell-goroutines"><span class="metric">${process.pid && Number.isInteger(process.goroutines) ? process.goroutines : '-'}</span></td>
    <td class="num-col cell-up">${escapeHTML(process.uptime || '-')}</td>
    <td class="num-col cell-restarts">${process.restarts || 0}</td>
    <td><div class="row-actions">${rowActionButtons(process)}</div></td>
  </tr>`;
}
function renderSkeleton() {
  $('#process-count').textContent = '加载中…';
  $('#empty-state').classList.add('hidden');
  $('#process-list').innerHTML = Array.from({ length: 6 }).map(() =>
    `<tr class="skeleton-row"><td class="check-column"></td>${Array.from({ length: 11 }).map(() => '<td><span class="skeleton-cell">&nbsp;</span></td>').join('')}</tr>`
  ).join('');
}
function renderProcesses() {
  const processes = displayedProcesses();
  $('#process-count').textContent = `${processes.length} / ${state.processes.length} 个进程`;
  updateSelectionUI();
  if (processes.length === 0) {
    closeMoreMenu();
    $('#process-list').innerHTML = '';
    $('#empty-state').classList.remove('hidden');
    state.lastSig = ''; state.lastCount = -1;
    return;
  }
  $('#empty-state').classList.add('hidden');

  const sig = processes.map(p => `${p.name}:${p.group}:${p.state}:${p.paused}:${p.disabled}:${commandText(p)}`).join('|');
  if (sig === state.lastSig && processes.length === state.lastCount) {
    updateRowsInPlace(processes);   // smooth: only metrics drifted
    return;
  }
  const animate = state.firstLoad || state.lastCount !== processes.length;
  closeMoreMenu();
  $('#process-list').innerHTML = processes.map(p => rowMarkup(p, animate)).join('');
  state.lastSig = sig; state.lastCount = processes.length;
}
function updateRowsInPlace(processes) {
  const rows = $('#process-list').children;
  for (let i = 0; i < processes.length; i++) {
    const p = processes[i];
    const row = rows[i];
    if (!row || row.dataset.name !== p.name) continue;
    const setHTML = (sel, html) => { const el = row.querySelector(sel); if (el) el.innerHTML = html; };
    setHTML('.cell-pid', `<span class="metric">${p.pid || '-'}</span>`);
    setHTML('.cell-cpu', `<span class="metric">${p.pid ? `${(p.cpu_percent || 0).toFixed(1)}%` : '-'}</span>`);
    setHTML('.cell-mem', `<span class="metric">${p.pid ? formatBytes(p.memory_bytes || 0) : '-'}</span>`);
    setHTML('.cell-children', `<span class="metric">${p.pid ? (p.child_processes || 0) : '-'}</span>`);
    setHTML('.cell-goroutines', `<span class="metric">${p.pid && Number.isInteger(p.goroutines) ? p.goroutines : '-'}</span>`);
    const up = row.querySelector('.cell-up'); if (up) up.textContent = p.uptime || '-';
    const rs = row.querySelector('.cell-restarts'); if (rs) rs.textContent = String(p.restarts || 0);
  }
}

function renderGroupOptions() {
  const groups = [...new Set(state.processes.map(process => process.group || 'default'))].sort();
  const filter = $('#group-filter');
  filter.innerHTML = '<option value="all">全部分组</option>' + groups.map(group => `<option value="${escapeAttr(group)}">${escapeHTML(group)}</option>`).join('');
  if (groups.includes(state.groupFilter)) filter.value = state.groupFilter; else state.groupFilter = 'all';
  $('#group-options').innerHTML = groups.map(group => `<option value="${escapeAttr(group)}"></option>`).join('');
}

function updateSelectionUI() {
  const visible = visibleProcesses().map(process => process.name);
  const selectedVisible = visible.filter(name => state.selectedNames.has(name)).length;
  const selectAll = $('#select-all');
  selectAll.checked = visible.length > 0 && selectedVisible === visible.length;
  selectAll.indeterminate = selectedVisible > 0 && selectedVisible < visible.length;
  $('#selected-count').textContent = `已选择 ${state.selectedNames.size} 项`;
  $('#selection-bar').classList.toggle('hidden', state.selectedNames.size === 0);
  $('#bulk-apply').disabled = state.selectedNames.size === 0 || !$('#bulk-action').value;
}

function actionButtons(process) {
  const logs = `<button class="button quiet" data-logs="${escapeAttr(process.name)}">日志</button>`;
  if (process.paused) {
    return `${logs}<button class="button primary" data-action="resume" data-name="${escapeAttr(process.name)}">恢复</button>`;
  }
  if (process.state === 'RUNNING' || process.state === 'BACKOFF' || process.state === 'STOPPING') {
    return `${logs}<button class="button secondary" data-action="restart" data-name="${escapeAttr(process.name)}">重启</button><button class="button secondary" data-action="stop" data-name="${escapeAttr(process.name)}">停止</button><button class="button secondary" data-action="pause" data-name="${escapeAttr(process.name)}">暂停</button>`;
  }
  return `${logs}<button class="button secondary" data-action="pause" data-name="${escapeAttr(process.name)}">暂停</button><button class="button primary" data-action="start" data-name="${escapeAttr(process.name)}">启动</button>`;
}

function rowActionButtons(process) {
  let primary = '';
  if (process.paused) {
    primary = `<button class="button primary" data-action="resume" data-name="${escapeAttr(process.name)}">恢复</button>`;
  } else if (process.state === 'RUNNING' || process.state === 'BACKOFF' || process.state === 'STOPPING') {
    primary = `<button class="button secondary" data-action="restart" data-name="${escapeAttr(process.name)}">重启</button><button class="button secondary" data-action="stop" data-name="${escapeAttr(process.name)}">停止</button>`;
  } else {
    primary = `<button class="button primary" data-action="start" data-name="${escapeAttr(process.name)}">启动</button>`;
  }
  return `${primary}<button class="button secondary more-button" data-more="${escapeAttr(process.name)}" aria-expanded="false">更多<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m6 9 6 6 6-6"/></svg></button>`;
}

function moreMenuMarkup(process) {
  const pause = process.paused ? '' : `<button role="menuitem" data-action="pause" data-name="${escapeAttr(process.name)}"><span>暂停</span></button>`;
  const remove = process.paused ? `<button role="menuitem" class="danger" data-delete="${escapeAttr(process.name)}"><span>删除</span></button>` : '';
  return `<button role="menuitem" data-logs="${escapeAttr(process.name)}"><span>查看日志</span></button>
    <button role="menuitem" data-edit="${escapeAttr(process.name)}"><span>编辑配置</span></button>
    ${pause}${remove}`;
}

function closeMoreMenu() {
  $('#row-more-menu').classList.add('hidden');
  $$('[data-more][aria-expanded="true"]').forEach(button => button.setAttribute('aria-expanded', 'false'));
  state.moreMenuName = null;
}

function openMoreMenu(button) {
  const name = button.dataset.more;
  if (state.moreMenuName === name) return closeMoreMenu();
  const process = state.processes.find(item => item.name === name);
  if (!process) return;
  closeMoreMenu();
  const menu = $('#row-more-menu');
  menu.innerHTML = moreMenuMarkup(process);
  menu.classList.remove('hidden');
  button.setAttribute('aria-expanded', 'true');
  state.moreMenuName = name;
  const rect = button.getBoundingClientRect();
  const top = rect.bottom + 6 + menu.offsetHeight <= window.innerHeight
    ? rect.bottom + 6
    : Math.max(8, rect.top - menu.offsetHeight - 6);
  menu.style.top = `${top}px`;
  menu.style.left = `${Math.max(8, rect.right - menu.offsetWidth)}px`;
}

/* row interaction */
$('#process-list').addEventListener('click', (event) => {
  const checkbox = event.target.closest('[data-select]');
  if (checkbox) {
    if (checkbox.checked) state.selectedNames.add(checkbox.dataset.select); else state.selectedNames.delete(checkbox.dataset.select);
    updateSelectionUI(); return;
  }
  const detail = event.target.closest('[data-detail]');
  if (detail) return openDrawer(detail.dataset.detail);
  const group = event.target.closest('[data-edit-group]');
  if (group) return openProcessForm(group.dataset.editGroup, '#process-group');
  const logs = event.target.closest('[data-logs]');
  if (logs) { closeMoreMenu(); return openDrawer(logs.dataset.logs, 'logs'); }
  const more = event.target.closest('[data-more]');
  if (more) return openMoreMenu(more);
  const button = event.target.closest('[data-action]');
  if (button) { closeMoreMenu(); return requestAction(button.dataset.action, button.dataset.name); }
});

$('#row-more-menu').addEventListener('click', event => {
  const logs = event.target.closest('[data-logs]');
  if (logs) { closeMoreMenu(); return openDrawer(logs.dataset.logs, 'logs'); }
  const edit = event.target.closest('[data-edit]');
  if (edit) { closeMoreMenu(); return openProcessForm(edit.dataset.edit); }
  const remove = event.target.closest('[data-delete]');
  if (remove) {
    closeMoreMenu();
    return confirm(`确认删除已暂停的进程 ${remove.dataset.delete}？`, () => deleteProcess(remove.dataset.delete));
  }
  const action = event.target.closest('[data-action]');
  if (action) { closeMoreMenu(); return requestAction(action.dataset.action, action.dataset.name); }
});
document.addEventListener('click', event => {
  if (!event.target.closest('[data-more]') && !event.target.closest('#row-more-menu')) closeMoreMenu();
});
window.addEventListener('resize', closeMoreMenu);
window.addEventListener('scroll', closeMoreMenu, true);
$('#search').addEventListener('input', e => { state.query = e.target.value; renderProcesses(); });
$('#group-filter').addEventListener('change', e => { state.groupFilter = e.target.value; renderProcesses(); });
$('#state-filter').addEventListener('click', e => {
  const button = e.target.closest('button'); if (!button) return;
  setStateFilter(button.dataset.state);
});
function setStateFilter(filter) {
  $$('#state-filter button').forEach(item => item.classList.toggle('active', item.dataset.state === filter));
  state.filter = filter; renderProcesses();
}
$('#select-all').addEventListener('change', event => {
  visibleProcesses().forEach(process => event.target.checked ? state.selectedNames.add(process.name) : state.selectedNames.delete(process.name));
  renderProcesses();
});
$('#select-visible').addEventListener('click', () => {
  visibleProcesses().forEach(process => state.selectedNames.add(process.name)); renderProcesses();
});
$('#selection-clear').addEventListener('click', () => { state.selectedNames.clear(); renderProcesses(); });

/* ============================================================
   Actions (start / stop / restart / bulk)
   ============================================================ */
async function requestAction(action, name = 'all') {
  const destructive = action === 'stop' || action === 'restart' || action === 'pause';
  if (destructive) {
    const target = name === 'all' ? '全部进程' : name;
    const label = { stop: '停止', restart: '重启', pause: '暂停' }[action];
    return confirm(`确认${label} ${target}？`, () => runAction(action, name));
  }
  return runAction(action, name);
}
async function runAction(action, name) {
  try {
    const path = name === 'all' ? `/api/v1/actions/${action}` : `/api/v1/processes/${encodeURIComponent(name)}/${action}`;
    const result = await api(path, { method: 'POST' });
    toast(result.message || '操作完成'); spinRefresh(); await loadProcesses();
  } catch (error) { toast(error.message, true); }
}
async function runBulkAction(action, names) {
  try {
    const result = await api(`/api/v1/actions/${action}`, { method: 'POST', body: JSON.stringify({ names }) });
    toast(result.message || '批量操作完成'); state.selectedNames.clear(); $('#bulk-action').value = ''; await loadProcesses();
  } catch (error) { toast(error.message, true); }
}

$('#bulk-apply').addEventListener('click', () => {
  const action = $('#bulk-action').value;
  if (!action) return toast('请选择批量操作', true);
  const names = [...state.selectedNames];
  if (!names.length) return toast('请先选择进程', true);
  const execute = () => runBulkAction(action, names);
  if (action === 'stop' || action === 'restart' || action === 'pause') {
    const label = { stop: '停止', restart: '重启', pause: '暂停' }[action];
    confirm(`确认${label}选中的 ${names.length} 个进程？`, execute);
  }
  else execute();
});
$('#bulk-action').addEventListener('change', updateSelectionUI);
$('#refresh').addEventListener('click', () => { spinRefresh(); refreshAll(); });
$('#events-refresh').addEventListener('click', loadEvents);
function spinRefresh() {
  $$('.refresh-ic').forEach(ic => { ic.classList.add('spinning'); setTimeout(() => ic.classList.remove('spinning'), 800); });
}

/* ============================================================
   Drawer
   ============================================================ */
function openDrawer(name, tab = 'overview') {
  state.selected = state.processes.find(p => p.name === name); if (!state.selected) return;
  $('#drawer-backdrop').classList.remove('hidden'); $('#process-drawer').classList.add('open');
  $('#process-drawer').setAttribute('aria-hidden', 'false'); renderDrawer(); switchTab(tab);
}
function closeDrawer() {
  stopLogStream(); $('#drawer-backdrop').classList.add('hidden'); $('#process-drawer').classList.remove('open');
  $('#process-drawer').setAttribute('aria-hidden', 'true'); state.selected = null;
}
$('#drawer-close').addEventListener('click', closeDrawer);
$('#drawer-backdrop').addEventListener('click', closeDrawer);

function renderDrawer() {
  const p = state.selected; if (!p) return;
  $('#drawer-title').textContent = p.name; $('#drawer-state').outerHTML = statusBadge(p.state, 'drawer-state', p.paused);
  $('#tab-overview').innerHTML = `<div class="detail-grid">
    ${detail('状态', p.state)}${detail('所属分组', p.group || 'default')}${detail('PID', p.pid || '-')}${detail('CPU', p.pid ? `${(p.cpu_percent || 0).toFixed(1)}%` : '-')}${detail('内存', p.pid ? formatBytes(p.memory_bytes || 0) : '-')}
    ${detail('直接子进程', p.pid ? (p.child_processes || 0) : '-')}${detail('全部后代进程', p.pid ? (p.descendant_processes || 0) : '-')}${detail('Go 协程', p.pid && Number.isInteger(p.goroutines) ? p.goroutines : '-')}
    ${detail('运行时间', p.uptime || '-')}${detail('启动次数', p.starts)}${detail('重启次数', p.restarts)}${detail('重启策略', p.restart_policy)}
    ${detail('命令', commandText(p), true)}${detail('工作目录', p.directory || '-', true)}${detail('标准输出', p.stdout_log || '未配置', true)}${detail('标准错误', p.stderr_log || '未配置', true)}
    ${p.last_error ? detail('最近错误', p.last_error, true) : ''}</div>`;
  const deleteHint = p.paused ? '删除进程' : '请先暂停进程后再删除';
  $('#drawer-actions').innerHTML = `<button class="button quiet" data-delete="${escapeAttr(p.name)}" title="${deleteHint}" ${p.paused ? '' : 'disabled'}>删除</button><button class="button secondary" data-edit="${escapeAttr(p.name)}">编辑配置</button>${actionButtons(p)}`;
  renderDrawerEvents();
}
$('#drawer-actions').addEventListener('click', e => {
  const edit = e.target.closest('[data-edit]'); if (edit) return openProcessForm(edit.dataset.edit);
  const remove = e.target.closest('[data-delete]'); if (remove) return confirm(`确认删除已暂停的进程 ${remove.dataset.delete}？`, () => deleteProcess(remove.dataset.delete));
  const logs = e.target.closest('[data-logs]'); if (logs) return switchTab('logs');
  const button = e.target.closest('[data-action]'); if (button) requestAction(button.dataset.action, button.dataset.name);
});

$$('.drawer-tabs button').forEach(button => button.addEventListener('click', () => switchTab(button.dataset.tab)));
function switchTab(tab) {
  $$('.drawer-tabs button').forEach(button => button.classList.toggle('active', button.dataset.tab === tab));
  $$('.tab-panel').forEach(panel => panel.classList.add('hidden')); $(`#tab-${tab}`).classList.remove('hidden');
  if (tab === 'logs') startLogStream(); else stopLogStream();
}

/* ============================================================
   Events
   ============================================================ */
function renderEvents() { $('#event-list').innerHTML = eventMarkup(state.events.slice(0, 50)); renderDrawerEvents(); }
function renderDrawerEvents() {
  if (!state.selected) return;
  $('#drawer-events').innerHTML = eventMarkup(state.events.filter(e => e.program === state.selected.name));
}
function eventMarkup(events) {
  if (!events.length) return '<div class="empty-state compact">暂无事件</div>';
  return events.map(event => `<div class="event-item"><span class="event-time">${formatDate(event.time)}</span><strong>${escapeHTML(event.program || 'daemon')}</strong><span class="event-type">${escapeHTML(event.type)}</span><span class="event-message">${escapeHTML(event.message || event.state || '')}</span></div>`).join('');
}

/* ============================================================
   Log streaming
   ============================================================ */
$('#log-stream').addEventListener('change', startLogStream);
$('#log-follow').addEventListener('change', e => { state.logPaused = !e.target.checked; });
$('#log-clear').addEventListener('click', () => { state.logContent = ''; state.logMatchIndex = -1; renderLog(); });
$('#log-search').addEventListener('input', event => {
  state.logQuery = event.target.value;
  state.logMatchIndex = state.logQuery ? 0 : -1;
  renderLog(true);
});
$('#log-search').addEventListener('keydown', event => {
  if (event.key !== 'Enter') return;
  event.preventDefault();
  moveLogMatch(event.shiftKey ? -1 : 1);
});
$('#log-match-prev').addEventListener('click', () => moveLogMatch(-1));
$('#log-match-next').addEventListener('click', () => moveLogMatch(1));
async function startLogStream() {
  stopLogStream(); if (!state.selected) return;
  state.logContent = '正在连接日志流...\n'; state.logMatchIndex = -1; renderLog();
  const controller = new AbortController(); state.logController = controller;
  try {
    const headers = state.token ? { Authorization: `Bearer ${state.token}` } : {};
    const response = await fetch(`/api/v1/logs/${encodeURIComponent(state.selected.name)}/stream?stream=${$('#log-stream').value}&tail=2000`, { headers, signal: controller.signal });
    if (!response.ok) { const body = await response.json(); throw new Error(body.error); }
    state.logContent = ''; renderLog();
    const reader = response.body.getReader(); const decoder = new TextDecoder(); let pending = '';
    while (true) {
      const { value, done } = await reader.read(); if (done) break;
      pending += decoder.decode(value, { stream: true });
      const frames = pending.split('\n\n'); pending = frames.pop();
      for (const frame of frames) {
        const line = frame.split('\n').find(item => item.startsWith('data: '));
        if (line) appendLog(JSON.parse(line.slice(6)));
      }
    }
  } catch (error) { if (error.name !== 'AbortError') appendLog(`\n[日志流错误] ${error.message}`); }
}
function stopLogStream() { if (state.logController) state.logController.abort(); state.logController = null; }
function appendLog(chunk) {
  state.logContent += chunk;
  if (state.logContent.length > 500000) state.logContent = state.logContent.slice(-400000);
  renderLog();
}
function logMatches() {
  const query = state.logQuery.toLocaleLowerCase();
  if (!query) return [];
  const content = state.logContent.toLocaleLowerCase();
  const matches = [];
  let offset = 0;
  while (matches.length < 1000) {
    const index = content.indexOf(query, offset);
    if (index < 0) break;
    matches.push(index); offset = index + Math.max(query.length, 1);
  }
  return matches;
}
function renderLog(focusMatch = false) {
  const output = $('#log-output');
  const matches = logMatches();
  if (!matches.length || !state.logQuery) {
    output.textContent = state.logContent;
    state.logMatchIndex = -1;
  } else {
    state.logMatchIndex = Math.max(0, Math.min(state.logMatchIndex, matches.length - 1));
    const fragment = document.createDocumentFragment();
    let offset = 0;
    matches.forEach((index, matchIndex) => {
      fragment.append(document.createTextNode(state.logContent.slice(offset, index)));
      const mark = document.createElement('mark');
      mark.className = matchIndex === state.logMatchIndex ? 'current' : '';
      mark.textContent = state.logContent.slice(index, index + state.logQuery.length);
      fragment.append(mark); offset = index + state.logQuery.length;
    });
    fragment.append(document.createTextNode(state.logContent.slice(offset)));
    output.replaceChildren(fragment);
  }
  const total = matches.length;
  $('#log-match-count').textContent = total ? `${state.logMatchIndex + 1} / ${total}${total === 1000 ? '+' : ''}` : '0 / 0';
  $('#log-match-prev').disabled = total === 0;
  $('#log-match-next').disabled = total === 0;
  if (focusMatch && total) output.querySelector('mark.current')?.scrollIntoView({ block: 'center' });
  else if (!state.logPaused && !state.logQuery) output.scrollTop = output.scrollHeight;
}
function moveLogMatch(direction) {
  const total = logMatches().length;
  if (!total) return;
  state.logMatchIndex = (state.logMatchIndex + direction + total) % total;
  renderLog(true);
}

/* ============================================================
   Process form (create / edit)
   ============================================================ */
$('#process-add').addEventListener('click', () => openProcessForm());
$('#process-form-close').addEventListener('click', closeProcessForm);
$('#process-form-cancel').addEventListener('click', closeProcessForm);
$('#process-name').addEventListener('blur', () => {
  const name = $('#process-name').value.trim();
  if (!name || state.editingName) return;
  if (!$('#process-stdout').value) $('#process-stdout').value = `logs/${name}.log`;
  if (!$('#process-stderr').value) $('#process-stderr').value = `logs/${name}.error.log`;
});

async function openProcessForm(name = '', focusField = '') {
  state.editingName = name || null;
  $('#process-form').reset();
  $('#process-group').value = 'default';
  $('#process-form-status').textContent = '';
  $('#process-form-title').textContent = name ? '编辑进程' : '添加进程';
  $('#process-modal').classList.remove('hidden');
  if (!name) { $('#process-name').focus(); return; }
  $('#process-form-status').textContent = '正在读取配置...';
  try {
    const data = await api(`/api/v1/processes/${encodeURIComponent(name)}/config`);
    fillProcessForm(data.program); $('#process-form-status').textContent = '';
    if (focusField) $(focusField)?.focus();
  } catch (error) { $('#process-form-status').textContent = error.message; }
}
function closeProcessForm() { $('#process-modal').classList.add('hidden'); state.editingName = null; }

function fillProcessForm(program) {
  $('#process-name').value = program.name || '';
  $('#process-group').value = program.group || 'default';
  $('#process-command').value = program.command || '';
  $('#process-args').value = (program.args || []).join('\n');
  $('#process-directory').value = program.directory || '';
  $('#process-environment').value = Object.entries(program.environment || {}).sort(([a], [b]) => a.localeCompare(b)).map(([key, value]) => `${key}=${value}`).join('\n');
  $('#process-autostart').checked = Boolean(program.autostart);
  $('#process-restart').value = program.restart || 'unexpected';
  $('#process-restart-delay').value = program.restart_delay || '1s';
  $('#process-max-restarts').value = program.max_restarts ?? 5;
  $('#process-restart-window').value = program.restart_window || '1m';
  $('#process-stop-signal').value = program.stop_signal || 'TERM';
  $('#process-stop-timeout').value = program.stop_timeout || '10s';
  $('#process-stdout').value = program.stdout_log || '';
  $('#process-stderr').value = program.stderr_log || '';
  $('#process-pprof-url').value = program.pprof_url || '';
  $('#process-log-size').value = Math.round((program.log_max_bytes || 0) / 1048576);
  $('#process-log-backups').value = program.log_backups ?? 3;
}

function processFormValue() {
  const environment = {};
  for (const rawLine of $('#process-environment').value.split('\n')) {
    const line = rawLine.trim(); if (!line) continue;
    const separator = line.indexOf('=');
    if (separator <= 0) throw new Error(`环境变量格式错误：${line}`);
    environment[line.slice(0, separator).trim()] = line.slice(separator + 1);
  }
  return {
    name: $('#process-name').value.trim(), group: $('#process-group').value.trim(), command: $('#process-command').value.trim(),
    args: $('#process-args').value.split('\n').map(value => value.trim()).filter(Boolean),
    directory: $('#process-directory').value.trim(), environment, autostart: $('#process-autostart').checked,
    restart: $('#process-restart').value, restart_delay: $('#process-restart-delay').value.trim(),
    max_restarts: Number($('#process-max-restarts').value), restart_window: $('#process-restart-window').value.trim(),
    stop_signal: $('#process-stop-signal').value, stop_timeout: $('#process-stop-timeout').value.trim(),
    stdout_log: $('#process-stdout').value.trim(), stderr_log: $('#process-stderr').value.trim(),
    pprof_url: $('#process-pprof-url').value.trim(),
    log_max_bytes: Math.round(Number($('#process-log-size').value) * 1048576), log_backups: Number($('#process-log-backups').value),
  };
}

$('#process-form').addEventListener('submit', async event => {
  event.preventDefault(); $('#process-form-status').textContent = '正在保存...';
  try {
    const program = processFormValue();
    const editing = state.editingName;
    const path = editing ? `/api/v1/processes/${encodeURIComponent(editing)}` : '/api/v1/processes';
    const result = await api(path, { method: editing ? 'PUT' : 'POST', body: JSON.stringify(program) });
    toast(result.message || '进程已保存'); closeProcessForm(); closeDrawer(); await refreshAll();
  } catch (error) { $('#process-form-status').textContent = error.message; }
});

async function deleteProcess(name) {
  try {
    const result = await api(`/api/v1/processes/${encodeURIComponent(name)}`, { method: 'DELETE' });
    toast(result.message || '进程已删除'); state.selectedNames.delete(name); closeDrawer(); await refreshAll();
  } catch (error) { toast(error.message, true); }
}

/* ============================================================
   Config editor
   ============================================================ */
$('#config-open').addEventListener('click', openConfig);
$('#config-close').addEventListener('click', () => $('#config-modal').classList.add('hidden'));
async function openConfig() {
  $('#config-modal').classList.remove('hidden'); $('#config-status').textContent = '正在读取...';
  try { const data = await api('/api/v1/config'); $('#config-editor').value = data.content; $('#config-path').textContent = data.path; $('#config-status').textContent = ''; }
  catch (error) { $('#config-status').textContent = error.message; }
}
$('#config-validate').addEventListener('click', async () => {
  try { await api('/api/v1/config/validate', { method: 'POST', body: JSON.stringify({ content: $('#config-editor').value }) }); $('#config-status').textContent = '配置有效'; }
  catch (error) { $('#config-status').textContent = error.message; }
});
$('#config-save').addEventListener('click', () => confirm('保存配置并重新加载所有进程？', saveConfig));
async function saveConfig() {
  $('#config-status').textContent = '正在保存...';
  try {
    const data = await api('/api/v1/config', { method: 'PUT', body: JSON.stringify({ content: $('#config-editor').value, apply: true }) });
    $('#config-status').textContent = data.restart_required ? '已保存；Web 或运行目录变更需重启守护进程' : data.message;
    toast($('#config-status').textContent); if (!data.restart_required) refreshAll();
  } catch (error) { $('#config-status').textContent = error.message; toast(error.message, true); }
}

/* ============================================================
   Confirm modal
   ============================================================ */
function confirm(message, action) {
  $('#confirm-message').textContent = message; state.confirmAction = action; $('#confirm-modal').classList.remove('hidden');
}
$('#confirm-cancel').addEventListener('click', closeConfirm);
$('#confirm-ok').addEventListener('click', () => { const action = state.confirmAction; closeConfirm(); action?.(); });
function closeConfirm() { $('#confirm-modal').classList.add('hidden'); state.confirmAction = null; }

/* ============================================================
   Toast (animated + dismissible)
   ============================================================ */
function toast(message, error = false) {
  const item = document.createElement('div'); item.className = `toast ${error ? 'error' : ''}`; item.textContent = message;
  let timer;
  const remove = () => { clearTimeout(timer); item.classList.add('leaving'); setTimeout(() => item.remove(), 240); };
  item.addEventListener('click', remove);
  $('#toasts').appendChild(item);
  timer = setTimeout(remove, 3500);
}

/* ============================================================
   Global keys: Esc closes overlays
   ============================================================ */
function closeTopmost() {
  if (!$('#row-more-menu').classList.contains('hidden')) return closeMoreMenu();
  if (!$('#confirm-modal').classList.contains('hidden')) return closeConfirm();
  if (!$('#process-modal').classList.contains('hidden')) return closeProcessForm();
  if (!$('#config-modal').classList.contains('hidden')) return $('#config-modal').classList.add('hidden');
  if ($('#process-drawer').classList.contains('open')) return closeDrawer();
}
document.addEventListener('keydown', event => {
  if (event.key === 'Escape') { closeTopmost(); return; }
});

/* ============================================================
   Helpers
   ============================================================ */
function statusBadge(status, id = '', paused = false) {
  const labels = { RUNNING: '运行中', STOPPED: '已停止', EXITED: '已退出', BACKOFF: '等待重启', STOPPING: '停止中', STARTING: '启动中', FATAL: '异常' };
  const cls = status === 'RUNNING' ? 'status-running' : status === 'FATAL' ? 'status-fatal' : status === 'BACKOFF' ? 'status-backoff' : '';
  return `<span ${id ? `id="${id}"` : ''} class="status-badge ${paused ? 'status-backoff' : cls}">${paused ? '已暂停' : (labels[status] || status)}</span>`;
}
function detail(label, value, wide = false) { return `<div class="detail-item ${wide ? 'wide' : ''}"><span>${label}</span><code>${escapeHTML(String(value ?? '-'))}</code></div>`; }
function commandText(p) { return [p.command, ...(p.args || [])].join(' '); }
function formatBytes(bytes) {
  if (!bytes) return '0 B'; const units = ['B', 'KB', 'MB', 'GB']; let value = bytes, unit = 0;
  while (value >= 1024 && unit < units.length - 1) { value /= 1024; unit++; }
  return `${value.toFixed(unit > 1 ? 1 : 0)} ${units[unit]}`;
}
function formatTime(value) { return new Date(value).toLocaleTimeString('zh-CN', { hour12: false }); }
function formatDate(value) { return new Date(value).toLocaleString('zh-CN', { hour12: false }); }
function escapeHTML(value) { const node = document.createElement('span'); node.textContent = value ?? ''; return node.innerHTML; }
function escapeAttr(value) { return escapeHTML(value).replaceAll('"', '&quot;'); }

bootstrap();
