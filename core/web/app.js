// FileDrop 前端：分块上传 + 列表 + 下载
'use strict';

const CHUNK = 8 * 1024 * 1024; // 8 MiB
const token = new URLSearchParams(location.search).get('t') || '';
// 对端批准的临时写授权：跳进别人页面时带在 ?g= 上，绑本机 IP、会过期，权限范围与令牌相同。
const grant = new URLSearchParams(location.search).get('g') || '';

// authPair 生成写请求的鉴权查询项（不含分隔符）。令牌优先：两端都有时说明这是自己人的页面，
// 用终身令牌不必担心授权半小时后过期。无凭证时返回空串，调用方拼出的仍是合法 URL。
function authPair() {
  if (token) return 't=' + encodeURIComponent(token);
  if (grant) return 'g=' + encodeURIComponent(grant);
  return '';
}

const $ = (id) => document.getElementById(id);
const drop = $('drop'), fileInput = $('file'), upBar = $('upBar'), upStatus = $('upStatus');
const addrEl = $('addr'), copyBtn = $('copyAddr'), fileListEl = $('fileList');
const recvDirEl = $('recvDir'), dirInput = $('dirInput'), prefixInput = $('prefixInput');
const PREFIX_KEY = 'fd_prefix';
const THEME_KEY='fd_theme';

// 主题：跟随系统，本地记忆覆盖
(function initTheme(){
  const saved=localStorage.getItem(THEME_KEY);
  const prefersDark=window.matchMedia&&window.matchMedia('(prefers-color-scheme: dark)').matches;
  const theme=saved||(prefersDark?'dark':'light');
  if(theme==='dark') document.documentElement.setAttribute('data-theme','dark');
  const btn=$('themeToggle');
  if(btn){btn.addEventListener('click',()=>{const isDark=document.documentElement.getAttribute('data-theme')==='dark';const next=isDark?'light':'dark';if(next==='dark')document.documentElement.setAttribute('data-theme','dark');else document.documentElement.removeAttribute('data-theme');localStorage.setItem(THEME_KEY,next);toast(next==='dark'?'已切换深色':'已切换浅色','info')})}
})();

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

// ---- 统一非阻塞提示 toast ----
// 关键成功 / 失败 / 警告用一条短暂浮层提示，替代散落的小号 meta 文字与打断式原生 alert()。
// 用法：toast('已复制', 'ok'); toast('删除失败', 'err'); toast('请注意', 'warn'); toast('信息', 'info');
function toast(msg, kind) {
  const wrap = document.getElementById('toastWrap');
  if (!wrap) return;
  const el = document.createElement('div');
  el.className = 'toast ' + (kind || 'info');
  el.textContent = msg;
  wrap.appendChild(el);
  // 触发动画后再移除，避免刚插入就被清掉导致无动画
  requestAnimationFrame(() => el.classList.add('show'));
  setTimeout(() => el.remove(), 1500);
}

// ---- 页内选择弹层（替代原生 confirm / prompt，语义更清晰、风格更统一）----
// 返回 Promise，resolve 为 true / false；type 可为 'confirm'（确定/取消）或 'choice'（自定义两按钮文本）。
function modalPrompt({ title, body, sub, okText = '确定', cancelText = '取消', danger = false }) {
  return new Promise((resolve) => {
    const mask = document.createElement('div');
    mask.className = 'modal-mask';
    const box = document.createElement('div');
    box.className = 'modal';
    const h = document.createElement('h3'); h.textContent = title;
    const p = document.createElement('p'); p.textContent = body;
    box.appendChild(h); box.appendChild(p);
    if (sub) { const s = document.createElement('div'); s.className = 'msub'; s.textContent = sub; box.appendChild(s); }
    const mac = document.createElement('div'); mac.className = 'mact';
    const cancel = document.createElement('button'); cancel.textContent = cancelText;
    const ok = document.createElement('button'); ok.className = danger ? '' : 'primary'; ok.textContent = okText;
    if (danger) { ok.style.background = 'var(--err)'; ok.style.borderColor = 'var(--err)'; ok.style.color = '#fff'; }
    const close = (v) => { mask.remove(); document.removeEventListener('keydown', onKey); resolve(v); };
    const onKey = (e) => { if (e.key === 'Escape') close(false); if (e.key === 'Enter') close(true); };
    cancel.addEventListener('click', () => close(false));
    ok.addEventListener('click', () => close(true));
    mask.addEventListener('click', (e) => { if (e.target === mask) close(false); });
    mac.append(cancel, ok);
    box.appendChild(mac);
    mask.appendChild(box);
    document.body.appendChild(mask);
    document.addEventListener('keydown', onKey);
    ok.focus();
  });
}

// ---- 本机地址 ----
async function loadInfo() {
  try {
    const r = await fetch('/api/info');
    if (r.ok) {
      const j = await r.json();
      const url = j.url || (j.ip + ':' + j.port);
      addrEl.textContent = url;
      if(j.version){ const vb=$('verBadge'), fv=$('footVer'); if(vb) vb.textContent='v'+j.version; if(fv) fv.textContent='v'+j.version+' · 已就绪'; }
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
// copyText 复制一段文本。navigator.clipboard 只在安全上下文存在，而本项目跑在
// 局域网 http://192.168.x.x 上——手机上它就是 undefined，之前点「复制」没任何反应。
// 因此保留 execCommand('copy') 兜底（明文源上依然可用）。
async function copyText(s) {
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(s);
      return true;
    }
  } catch (_) { /* 落到兜底路径 */ }
  const ta = document.createElement('textarea');
  ta.value = s;
  ta.setAttribute('readonly', '');
  ta.style.position = 'fixed';
  ta.style.top = '-1000px';
  document.body.appendChild(ta);
  ta.select();
  ta.setSelectionRange(0, s.length);
  let ok = false;
  try { ok = document.execCommand('copy'); } catch (_) { ok = false; }
  ta.remove();
  return ok;
}

function flashCopied(btn, ok) {
  const t = btn.textContent;
  btn.textContent = ok ? '已复制' : '复制失败';
  setTimeout(() => (btn.textContent = t), 1200);
}

copyBtn.addEventListener('click', async () => {
  const ok=await copyText(addrEl.textContent);
  flashCopied(copyBtn, ok);
  toast(ok?'已复制连接地址':'复制失败，请长按选择','info');
});
$('refreshInfo')?.addEventListener('click', loadInfo);
$('refreshFiles')?.addEventListener('click', loadFiles);
$('aboutBtn')?.addEventListener('click', ()=> modalPrompt({title:'关于 FileDrop',body:'FileDrop · 局域网文件快传 · 便携商业版',sub:'分块断点续传 · SHA256校验 · 二维码秒连 · 局域网发现 · 文本快传 · 单文件便携',okText:'知道了',cancelText:'关闭'}));
$('previewClose')?.addEventListener('click',()=>{$('previewMask').style.display='none'});
$('previewMask')?.addEventListener('click',(e)=>{if(e.target.id==='previewMask') e.currentTarget.style.display='none'});

// ---- 文件列表 ----
const zipSelBtn = $('zipSel');

function selectedNames() {
  return Array.from(fileListEl.querySelectorAll('input[type=checkbox]:checked'))
    .map((c) => c.dataset.name);
}

function refreshZipBtn() {
  if (!zipSelBtn) return;
  zipSelBtn.style.display = selectedNames().length ? '' : 'none';
}

let allFiles=[], curSearch='', curSort='mtime_desc', curPage=1; const PAGE_SIZE=20;
function fileIcon(name){const ext=(name.split('.').pop()||'').toLowerCase();const map={jpg:'🖼️',jpeg:'🖼️',png:'🖼️',gif:'🖼️',webp:'🖼️',mp4:'🎬',mov:'🎬',avi:'🎬',mkv:'🎬',mp3:'🎵',wav:'🎵',pdf:'📄',zip:'🗜️',rar:'🗜️',docx:'📝',xlsx:'📊',pptx:'📊',txt:'📃'};return map[ext]||'📦'}
function isPreviewable(name){return /\.(jpg|jpeg|png|gif|webp|txt|md|log|json|csv|html|pdf|mp4|mp3)$/i.test(name)}
function showPreview(name){
  const mask=$('previewMask'), body=$('previewBody'), title=$('previewTitle');
  if(!mask) return; title.textContent=name; body.textContent='加载中…'; mask.style.display='flex';
  const url='/api/download?name='+encodeURIComponent(name);
  if(/\.(jpg|jpeg|png|gif|webp)$/i.test(name)){body.innerHTML='<img src="'+url+'" style="max-width:100%;border-radius:10px;border:1px solid var(--border)" />';}
  else if(/\.(mp4|mov|webm)$/i.test(name)){body.innerHTML='<video src="'+url+'" controls style="max-width:100%;border-radius:10px"></video>';}
  else if(/\.pdf$/i.test(name)){body.innerHTML='<iframe src="'+url+'" style="width:100%;height:55vh;border:1px solid var(--border);border-radius:10px"></iframe>';}
  else { fetch(url).then(r=>r.text()).then(t=>{body.innerHTML='<pre style="white-space:pre-wrap;word-break:break-all;background:var(--bg);padding:10px;border-radius:10px;border:1px solid var(--border);max-height:55vh;overflow:auto">'+escapeHtml(t.slice(0,200000))+'</pre>'}).catch(()=>body.textContent='预览失败');}
}
function renderFiles(){
  let list=[...allFiles];
  if(curSearch) {const k=curSearch.toLowerCase(); list=list.filter(f=>f.name.toLowerCase().includes(k));}
  if(curSort==='name_asc') list.sort((a,b)=>a.name.localeCompare(b.name));
  else if(curSort==='size_desc') list.sort((a,b)=>b.size-a.size);
  else if(curSort==='size_asc') list.sort((a,b)=>a.size-b.size);
  else if(curSort==='mtime_asc') list.sort((a,b)=>a.mtime-b.mtime);
  else list.sort((a,b)=>b.mtime-a.mtime);
  const total=list.length; const pages=Math.max(1,Math.ceil(total/PAGE_SIZE)); if(curPage>pages) curPage=pages;
  const slice=list.slice((curPage-1)*PAGE_SIZE, curPage*PAGE_SIZE);
  const cntEl=$('fileCount'); if(cntEl) cntEl.textContent=total?`共 ${total} 个 · 第 ${curPage}/${pages} 页`:'';
  const pager=$('filePager'); if(pager){pager.style.display=total>PAGE_SIZE?'':'none'; pager.textContent=''; for(let i=1;i<=pages;i++){const b=document.createElement('button');b.textContent=String(i);if(i===curPage)b.className='cur';b.addEventListener('click',()=>{curPage=i;renderFiles()});pager.appendChild(b);} }
  if(!slice.length){ fileListEl.innerHTML='<div class="empty"><div class="illus">∅</div><div>'+(curSearch?'无匹配结果':'还没有文件 — 拖拽或点击上传')+'</div><div class="hint">支持图片/视频/文档预览，长按卡片可多选</div></div>'; if(zipSelBtn) zipSelBtn.style.display='none'; return; }
  const vm=$('viewMode')?.value || 'auto';
  const useCards = vm==='cards' || (vm==='auto' && (window.innerWidth<720 || isIOS));
  const prefix=getPrefix();
  if(useCards){
    let html='<div class="cards">';
    for(const f of slice){
      const enc=encodeURIComponent(f.name); const dlName=prefix+(f.name.split('/').pop());
      html+='<div class="fcard"><div class="top"><div class="fico">'+fileIcon(f.name)+'</div><div class="fname" title="'+escapeHtml(f.name)+'">'+escapeHtml(f.name)+'</div><input type="checkbox" data-name="'+escapeHtml(f.name)+'" /></div><div class="fmeta">'+fmtSize(f.size)+' · '+new Date(f.mtime).toLocaleString()+'</div><div class="file-actions"><a class="dl" href="/api/download?name='+enc+'" download="'+escapeHtml(dlName)+'" data-act="dl">下载</a>'+(isPreviewable(f.name)?' · <a href="#" class="dl" data-act="preview" data-name="'+escapeHtml(f.name)+'">预览</a>':'')+' · <a href="#" class="dl" data-act="rename" data-name="'+escapeHtml(f.name)+'">重命名</a> · <a href="#" class="dl" data-act="del" data-name="'+escapeHtml(f.name)+'">删除</a></div></div>';
    }
    html+='</div>'; fileListEl.innerHTML=html;
  } else {
    let html='<table><thead><tr><th></th><th>文件名</th><th>大小</th><th>操作</th></tr></thead><tbody>';
    for(const f of slice){
      const enc=encodeURIComponent(f.name); const dlName=prefix+(f.name.split('/').pop());
      html+='<tr><td><input type="checkbox" data-name="'+escapeHtml(f.name)+'" /></td><td><span style="margin-right:6px">'+fileIcon(f.name)+'</span><span>'+escapeHtml(f.name)+'</span>'+(isPreviewable(f.name)?' <a href="#" class="dl" data-act="preview" data-name="'+escapeHtml(f.name)+'">预览</a>':'')+'</td><td class="size">'+fmtSize(f.size)+'</td><td class="file-actions"><a class="dl" href="/api/download?name='+enc+'" download="'+escapeHtml(dlName)+'" data-act="dl">下载</a> · <a href="#" class="dl" data-act="rename" data-name="'+escapeHtml(f.name)+'">重命名</a> · <a href="#" class="dl" data-act="del" data-name="'+escapeHtml(f.name)+'">删除</a></td></tr>';
    }
    html+='</tbody></table>'; fileListEl.innerHTML=html;
  }
  refreshZipBtn();
}
async function loadFiles() {
  try {
    const r = await fetch('/api/files');
    if (!r.ok) throw new Error('list ' + r.status);
    allFiles = await r.json();
    renderFiles();
  } catch (e) {
    fileListEl.innerHTML = '<div class="empty">列表加载失败：' + escapeHtml(String(e)) + '</div>';
  }
}
$('searchInput')?.addEventListener('input',(e)=>{curSearch=e.target.value;curPage=1;renderFiles()});
$('sortSelect')?.addEventListener('change',(e)=>{curSort=e.target.value;renderFiles()});
(function initViewMode(){
  const sel=$('viewMode'); if(!sel) return;
  const saved=localStorage.getItem('fd_view');
  if(saved) sel.value=saved;
  else if(window.innerWidth<680) sel.value='cards';
  sel.addEventListener('change',()=>{localStorage.setItem('fd_view',sel.value);renderFiles()});
})();
// 全屏拖拽蒙层
(function initDropOverlay(){
  const ov=$('dropOverlay'); if(!ov) return;
  let cnt=0;
  ['dragenter','dragover'].forEach(ev=> window.addEventListener(ev,(e)=>{ if(e.dataTransfer && [...(e.dataTransfer.types||[])].includes('Files')){e.preventDefault(); cnt++; ov.classList.add('show');}}));
  ['dragleave','drop'].forEach(ev=> window.addEventListener(ev,(e)=>{ if(e.dataTransfer){cnt=Math.max(0,cnt-1); if(cnt===0) ov.classList.remove('show');}}));
})();

if (zipSelBtn) {
  zipSelBtn.addEventListener('click', () => {
    const names = selectedNames();
    if (!names.length) return;
    // 直接用 <a download> 让浏览器把 ZIP 存下来；服务端流式打包，不占磁盘
    const a = document.createElement('a');
    a.href = '/api/zip?names=' + encodeURIComponent(names.join(','));
    a.download = 'FileDrop_打包.zip';
    document.body.appendChild(a);
    a.click();
    a.remove();
  });
}
function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

// ---- 分块上传：任务队列（一行一个文件，可单独取消 / 原地续传） ----
// v0.2：位图断点续传 + 并发分块 + complete 收尾
// v0.4：队列化。此前每选一次文件就新起一条 Promise 链，两条链同时写同一个进度条
//       和状态行，数字会来回跳；而且 4GB 的传输一旦开始就停不下来。
const isIOS = /iPad|iPhone|iPod/.test(navigator.userAgent) || (navigator.platform==='MacIntel' && navigator.maxTouchPoints>1);
const isMac = navigator.platform.toUpperCase().indexOf('MAC')>=0;
const isHarmony = /Harmony|OpenHarmony|HMOS/i.test(navigator.userAgent);
const isLocal = ['127.0.0.1', 'localhost', '[::1]'].includes(location.hostname);
const canWriteHere = () => !!(token || grant || isLocal);
const CONC = isIOS ? 1 : 3; // iOS 内存/并发受限，单路更稳

const queueEl = $('upQueue'), summaryEl = $('upSummary');
const cancelAllBtn = $('cancelAll'), clearDoneBtn = $('clearDone');

let tasks = [], taskIdSeq = 0, runner = null;
// 同名冲突的会话级选择：null=还没问过；'overwrite'=覆盖；'keep'=共存改名。
// 记住一次选择是为了批量传照片时不被确认框刷屏。
let conflictChoice = null;

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
  // 文件夹上传：保留相对目录结构（"相册/IMG_1.jpg"），服务端按子路径落盘
  const relPath = file.webkitRelativePath || file.name;
  const t = {
    id: ++taskIdSeq, file, name: relPath, size: file.size,
    total: Math.max(1, Math.ceil(file.size / CHUNK)),
    sent: 0, bytes: 0, bytesAt: 0, tAt: 0, speed: 0,
    status: 'queued', aborted: false, controller: null, err: '', finalName: '', reused: false,
    overwrite: false,
  };
  const row = document.createElement('div');
  row.className = 'qrow queued';

  const head = document.createElement('div'); head.className = 'qhead';
  const nm = document.createElement('span'); nm.className = 'qname';
  nm.textContent = relPath;                       // textContent：文件名里的 <>"& 不会被解析成标签
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
  else if (t.status === 'cancelled') {
    // 取消可能正好赶在最后一个分块发完、收尾请求发出之前：此时电脑端其实已经
    // 有全部字节，说「半截数据可续传」会让人以为还得再传一遍。
    t.text.textContent = t.sent >= t.total
      ? '已取消（' + t.total + ' 块其实都已到电脑端，点「续传」直接收尾）'
      : '已取消（' + t.sent + '/' + t.total + ' 块已在电脑端，可续传）';
  }
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
  if (!canWriteHere()) {
    setStatus('当前页面没有写权限：请用电脑上显示的「带令牌地址」打开本页，或在对方设备上点「请求上传」并取得批准。');
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
  const chunkURL = (i) => '/api/upload/chunk?' + authPair() +
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
  // 同名同大小的文件已在本机，但内容指纹对不上：问一次「覆盖还是共存」，
  // 选择记到会话里，本批后面的同名冲突沿用，不为每张照片都弹一次窗。
  if (st.exists && !t.overwrite && conflictChoice === null) {
    conflictChoice = (await modalPrompt({
      title: '已有同名文件',
      body: '本机已有同名同大小的「' + name + '」，但内容不同。',
      sub: '「覆盖」会用新文件替换旧文件（本批所有同名冲突都覆盖）；「两份都保留」会把新文件自动改名，旧文件不动（本批所有同名冲突都共存）。',
      okText: '覆盖',
      cancelText: '两份都保留',
      danger: true,
    })) ? 'overwrite' : 'keep';
  }
  if (st.exists && conflictChoice === 'overwrite') t.overwrite = true;
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
    const r = await fetch('/api/upload/complete?' + authPair() +
      '&name=' + encodeURIComponent(name) + '&size=' + size +
      (t.overwrite ? '&mode=overwrite' : ''), { method: 'POST', signal });
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

// 文件夹上传：只在实际支持的浏览器显示入口（安卓 WebView 的文件选择不带目录信息）
const supportsDirPick = (() => {
  try { const i = document.createElement('input'); return 'webkitdirectory' in i && !/; wv\)/.test(navigator.userAgent); }
  catch (_) { return false; }
})();
const folderInput = $('folder'), pickFolderBtn = $('pickFolderBtn');
if (pickFolderBtn) {
  pickFolderBtn.style.display = supportsDirPick ? '' : 'none';
  const fh=$('folderHint'); if(fh) fh.style.display=supportsDirPick?'':'none';
  if (supportsDirPick) {
    pickFolderBtn.addEventListener('click', () => folderInput.click());
    folderInput.addEventListener('change', () => { enqueueFiles(folderInput.files); folderInput.value = ''; });
  }
}

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
fileListEl.addEventListener('change', (e) => {
  if (e.target && e.target.type === 'checkbox') refreshZipBtn();
});
fileListEl.addEventListener('click', async (e) => {
  const a = e.target.closest('a[data-act]');
  if (!a) return;
  e.preventDefault();
  const name = a.dataset.name;
  const p = authPair();
  const tqs = p ? '&' + p : '';
  if (a.dataset.act === 'del') {
    if (!(await modalPrompt({ title: '删除文件', body: '确定删除「' + name + '」？', sub: '此操作不可恢复。', okText: '删除', cancelText: '取消', danger: true }))) return;
    const r = await fetch('/api/files?name=' + encodeURIComponent(name) + tqs, { method: 'DELETE' });
    if (r.ok) loadFiles();
    else { const j = await r.json().catch(() => ({})); toast('删除失败：' + (j.error || r.status), 'err'); }
  } else if (a.dataset.act === 'rename') {
    const nn = await inputModal('重命名', '输入新文件名', name);
    if (!nn || nn === name) return;
    const r = await fetch('/api/rename?' + authPair(), {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name: name, newName: nn })
    });
    const j = await r.json().catch(() => ({}));
    if (r.ok) { toast('已重命名','ok'); loadFiles(); }
    else toast('重命名失败：' + (j.error || r.status), 'err');
  } else if (a.dataset.act === 'preview') {
    showPreview(name);
  } else if (a.dataset.act === 'dl' && isIOS) {
    // iOS 对 download 属性支持弱，改走 blob + share/open 兜底
    try{
      const r=await fetch('/api/download?name='+encodeURIComponent(name));
      if(!r.ok) throw new Error('HTTP '+r.status);
      const blob=await r.blob();
      const url=URL.createObjectURL(blob);
      if(navigator.share && navigator.canShare && blob.size < 30*1024*1024){
        const file=new File([blob], name, {type: blob.type});
        if(navigator.canShare({files:[file]})) { await navigator.share({files:[file], title:name}); URL.revokeObjectURL(url); return; }
      }
      const a2=document.createElement('a'); a2.href=url; a2.download=name; document.body.appendChild(a2); a2.click(); a2.remove();
      setTimeout(()=>URL.revokeObjectURL(url), 8000);
    }catch(err){ toast('下载失败：'+err,'err'); }
  }
});

function inputModal(title, body, defVal){
  return new Promise((resolve)=>{
    const mask=document.createElement('div');mask.className='modal-mask';
    const box=document.createElement('div');box.className='modal';
    const h=document.createElement('h3');h.textContent=title;
    const p=document.createElement('p');p.textContent=body;
    const inp=document.createElement('input');inp.value=defVal||'';inp.placeholder='新文件名';
    const mact=document.createElement('div');mact.className='mact';
    const cancel=document.createElement('button');cancel.textContent='取消';
    const ok=document.createElement('button');ok.className='primary';ok.textContent='确定';
    const close=(v)=>{mask.remove();document.removeEventListener('keydown',onKey);resolve(v);};
    const onKey=(e)=>{if(e.key==='Escape')close(null);if(e.key==='Enter')close(inp.value.trim());};
    cancel.addEventListener('click',()=>close(null));
    ok.addEventListener('click',()=>close(inp.value.trim()));
    mask.addEventListener('click',(e)=>{if(e.target===mask)close(null);});
    mact.append(cancel,ok);box.append(h,p,inp,mact);mask.appendChild(box);document.body.appendChild(mask);
    document.addEventListener('keydown',onKey); inp.focus(); inp.select();
  });
}

// ---- 传文字（剪贴板快传）----
// 正文一律走 textContent：便签里可能就是别人贴来的一段 HTML / 脚本，
// 用 innerHTML 渲染等于把「谁发了一条文字」变成「谁在你的页面上执行代码」。
const noteInput = $('noteInput'), noteList = $('noteList'), noteStatus = $('noteStatus');
const noteSendBtn = $('noteSend');
let notes = [];

function setNoteStatus(t) { if (noteStatus) noteStatus.textContent = t; }

function fmtWhen(ms) {
  const d = new Date(ms);
  const p = (n) => String(n).padStart(2, '0');
  const sameDay = new Date().toDateString() === d.toDateString();
  return sameDay ? p(d.getHours()) + ':' + p(d.getMinutes()) + ':' + p(d.getSeconds())
    : (d.getMonth() + 1) + '/' + d.getDate() + ' ' + p(d.getHours()) + ':' + p(d.getMinutes());
}

function renderNotes() {
  if (!noteList) return;
  noteList.textContent = '';
  if (!notes.length) {
    const e = document.createElement('div');
    e.className = 'empty';
    e.textContent = '还没有便签';
    noteList.appendChild(e);
    return;
  }
  for (const n of notes) {
    const box = document.createElement('div'); box.className = 'note';
    const head = document.createElement('div'); head.className = 'nhead';
    const when = document.createElement('span'); when.textContent = fmtWhen(n.at);
    const size = document.createElement('span'); size.textContent = (n.size || n.text.length) + ' 字节 · ' + n.text.split('\n').length + ' 行';
    const spacer = document.createElement('span'); spacer.className = 'spacer';
    const copy = document.createElement('button'); copy.className = 'nact'; copy.textContent = '复制';
    copy.addEventListener('click', async () => {
      const ok = await copyText(n.text);
      flashCopied(copy, ok);
      setNoteStatus(ok ? '已复制到剪贴板' : '复制失败：请长按正文手动选中');
    });
    const del = document.createElement('button'); del.className = 'nact danger'; del.textContent = '删除';
    del.addEventListener('click', () => deleteNote(n.id));
    head.append(when, size, spacer, copy, del);
    const body = document.createElement('pre'); body.className = 'nbody'; body.textContent = n.text;
    box.append(head, body);
    noteList.appendChild(box);
  }
}

async function loadNotes() {
  if (!noteList) return;
  try {
    const r = await fetch('/api/notes?' + authPair());
    if (r.status === 403) { notes = []; setNoteStatus(''); renderNotesAsDenied(); return; }
    if (!r.ok) throw new Error('HTTP ' + r.status);
    notes = await r.json();
    renderNotes();
  } catch (e) {
    notes = [];
    const e2 = document.createElement('div'); e2.className = 'empty';
    e2.textContent = '便签读取失败：' + e;
    noteList.textContent = '';
    noteList.appendChild(e2);
  }
}

function renderNotesAsDenied() {
  const e = document.createElement('div');
  e.className = 'empty';
  e.textContent = '便签需要写权限才能读写：请用电脑上显示的「带令牌地址」打开本页，或向对方请求上传并获批准。';
  noteList.textContent = '';
  noteList.appendChild(e);
}

async function sendNote() {
  const text = noteInput.value;
  if (!text.trim()) { setNoteStatus('先写点内容'); return; }
  if (!canWriteHere()) { setNoteStatus('当前页面没有写权限，无法发送'); return; }
  setNoteStatus('发送中…');
  try {
    const r = await fetch('/api/notes?' + authPair(), {
      method: 'POST', headers: { 'Content-Type': 'text/plain; charset=utf-8' }, body: text,
    });
    const j = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(j.error || ('HTTP ' + r.status));
    noteInput.value = '';
    setNoteStatus('已发送');
    await loadNotes();
    setTimeout(() => setNoteStatus(''), 2000);
  } catch (e) {
    setNoteStatus('发送失败：' + e);
  }
}

async function deleteNote(id) {
  try {
    const r = await fetch('/api/notes?' + authPair() + '&id=' + encodeURIComponent(id), { method: 'DELETE' });
    if (!r.ok) throw new Error('HTTP ' + r.status);
    await loadNotes();
  } catch (e) {
    setNoteStatus('删除失败：' + e);
  }
}

if (noteSendBtn) {
  noteSendBtn.addEventListener('click', sendNote);
  noteInput.addEventListener('keydown', (e) => {
    if ((e.ctrlKey || e.metaKey) && e.key === 'Enter') { e.preventDefault(); sendNote(); }
  });
}

// ---- 局域网里的其他 FileDrop：发现 · 请求上传 · 批准对方 ----
// 广播信标里不带配对令牌，所以「发现到」不等于「能上传」。路径是：
// 本机点「请求上传」→ 对端服务端把请求挂住，等它屏幕上有人点「允许」→
// 批准后换回一个绑本机 IP、30 分钟过期的 ?g= 授权，然后浏览器直接跳进对方页面。
// 全程请求都只发给自己的服务端（对端那一路由服务端去敲门），所以不需要开 CORS。
const peerSelfEl = $('peerSelf'), peerListEl = $('peerList'), peerStatusEl = $('peerStatus');
const peerAddrEl = $('peerAddr'), peerAddBtn = $('peerAdd');
const askBox = $('peerAsk'), askList = $('peerAskList');
const askHint = $('peerAskInline');

function setPeerStatus(t) { if (peerStatusEl) peerStatusEl.textContent = t; }

function addDiv(parent, cls, text) {
  const d = document.createElement('div');
  if (cls) d.className = cls;
  if (text !== undefined) d.textContent = text;
  parent.appendChild(d);
  return d;
}

// 服务端拼好的 url 已经保证是 http://IP:端口/，这里再兜一次：
// 名字和地址来自对面机器，不能让它把「打开」变成 javascript: 之类的链接。
function peerHref(p) {
  if (/^http:\/\/[^\s]+$/.test(p.url || '')) return p.url;
  return 'http://' + p.host + ':' + p.port + '/';
}

async function loadPeers() {
  if (!peerListEl) return;
  let j;
  try {
    const r = await fetch('/api/peers');
    if (!r.ok) throw new Error('HTTP ' + r.status);
    j = await r.json();
  } catch (e) {
    peerListEl.textContent = '';
    addDiv(peerListEl, 'empty', '设备列表加载失败：' + e);
    return;
  }
  const self = j.self || {};
  const list = j.peers || [];
  if (peerSelfEl) {
    let line = '本机：' + (self.name || '（未知）') + ' · ' + (self.ip || '?') + ':' + (self.port || '?');
    if (!j.listening) line += ' · 自动发现未开启，只能手动添加';
    peerSelfEl.textContent = line;
  }
  if (j.discover_error) setPeerStatus('自动发现不可用：' + j.discover_error + '（多半是端口被占或防火墙）');
  peerListEl.textContent = '';
  if (!list.length) {
    addDiv(peerListEl, 'empty', j.listening
      ? '还没发现其他设备。对面也要开着 FileDrop；有些 AP / 交换机会屏蔽广播，此时用上面的「手动添加」填 IP:端口。'
      : '发现通道没开，暂无设备列表。可用「手动添加」按 IP:端口连接。');
    return;
  }
  for (const p of list) peerListEl.appendChild(peerRow(p));
}

function peerRow(p) {
  const row = document.createElement('div');
  row.className = 'peer';
  const head = document.createElement('div');
  head.className = 'phead';
  const nm = document.createElement('b');
  nm.textContent = p.name || p.host || '未命名设备';
  const meta = document.createElement('span');
  meta.className = 'meta';
  meta.textContent = p.host + ':' + p.port +
    (p.version ? ' · v' + p.version : '') + (p.via === 'manual' ? ' · 手动添加' : '');
  const sp = document.createElement('span');
  sp.className = 'spacer';
  head.appendChild(nm); head.appendChild(meta); head.appendChild(sp);

  const open = document.createElement('a');
  open.className = 'dl';
  open.href = peerHref(p);
  open.target = '_blank';
  open.rel = 'noopener';
  open.textContent = '打开';
  const req = document.createElement('button');
  req.className = 'qact';
  req.style.marginLeft = '10px';
  req.textContent = '请求上传';
  req.addEventListener('click', () => requestUpload(p, req));
  head.appendChild(open); head.appendChild(req);
  row.appendChild(head);
  return row;
}

async function requestUpload(p, btn) {
  if (!canWriteHere()) {
    setPeerStatus('当前页面没有写权限，无法向对方发起请求：请用带令牌地址或本机回环地址打开本页。');
    return;
  }
  const label = btn.textContent;
  btn.disabled = true;
  btn.textContent = '等待对方确认…';
  setPeerStatus('请求已发出，请在「' + (p.name || p.host) + '」的屏幕上点「允许」（最多等 90 秒）');
  try {
    const r = await fetch('/api/peer/request?' + authPair() +
      '&to=' + encodeURIComponent(p.host + ':' + p.port), { method: 'POST' });
    const j = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(j.error || ('HTTP ' + r.status));
    setPeerStatus('对方已批准，正在打开它的页面…（授权半小时内有效）');
    if (/^http:\/\/[^\s]+$/.test(j.url || '')) {
      // 记住这个对端（含凭据）：安卓 App 收到系统分享时直接把它当上传目标，
      // 相册里的照片就能一步发到这台设备，不用先开 App 再选。
      fetch('/api/remember-peer?' + authPair() + '&url=' + encodeURIComponent(j.url), { method: 'POST' }).catch(() => {});
      setTimeout(() => { location.href = j.url; }, 700);
    }
  } catch (e) {
    setPeerStatus('请求未成功：' + e);
    btn.disabled = false;
    btn.textContent = label;
  }
}

async function addPeerManually() {
  const to = (peerAddrEl.value || '').trim();
  if (!to) { setPeerStatus('请填写对方地址，形如 192.168.1.20:28080'); return; }
  peerAddBtn.disabled = true;
  setPeerStatus('正在探测 ' + to + ' …');
  try {
    const r = await fetch('/api/peers?' + authPair() + '&to=' + encodeURIComponent(to), { method: 'POST' });
    const j = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(j.error || ('HTTP ' + r.status));
    setPeerStatus('已添加 ' + to);
    peerAddrEl.value = '';
    loadPeers();
  } catch (e) {
    setPeerStatus('添加失败：' + e);
  } finally {
    peerAddBtn.disabled = false;
  }
}

// ---- 对方屏幕上的批准横幅 ----
// decide / pending 都要求本机写权限：否则任何同网主机都能替屏幕前的人点「允许」。
const pendingMaxAge = 100 * 1000; // 服务端最长挂 90 秒，超过就没人在等了

async function refreshPending() {
  if (!askBox || !askList) return;
  if (!canWriteHere()) { showAskHint(); askBox.hidden = true; return; }
  try {
    const r = await fetch('/api/peer/pending?' + authPair());
    if (r.status === 403) { showAskHint(); askBox.hidden = true; return; }
    if (!r.ok) throw new Error('HTTP ' + r.status);
    hideAskHint();
    const now = Date.now();
    const list = ((await r.json()) || []).filter((q) => now - (q.at || 0) < pendingMaxAge);
    renderAsk(list);
  } catch (_) { /* 服务重启之类，下一轮再来 */ }
}

function showAskHint() { if (askHint) askHint.style.display = ''; }
function hideAskHint() { if (askHint) askHint.style.display = 'none'; }

function renderAsk(list) {
  askList.textContent = '';
  if (!list.length) { askBox.hidden = true; return; }
  askBox.hidden = false;
  for (const q of list) {
    const row = addDiv(askList, 'arow');
    const span = document.createElement('span');
    span.textContent = (q.name || '一台设备') + '（' + q.ip + '）想给你传文件';
    const ok = document.createElement('button');
    ok.className = 'primary';
    ok.textContent = '允许';
    const no = document.createElement('button');
    no.textContent = '拒绝';
    ok.addEventListener('click', () => decidePeer(q.ip, true, ok, no));
    no.addEventListener('click', () => decidePeer(q.ip, false, ok, no));
    row.appendChild(span); row.appendChild(ok); row.appendChild(no);
  }
}

async function decidePeer(ip, ok, btnA, btnB) {
  btnA.disabled = true; btnB.disabled = true;
  try {
    const r = await fetch('/api/peer/decide?' + authPair() +
      '&ip=' + encodeURIComponent(ip) + '&ok=' + (ok ? '1' : '0'), { method: 'POST' });
    if (!r.ok && r.status !== 404) {
      const j = await r.json().catch(() => ({}));
      setPeerStatus('操作失败：' + (j.error || r.status));
    }
  } catch (e) {
    setPeerStatus('操作失败：' + e);
  }
  refreshPending();
}

// ---- SSE 实时刷新（任一设备完成上传 / 删除 / 改名，所有页面同步列表） ----
let filesRefreshTimer = null;
let notesRefreshTimer = null;
function listenEvents() {
  try {
    const es = new EventSource('/api/events');
    es.onmessage = (ev) => {
      try {
        const j = JSON.parse(ev.data);
        if (j.type === 'files') {
          clearTimeout(filesRefreshTimer);
          filesRefreshTimer = setTimeout(loadFiles, 200);
        } else if (j.type === 'notes') {
          clearTimeout(notesRefreshTimer);
          notesRefreshTimer = setTimeout(loadNotes, 200);
        } else if (j.type === 'peers') {
          loadPeers();
        } else if (j.type === 'peer_request') {
          refreshPending();
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
  const p = authPair();
  let qs = '/api/uploads';
  if (name) qs += '?name=' + encodeURIComponent(name) + (p ? '&' + p : '');
  else if (p) qs += '?' + p;
  try {
    const r = await fetch(qs, { method: 'DELETE' });
    const j = await r.json().catch(() => ({}));
    if (!r.ok) { toast('清理失败：' + (j.error || r.status), 'err'); return; }
    setStatus('已清理 ' + (j.removed || 0) + ' 项中断的传输');
    loadPartials();
  } catch (e) { toast('清理失败：' + e, 'err'); }
}

if (partialListEl) {
  partialListEl.addEventListener('click', async (e) => {
    const a = e.target.closest('a[data-act="purge"]');
    if (!a) return;
    e.preventDefault();
    if (!(await modalPrompt({ title: '清理中断的传输', body: '清理「' + a.dataset.name + '」尚未传完的数据？', sub: '未传完的临时数据会被删除，已传完的文件不受影响。', okText: '清理', cancelText: '取消', danger: true }))) return;
    await purgePartials(a.dataset.name);
  });
}
if (purgeAllBtn) {
  purgeAllBtn.addEventListener('click', async () => {
    if (!(await modalPrompt({ title: '清理所有中断的传输', body: '清理所有尚未传完的临时数据？', sub: '未传完的数据会被删除，已传完的文件不受影响。', okText: '全部清理', cancelText: '取消', danger: true }))) return;
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
  if (!d) { toast('请先填写目录路径', 'warn'); return; }
  try {
    const r = await fetch('/api/settings?' + authPair(), {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ dir: d })
    });
    const j = await r.json().catch(() => ({}));
    if (!r.ok) { toast('切换失败：' + (j.error || r.status), 'err'); return; }
    loadSettings();
    loadFiles();
  } catch (e) { toast('切换失败：' + e, 'err'); }
});

$('pickFolder').addEventListener('click', async () => {
  try {
    const r = await fetch('/api/pick-folder?' + authPair(), { method: 'POST' });
    const j = await r.json().catch(() => ({}));
    if (j.path) dirInput.value = j.path;
    else if (j.error) toast('打开文件夹选择器失败：' + j.error, 'err');
  } catch (e) { toast('无法打开选择器：' + e, 'err'); }
});

$('openFolder').addEventListener('click', () => { fetch('/api/open-folder?' + authPair(), { method: 'POST' }); });

prefixInput.value = getPrefix();
prefixInput.addEventListener('input', () => {
  localStorage.setItem(PREFIX_KEY, prefixInput.value);
  loadFiles();
});

loadInfo();
loadFiles();
loadPartials();
loadSettings();
loadNotes();
listenEvents();

if (peerListEl) {
  loadPeers();
  setInterval(loadPeers, 5000);
  if (peerAddBtn) peerAddBtn.addEventListener('click', addPeerManually);
  if (peerAddrEl) peerAddrEl.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') { e.preventDefault(); addPeerManually(); }
  });
}
if (askList) {
  refreshPending();
  setInterval(refreshPending, 4000);
}
// ---- 键盘快捷键（商业级效率） ----
document.addEventListener('keydown',(e)=>{
  if((e.ctrlKey||e.metaKey)&&e.key.toLowerCase()==='k'){e.preventDefault();$('searchInput')?.focus();}
  if((e.ctrlKey||e.metaKey)&&e.key.toLowerCase()==='u'){e.preventDefault();$('drop')?.click();}
  if(e.key==='Escape'){const m=$('previewMask');if(m&&m.style.display!=='none') m.style.display='none';}
});
// 输入框长度保护
if(prefixInput) prefixInput.maxLength=32;
