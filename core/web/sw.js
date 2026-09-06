const CACHE='filedrop-v1';
const ASSETS=['/','/app.js','/manifest.json'];
self.addEventListener('install', e=>{
  e.waitUntil(caches.open(CACHE).then(c=>c.addAll(ASSETS)).then(()=>self.skipWaiting()));
});
self.addEventListener('activate', e=>{
  e.waitUntil(caches.keys().then(ks=>Promise.all(ks.filter(k=>k!==CACHE).map(k=>caches.delete(k)))).then(()=>self.clients.claim()));
});
self.addEventListener('fetch', e=>{
  const url=new URL(e.request.url);
  if(url.pathname.startsWith('/api/')){
    // API 网络优先，失败回退缓存（仅 GET）
    if(e.request.method!=='GET'){ return; }
    e.respondWith(fetch(e.request).then(r=>{
      const clone=r.clone(); caches.open(CACHE).then(c=>c.put(e.request, clone)); return r;
    }).catch(()=>caches.match(e.request)));
    return;
  }
  e.respondWith(caches.match(e.request).then(cached=> cached || fetch(e.request).then(r=>{
    const clone=r.clone(); caches.open(CACHE).then(c=>c.put(e.request, clone)); return r;
  })));
});
