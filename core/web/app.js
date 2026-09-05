// FileDrop 前端：分块上传 + 列表 + 下载
'use strict';

const CHUNK = 8 * 1024 * 1024; // 8 MiB
const token = new URLSearchParams(location.search).get('t') || '';

const $ = (id) => document.getElementById(id);
const drop = $('drop'), fileInput = $('file'), upBar = $('upBar'), upStatus = $('upStatus');
const addrEl = $('addr'), copyBtn = $('copyAddr'), fileListEl = $('fileList');
const recvDirEl = $('recvDir'), dirInput = $('dirInput'), prefixInput = $('prefixInput');
const PREFIX_KEY = 'fd_prefix';

// 下载文件名前缀（存在手机/浏览器本地，默认加 FileDrop_ 便于在下载目录里识别）
function getPrefix() {
  const v = localStorage.getItem(PREFIX_KEY);
  return v === null ? 'FileDrop_' : v;
}

function fmtSize(b) {
  if (b < 1024) return b + ' B';
  const u = ['KB', 'MB', 'GB', 'TB'];
  let i = -1; do { b /= 1024; i++; } while (b >= 1024 && i < u.length - 1);
  return b.toFixed(2) + ' ' + u[i];
}
function fmtSpeed(bps) {
  return fmtSize(bps) + '/s';
}
function fmtTime(s) {
  s = Math.max(0, Math.round(s));
  if (s < 60) return s + ' 秒';
  if (s < 3600) return Math.floor(s / 60) + ' 分 ' + (s % 60) + ' 秒';
  return Math.floor(s / 3600) + ' 时 ' + Math.floor((s % 3600) / 60) + ' 分';
}
function setStatus(t) { upStatus.textContent = t; }

// ---- 本机地址 ----
async function loadInfo() {
  try {
    const r = await fetch('/api/info');
    if (r.ok) {
      const j = await r.json();
      const url = j.url || (j.ip + ':' + j.port);
      addrEl.textContent = url;
      if (/^https?:\/\//.test(url)) {
        const qr = document.getElementById('qr');
        qr.src = '/api/qr?text=' + encodeURIComponent(url);
        qr.style.display = 'block';
      }
      return;
    }
  } catch (_) {}
  addrEl.textContent = '（无法获取，请检查服务）';
}
copyBtn.addEventListener('click', () => {
  navigator.clipboard?.writeText(addrEl.textContent).then(
    () => { const t = copyBtn.textContent; copyBtn.textContent = '已复制'; setTimeout(() => (copyBtn.textContent = t), 1200); },
    () => {}
  );
});

// ---- 文件列表 ----
async function loadFiles() {
  try {
    const r = await fetch('/api/files');
    if (!r.ok) throw new Error('list ' + r.status);
    const list = await r.json();
    if (!list.length) { fileListEl.innerHTML = '<div class="empty">本机还没有可下载的文件</div>'; return; }
    let html = '<table><thead><tr><th>文件名</th><th>大小</th><th>操作</th></tr></thead><tbody>';
    const prefix = getPrefix();
    for (const f of list) {
      const enc = encodeURIComponent(f.name);
      const dlName = prefix + f.name;
      html += '<tr><td>' + escapeHtml(f.name) + '</td><td class="size">' + fmtSize(f.size) +
        '</td><td><a class="dl" href="/api/download?name=' + enc + '" download="' + escapeHtml(dlName) + '">下载</a>' +
        ' · <a href="#" class="dl" data-act="rename" data-name="' + escapeHtml(f.name) + '">重命名</a>' +
        ' · <a href="#" class="dl" data-act="del" data-name="' + escapeHtml(f.name) + '">删除</a></td></tr>';
    }
    html += '</tbody></table>';
    fileListEl.innerHTML = html;
  } catch (e) {
    fileListEl.innerHTML = '<div class="empty">列表加载失败：' + escapeHtml(String(e)) + '</div>';
  }
}
function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

// ---- 分块上传：任务队列（一行一个文件，可单独取消 / 原地续传） ----
// v0.2：位图断点续传 + 并发分块 + complete 收尾
// v0.4：队列化。此前每选一次文件就新起一条 Promise 链，两条链同时写同一个进度条
//       和状态行，数字会来回跳；而且 4GB 的传输一旦开始就停不下来。
const isLocal = ['127.0.0.1', 'localhost', '[::1]'].includes(location.hostname);
const CONC = 3; // 单文件内的分块并发度：3 路大致能压满 5GHz WiFi，再高手机侧反而堵

const queueEl = $('upQueue'), summaryEl = $('upSummary');
const cancelAllBtn = $('cancelAll'), clearDoneBtn = $('clearDone');

let tasks = [], taskIdSeq = 0, runner = null;

const isAbort = (e) => !!e && (e.name === 'AbortError' || e.code === 20);

// 取样指纹：头 / 中 / 尾各 64KiB 加上 8 字节小端 size，双 lane FNV-1a。
// 服务端拿它判断「本机已经存有同一份内容」，算法必须与 core/upload.go 逐字节一致。
// 明面上只用名字 + 大小判重是不够的：改了内容再导出一份常常同名同大小，
// 判成重复就等于把用户刚发的文件悄悄丢掉。这里是 http 源，crypto.subtle 用不了，
// 而判重也不需要加密强度。
const SAMPLE = 64 * 1024;

function fnv1a(h, b) {
  for (let i = 0; i < b.length; i++) { h ^= b[i]; h = Math.imul(h, 0x01000193); }
  return h >>> 0;
}

async function fingerprint(file) {
  const size = file.size;
  let half = Math.floor(size / 2);
  if (half > SAMPLE / 2) half -= Math.floor(SAMPLE / 2);
  const tail = Math.max(0, size - SAMPLE);
  let h1 = 0x811c9dc5, h2 = 0x01000193;
  const mix = (b) => { h1 = fnv1a(h1, b); h2 = fnv1a(h2 ^ b.length, b); };
  const sz = new Uint8Array(8);
  for (let i = 0; i < 8; i++) sz[i] = Math.floor(size / Math.pow(2, 8 * i)) & 0xff;
  mix(sz);
  for (const off of [0, half, tail]) {
    const n = Math.min(SAMPLE, size - off);
    const b = new Uint8Array(await file.slice(off, off + n).arrayBuffer());
    if (b.length < n) { const p = new Uint8Array(n); p.set(b); mix(p); } else { mix(b); } // 与服务端 ReadAt 短读补零一致
  }
  return (h1 >>> 0).toString(16).padStart(8, '0') + (h2 >>> 0).toString(16).padStart(8, '0');
}

function makeTask(file) {
  const t = {
    id: ++taskIdSeq, file, name: file.name, size: file.size,
    total: Math.max(1, Math.ceil(file.size / CHUNK)),
    sent: 0, bytes: 0, bytesAt: 0, tAt: 0, speed: 0,
    status: 'queued', aborted: false, controller: null, err: '', finalName: '', reused: false,
  };
  const row = document.createElement('div');
  row.className = 'qrow queued';

  const head = document.createElement('div'); head.className = 'qhead';
  const nm = document.createElement('span'); nm.className = 'qname';
  nm.textContent = file.name;                     // textContent：文件名里的 <>"& 不会被解析成标签
  const sz = document.createElement('span'); sz.className = 'qsize';
  sz.textContent = fmtSize(file.size);
  head.append(nm, sz);

  const bar = document.createElement('div'); bar.className = 'qbar';
  const fill = document.createElement('span'); bar.append(fill);

  const stat = document.createElement('div'); stat.className = 'qstat';
  const text = document.createElement('span');
  const spacer = document.createElement('span'); spacer.className = 'spacer';
  const act = document.createElement('button'); act.className = 'qact danger';
  stat.append(text, spacer, act);

  row.append(head, bar, stat);
  Object.assign(t, { row, fill, text, act });
  act.addEventListener('click', () => {
    if (t.status === 'running') cancelTask(t);
    else if (t.status === 'failed' || t.status === 'cancelled') retryTask(t);
    else removeTask(t);
  });
  return t;
}

function renderTask(t) {
  t.row.className = 'qrow ' + t.status + (t.status === 'running' ? ' active' : '');
  t.fill.style.width = Math.min(100, (t.sent / t.total) * 100).toFixed(1) + '%';
  t.act.textContent = { running: '取消', queued: '移除', failed: '续传', cancelled: '续传', done: '移除' }[t.status] || '移除';
  t.act.classList.toggle('danger', t.status === 'running' || t.status === 'queued');
  if (t.status === 'running') {
    const pct = ((t.sent / t.total) * 100).toFixed(0);
    const left = Math.max(0, t.total - t.sent) * CHUNK;
    t.text.textContent = t.sent === 0 && t.speed === 0
      ? '准备中…'
      : pct + '% · ' + fmtSpeed(t.speed) + ' · 剩 ' + fmtTime(left / (t.speed || 1)) +
        ' · ' + t.sent + '/' + t.total + ' 块';
  } else if (t.status === 'queued') t.text.textContent = '排队中';
  else if (t.status === 'done') {
    // 跳过与改名可能同时发生：服务端既复用了旧文件，又把非法字符清洗成了另一个名字
    const renamed = t.finalName && t.finalName !== t.name ? ' · 已存为 ' + t.finalName : '';
    t.text.textContent = '完成 ✓' + (t.reused ? '（本机已有相同内容，未重复保存）' : '') + renamed;
  } else if (t.status === 'failed') t.text.textContent = '失败：' + t.err;
  else if (t.status === 'cancelled') t.text.textContent = '已取消（' + t.sent + '/' + t.total + ' 块已在电脑端，可续传）';
}

function renderTotals() {
  const totalChunks = tasks.reduce((n, t) => n + t.total, 0) || 1;
  const doneChunks = tasks.reduce((n, t) => n + Math.min(t.sent, t.total), 0);
  upBar.style.width = ((doneChunks / totalChunks) * 100).toFixed(1) + '%';

  const active = tasks.filter((t) => t.status === 'running' || t.status === 'queued').length;
  const done = tasks.filter((t) => t.status === 'done').length;
  const bad = tasks.filter((t) => t.status === 'failed' || t.status === 'cancelled').length;
  const speed = tasks.reduce((n, t) => n + (t.speed || 0), 0);
  const leftBytes = tasks.reduce((n, t) => n + Math.max(0, t.total - t.sent) * CHUNK, 0);
  const bits = [];
  if (tasks.length) bits.push(done + '/' + tasks.length + ' 个');
  if (active) bits.push(fmtSpeed(speed) + ' · 剩 ' + fmtTime(leftBytes / (speed || 1)));
  if (bad) bits.push(bad + ' 个未完成');
  summaryEl.textContent = bits.join(' · ');
  cancelAllBtn.style.display = active ? '' : 'none';
  clearDoneBtn.style.display = tasks.some((t) => t.status === 'done' || t.status === 'failed' || t.status === 'cancelled') ? '' : 'none';
}

function sampleSpeed(t) {
  const now = performance.now(), dt = (now - t.tAt) / 1000;
  if (dt >= 0.7) {
    t.speed = (t.bytes - t.bytesAt) / dt;
    t.tAt = now; t.bytesAt = t.bytes;
  }
}

function cancelTask(t) {
  t.aborted = true;
  if (t.controller) t.controller.abort(); // 掐断在途分块；服务端保留已收块，之后可续传
  renderTask(t); renderTotals();
}

function removeTask(t) {
  if (t.status === 'running') cancelTask(t);
  t.row.remove();
  tasks = tasks.filter((x) => x !== t);
  renderTotals();
}

function retryTask(t) {
  t.status = 'queued'; t.err = ''; t.aborted = false;
  renderTask(t); renderTotals();
  runQueue();
}

function enqueueFiles(fileList) {
  const files = Array.from(fileList || []);
  if (!files.length) return;
  if (!token && !isLocal) {
    setStatus('当前页面没有上传令牌：请用电脑上显示的「带令牌地址」打开本页后再上传。');
    return;
  }
  const empties = files.filter((f) => f.size === 0).map((f) => f.name);
  files.filter((f) => f.size > 0).forEach((f) => {
    const t = makeTask(f);
    tasks.push(t);
    queueEl.appendChild(t.row);
    renderTask(t);
  });
  if (empties.length) setStatus('已跳过空文件：' + empties.join('、'));
  renderTotals();
  runQueue();
}

async function runTask(t) {
  t.status = 'running';
  t.controller = new AbortController();
  t.tAt = performance.now(); t.bytesAt = 0; t.speed = 0;
  renderTask(t); renderTotals();

  const { name, size } = t;
  const signal = t.controller.signal;
  const chunkURL = (i) => '/api/upload/chunk?t=' + encodeURIComponent(token) +
    '&name=' + encodeURIComponent(name) + '&index=' + i + '&size=' + size;
  const sendChunk = async (i) => {
    const begin = i * CHUNK, end = Math.min(begin + CHUNK, size);
    const r = await fetch(chunkURL(i), {
      method: 'POST', headers: { 'Content-Type': 'application/octet-stream' },
      body: t.file.slice(begin, end), signal,
    });
    if (!r.ok) throw new Error('分块 ' + i + ' 失败（HTTP ' + r.status + '）');
    t.sent++; t.bytes += end - begin;
  };

  // 1) 查缺块。查询失败就直接判失败：默默按「全新上传」重发几个 GB 才是更大的浪费，
  //    手机端此时多半已经掉线，续传按钮才是用户要的。
  let st;
  let fp = '';
  try { fp = await fingerprint(t.file); } catch (e) { fp = ''; }
  for (let i = 0; ; i++) {
    try {
      const r = await fetch('/api/upload/status?name=' + encodeURIComponent(name) + '&size=' + size +
        (fp ? '&fp=' + fp : ''), { signal });
      if (!r.ok) throw new Error('HTTP ' + r.status);
      st = await r.json();
      break;
    } catch (e) {
      if (isAbort(e) || t.aborted) throw e;
      if (i >= 2) throw new Error('连不上服务（' + e + '），可点「续传」重试');
      await new Promise((res) => setTimeout(res, 400 * (i + 1)));
    }
  }
  t.total = st.total || t.total;
  if (st.complete) {
    t.sent = t.total; t.status = 'done'; t.reused = true; t.finalName = st.name || name;
    renderTask(t); renderTotals();
    return;
  }
  const missing = (st.missing || []).slice();
  t.sent = Math.max(0, t.total - missing.length);
  renderTask(t);

  // 2) 并发补缺块
  let firstErr = null;
  async function worker() {
    while (missing.length && !t.aborted && !firstErr) {
      try {
        await sendChunk(missing.shift());
      } catch (e) { if (!firstErr) firstErr = e; break; }
      sampleSpeed(t);
      renderTask(t); renderTotals();
    }
  }
  await Promise.all(Array.from({ length: Math.min(CONC, missing.length) }, worker));
  if (t.aborted) { t.status = 'cancelled'; t.speed = 0; renderTask(t); renderTotals(); return; }
  if (firstErr) throw firstErr;

  // 3) 收尾；服务端可能回 missing 让补传
  for (let attempt = 0; attempt < 3; attempt++) {
    const r = await fetch('/api/upload/complete?t=' + encodeURIComponent(token) +
      '&name=' + encodeURIComponent(name) + '&size=' + size, { method: 'POST', signal });
    const j = await r.json().catch(() => ({}));
    if (r.ok) {
      t.sha256 = j.sha256 || '';
      t.finalName = j.name || name;
      t.reused = !!j.reused;
      t.sent = t.total; t.status = 'done'; t.speed = 0;
      renderTask(t); renderTotals();
      return;
    }
    if (Array.isArray(j.missing) && j.missing.length) {
      t.text.textContent = '补传缺失的 ' + j.missing.length + ' 块…';
      await Promise.all(j.missing.map(sendChunk));
      continue;
    }
    throw new Error('收尾失败：' + (j.error || r.status));
  }
  throw new Error('收尾重试次数用尽');
}

// 全局只有一个 runner：排队中的文件由它按序取走，新加入的文件自然排在队尾。
function runQueue() {
  if (runner) return;
  runner = Promise.resolve().then(async () => {
    try {
      while (true) {
        const t = tasks.find((x) => x.status === 'queued');
        if (!t) break;
        try {
          await runTask(t);
        } catch (e) {
          if (t.aborted || isAbort(e)) t.status = 'cancelled';
          else { t.status = 'failed'; t.err = String(e && e.message ? e.message : e); }
          t.speed = 0;
          renderTask(t);
        }
        loadFiles();
        loadPartials();   // 取消 / 失败都会改变「中断的传输」
      }
    } finally {
      runner = null;
      renderTotals();
    }
  });
}

if (cancelAllBtn) {
  cancelAllBtn.addEventListener('click', () => {
    tasks.filter((t) => t.status === 'running' || t.status === 'queued').forEach(cancelTask);
    setStatus('已取消全部上传');
  });
}
if (clearDoneBtn) {
  clearDoneBtn.addEventListener('click', () => {
    tasks.filter((t) => t.status === 'done' || t.status === 'failed' || t.status === 'cancelled')
      .forEach((t) => t.row.remove());
    tasks = tasks.filter((t) => t.status === 'running' || t.status === 'queued');
    renderTotals();
  });
}

// ---- 交互绑定 ----
drop.addEventListener('click', () => fileInput.click());
fileInput.addEventListener('change', () => { enqueueFiles(fileInput.files); fileInput.value = ''; });
['dragover', 'dragenter'].forEach((ev) => drop.addEventListener(ev, (e) => { e.preventDefault(); drop.classList.add('over'); }));
['dragleave', 'drop'].forEach((ev) => drop.addEventListener(ev, (e) => { e.preventDefault(); drop.classList.remove('over'); }));
drop.addEventListener('drop', (e) => {
  if (e.dataTransfer && e.dataTransfer.files && e.dataTransfer.files.length) enqueueFiles(e.dataTransfer.files);
});
// 没拖中虚线框时，浏览器会把整个页面替换成该文件；在 window 上兜底阻止
['dragover', 'drop'].forEach((ev) => window.addEventListener(ev, (e) => {
  if (e.dataTransfer && Array.from(e.dataTransfer.types || []).includes('Files')) e.preventDefault();
}));

// ---- 删除 / 重命名（事件委托） ----
fileListEl.addEventListener('click', async (e) => {
  const a = e.target.closest('a[data-act]');
  if (!a) return;
  e.preventDefault();
  const name = a.dataset.name;
  const tqs = token ? '&t=' + encodeURIComponent(token) : '';
  if (a.dataset.act === 'del') {
    if (!confirm('确定删除 ' + name + ' ？')) return;
    const r = await fetch('/api/files?name=' + encodeURIComponent(name) + tqs, { method: 'DELETE' });
    if (r.ok) loadFiles();
    else { const j = await r.json().catch(() => ({})); alert('删除失败：' + (j.error || r.status)); }
  } else if (a.dataset.act === 'rename') {
    const nn = prompt('新文件名：', name);
    if (!nn || nn === name) return;
    const r = await fetch('/api/rename?t=' + encodeURIComponent(token), {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name: name, newName: nn })
    });
    const j = await r.json().catch(() => ({}));
    if (r.ok) loadFiles();
    else alert('重命名失败：' + (j.error || r.status));
  }
});

// ---- SSE 实时刷新（任一设备完成上传 / 删除 / 改名，所有页面同步列表） ----
let filesRefreshTimer = null;
function listenEvents() {
  try {
    const es = new EventSource('/api/events');
    es.onmessage = (ev) => {
      try {
        const j = JSON.parse(ev.data);
        if (j.type === 'files') {
          clearTimeout(filesRefreshTimer);
          filesRefreshTimer = setTimeout(loadFiles, 200);
        }
      } catch (_) {}
    };
  } catch (_) {}
}

// ---- 中断的传输（残留分块清理） ----
// 上传中断会在电脑端留下 .part 残留（可能几个 GB），在文件列表里不可见，
// 这里把它们列出来，支持单个或全部清理。
const partialListEl = $('partialList'), purgeAllBtn = $('purgeAll');

async function loadPartials() {
  if (!partialListEl) return;
  try {
    const r = await fetch('/api/uploads');
    if (!r.ok) throw new Error('HTTP ' + r.status);
    const list = await r.json();
    if (!list.length) {
      partialListEl.innerHTML = '<div class="empty">没有中断的传输</div>';
      if (purgeAllBtn) purgeAllBtn.style.display = 'none';
      return;
    }
    let html = '<table><thead><tr><th>文件名</th><th>已接收</th><th>操作</th></tr></thead><tbody>';
    for (const p of list) {
      html += '<tr><td>' + escapeHtml(p.name) + ' <span class="hint">（' + fmtSize(p.size) + '）</span></td>' +
        '<td class="size">' + p.have + '/' + p.chunks + ' 块 · ' + fmtSize(p.partSize) + '</td>' +
        '<td><a href="#" class="dl" data-act="purge" data-name="' + escapeHtml(p.name) + '">清理</a></td></tr>';
    }
    html += '</tbody></table>';
    partialListEl.innerHTML = html;
    if (purgeAllBtn) purgeAllBtn.style.display = '';
  } catch (e) {
    partialListEl.innerHTML = '<div class="empty">加载失败：' + escapeHtml(String(e)) + '</div>';
  }
}

async function purgePartials(name) {
  const t = token ? 't=' + encodeURIComponent(token) : '';
  let qs = '/api/uploads';
  if (name) qs += '?name=' + encodeURIComponent(name) + (t ? '&' + t : '');
  else if (t) qs += '?' + t;
  try {
    const r = await fetch(qs, { method: 'DELETE' });
    const j = await r.json().catch(() => ({}));
    if (!r.ok) { alert('清理失败：' + (j.error || r.status)); return; }
    setStatus('已清理 ' + (j.removed || 0) + ' 项中断的传输');
    loadPartials();
  } catch (e) { alert('清理失败：' + e); }
}

if (partialListEl) {
  partialListEl.addEventListener('click', async (e) => {
    const a = e.target.closest('a[data-act="purge"]');
    if (!a) return;
    e.preventDefault();
    if (!confirm('清理「' + a.dataset.name + '」尚未传完的数据？')) return;
    await purgePartials(a.dataset.name);
  });
}
if (purgeAllBtn) {
  purgeAllBtn.addEventListener('click', async () => {
    if (!confirm('清理所有中断的传输？未传完的数据会被删除，已传完的文件不受影响。')) return;
    await purgePartials('');
  });
}

// ---- 接收目录设置（电脑端） ----
async function loadSettings() {
  try {
    const r = await fetch('/api/settings');
    if (r.ok) {
      const j = await r.json();
      recvDirEl.textContent = j.dir || '（未知）';
      dirInput.value = j.dir || '';
      return;
    }
  } catch (_) {}
  recvDirEl.textContent = '（读取失败）';
}

$('saveDir').addEventListener('click', async () => {
  const d = dirInput.value.trim();
  if (!d) { alert('请先填写目录路径'); return; }
  try {
    const r = await fetch('/api/settings', {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ dir: d })
    });
    const j = await r.json().catch(() => ({}));
    if (!r.ok) { alert('切换失败：' + (j.error || r.status)); return; }
    loadSettings();
    loadFiles();
  } catch (e) { alert('切换失败：' + e); }
});

$('pickFolder').addEventListener('click', async () => {
  try {
    const r = await fetch('/api/pick-folder', { method: 'POST' });
    const j = await r.json().catch(() => ({}));
    if (j.path) dirInput.value = j.path;
    else if (j.error) alert('打开文件夹选择器失败：' + j.error);
  } catch (e) { alert('无法打开选择器：' + e); }
});

$('openFolder').addEventListener('click', () => { fetch('/api/open-folder', { method: 'POST' }); });

prefixInput.value = getPrefix();
prefixInput.addEventListener('input', () => {
  localStorage.setItem(PREFIX_KEY, prefixInput.value);
  loadFiles();
});

loadInfo();
loadFiles();
loadPartials();
loadSettings();
listenEvents();
