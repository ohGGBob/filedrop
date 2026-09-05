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

// ---- 分块上传（v0.2：位图断点续传 + 并发分块 + complete 收尾） ----
const isLocal = ['127.0.0.1', 'localhost', '[::1]'].includes(location.hostname);

// uploadFile 上传单个文件；出错时抛出，由队列层汇总。
// opts.label 是多文件时加的前缀（如 [2/5]）；opts.onProgress(chunksDone, chunksTotal) 汇报进度。
async function uploadFile(file, opts) {
  opts = opts || {};
  const label = opts.label || '';
  const onProgress = opts.onProgress || function (done, total) {
    upBar.style.width = ((done / total) * 100).toFixed(1) + '%';
  };
  const say = (t) => setStatus(label + t);

  if (!token && !isLocal) {
    throw new Error('当前页面没有上传令牌：请用电脑上显示的「带令牌地址」打开本页后再上传。');
  }
  const name = file.name, size = file.size;
  if (size === 0) { say('空文件，已跳过'); return { skipped: true }; }
  const total = Math.ceil(size / CHUNK);

  // 1) 查询缺块
  let st = { missing: [], complete: false, total: total };
  try {
    const r = await fetch('/api/upload/status?name=' + encodeURIComponent(name) + '&size=' + size);
    if (r.ok) st = await r.json();
  } catch (_) {}
  if (st.complete) {
    onProgress(total, total);
    say(name + ' 已传输完成，跳过');
    loadFiles();
    return { skipped: true };
  }
  const missing = st.missing || [];
  const haveChunks = total - missing.length;
  onProgress(haveChunks, total);
  say('准备上传：' + name + '（' + fmtSize(size) + '）' +
    (haveChunks > 0 ? '，已有 ' + haveChunks + '/' + total + ' 块，续传 ' + missing.length + ' 块' : ''));

  // 2) 并发补缺块（3 路并行，充分利用 WiFi 吞吐）
  const CONC = 3;
  const queue = missing.slice();
  let done = 0, firstErr = null;
  let lastT = performance.now(), lastChunks = haveChunks;

  async function sendChunk(i) {
    const begin = i * CHUNK, end = Math.min(begin + CHUNK, size);
    const blob = file.slice(begin, end);
    const url = '/api/upload/chunk?t=' + encodeURIComponent(token) +
      '&name=' + encodeURIComponent(name) + '&index=' + i + '&size=' + size;
    const resp = await fetch(url, { method: 'POST', headers: { 'Content-Type': 'application/octet-stream' }, body: blob });
    if (!resp.ok) throw new Error('分块 ' + i + ' 失败（HTTP ' + resp.status + '）');
  }

  await Promise.all(Array.from({ length: Math.min(CONC, queue.length) || 1 }, async () => {
    while (queue.length && !firstErr) {
      const i = queue.shift();
      try {
        await sendChunk(i);
      } catch (e) { firstErr = firstErr || e; break; }
      done++;
      const chunks = haveChunks + done, pct = (chunks / total) * 100;
      onProgress(chunks, total);
      const now = performance.now(), dt = (now - lastT) / 1000;
      if (dt >= 0.3) {
        const bytes = chunks * CHUNK, speed = (bytes - lastChunks * CHUNK) / dt;
        say('传输中 ' + pct.toFixed(0) + '% · ' + fmtSpeed(speed) + ' · 剩余 ' + fmtTime((total - chunks) * CHUNK / (speed || 1)));
        lastT = now; lastChunks = chunks;
      }
    }
  }));
  if (firstErr) throw firstErr;

  // 3) complete 收尾（服务端校验位图、算 SHA-256、落盘改名）
  for (let attempt = 0; attempt < 3; attempt++) {
    const r = await fetch('/api/upload/complete?t=' + encodeURIComponent(token) +
      '&name=' + encodeURIComponent(name) + '&size=' + size, { method: 'POST' });
    const j = await r.json().catch(() => ({}));
    if (r.ok) {
      const sha = j.sha256 || '';
      onProgress(total, total);
      say(name + ' 完成 ✓  SHA-256: ' + (sha ? sha.slice(0, 16) + '…' : '已保存'));
      loadFiles();
      return { sha256: j.sha256 };
    }
    if (Array.isArray(j.missing) && j.missing.length) {
      say('补传缺失的 ' + j.missing.length + ' 块…');
      await Promise.all(j.missing.map(sendChunk));
      continue;
    }
    throw new Error('收尾失败：' + (j.error || r.status));
  }
  throw new Error('收尾重试次数用尽');
}

// uploadFiles 串行上传一批文件，进度条按「总字节数」汇总，出错的跳过并计入汇总。
async function uploadFiles(fileList) {
  let files = Array.from(fileList || []);
  if (!files.length) return;

  const empty = files.filter((f) => f.size === 0).map((f) => f.name);
  files = files.filter((f) => f.size > 0);
  if (!files.length) { setStatus('所选文件都是空文件，已跳过' + (empty.length ? '：' + empty.join('、') : '')); return; }
  if (!token && !isLocal) {
    setStatus('当前页面没有上传令牌：请用电脑上显示的「带令牌地址」打开本页后再上传。');
    return;
  }

  const totalChunks = files.reduce((n, f) => n + Math.ceil(f.size / CHUNK), 0);
  let doneChunks = 0;
  const results = [];

  for (let k = 0; k < files.length; k++) {
    const f = files[k];
    const label = files.length > 1 ? '[' + (k + 1) + '/' + files.length + '] ' : '';
    const base = Math.ceil(f.size / CHUNK);
    try {
      await uploadFile(f, {
        label: label,
        onProgress: (d, t) => {
          const pct = ((doneChunks + d) / totalChunks) * 100;
          upBar.style.width = pct.toFixed(1) + '%';
        }
      });
      results.push({ name: f.name, ok: true });
    } catch (e) {
      results.push({ name: f.name, ok: false, err: String(e) });
    }
    doneChunks += base;
  }

  const bad = results.filter((r) => !r.ok);
  const ok = results.length - bad.length;
  upBar.style.width = '100%';
  if (bad.length) {
    setStatus('完成 ' + ok + '/' + results.length + '，失败 ' + bad.length + ' 个：' +
      bad.map((b) => b.name).join('、') + '（可重新选择失败的文件续传）');
  } else {
    setStatus('全部完成 ✓ 共 ' + ok + ' 个文件' + (empty.length ? '（跳过空文件：' + empty.join('、') + '）' : ''));
  }
  loadFiles();
}

// ---- 交互绑定 ----
drop.addEventListener('click', () => fileInput.click());
fileInput.addEventListener('change', () => { uploadFiles(fileInput.files); fileInput.value = ''; });
['dragover', 'dragenter'].forEach((ev) => drop.addEventListener(ev, (e) => { e.preventDefault(); drop.classList.add('over'); }));
['dragleave', 'drop'].forEach((ev) => drop.addEventListener(ev, (e) => { e.preventDefault(); drop.classList.remove('over'); }));
drop.addEventListener('drop', (e) => {
  if (e.dataTransfer && e.dataTransfer.files && e.dataTransfer.files.length) uploadFiles(e.dataTransfer.files);
});

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
