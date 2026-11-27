'use strict';
Object.assign(I, {
  cloud:'<path d="M7 18a4 4 0 0 1-.6-7.96A6 6 0 0 1 18 9.5a4.25 4.25 0 0 1-.5 8.5z"/>',
  logout:'<path d="M10 4H5v16h5M15 8l4 4-4 4M19 12H9"/>',
  left:'<path d="m15 5-7 7 7 7"/>',
  right:'<path d="m9 5 7 7-7 7"/>',
  sync:'<path d="M4 12a8 8 0 0 1 13.7-5.6L20 9M20 4v5h-5M20 12a8 8 0 0 1-13.7 5.6L4 15M4 20v-5h5"/>',
  sliders:'<path d="M4 6h9M19 6h1M4 12h3M13 12h7M4 18h11"/><circle cx="16" cy="6" r="2.5"/><circle cx="10" cy="12" r="2.5"/><circle cx="18" cy="18" r="2.5"/>'
});

/* ---------- 基础工具 ---------- */
const $ = id => document.getElementById(id);
const icon = n => `<svg class="ic" viewBox="0 0 24 24" aria-hidden="true">${I[n] || I.file}</svg>`;
const esc = s => String(s == null ? '' : s).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const enc = encodeURIComponent;
const FOLDER = '<svg class="fold" viewBox="0 0 64 64" aria-hidden="true"><path d="M6 16a6 6 0 0 1 6-6h13l6 7h21a6 6 0 0 1 6 6v25a6 6 0 0 1-6 6H12a6 6 0 0 1-6-6z"/></svg>';
const SEP = '<span class="sep">›</span>';

const TYPES = {
  folder:{label:'文件夹'}, image:{label:'图片',icon:'image'}, video:{label:'视频',icon:'video'},
  doc:{label:'文档',icon:'doc'}, audio:{label:'音乐',icon:'audio'}, other:{label:'其他',icon:'file'}
};
const NAV = [
  {id:'files',label:'全部文件',icon:'folder'},{id:'recent',label:'最近',icon:'clock'},
  {id:'image',label:'图片',icon:'image'},{id:'video',label:'视频',icon:'video'},
  {id:'doc',label:'文档',icon:'doc'},{id:'audio',label:'音乐',icon:'audio'},
  {id:'trash',label:'回收站',icon:'trash'},
  null,
  {id:'disks',label:'磁盘文件',icon:'disk'},{id:'mounts',label:'外部网盘',icon:'cloud'},{id:'downloads',label:'离线下载',icon:'download'},
  {id:'dav',label:'WebDAV 同步',icon:'sync'},
  {id:'ext',label:'扩展',icon:'apps'},{id:'settings',label:'设置',icon:'sliders'}
];
const LOCAL = ['files','recent','image','video','doc','audio'];
const DL_STATUS = {queued:'排队中', running:'下载中', paused:'已暂停', done:'已完成', error:'失败', canceled:'已取消'};
const NO_PREVIEW = /\.(svg|heic|heif|tiff?)$/i;

const S = {
  nav:'files', cwd:'', view:'grid', sel:new Set(), q:'', link:null, mount:null, mpath:'',
  entries:[], trash:[], trashDays:30, dav:[], davNew:null, spaces:[], space:null, disks:[], disk:null, diskInfo:null, disksOff:false, truncated:false, settings:null, links:[], mounts:[], downloads:[], uploads:[], checks:{},
  storage:null, error:'', authed:false, editLink:null, editMount:null
};

function fmtSize(b){
  if(b >= 2**40) return (b/2**40).toFixed(2) + ' TB';
  if(b >= 2**30) return (b/2**30).toFixed(1) + ' GB';
  if(b >= 2**20) return (b/2**20).toFixed(1) + ' MB';
  if(b >= 1024) return Math.round(b/1024) + ' KB';
  return b + ' B';
}
function fmtDate(ms){
  if(!ms) return '—';
  const d = new Date(ms), p = n => String(n).padStart(2,'0');
  return `${d.getFullYear()}-${p(d.getMonth()+1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}
function typeOf(name){
  const e = (name.split('.').pop() || '').toLowerCase();
  if(/^(jpe?g|png|gif|webp|bmp|heic|svg)$/.test(e)) return 'image';
  if(/^(mp4|mkv|mov|avi|webm|m4v|flv|wmv)$/.test(e)) return 'video';
  if(/^(mp3|flac|wav|aac|m4a|ogg)$/.test(e)) return 'audio';
  if(/^(pdf|docx?|xlsx?|pptx?|txt|md|csv)$/.test(e)) return 'doc';
  return 'other';
}

let toastTimer;
function toast(msg){ const t = $('toast'); t.textContent = msg; t.classList.add('show'); clearTimeout(toastTimer); toastTimer = setTimeout(() => t.classList.remove('show'), 2600); }

async function api(method, url, body){
  const opt = {method, headers:{'X-Requested-With':'mynas'}, credentials:'same-origin'};
  if(body !== undefined){ opt.headers['Content-Type'] = 'application/json'; opt.body = JSON.stringify(body); }
  let res;
  try { res = await fetch(url, opt); } catch(e){ throw new Error('无法连接到 NAS 服务'); }
  let data = null;
  try { data = await res.json(); } catch(e){ /* 非 JSON 响应 */ }
  if(res.status === 401 && S.authed){ showAuth('login'); throw new Error('登录已失效，请重新登录'); }
  if(!res.ok) throw new Error(data && data.error || `请求失败（${res.status}）`);
  return data;
}

/* ---------- 登录 ---------- */
let authMode = 'login';
function showAuth(mode){
  authMode = mode; S.authed = false;
  $('app').hidden = true; $('auth').hidden = false;
  const setup = mode === 'setup';
  $('authTitle').textContent = setup ? '创建管理员账号' : '登录';
  $('authHint').textContent = setup ? '首次使用，请设置管理员的用户名和密码（密码至少 8 位）。' : '请输入管理员账号。';
  $('auSubmit').textContent = setup ? '创建并进入' : '登录';
  $('auPass').autocomplete = setup ? 'new-password' : 'current-password';
  $('auErr').textContent = '';
  $('auUser').focus();
}
$('authForm').addEventListener('submit', async e => {
  e.preventDefault();
  $('auErr').textContent = '';
  try {
    await api('POST', authMode === 'setup' ? '/api/setup' : '/api/login', {username:$('auUser').value, password:$('auPass').value});
    $('auPass').value = '';
    start();
  } catch(err){ $('auErr').textContent = err.message; }
});
async function boot(){
  try {
    const s = await api('GET', '/api/session');
    if(!s.setupDone) showAuth('setup'); else if(!s.authed) showAuth('login'); else start();
  } catch(e){ showAuth('login'); $('auErr').textContent = e.message; }
}
async function start(){
  S.authed = true;
  $('auth').hidden = true; $('app').hidden = false;
  await Promise.all([loadLinks(), refreshStorage(), refreshDownloads()]);
  load();
  // 上次选的存储位置用不了时（例如硬盘没接上），登录后提醒一次
  api('GET', '/api/settings').then(s => { S.settings = s; if(s.warn) toast(s.warn); }).catch(() => {});
}

/* ---------- 数据加载 ---------- */
function kind(){
  if(inDisk()) return 'disk';
  if(S.q) return 'local';
  if(S.nav === 'trash') return 'trash';
  if(S.nav === 'mounts' && S.mount) return 'mount';
  if(inSpace()) return 'local';
  return LOCAL.includes(S.nav) ? 'local' : '';
}
// 同步空间：打开某个空间后，文件接口都作用在这个空间里，路径相对于空间。
const inSpace = () => S.nav === 'dav' && !!S.space;
const sp = () => inSpace() ? '&space=' + enc(S.space) : '';
const spq = () => inSpace() ? '?space=' + enc(S.space) : '';
// 磁盘文件：打开某块磁盘后，直接读磁盘上的内容（只读），路径相对于磁盘的最上层。
const inDisk = () => S.nav === 'disks' && !!S.disk;
const dq = () => 'disk=' + enc(S.disk);
const diskLabel = () => { const d = S.diskInfo && S.diskInfo.id === S.disk ? S.diskInfo : S.disks.find(x => x.id === S.disk); return d ? d.name : S.disk; };
// 一个文件的下载地址；inline 表示在页面里预览
const fileUrl = (e, inline) => inDisk() ? `/api/disks/download?${dq()}&path=${enc(e.path)}${inline ? '&inline=1' : ''}`
  : `/api/files/download?${inline ? 'inline=1&' : ''}path=${enc(e.path)}${sp()}`;
const browsing = () => (S.nav === 'files' || inSpace()) && !S.q;
function current(){
  if(kind() !== 'trash') return S.entries;
  return S.trash.map(t => ({name:t.name, path:t.id, dir:t.dir, size:t.size, mtime:Date.parse(t.deletedAt), type:t.dir ? 'folder' : typeOf(t.name), orig:t.origPath}));
}
function sorted(list){
  if(!S.q && (S.nav === 'recent' || S.nav === 'trash')) return list;
  return list.slice().sort((a,b) => (b.dir - a.dir) || a.name.localeCompare(b.name, 'zh'));
}
async function loadLinks(){ try { S.links = (await api('GET', '/api/links')).links; } catch(e){ /* 保留旧数据 */ } }
async function refreshStorage(){ try { S.storage = await api('GET', '/api/storage'); } catch(e){ S.storage = null; } renderSide(); }
async function refreshDownloads(){ try { S.downloads = (await api('GET', '/api/downloads')).downloads; } catch(e){ return; } renderSide(); if(S.nav === 'downloads' && !S.q) renderDlList(); }

let loadSeq = 0;
async function load(){
  const seq = ++loadSeq;
  let err = '';
  try {
    if(inDisk() && S.q){ const r = await api('GET', `/api/disks/find?${dq()}&path=${enc(S.cwd)}&q=` + enc(S.q)); S.entries = r.entries; S.truncated = !!r.truncated; S.diskInfo = r.disk; }
    else if(inDisk()){ S.truncated = false; const r = await api('GET', `/api/disks/list?${dq()}&path=` + enc(S.cwd)); S.entries = r.entries; S.diskInfo = r.disk; }
    else if(S.q) S.entries = (await api('GET', '/api/files/find?q=' + enc(S.q) + sp())).entries;
    else if(S.nav === 'files') S.entries = (await api('GET', '/api/files/list?path=' + enc(S.cwd))).entries;
    else if(S.nav === 'recent') S.entries = (await api('GET', '/api/files/find?recent=1')).entries;
    else if(LOCAL.includes(S.nav)) S.entries = (await api('GET', '/api/files/find?type=' + S.nav)).entries;
    else if(S.nav === 'trash'){ const r = await api('GET', '/api/trash'); S.trash = r.items; S.trashDays = r.trashDays; }
    else if(S.nav === 'mounts'){
      S.mounts = (await api('GET', '/api/mounts')).mounts;
      if(S.mount){ S.entries = []; S.entries = (await api('GET', `/api/mounts/${S.mount}/list?path=` + enc(S.mpath))).entries; }
    }
    else if(S.nav === 'disks'){ const r = await api('GET', '/api/disks'); S.disks = r.disks; S.disksOff = !!r.off; }
    else if(S.nav === 'downloads') S.downloads = (await api('GET', '/api/downloads')).downloads;
    else if(inSpace()) S.entries = (await api('GET', '/api/files/list?path=' + enc(S.cwd) + sp())).entries;
    else if(S.nav === 'dav'){ S.dav = (await api('GET', '/api/dav')).keys; S.spaces = (await api('GET', '/api/spaces')).spaces; }
    else if(S.nav === 'settings') S.settings = await api('GET', '/api/settings');
    else if(S.nav === 'ext') await loadLinks();
  } catch(e){ err = e.message; if(kind()) S.entries = []; }
  if(seq !== loadSeq) return;
  S.error = err;
  render();
}
function go(patch){
  Object.assign(S, patch);
  if(patch.q === '') $('q').value = '';
  S.sel.clear();
  renderSide();
  return load();
}
function navTo(patch){ closeSide(); return go(Object.assign({cwd:'', q:'', mount:null, mpath:'', link:null, davNew:null, space:null, disk:null}, patch)); }

/* ---------- 侧栏 ---------- */
function renderSide(){
  const active = S.downloads.filter(d => d.status === 'running' || d.status === 'queued').length;
  let h = NAV.map(n => {
    if(n === null) return '<div class="nav-sep"></div>';
    const on = !S.q && S.nav === n.id && !(n.id === 'ext' && S.link);
    const count = n.id === 'downloads' && active ? `<span class="nav-count">${active}</span>` : '';
    return `<button class="nav-item ${on ? 'on' : ''}" data-nav="${n.id}">${icon(n.icon)}<span>${n.label}</span>${count}</button>`;
  }).join('');
  const pinned = S.links.filter(l => l.pin);
  if(pinned.length) h += '<div class="nav-label">已固定的链接</div>' + pinned.map(l =>
    `<button class="nav-item ${!S.q && S.nav === 'ext' && S.link === l.id ? 'on' : ''}" data-link="${l.id}">${icon('globe')}<span>${esc(l.name)}</span></button>`).join('');
  if(S.storage && S.storage.total){
    const pct = Math.round(S.storage.used / S.storage.total * 100);
    h += `<div class="quota"><div class="meter" role="img" aria-label="已用 ${pct}%"><i style="width:${pct}%"></i></div>
      <div class="quota-text">${fmtSize(S.storage.used)} / ${fmtSize(S.storage.total)} 已用</div>
      <div class="quota-note ell" title="“全部文件”对应的目录：${esc(S.storage.dir || '')}">目录 ${esc(S.storage.dir || '')}</div></div>`;
  }
  $('side').innerHTML = h;
}
function closeSide(){ $('side').classList.remove('open'); $('scrim').hidden = true; }

/* ---------- 文件列表（本机 / 回收站 / 外部网盘共用） ---------- */
function crumbs(){
  const trail = (segs, attr) => segs.map((s,i) => SEP + (i === segs.length - 1
    ? `<span class="cur">${esc(s)}</span>`
    : `<button ${attr}="${esc(segs.slice(0, i+1).join('/'))}">${esc(s)}</button>`)).join('');
  if(inDisk()){
    const segs = S.cwd ? S.cwd.split('/') : [], name = esc(diskLabel()), top = '<button data-nav="disks">磁盘文件</button>' + SEP;
    if(S.q) return top + `<button data-dcd="${esc(S.cwd)}">${S.cwd ? esc(segs[segs.length - 1]) : name}</button>` + SEP + `<span class="cur">搜索“${esc(S.q)}”</span>`;
    return top + (segs.length ? `<button data-dcd="">${name}</button>` : `<span class="cur">${name}</span>`) + trail(segs, 'data-dcd');
  }
  if(S.q) return `<span class="cur">搜索“${esc(S.q)}”</span>`;
  if(inSpace()){
    const segs = S.cwd ? S.cwd.split('/') : [], name = esc(S.space);
    return '<button data-nav="dav">WebDAV 同步</button>' + SEP +
      (segs.length ? `<button data-scd="">${name}</button>` : `<span class="cur">${name}</span>`) + trail(segs, 'data-scd');
  }
  if(S.nav === 'files'){
    const segs = S.cwd ? S.cwd.split('/') : [];
    return (segs.length ? '<button data-cd="">全部文件</button>' : '<span class="cur">全部文件</span>') + trail(segs, 'data-cd');
  }
  if(S.nav === 'mounts' && S.mount){
    const m = S.mounts.find(x => x.id === S.mount), name = esc(m ? m.name : '网盘');
    const segs = S.mpath ? S.mpath.split('/') : [];
    return '<button data-nav="mounts">外部网盘</button>' + SEP +
      (segs.length ? `<button data-mcd="">${name}</button>` : `<span class="cur">${name}</span>`) + trail(segs, 'data-mcd');
  }
  const n = NAV.find(x => x && x.id === S.nav);
  return `<span class="cur">${n ? n.label : ''}</span>`;
}
function thumbOf(e){
  if(e.dir) return FOLDER;
  if((kind() === 'local' || kind() === 'disk') && e.type === 'image' && e.size < 4 * 2**20 && !NO_PREVIEW.test(e.name))
    return `<img class="pic" src="${fileUrl(e, true)}" alt="" loading="lazy">`;
  return `<div class="tile t-${e.type}">${icon(TYPES[e.type].icon)}</div>`;
}
const metaOf = e => e.cloud ? '云盘存储位置' : e.dir ? '文件夹' : fmtSize(e.size);
// An empty top level usually means the files directory points at a new, empty
// folder rather than at the disk that already holds the data. Say which one it is.
function rootHint(){
  if(S.cwd !== '' || !S.storage || !S.storage.dir || inSpace()) return '';
  return `<div class="sub" style="margin-top:10px">“全部文件”对应服务器上的目录 ${esc(S.storage.dir)}。<br>只想查看硬盘上已有的文件，点左侧的“磁盘文件”。想在这里管理它们，到“设置”里把存储位置改成硬盘的挂载点。</div>`;
}

/* 回收站的自动清除设置 */
function trashTools(){
  const opts = [7, 15, 30, 90, 0];
  if(!opts.includes(S.trashDays)) opts.unshift(S.trashDays);
  return `<label class="mini"><span class="lbl-wide">自动清除</span><select id="trashDays" aria-label="回收站自动清除的时限">` +
    opts.map(d => `<option value="${d}" ${d === S.trashDays ? 'selected' : ''}>${d > 0 ? '删除满 ' + d + ' 天' : '永不'}</option>`).join('') + '</select></label>';
}
function expiryText(e){
  if(!(S.trashDays > 0)) return '不会自动清除';
  const left = e.mtime + S.trashDays * 86400000 - Date.now();
  if(left <= 0) return '即将清除';
  const d = Math.ceil(left / 86400000);
  return d > 1 ? `约 ${d} 天后` : '1 天内';
}

function renderEntries(){
  const k = kind(), list = sorted(current());
  const ck = `<button class="ck" data-ck aria-label="选择">${icon('check')}</button>`;
  const tools = k === 'local' && browsing() ? `<button class="btn sm" data-act="mkdir">${icon('plus')}<span class="lbl-wide">新建文件夹</span></button>` : k === 'trash' ? trashTools() : '';
  let h = `<div class="bar"><div class="crumbs">${crumbs()}</div><div class="bar-right">${tools}
    <div class="seg"><button class="${S.view === 'grid' ? 'on' : ''}" data-view="grid" aria-label="网格视图">${icon('grid')}</button>
    <button class="${S.view === 'list' ? 'on' : ''}" data-view="list" aria-label="列表视图">${icon('list')}</button></div></div></div>`;
  if(S.sel.size){
    let acts;
    if(k === 'trash') acts = `<button class="btn sm" data-act="restore">${icon('restore')}还原</button><button class="btn sm" data-act="purge">${icon('trash')}彻底删除</button>`;
    else if(k === 'mount') acts = `<button class="btn sm" data-act="download">${icon('download')}下载</button><button class="btn sm" data-act="save-nas">${icon('plus')}保存到 NAS</button>`;
    else if(k === 'disk') acts = `<button class="btn sm" data-act="download">${icon('download')}下载</button>`;
    else acts = `<button class="btn sm" data-act="download">${icon('download')}下载</button>` +
      (S.sel.size === 1 ? `<button class="btn sm" data-act="rename">${icon('edit')}重命名</button>` : '') +
      `<button class="btn sm" data-act="delete">${icon('trash')}删除</button>`;
    h += `<div class="selbar"><b>已选 ${S.sel.size} 项</b>${acts}<button class="btn sm sp" data-act="clear">取消选择</button></div>`;
  } else {
    const note = k === 'trash' ? ' · ' + (S.trashDays > 0 ? `删除满 ${S.trashDays} 天后自动清除` : '不会自动清除') + '，还原后回到原来的位置' : k === 'mount' ? ' · 外部网盘为只读，可下载或保存到 NAS' : k === 'disk' ? (S.q ? (S.truncated ? ' · 内容较多，只搜索了一部分。进入更具体的文件夹再搜，结果更完整' : ' · 在当前文件夹及其子文件夹里搜索') : ' · 磁盘文件为只读：可以浏览、预览和下载') : inSpace() ? ' · 同步空间里的内容不会出现在“全部文件”、分类和搜索里' : '';
    h += `<div class="count">共 ${list.length} 项${note}</div>`;
  }
  if(S.error) h += `<div class="empty">${esc(S.error)}</div>`;
  else if(!list.length) h += `<div class="empty">${S.q ? '没有找到匹配的文件' : k === 'trash' ? '回收站是空的' : k === 'mount' ? '这个目录是空的' : k === 'disk' ? '这个文件夹是空的' : (S.nav === 'files' || inSpace()) ? '这个文件夹是空的，点击右上角“上传”或把文件拖到这里' + rootHint() : '还没有这类文件'}</div>`;
  else if(S.view === 'grid'){
    h += '<div class="grid">' + list.map(e =>
      `<div class="card ${S.sel.has(e.path) ? 'sel' : ''}" data-key="${esc(e.path)}" title="${esc(e.name)}">${ck}
        <div class="thumb">${thumbOf(e)}</div><div class="nm">${esc(e.name)}</div><div class="meta">${metaOf(e)}</div></div>`).join('') + '</div>';
  } else {
    h += `<div class="rows"><div class="rhead"><span></span><span>名称</span><span>大小</span><span class="c-type">类型</span><span class="c-date">${k === 'trash' ? '删除时间' : '修改时间'}</span></div>` +
      list.map(e => `<div class="row ${S.sel.has(e.path) ? 'sel' : ''}" data-key="${esc(e.path)}">${ck}
        <div class="nmcell"><div class="thumb">${thumbOf(e)}</div><span title="${esc(e.name)}">${esc(e.name)}</span></div>
        <span class="num">${metaOf(e)}</span><span class="num c-type">${TYPES[e.type].label}</span><span class="num c-date">${fmtDate(e.mtime)}</span></div>`).join('') + '</div>';
  }
  $('main').innerHTML = h;
}

function renderDetail(){
  const el = $('detail'), k = kind();
  const e = k && S.sel.size === 1 ? current().find(x => x.path === [...S.sel][0]) : null;
  if(!e){ el.hidden = true; el.innerHTML = ''; return; }
  let preview = '';
  if((k === 'local' || k === 'disk') && !e.dir){
    const src = fileUrl(e, true);
    if(e.type === 'image' && !NO_PREVIEW.test(e.name)) preview = `<div class="preview"><img src="${src}" alt="" data-preview="${esc(e.path)}" title="点击查看大图"></div>`;
    else if(e.type === 'video') preview = `<div class="preview"><video src="${src}" controls preload="metadata"></video></div>`;
    else if(e.type === 'audio') preview = `<div class="preview"><audio src="${src}" controls preload="none"></audio></div>`;
  }
  if(!preview) preview = `<div class="thumb">${thumbOf(e)}</div>`;
  const parent = p => p.includes('/') ? p.slice(0, p.lastIndexOf('/')) : '';
  let loc, actions;
  if(k === 'trash'){
    loc = '全部文件' + (parent(e.orig) ? ' / ' + parent(e.orig).split('/').join(' / ') : '');
    actions = `<button class="btn sm primary" data-act="restore">${icon('restore')}还原</button><button class="btn sm" data-act="purge">彻底删除</button>`;
  } else if(k === 'disk'){
    loc = '磁盘文件 / ' + diskLabel() + (parent(e.path) ? ' / ' + parent(e.path).split('/').join(' / ') : '');
    actions = e.cloud ? `<button class="btn sm primary" data-nav="files">到“全部文件”里查看</button>`
      : e.dir ? `<button class="btn sm primary" data-dcd="${esc(e.path)}">打开</button>`
      : `<a class="btn sm primary" href="${fileUrl(e)}" download>${icon('download')}下载</a>`;
  } else if(k === 'mount'){
    const m = S.mounts.find(x => x.id === S.mount);
    loc = (m ? m.name : '网盘') + (parent(e.path) ? ' / ' + parent(e.path).split('/').join(' / ') : '');
    actions = (e.dir ? '' : `<a class="btn sm primary" href="/api/mounts/${S.mount}/download?path=${enc(e.path)}" download>${icon('download')}下载</a>`) +
      `<button class="btn sm" data-act="save-nas">保存到 NAS</button>`;
  } else {
    loc = (inSpace() ? '同步空间 / ' + S.space : '全部文件') + (parent(e.path) ? ' / ' + parent(e.path).split('/').join(' / ') : '');
    actions = (e.dir ? `<button class="btn sm primary" ${inSpace() ? 'data-scd' : 'data-cd'}="${esc(e.path)}">打开</button>`
      : `<a class="btn sm primary" href="${fileUrl(e)}" download>${icon('download')}下载</a>`) +
      `<button class="btn sm" data-act="rename">重命名</button><button class="btn sm" data-act="delete">删除</button>`;
  }
  el.hidden = false;
  el.innerHTML = `<div class="detail-head"><span>文件详情</span><button class="icon-btn" data-act="clear" aria-label="关闭详情">${icon('x')}</button></div>
    ${preview}<h3>${esc(e.name)}</h3>
    <dl class="kv"><dt>类型</dt><dd>${TYPES[e.type].label}</dd><dt>大小</dt><dd>${metaOf(e)}</dd>
    <dt>${k === 'trash' ? '删除时间' : '修改时间'}</dt><dd>${fmtDate(e.mtime)}</dd><dt>${k === 'trash' ? '原位置' : '位置'}</dt><dd>${esc(loc)}</dd>${k === 'trash' ? `<dt>自动清除</dt><dd>${expiryText(e)}</dd>` : ''}</dl>
    <div class="detail-actions">${actions}</div>`;
}

/* ---------- 磁盘文件 ---------- */
function renderDisks(){
  let h = `<div class="page-title">磁盘文件<span class="sub">直接查看磁盘上的内容 · 只读</span></div>
    <div class="count">这里显示接在这台设备上的磁盘本身的内容，从磁盘的最上层开始。只能浏览、预览和下载，不会改动磁盘上的任何东西。云盘自己的文件仍然在“全部文件”里管理，不会在这里重复出现。</div>`;
  if(S.error) h += `<div class="empty">${esc(S.error)}</div>`;
  else if(!S.disks.length) h += `<div class="empty">${S.disksOff ? '“磁盘文件”已经在服务设置里关闭' : '没有发现磁盘<div class="sub" style="margin-top:10px">请确认硬盘已经接好并且已经挂载（例如挂载在 /mnt/sda1）。<br>系统自己所在的盘不会显示在这里。</div>'}</div>`;
  else h += '<div class="cards" style="grid-template-columns:repeat(auto-fill,minmax(290px,1fr))">' + S.disks.map(d => {
    const pct = d.total ? Math.round((d.total - d.free) / d.total * 100) : 0;
    const usage = d.total ? `<div class="meter" role="img" aria-label="已用 ${pct}%"><i style="width:${pct}%"></i></div>
      <div class="line mt"><span>共 ${fmtSize(d.total)}</span><span>剩余 ${fmtSize(d.free)}</span></div>` : '';
    const where = d.cloudRoot ? '<div class="note">这块磁盘就是云盘的存储位置，上面的内容都在“全部文件”里。</div>'
      : d.cloudPath ? `<div class="note">云盘的存储位置是这块磁盘上的“${esc(d.cloudPath)}”文件夹，那一部分在“全部文件”里管理。</div>` : '';
    return `<div class="panel" data-diskcard="${esc(d.id)}"><div class="dev-head"><div class="avatar">${icon('disk')}</div><div style="min-width:0"><h4 class="ell">${esc(d.name)}</h4><div class="sub ell" title="${esc(d.path)}">${esc(d.path)}</div></div></div>
      ${usage}<div class="taglist"><span class="tag">只读</span>${d.fs ? `<span class="tag">${esc(d.fs)}</span>` : ''}${d.cloudRoot ? '<span class="tag">云盘存储位置</span>' : ''}</div>${where}
      <div class="detail-actions mt">${d.cloudRoot ? '<button class="btn sm primary" data-nav="files">打开“全部文件”</button>' : `<button class="btn sm primary" data-disk="${esc(d.id)}">打开</button>`}</div></div>`;
  }).join('') + '</div>';
  $('main').innerHTML = h;
}

/* ---------- 外部网盘 ---------- */
function renderMounts(){
  let h = `<div class="bar"><div class="page-title" style="margin:0">外部网盘<span class="sub">已接入 ${S.mounts.length} 个</span></div>
    <div class="bar-right"><button class="btn sm primary" data-act="addmount">${icon('plus')}添加网盘</button></div></div>
    <div class="count">通过 WebDAV 接入坚果云、Nextcloud、AList 等网盘。目前为只读：可以浏览、下载，或把文件保存到 NAS。</div>`;
  if(S.error) h += `<div class="empty">${esc(S.error)}</div>`;
  else if(!S.mounts.length) h += '<div class="empty">还没有接入网盘，点击右上角“添加网盘”</div>';
  else h += '<div class="cards" style="grid-template-columns:repeat(auto-fill,minmax(270px,1fr))">' + S.mounts.map(m =>
    `<div class="panel"><div class="dev-head"><div class="avatar">${esc([...m.name][0] || '?')}</div><div style="min-width:0"><h4 class="ell">${esc(m.name)}</h4><div class="sub ell">${esc(m.url)}</div></div></div>
      <div class="taglist"><span class="tag">WebDAV</span><span class="tag">只读</span>${m.username ? `<span class="tag">账号 ${esc(m.username)}</span>` : ''}</div>
      <div class="detail-actions mt"><button class="btn sm primary" data-mount="${m.id}">打开</button><button class="btn sm" data-act="editmount" data-id="${m.id}">编辑</button><button class="btn sm" data-act="delmount" data-id="${m.id}">移除</button></div></div>`).join('') + '</div>';
  $('main').innerHTML = h;
}
function openMountDlg(m){
  S.editMount = m ? m.id : null;
  $('mountDlgTitle').textContent = m ? '编辑外部网盘' : '添加外部网盘';
  $('mtName').value = m ? m.name : ''; $('mtUrl').value = m ? m.url : ''; $('mtUser').value = m ? m.username : '';
  $('mtPass').value = ''; $('mtPass').placeholder = m && m.hasPassword ? '留空表示不修改' : '';
  $('mtInsecure').checked = !!(m && m.insecure);
  setMsg('mtErr', '');
  $('mountDlg').showModal();
}
function setMsg(id, msg, ok){ const el = $(id); el.textContent = msg; el.style.color = ok ? 'var(--ink2)' : ''; }
const mountBody = () => ({id:S.editMount || '', name:$('mtName').value, type:'webdav', url:$('mtUrl').value, username:$('mtUser').value, password:$('mtPass').value, insecure:$('mtInsecure').checked});
$('mtTest').addEventListener('click', async () => {
  setMsg('mtErr', '正在连接…', true);
  try { const r = await api('POST', '/api/mounts/test', Object.assign(mountBody(), {name:$('mtName').value || '测试'})); setMsg('mtErr', `连接成功，根目录下有 ${r.count} 项`, true); }
  catch(e){ setMsg('mtErr', e.message); }
});
$('mountForm').addEventListener('submit', async e => {
  e.preventDefault();
  try {
    if(S.editMount) await api('PUT', '/api/mounts/' + S.editMount, mountBody()); else await api('POST', '/api/mounts', mountBody());
    $('mountDlg').close(); toast('已保存'); load();
  } catch(err){ setMsg('mtErr', err.message); }
});

/* ---------- 离线下载 ---------- */
function renderDownloads(){
  $('main').innerHTML = `<div class="page-title">离线下载<span class="sub">由 NAS 直接把文件下载回来，关掉浏览器也会继续</span></div>
    <div class="panel"><label class="field">下载链接（每行一个，支持 http / https 直链）<textarea id="dlUrls" rows="3" placeholder="https://example.com/file.zip"></textarea></label>
    <div class="form-row"><label class="field">保存到（“全部文件”下的文件夹）<input type="text" id="dlDir" value="离线下载"></label>
    <button class="btn primary" data-act="dl-add">${icon('download')}开始下载</button></div><div class="err" id="dlErr" role="alert"></div></div>
    <div id="dlList"></div>`;
  renderDlList();
}
function renderDlList(){
  const box = $('dlList'); if(!box) return;
  if(S.error){ box.innerHTML = `<div class="empty">${esc(S.error)}</div>`; return; }
  if(!S.downloads.length){ box.innerHTML = '<div class="empty">还没有下载任务</div>'; return; }
  box.innerHTML = S.downloads.map(d => {
    const pct = d.total > 0 ? Math.min(100, d.done / d.total * 100) : d.status === 'done' ? 100 : 0;
    const size = d.total > 0 ? `${fmtSize(d.done)} / ${fmtSize(d.total)}` : fmtSize(d.done);
    const dot = d.status === 'done' ? 'on' : d.status === 'error' ? 'bad' : d.status === 'running' ? 'run' : '';
    const b = (op, label) => `<button class="btn sm" data-act="dl-op" data-id="${d.id}" data-op="${op}">${label}</button>`;
    let acts = '';
    if(d.status === 'running' || d.status === 'queued') acts = b('pause','暂停') + b('cancel','取消');
    else if(d.status === 'paused') acts = b('resume','继续') + b('cancel','取消');
    else if(d.status === 'error' || d.status === 'canceled') acts = b('retry','重试');
    else if(d.status === 'done') acts = `<button class="btn sm" data-cd="${esc(d.dir)}">打开所在文件夹</button>`;
    acts += `<button class="btn sm" data-act="dl-remove" data-id="${d.id}">移除记录</button>`;
    return `<div class="panel task"><div class="task-head"><div class="tile t-${typeOf(d.name)}">${icon(TYPES[typeOf(d.name)].icon)}</div>
      <div class="task-name"><b title="${esc(d.name)}">${esc(d.name)}</b><div class="sub ell">${esc(d.url)}</div></div></div>
      <div class="meter" role="img" aria-label="进度 ${Math.round(pct)}%"><i style="width:${pct}%"></i></div>
      <div class="task-foot"><span class="st"><i class="dot ${dot}"></i>${DL_STATUS[d.status] || d.status}</span><span>${size}</span>
      ${d.status === 'running' ? `<span>${fmtSize(d.speed)}/s</span>` : ''}<span>保存到 ${esc(d.dir || '全部文件')}</span><span class="sp">${acts}</span></div>
      ${d.error ? `<div class="task-err">${esc(d.error)}</div>` : ''}</div>`;
  }).join('');
}
setInterval(() => {
  if(!S.authed) return;
  const active = S.downloads.some(d => d.status === 'running' || d.status === 'queued');
  if(active || (S.nav === 'downloads' && !S.q)) refreshDownloads();
}, 1500);

/* ---------- 扩展：外部链接 ---------- */
function frameSrc(l){
  if(!(l.proxy && l.proxyPort)) return l.url;
  const u = new URL(l.url);
  return `http://${location.hostname}:${l.proxyPort}${u.pathname}${u.search}${u.hash}`;
}
function checkHtml(l){
  const c = S.checks[l.id];
  if(!c) return '';
  if(c.pending) return '<span>正在检测…</span>';
  if(!c.reachable) return `<span class="check-bad">NAS 访问不到这个地址：${esc(c.error || '')}</span>`;
  let h;
  if(c.frameBlocked && l.mode === 'embed' && !l.proxy) h = `<span>可以访问，但这个网站禁止被内嵌（${esc(c.reason)}）。</span><button class="btn sm" data-act="enableproxy" data-id="${l.id}">开启代理内嵌</button>`;
  else if(c.frameBlocked && l.proxy) h = `<span class="check-ok">可以访问（状态 ${c.status}）。网站本身禁止内嵌，已由 NAS 代理解决。</span>`;
  else h = `<span class="check-ok">可以访问（状态 ${c.status}）${c.frameBlocked ? '，网站禁止内嵌，适合新窗口打开' : '，允许内嵌'}。</span>`;
  if(l.mode === 'embed' && location.protocol === 'https:' && frameSrc(l).startsWith('http:')) h += '<span class="check-bad">当前管理页面是 HTTPS，浏览器会拦截 HTTP 的内嵌页面，请改用新窗口打开。</span>';
  return h;
}
function linkCard(l){
  const openBtn = l.mode === 'embed'
    ? `<button class="btn sm primary" data-link="${l.id}">打开</button>`
    : `<a class="btn sm primary" href="${esc(l.url)}" target="_blank" rel="noopener noreferrer">${icon('extopen')}打开</a>`;
  return `<div class="panel"><div class="dev-head"><div class="avatar">${esc([...l.name][0] || '?')}</div><div style="min-width:0"><h4 class="ell">${esc(l.name)}</h4><div class="sub ell">${esc(l.url)}</div></div></div>
    <div class="sub" style="min-height:18px">${esc(l.desc || '')}</div>
    <div class="taglist"><span class="tag">${l.mode === 'embed' ? '页面内打开' : '新窗口打开'}</span>${l.proxy ? `<span class="tag">经 NAS 代理 · 端口 ${l.proxyPort}</span>` : ''}${l.pin ? '<span class="tag">已固定到侧栏</span>' : ''}</div>
    <div class="detail-actions mt">${openBtn}<button class="btn sm" data-act="editlink" data-id="${l.id}">编辑</button>
    <button class="btn sm" data-act="pin" data-id="${l.id}">${l.pin ? '取消固定' : '固定到侧栏'}</button>
    <button class="btn sm" data-act="checklink" data-id="${l.id}">检测</button><button class="btn sm" data-act="dellink" data-id="${l.id}">移除</button></div>
    <div class="note" data-check="${l.id}">${checkHtml(l)}</div></div>`;
}
function renderExt(){
  let h = `<div class="bar"><div class="page-title" style="margin:0">扩展<span class="sub">外部链接 · 已接入 ${S.links.length} 个</span></div>
    <div class="bar-right"><button class="btn sm primary" data-act="addlink">${icon('plus')}添加链接</button></div></div>
    <div class="count">把路由器后台、影音库、下载器等其他服务的网址接进来，统一从这里打开。</div>`;
  if(S.error) h += `<div class="empty">${esc(S.error)}</div>`;
  else if(!S.links.length) h += '<div class="empty">还没有接入任何链接，点击右上角“添加链接”</div>';
  else h += '<div class="cards" style="grid-template-columns:repeat(auto-fill,minmax(290px,1fr))">' + S.links.map(linkCard).join('') + '</div>';
  $('main').innerHTML = h;
}
function renderLinkView(l){
  $('main').innerHTML = `<div class="bar"><div class="crumbs"><button data-nav="ext">扩展</button>${SEP}<span class="cur">${esc(l.name)}</span></div>
    <div class="bar-right"><button class="btn sm" data-act="editlink" data-id="${l.id}">${icon('edit')}<span class="lbl-wide">编辑</span></button>
    <a class="btn sm" href="${esc(l.url)}" target="_blank" rel="noopener noreferrer">${icon('extopen')}<span class="lbl-wide">新窗口打开</span></a></div></div>
    <div class="frame"><div class="frame-bar">${icon('globe')}<span class="ell">${esc(l.url)}</span>${l.proxy ? '<span class="tag">经 NAS 代理</span>' : ''}
    <button class="linkbtn" data-act="checklink" data-id="${l.id}">页面打不开？</button></div>
    <div class="frame-note" data-check="${l.id}" ${S.checks[l.id] ? '' : 'hidden'}>${checkHtml(l)}</div>
    <iframe src="${esc(frameSrc(l))}" title="${esc(l.name)}" referrerpolicy="no-referrer"
      sandbox="allow-scripts allow-forms allow-same-origin allow-popups allow-downloads allow-modals"></iframe></div>`;
}
function showCheck(l){
  document.querySelectorAll(`[data-check="${l.id}"]`).forEach(el => { el.innerHTML = checkHtml(l); el.hidden = false; });
}
function openLinkDlg(l){
  S.editLink = l ? l.id : null;
  $('linkDlgTitle').textContent = l ? '编辑外部链接' : '添加外部链接';
  $('lkName').value = l ? l.name : ''; $('lkUrl').value = l ? l.url : ''; $('lkDesc').value = l ? l.desc : '';
  $('lkMode').value = l ? l.mode : 'embed';
  $('lkProxy').checked = !!(l && l.proxy); $('lkInsecure').checked = !!(l && l.insecure); $('lkPin').checked = l ? l.pin : true;
  $('lkEmbedOpts').hidden = $('lkMode').value !== 'embed';
  $('lkErr').textContent = '';
  $('linkDlg').showModal();
}
$('lkMode').addEventListener('change', () => { $('lkEmbedOpts').hidden = $('lkMode').value !== 'embed'; });
$('linkForm').addEventListener('submit', async e => {
  e.preventDefault();
  const body = {name:$('lkName').value, url:$('lkUrl').value, desc:$('lkDesc').value, mode:$('lkMode').value,
    proxy:$('lkProxy').checked, insecure:$('lkInsecure').checked, pin:$('lkPin').checked};
  try {
    const saved = S.editLink ? await api('PUT', '/api/links/' + S.editLink, body) : await api('POST', '/api/links', body);
    delete S.checks[saved.id];
    $('linkDlg').close(); toast('已保存链接');
    await loadLinks(); render();
  } catch(err){ $('lkErr').textContent = err.message; }
});
const linkBody = l => ({name:l.name, url:l.url, desc:l.desc, mode:l.mode, proxy:l.proxy, insecure:l.insecure, pin:l.pin});

/* ---------- 上传 ---------- */
const upQueue = []; let upBusy = false;
function queueUploads(files){
  if(S.nav === 'disks'){ toast('“磁盘文件”是只读的。要上传，请先到“全部文件”'); return; }
  const here = browsing(), dir = here ? S.cwd : '', space = here && inSpace() ? S.space : null;
  for(const f of files){ const u = {name:f.name, file:f, dir, space, pct:0, state:'wait', err:''}; S.uploads.unshift(u); upQueue.push(u); }
  if(!files.length) return;
  $('transfer').hidden = false; renderTransfer(); pumpUploads();
}
function pumpUploads(){
  if(upBusy) return;
  const u = upQueue.shift(); if(!u) return;
  upBusy = true; u.state = 'run';
  const x = new XMLHttpRequest();
  const finish = () => {
    u.file = null; upBusy = false; renderTransfer();
    if(browsing() && S.cwd === u.dir && (inSpace() ? S.space : null) === u.space) load();
    if(!upQueue.length) refreshStorage();
    pumpUploads();
  };
  x.open('POST', `/api/files/upload?dir=${enc(u.dir)}&name=${enc(u.name)}${u.space ? '&space=' + enc(u.space) : ''}`);
  x.setRequestHeader('X-Requested-With', 'mynas');
  x.upload.onprogress = e => { if(e.lengthComputable){ u.pct = e.loaded / e.total * 100; renderTransfer(); } };
  x.onload = () => {
    if(x.status === 200){ u.state = 'done'; u.pct = 100; }
    else { u.state = 'error'; try { u.err = JSON.parse(x.responseText).error; } catch(e){ u.err = '上传失败'; } }
    finish();
  };
  x.onerror = () => { u.state = 'error'; u.err = '网络中断'; finish(); };
  x.send(u.file);
}
function renderTransfer(){
  const active = S.uploads.filter(u => u.state === 'wait' || u.state === 'run').length;
  $('transferBtn').innerHTML = icon('transfer') + (active ? `<span class="badge">${active}</span>` : '');
  const label = u => u.state === 'done' ? '已完成' : u.state === 'error' ? (u.err || '失败') : u.state === 'wait' ? '等待中' : Math.floor(u.pct) + '%';
  $('transfer').innerHTML = `<header><span>上传列表 <span class="sub">${active ? active + ' 项进行中' : S.uploads.length ? '全部完成' : '暂无任务'}</span></span>
    <button class="icon-btn" data-act="close-transfer" aria-label="关闭上传列表">${icon('x')}</button></header>
    <div class="tlist">` + S.uploads.map(u => `<div class="titem"><div class="tn"><span>${esc(u.name)}</span><span>${esc(label(u))}</span></div>
    <div class="meter"><i style="width:${u.pct}%"></i></div></div>`).join('') + '</div>';
}

/* ---------- 名称输入框 ---------- */
let nameResolve = null;
function askName(title, label, value){
  $('nameDlgTitle').textContent = title; $('nameLabel').textContent = label; $('nmInput').value = value || ''; $('nmErr').textContent = '';
  $('nameDlg').showModal(); $('nmInput').select();
  return new Promise(res => { nameResolve = res; });
}
$('nameForm').addEventListener('submit', e => {
  e.preventDefault();
  const v = $('nmInput').value.trim();
  if(!v){ $('nmErr').textContent = '请填写名称'; return; }
  const res = nameResolve; nameResolve = null; $('nameDlg').close(); if(res) res(v);
});
$('nameDlg').addEventListener('close', () => { if(nameResolve){ const res = nameResolve; nameResolve = null; res(null); } });

/* ---------- 预览查看器（图片 / 视频 / 音频） ---------- */
const V = {list:[], i:0, open:false};
const previewable = e => !e.dir && (e.type === 'image' || e.type === 'video' || e.type === 'audio');
function openViewer(entry){
  V.list = sorted(current()).filter(previewable);
  if(!V.list.length) return;
  V.i = Math.max(0, V.list.findIndex(x => x.path === entry.path));
  V.open = true;
  $('viewer').hidden = false;
  renderViewer();
  $('vwClose').focus();
}
function closeViewer(){
  if(!V.open) return;
  V.open = false;
  $('vwStage').innerHTML = '';   // 同时停止正在播放的视频和音频
  $('viewer').hidden = true;
}
function stepViewer(d){
  if(V.list.length < 2) return;
  V.i = (V.i + d + V.list.length) % V.list.length;
  renderViewer();
}
function renderViewer(){
  const e = V.list[V.i], stage = $('vwStage');
  $('vwName').textContent = e.name;
  $('vwCount').textContent = V.list.length > 1 ? `${V.i + 1} / ${V.list.length}` : '';
  $('vwDownload').href = fileUrl(e);
  $('vwDownload').setAttribute('download', e.name);
  $('vwPrev').hidden = $('vwNext').hidden = V.list.length < 2;
  stage.innerHTML = '';
  const el = document.createElement(e.type === 'image' ? 'img' : e.type);
  el.addEventListener('error', () => {
    if(!V.open || V.list[V.i] !== e) return;
    const ext = e.name.includes('.') ? e.name.split('.').pop().toUpperCase() + ' ' : '';
    stage.innerHTML = `<div class="viewer-msg">浏览器无法直接预览这个 ${esc(ext)}文件。<br>点右上角的下载按钮，下载后查看。</div>`;
  });
  if(e.type === 'image') el.alt = e.name; else { el.controls = true; el.autoplay = true; }
  el.src = fileUrl(e, true);
  stage.appendChild(el);
}
$('vwClose').addEventListener('click', closeViewer);
$('vwPrev').addEventListener('click', () => stepViewer(-1));
$('vwNext').addEventListener('click', () => stepViewer(1));
$('vwStage').addEventListener('click', e => { if(e.target === e.currentTarget) closeViewer(); });
document.addEventListener('keydown', e => {
  if(!V.open) return;
  if(e.key === 'Escape') closeViewer();
  else if(e.key === 'ArrowLeft') stepViewer(-1);
  else if(e.key === 'ArrowRight') stepViewer(1);
});
let viewerTouchX = null;
$('vwStage').addEventListener('touchstart', e => { viewerTouchX = e.touches[0].clientX; }, {passive:true});
$('vwStage').addEventListener('touchend', e => {
  if(viewerTouchX == null) return;
  const dx = e.changedTouches[0].clientX - viewerTouchX;
  viewerTouchX = null;
  if(Math.abs(dx) > 60) stepViewer(dx < 0 ? 1 : -1);
}, {passive:true});

/* ---------- WebDAV 同步（Obsidian 等） ---------- */
async function copyText(text){
  // navigator.clipboard 只在 HTTPS 或 localhost 下可用，局域网地址要用旧办法。
  try { if(navigator.clipboard && window.isSecureContext){ await navigator.clipboard.writeText(text); return true; } } catch(e){ /* 改用下面的办法 */ }
  const ta = document.createElement('textarea');
  ta.value = text; ta.setAttribute('readonly', ''); ta.style.position = 'fixed'; ta.style.opacity = '0';
  document.body.appendChild(ta); ta.select();
  let ok = false;
  try { ok = document.execCommand('copy'); } catch(e){ ok = false; }
  ta.remove();
  return ok;
}
const whereOf = k => k.space ? '同步空间 / ' + k.space : '全部文件' + (k.dir ? ' / ' + k.dir.split('/').join(' / ') : '');
function renderDav(){
  const url = location.origin + '/dav/';
  const row = (label, value, id) => `<div class="kvline"><span class="sub">${label}</span><code ${id ? `id="${id}"` : ''}>${esc(value)}</code><button class="btn sm" data-copy="${esc(value)}">复制</button></div>`;
  const cards = inner => `<div class="cards" style="grid-template-columns:repeat(auto-fill,minmax(270px,1fr))">${inner}</div>`;
  let h = `<div class="bar"><div class="page-title" style="margin:0">WebDAV 同步<span class="sub">让 Obsidian 等软件把这里当作云端存储</span></div>
    <div class="bar-right"><button class="btn sm primary" data-act="adddav">${icon('plus')}新建访问密码</button></div></div>`;
  if(S.error){ $('main').innerHTML = h + `<div class="empty">${esc(S.error)}</div>`; return; }
  h += `<div class="panel">${row('服务器地址', url, 'davUrl')}
    <div class="note">同步软件用“账号 + 访问密码”登录，不使用管理员密码。每个访问密码只能进入指定的一个位置；通过它删除的文件会进回收站，可以还原。</div></div>`;
  if(S.davNew){
    h += `<div class="panel mt"><h4>访问密码已生成，请现在复制保存</h4>
      <div class="note" style="margin:4px 0 8px">密码只显示这一次，离开本页后无法再查看。忘了就在下面它的卡片上点“重新生成密码”。</div>
      ${row('账号', S.davNew.username, 'davUser')}${row('密码', S.davNew.password, 'davPass')}
      <div class="kvline"><span class="sub">位置</span><span>${esc(whereOf(S.davNew))}</span></div></div>`;
  }

  h += '<h3 class="sec">同步空间<span class="sub">单独存放同步来的数据，不会出现在“全部文件”、分类和搜索里</span></h3>';
  h += S.spaces.length ? cards(S.spaces.map(s =>
    `<div class="panel"><div class="dev-head"><div class="avatar">${esc([...s.name][0].toUpperCase())}</div><div style="min-width:0"><h4 class="ell">${esc(s.name)}</h4>
      <div class="sub ell">${s.entries} 项${s.keys.length ? ' · 账号 ' + esc(s.keys.join('、')) : ' · 没有访问密码在使用'}</div></div></div>
      <div class="detail-actions mt"><button class="btn sm primary" data-space="${esc(s.name)}">打开</button>${s.keys.length ? '' : `<button class="btn sm" data-act="delspace" data-name="${esc(s.name)}">删除</button>`}</div></div>`).join(''))
    : '<div class="note" style="margin:0 2px">还没有同步空间。新建访问密码时选“独立的同步空间”就会创建一个。</div>';

  h += '<h3 class="sec">访问密码</h3>';
  h += S.dav.length ? cards(S.dav.map(k =>
    `<div class="panel"><div class="dev-head"><div class="avatar">${esc([...k.username][0].toUpperCase())}</div><div style="min-width:0"><h4 class="ell">${esc(k.username)}</h4>
      <div class="sub ell">${esc(whereOf(k))}</div></div></div>
      <div class="taglist"><span class="tag">${k.readOnly ? '只读' : '可读写'}</span>${k.space ? '<span class="tag">同步空间</span>' : ''}${k.missing ? '<span class="tag warn">文件夹不存在</span>' : ''}</div>
      <div class="line mt"><span>创建时间</span><span>${fmtDate(Date.parse(k.created))}</span></div>
      <div class="line"><span>最近使用</span><span>${k.lastUsed ? fmtDate(Date.parse(k.lastUsed)) : '启动后还没用过'}</span></div>
      <div class="detail-actions mt">${k.space ? `<button class="btn sm" data-space="${esc(k.space)}">打开空间</button>`
        : k.dir ? `<button class="btn sm" data-cd="${esc(k.dir)}">打开文件夹</button><button class="btn sm" data-act="tospace" data-id="${k.id}">移到同步空间</button>` : ''}
        ${k.missing ? `<button class="btn sm" data-act="initdav" data-id="${k.id}">新建空文件夹</button>` : ''}<button class="btn sm" data-act="regendav" data-id="${k.id}">重新生成密码</button><button class="btn sm" data-act="deldav" data-id="${k.id}">移除</button></div></div>`).join(''))
    : (S.davNew ? '' : '<div class="note" style="margin:0 2px">还没有访问密码。点击右上角“新建访问密码”，给 Obsidian 建一个。</div>');

  h += `<details class="panel mt guide" ${S.dav.length ? '' : 'open'}><summary>在 Obsidian 里怎么设置</summary><ol>
    <li>在 Obsidian 的“设置 → 第三方插件”里安装并启用 <b>Remotely Save</b>。第一次使用会弹出确认窗口，勾选两项后点同意。</li>
    <li>打开这个插件的设置，找到“选择远程服务”，选 <b>Webdav</b>。</li>
    <li>服务器地址填上面的地址，用户名和密码填这里生成的账号和访问密码，然后点“检查”。</li>
    <li>手动同步：点 Obsidian 左侧边栏的同步图标。</li>
    <li>自动同步：在插件的“基本设置”里把“自动运行”设成一个间隔。它只在 Obsidian 开着的时候运行。</li>
    <li>第一次同步前建议先备份笔记库。同步时删掉的文件会在这里的回收站里。</li></ol>
    <div class="note">插件设置页顶部的“注册或登录”“连接”是它的付费功能，同步到这里不需要。其他支持 WebDAV 的软件也可以用同样的地址、账号和密码。</div></details>`;
  $('main').innerHTML = h;
}
function syncDavDlg(){
  const space = $('dvWhere').value === 'space';
  $('dvDirLabel').textContent = space ? '同步空间名称' : '文件夹（在“全部文件”下，不存在会自动创建）';
  $('dvNote').textContent = space
    ? '同步空间单独存放，不会和“全部文件”混在一起。多台设备同步同一个笔记库时，填同一个名称。密码自动生成，只显示一次。'
    : '同步来的内容会出现在“全部文件”的这个文件夹里。留空表示可以进入全部文件，不建议这样做。密码自动生成，只显示一次。';
}
$('dvWhere').addEventListener('change', syncDavDlg);
$('davForm').addEventListener('submit', async e => {
  e.preventDefault();
  $('dvErr').textContent = '';
  const body = {username:$('dvUser').value, readOnly:$('dvRO').checked};
  if($('dvWhere').value === 'space'){
    body.space = $('dvDir').value.trim();
    if(!body.space){ $('dvErr').textContent = '请填写同步空间名称'; return; }
  } else body.dir = $('dvDir').value;
  try {
    const r = await api('POST', '/api/dav', body);
    S.davNew = {username:r.key.username, password:r.password, space:r.key.space, dir:r.key.dir};
    $('davDlg').close();
    await load();
  } catch(err){ $('dvErr').textContent = err.message; }
});

/* ---------- 设置：存储位置 ---------- */
function renderSettings(){
  const s = S.settings || {};
  let h = `<div class="page-title">设置<span class="sub">版本 ${esc(s.version || '')}</span></div>`;
  if(S.error){ $('main').innerHTML = h + `<div class="empty">${esc(S.error)}</div>`; return; }
  if(s.warn) h += `<div class="panel warnbox">${esc(s.warn)}</div>`;
  h += `<div class="panel"><h4>管理员账号</h4>
    <div class="note" style="margin:4px 0 10px">登录这个管理界面用的账号。同步软件用的是“WebDAV 同步”页面里的访问密码，和它无关。</div>
    <div class="kvline"><span class="sub">用户名</span><code id="curUser">${esc(s.username || '')}</code></div>
    <div class="detail-actions mt"><button class="btn sm primary" data-act="account">修改用户名或密码</button></div></div>`;
  h += `<div class="panel mt"><h4>存储位置</h4>
    <div class="note" style="margin:4px 0 10px">“全部文件”、回收站和同步空间都存放在这个文件夹里。</div>
    <div class="kvline"><span class="sub">当前位置</span><code id="curDir">${esc(s.filesDir || '')}</code></div>
    ${S.storage && S.storage.total ? `<div class="kvline"><span class="sub">所在磁盘</span><span>共 ${fmtSize(S.storage.total)}，剩余 ${fmtSize(S.storage.free)}</span></div>` : ''}
    <div class="detail-actions mt"><button class="btn sm primary" data-act="changedir">更改存储位置</button></div>
    ${s.system ? '<div class="note">在这里更改后，路由器 LuCI 里“黑曜石云盘”的文件目录会同步更新。</div>' : ''}</div>`;
  $('main').innerHTML = h;
}
const joinDir = (base, name, sep) => base.endsWith(sep) ? base + name : base + sep + name;
// fill：把浏览到的文件夹填进输入框。刚打开对话框时不填，免得盖掉用户正在输入的内容。
async function browseDir(p, fill = true){
  const box = $('sdBrowser');
  try {
    const r = await api('GET', '/api/hostdirs?path=' + enc(p || ''));
    const roots = `<div class="b-roots">${r.roots.map(x => `<button type="button" class="btn sm" data-bdir="${esc(x)}">${esc(x)}</button>`).join('')}</div>`;
    if(!r.path){ box.innerHTML = roots + '<div class="note">点上面的一个位置开始浏览。</div>'; return; }
    box.innerHTML = roots + `<div class="b-path">${icon('folder')}<span class="ell" id="sdHere">${esc(r.path)}</span>${r.parent ? `<button type="button" class="btn sm" data-bdir="${esc(r.parent)}">上一级</button>` : ''}</div>
      <div class="b-list">${r.dirs.length ? r.dirs.map(d => `<button type="button" class="b-item" data-bdir="${esc(joinDir(r.path, d, r.sep))}">${FOLDER}<span class="ell">${esc(d)}</span></button>`).join('') : '<div class="note" style="margin:4px">这里没有子文件夹。可以在上面的输入框里接着写一个新文件夹的名字。</div>'}</div>`;
    if(fill) $('sdDir').value = r.path;
  } catch(e){
    box.innerHTML = `<div class="note">${esc(e.message)}</div><div class="b-roots mt"><button type="button" class="btn sm" data-bdir="">回到起点</button></div>`;
  }
}
function openDirDlg(){
  const cur = (S.settings && S.settings.filesDir) || '';
  $('sdDir').value = ''; $('sdPass').value = ''; $('sdErr').textContent = '';
  $('sdDir').placeholder = '例如 ' + ((S.settings && S.settings.example) || '');
  $('dirDlg').showModal();
  browseDir(cur, false);
}
$('dirForm').addEventListener('submit', async e => {
  e.preventDefault();
  $('sdErr').textContent = '';
  const dir = $('sdDir').value.trim();
  if(!dir){ $('sdErr').textContent = '请填写或选择一个文件夹'; return; }
  if(!$('sdPass').value){ $('sdErr').textContent = '请输入管理员密码'; return; }
  try {
    const r = await api('PUT', '/api/settings/files-dir', {dir, password:$('sdPass').value});
    $('sdPass').value = '';
    $('dirDlg').close();
    S.entries = []; S.trash = [];
    toast('存储位置已改为 ' + r.filesDir);
    refreshStorage();
    await load();
  } catch(err){ $('sdErr').textContent = err.message; }
});

/* ---------- 设置：管理员账号 ---------- */
function openAcctDlg(){
  $('acUser').value = (S.settings && S.settings.username) || '';
  for(const id of ['acOld', 'acNew', 'acNew2']) $(id).value = '';
  $('acErr').textContent = '';
  $('acctDlg').showModal();
  $('acOld').focus();
}
$('acctForm').addEventListener('submit', async e => {
  e.preventDefault();
  const err = m => { $('acErr').textContent = m; };
  err('');
  const username = $('acUser').value.trim(), pw = $('acNew').value;
  if(!username) return err('请填写用户名');
  if(!$('acOld').value) return err('请输入当前密码');
  if(pw && pw.length < 8) return err('新密码至少 8 位');
  if(pw !== $('acNew2').value) return err('两次输入的新密码不一样');
  if(!pw && username === ((S.settings && S.settings.username) || '')) return err('没有要修改的内容：改一下用户名，或者填写新密码');
  try {
    const r = await api('PUT', '/api/account', {password:$('acOld').value, username, newPassword:pw});
    for(const id of ['acOld', 'acNew', 'acNew2']) $(id).value = '';
    $('acctDlg').close();
    toast(r.passwordChanged ? '已修改。下次登录请用新密码，其他已登录的浏览器需要重新登录' : '用户名已修改，其他已登录的浏览器需要重新登录');
    await load();
  } catch(ex){ err(ex.message); }
});

/* ---------- 总渲染 ---------- */
function render(){
  renderSide();
  if(kind()) renderEntries();
  else if(S.nav === 'mounts') renderMounts();
  else if(S.nav === 'disks') renderDisks();
  else if(S.nav === 'downloads') renderDownloads();
  else if(S.nav === 'dav') renderDav();
  else if(S.nav === 'settings') renderSettings();
  else if(S.nav === 'ext'){ const l = S.links.find(x => x.id === S.link); if(l) renderLinkView(l); else renderExt(); }
  renderDetail();
}

/* ---------- 操作 ---------- */
function downloadSelected(){
  const k = kind(), files = current().filter(e => S.sel.has(e.path) && !e.dir);
  if(!files.length){ toast('暂不支持下载整个文件夹'); return; }
  files.forEach((e, i) => setTimeout(() => {
    const a = document.createElement('a');
    a.href = k === 'mount' ? `/api/mounts/${S.mount}/download?path=${enc(e.path)}` : fileUrl(e);
    a.download = e.name; document.body.appendChild(a); a.click(); a.remove();
  }, i * 400));
  if(files.length < S.sel.size) toast('已跳过文件夹，暂不支持下载整个文件夹');
}
async function act(a, el){
  const keys = [...S.sel], id = el && el.dataset.id;
  try {
    switch(a){
      case 'clear': S.sel.clear(); render(); break;
      case 'close-transfer': $('transfer').hidden = true; break;
      case 'mkdir': {
        const name = await askName('新建文件夹', '文件夹名称', '新建文件夹'); if(name == null) return;
        await api('POST', '/api/files/mkdir' + spq(), {dir:S.cwd, name}); toast('已新建文件夹'); await load(); break;
      }
      case 'rename': {
        const e = S.entries.find(x => x.path === keys[0]); if(!e) return;
        const name = await askName('重命名', '新名称', e.name); if(name == null || name === e.name) return;
        await api('POST', '/api/files/rename' + spq(), {path:e.path, name}); S.sel.clear(); toast('已重命名'); await load(); break;
      }
      case 'delete': {
        const r = await api('POST', '/api/files/delete' + spq(), {paths:keys}); S.sel.clear();
        toast(`已将 ${r.deleted} 项移入回收站`); await load(); break;
      }
      case 'restore': { const r = await api('POST', '/api/trash/restore', {ids:keys}); S.sel.clear(); toast(`已还原 ${r.restored} 项`); await load(); break; }
      case 'purge': {
        if(!confirm(`彻底删除选中的 ${keys.length} 项？此操作无法撤销。`)) return;
        const r = await api('POST', '/api/trash/purge', {ids:keys}); S.sel.clear(); toast(`已彻底删除 ${r.purged} 项`); await load(); refreshStorage(); break;
      }
      case 'download': downloadSelected(); break;
      case 'save-nas': {
        const items = keys.map(p => { const e = S.entries.find(x => x.path === p); return {path:p, dir:!!(e && e.dir)}; });
        const r = await api('POST', `/api/mounts/${S.mount}/save`, {items}); S.sel.clear();
        toast(`已加入 ${r.added} 个下载任务，保存到“${r.dir}”`); render(); refreshDownloads(); break;
      }
      case 'addmount': openMountDlg(null); break;
      case 'editmount': openMountDlg(S.mounts.find(m => m.id === id)); break;
      case 'delmount': {
        const m = S.mounts.find(x => x.id === id); if(!m || !confirm(`移除网盘“${m.name}”？网盘里的文件不受影响。`)) return;
        await api('DELETE', '/api/mounts/' + id); toast('已移除网盘'); await load(); break;
      }
      case 'dl-add': {
        const urls = $('dlUrls').value.split(/\r?\n/).map(s => s.trim()).filter(Boolean);
        $('dlErr').textContent = '';
        try { const r = await api('POST', '/api/downloads', {urls, dir:$('dlDir').value}); $('dlUrls').value = ''; toast(`已添加 ${r.added} 个下载任务`); await refreshDownloads(); }
        catch(e){ $('dlErr').textContent = e.message; }
        break;
      }
      case 'dl-op': await api('POST', `/api/downloads/${id}/${el.dataset.op}`); await refreshDownloads(); break;
      case 'dl-remove': await api('DELETE', '/api/downloads/' + id); await refreshDownloads(); break;
      case 'changedir': openDirDlg(); break;
      case 'account': openAcctDlg(); break;
      case 'regendav': {
        const k = S.dav.find(x => x.id === id);
        if(!k || !confirm(`给账号“${k.username}”重新生成密码？\n\n旧密码立即失效，正在用它的同步软件要换成新密码才能继续同步。账号和已经同步的文件不变。`)) return;
        const r = await api('POST', `/api/dav/${id}/password`);
        S.davNew = {username:r.key.username, password:r.password, space:r.key.space, dir:r.key.dir};
        toast('已生成新密码，请复制保存'); await load(); $('main').scrollTop = 0; window.scrollTo(0, 0); break;
      }
      case 'initdav': {
        const k = S.dav.find(x => x.id === id);
        if(!k || !confirm(`在当前存储位置为“${whereOf(k)}”新建一个空文件夹？\n\n如果同步软件之前已经同步过，它会看到云端是空的，可能把本地文件当成已删除。原来的内容还在的话，应该把原来的文件夹复制过来，而不是新建。`)) return;
        await api('POST', `/api/dav/${id}/init`); toast('已新建空文件夹'); await load(); break;
      }
      case 'tospace': {
        const k = S.dav.find(x => x.id === id);
        if(!k || !confirm(`把“${whereOf(k)}”移到同步空间？它会从“全部文件”里消失，改在“WebDAV 同步”页面里查看。同步软件那边不用做任何修改。`)) return;
        const r = await api('POST', `/api/dav/${id}/to-space`); if(S.davNew && S.davNew.username === k.username){ S.davNew.space = r.space; S.davNew.dir = ''; }
        toast(`已移到同步空间“${r.space}”`); await load(); break;
      }
      case 'delspace': {
        const name = el.dataset.name;
        if(!confirm(`删除同步空间“${name}”？里面的文件会进回收站。`)) return;
        await api('DELETE', '/api/spaces/' + enc(name)); toast('已删除，文件在回收站里'); await load(); break;
      }
      case 'adddav':
        $('dvUser').value = S.dav.some(k => k.username === 'obsidian') ? '' : 'obsidian'; $('dvDir').value = 'Obsidian'; $('dvWhere').value = 'space'; syncDavDlg();
        $('dvRO').checked = false; $('dvErr').textContent = ''; $('davDlg').showModal(); break;
      case 'deldav': {
        const k = S.dav.find(x => x.id === id);
        if(!k || !confirm(`移除账号“${k.username}”的访问密码？使用它的软件将无法再同步，已经同步过来的文件不受影响。`)) return;
        await api('DELETE', '/api/dav/' + id); if(S.davNew && S.davNew.username === k.username) S.davNew = null;
        toast('已移除访问密码'); await load(); break;
      }
      case 'addlink': openLinkDlg(null); break;
      case 'editlink': openLinkDlg(S.links.find(l => l.id === id)); break;
      case 'pin': {
        const l = S.links.find(x => x.id === id); if(!l) return;
        await api('PUT', '/api/links/' + id, Object.assign(linkBody(l), {pin:!l.pin})); toast(l.pin ? '已取消固定' : '已固定到侧栏');
        await loadLinks(); render(); break;
      }
      case 'enableproxy': {
        const l = S.links.find(x => x.id === id); if(!l) return;
        await api('PUT', '/api/links/' + id, Object.assign(linkBody(l), {proxy:true})); delete S.checks[id];
        toast('已开启代理内嵌'); await loadLinks(); render(); break;
      }
      case 'dellink': {
        const l = S.links.find(x => x.id === id); if(!l || !confirm(`移除链接“${l.name}”？`)) return;
        await api('DELETE', '/api/links/' + id); toast('已移除链接');
        if(S.link === id) S.link = null;
        await loadLinks(); render(); break;
      }
      case 'checklink': {
        const l = S.links.find(x => x.id === id); if(!l) return;
        S.checks[id] = {pending:true}; showCheck(l);
        try { S.checks[id] = await api('GET', `/api/links/${id}/check`); } catch(e){ S.checks[id] = {reachable:false, error:e.message}; }
        showCheck(S.links.find(x => x.id === id) || l); break;
      }
    }
  } catch(e){ toast(e.message); }
}

document.addEventListener('click', e => {
  const t = e.target;
  const closer = t.closest('[data-close]'); if(closer){ closer.closest('dialog').close(); return; }
  const lk = t.closest('[data-link]'); if(lk){ navTo({nav:'ext', link:lk.dataset.link}); return; }
  const nav = t.closest('[data-nav]'); if(nav){ navTo({nav:nav.dataset.nav}); return; }
  const bd = t.closest('[data-bdir]'); if(bd){ browseDir(bd.dataset.bdir); return; }
  const scd = t.closest('[data-scd]'); if(scd){ go({cwd:scd.dataset.scd, q:''}); return; }
  const so = t.closest('[data-space]'); if(so){ closeSide(); go({nav:'dav', space:so.dataset.space, cwd:'', q:'', davNew:null}); return; }
  const cd = t.closest('[data-cd]'); if(cd){ navTo({nav:'files', cwd:cd.dataset.cd}); return; }
  const dcd = t.closest('[data-dcd]'); if(dcd){ go({cwd:dcd.dataset.dcd, q:''}); return; }
  const dk = t.closest('[data-disk]'); if(dk){ closeSide(); go({nav:'disks', disk:dk.dataset.disk, diskInfo:null, cwd:'', q:''}); return; }
  const mcd = t.closest('[data-mcd]'); if(mcd){ go({mpath:mcd.dataset.mcd}); return; }
  const mt = t.closest('[data-mount]'); if(mt){ go({nav:'mounts', mount:mt.dataset.mount, mpath:'', q:''}); return; }
  const cp = t.closest('[data-copy]'); if(cp){ copyText(cp.dataset.copy).then(done => toast(done ? '已复制' : '复制失败，请手动选中后复制')); return; }
  const pv = t.closest('[data-preview]'); if(pv){ const en = current().find(x => x.path === pv.dataset.preview); if(en) openViewer(en); return; }
  const a = t.closest('[data-act]'); if(a){ act(a.dataset.act, a); return; }
  const v = t.closest('[data-view]'); if(v){ S.view = v.dataset.view; render(); return; }
  const card = t.closest('[data-key]');
  if(card){
    const key = card.dataset.key, k = kind(), entry = current().find(x => x.path === key);
    if(!entry) return;
    if(t.closest('[data-ck]')){ S.sel.has(key) ? S.sel.delete(key) : S.sel.add(key); }
    else if(entry.dir && k === 'mount'){ go({mpath:entry.path}); return; }
    else if(entry.dir && k === 'disk'){ if(entry.cloud) navTo({nav:'files'}); else go({cwd:entry.path, q:''}); return; }
    else if(entry.dir && k === 'local'){ if(inSpace()) go({cwd:entry.path, q:''}); else navTo({nav:'files', cwd:entry.path}); return; }
    else if((k === 'local' || k === 'disk') && previewable(entry)){ openViewer(entry); return; }   // 图片、视频、音频：点开预览；要选中请点左上角的方框
    else if(S.sel.size === 1 && S.sel.has(key)) S.sel.clear();
    else S.sel = new Set([key]);
    render();
  }
});

document.addEventListener('change', async e => {
  if(e.target.id !== 'trashDays') return;
  const days = Number(e.target.value);
  const doomed = days > 0 ? S.trash.filter(t => Date.now() - Date.parse(t.deletedAt) >= days * 86400000).length : 0;
  if(doomed && !confirm(`回收站里有 ${doomed} 项已经删除超过 ${days} 天，改完后会立即被彻底删除，无法恢复。确定吗？`)){
    e.target.value = String(S.trashDays);
    return;
  }
  try {
    const r = await api('PUT', '/api/settings', {trashDays:days});
    toast(days > 0 ? `已设置：删除满 ${days} 天后自动清除` + (r.purged ? `，已清除 ${r.purged} 项` : '') : '已设置：回收站不会自动清除');
    S.sel.clear();
    await load();
    if(r.purged) refreshStorage();
  } catch(err){
    toast(err.message);
    e.target.value = String(S.trashDays);
  }
});

let searchTimer;
$('q').addEventListener('input', e => {
  clearTimeout(searchTimer);
  searchTimer = setTimeout(() => { S.q = e.target.value.trim(); S.sel.clear(); load(); }, 250);
});
$('menuBtn').addEventListener('click', () => { $('side').classList.add('open'); $('scrim').hidden = false; });
$('scrim').addEventListener('click', closeSide);
$('uploadBtn').addEventListener('click', () => $('filePick').click());
$('filePick').addEventListener('change', e => { queueUploads([...e.target.files]); e.target.value = ''; });
$('transferBtn').addEventListener('click', () => { $('transfer').hidden = !$('transfer').hidden; });
$('themeBtn').addEventListener('click', () => {
  const r = document.documentElement; r.dataset.theme = r.dataset.theme === 'dark' ? 'light' : 'dark';
  try { localStorage.setItem('mynas-theme', r.dataset.theme); } catch(e){ /* 无法保存时仅本次生效 */ }
});
$('logoutBtn').addEventListener('click', async () => { try { await api('POST', '/api/logout'); } catch(e){ /* 仍然回到登录页 */ } location.reload(); });

const mainEl = $('main');
mainEl.addEventListener('dragover', e => { if(e.dataTransfer && [...e.dataTransfer.types].includes('Files')){ e.preventDefault(); mainEl.classList.add('drag'); } });
mainEl.addEventListener('dragleave', () => mainEl.classList.remove('drag'));
mainEl.addEventListener('drop', e => { e.preventDefault(); mainEl.classList.remove('drag'); queueUploads([...e.dataTransfer.files]); });

/* ---------- 启动 ---------- */
let savedTheme = null;
try { savedTheme = localStorage.getItem('mynas-theme'); } catch(e){ /* 使用默认浅色 */ }
document.documentElement.dataset.theme = savedTheme === 'dark' ? 'dark' : 'light';
$('searchIc').innerHTML = icon('search'); $('upIc').innerHTML = icon('upload');
$('menuBtn').innerHTML = icon('list'); $('themeBtn').innerHTML = icon('moon'); $('logoutBtn').innerHTML = icon('logout');
$('vwClose').innerHTML = icon('x'); $('vwPrev').innerHTML = icon('left'); $('vwNext').innerHTML = icon('right'); $('vwDownload').innerHTML = icon('download');
renderTransfer();
boot();
