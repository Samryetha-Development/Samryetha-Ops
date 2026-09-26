// 外壳模板。设计原则（docs/architecture.md §9）：
//   - 组件由外壳渲染，插件只声明"是什么类型 + 什么数据"，因此风格永远统一
//   - 设计令牌集中在 :root，暗色自动生效，插件无法也无需自定义样式
//   - 无构建步骤、无外部依赖：单二进制自带 UI
package ui

const shellHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1,viewport-fit=cover">
<meta name="color-scheme" content="light dark">
<title>{{.Title}}</title>
<style>
/* ---- 设计令牌：外壳与所有插件组件的唯一视觉来源 ---- */
:root{
  --bg:#f7f8f8; --surface:#fff; --surface-2:#f1f3f4; --ink:#171a1c; --muted:#70777b;
  --faint:#a7adaf; --line:#e1e4e5; --accent:#597585; --accent-fill:#3d7dbf;
  --accent-soft:#e7eef1; --ok:#15803d; --ok-soft:#e7f4ec; --warn:#b45309;
  --warn-soft:#fbf1e2; --danger:#9a5555; --danger-soft:#f7ecec;
  --radius:14px; --shadow:0 1px 0 rgba(20,24,26,.03);
}
@media (prefers-color-scheme:dark){:root{
  --bg:#111416; --surface:#171b1e; --surface-2:#1c2124; --ink:#eceeef; --muted:#8f989d;
  --faint:#687075; --line:#292e31; --accent:#87a8b7; --accent-fill:#4a86cf;
  --accent-soft:#1d2a30; --ok:#4ade80; --ok-soft:#16281d; --warn:#fbbf24;
  --warn-soft:#2a2313; --danger:#d48686; --danger-soft:#2a1c1c; --shadow:none;
}}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--ink);font-size:14px;
  font-family:ui-sans-serif,-apple-system,BlinkMacSystemFont,"SF Pro Text",Inter,"Noto Sans SC","PingFang SC",sans-serif;
  -webkit-font-smoothing:antialiased}
a{color:var(--accent);text-decoration:none}
button,input,select,textarea{font:inherit}
.shell{width:min(calc(100% - 32px),1080px);margin:0 auto}
.topbar{position:sticky;top:0;z-index:20;border-bottom:1px solid var(--line);
  background:color-mix(in srgb,var(--bg) 86%,transparent);backdrop-filter:saturate(140%) blur(18px)}
.tb{height:58px;display:flex;align-items:center;justify-content:space-between;gap:16px}
.brand{display:flex;align-items:baseline;gap:9px;font-weight:680;letter-spacing:-.01em}
.brand small{color:var(--muted);font-weight:500;font-size:12px}
.tb-r{display:flex;align-items:center;gap:10px}
.pill{display:inline-flex;align-items:center;gap:6px;height:22px;padding:0 9px;border-radius:999px;
  border:1px solid var(--line);font-size:11px;font-weight:600;color:var(--muted);white-space:nowrap}
.pill.ok{color:var(--ok);border-color:color-mix(in srgb,var(--ok) 32%,var(--line));background:var(--ok-soft)}
.pill.warn{color:var(--warn);border-color:color-mix(in srgb,var(--warn) 32%,var(--line));background:var(--warn-soft)}
.pill.err{color:var(--danger);border-color:color-mix(in srgb,var(--danger) 32%,var(--line));background:var(--danger-soft)}
main{padding:26px 0 90px;display:flex;flex-direction:column;gap:18px}
.card{border:1px solid var(--line);border-radius:var(--radius);background:var(--surface);
  padding:20px;box-shadow:var(--shadow);margin-bottom:18px}
.card-h{display:flex;align-items:center;justify-content:space-between;gap:12px;margin-bottom:16px}
.card-h h2{margin:0;font-size:15px;font-weight:640;letter-spacing:-.01em}
.grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(200px,1fr));gap:12px}
.stat{border:1px solid var(--line);border-radius:12px;padding:14px 15px;background:var(--surface-2)}
.stat .k{color:var(--muted);font-size:11.5px;font-weight:550}
.stat .v{font-size:19px;font-weight:680;margin-top:6px;font-variant-numeric:tabular-nums;overflow-wrap:anywhere}
.btn{display:inline-flex;align-items:center;gap:7px;border:1px solid var(--line);background:var(--surface);
  color:var(--ink);border-radius:9px;padding:8px 13px;font-size:13px;font-weight:550;cursor:pointer;transition:.14s}
.btn:hover{border-color:color-mix(in srgb,var(--accent) 42%,var(--line));background:var(--accent-soft)}
.btn.primary{background:var(--ink);color:var(--bg);border-color:transparent}
.btn.danger{color:var(--danger)}
.badge{display:inline-flex;align-items:center;height:20px;padding:0 8px;border-radius:999px;
  font-size:10.5px;font-weight:620;border:1px solid var(--line);color:var(--muted)}
.tone-ok{color:var(--ok)} .tone-warn{color:var(--warn)} .tone-err{color:var(--danger)} .tone-muted{color:var(--muted)}
table{width:100%;border-collapse:collapse;font-size:12.5px}
th{text-align:left;padding:8px 10px;color:var(--faint);font-weight:600;font-size:11px;
  text-transform:uppercase;letter-spacing:.04em;border-bottom:1px solid var(--line)}
td{padding:9px 10px;border-bottom:1px solid var(--line);vertical-align:top}
.list{display:flex;flex-direction:column}
.li{display:flex;align-items:center;justify-content:space-between;gap:12px;padding:11px 0;border-bottom:1px solid var(--line)}
.li:last-child{border-bottom:0}
pre.log{margin:0;max-height:420px;overflow:auto;background:var(--surface-2);border:1px solid var(--line);
  border-radius:11px;padding:14px;font-size:12px;line-height:1.6;white-space:pre-wrap;word-break:break-word;
  font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
.spark{display:flex;align-items:flex-end;gap:2px;height:40px}
.spark i{flex:1;background:color-mix(in srgb,var(--accent) 55%,transparent);border-radius:2px 2px 0 0;min-height:2px}
.kv{display:flex;justify-content:space-between;gap:12px;padding:9px 0;border-bottom:1px solid var(--line)}
.kv:last-child{border-bottom:0}
.kv .k{color:var(--muted);font-size:12px}
.kv .v{font-size:12.5px;font-variant-numeric:tabular-nums;text-align:right}
.timeline{display:flex;flex-direction:column;gap:10px}
.tl{display:flex;gap:12px;align-items:flex-start}
.tl .dot{width:8px;height:8px;border-radius:999px;background:var(--accent);margin-top:6px;flex:0 0 auto}
.tl .body{min-width:0}
.tl .when{color:var(--faint);font-size:11px;font-variant-numeric:tabular-nums}
textarea,input[type=text],input[type=password],input[type=number],select{
  width:100%;border:1px solid var(--line);border-radius:9px;background:var(--surface);
  color:var(--ink);padding:8px 10px;font-size:13px}
.field{display:flex;flex-direction:column;gap:6px;margin-bottom:12px}
.field label{color:var(--muted);font-size:11.5px;font-weight:600}
.field .help{color:var(--faint);font-size:11px}
.rescue{border:1px solid color-mix(in srgb,var(--danger) 40%,var(--line));background:var(--danger-soft);
  border-radius:var(--radius);padding:20px;margin-bottom:18px}
.rescue h2{margin:0 0 8px;color:var(--danger);font-size:16px}
.muted{color:var(--muted);font-size:12px}
.footer{border-top:1px solid var(--line);padding:16px 0;color:var(--faint);font-size:11.5px;
  display:flex;justify-content:space-between;gap:12px;flex-wrap:wrap}
#toast{position:fixed;right:18px;bottom:18px;display:flex;flex-direction:column;gap:9px;z-index:60;max-width:340px}
#toast .t{border:1px solid var(--line);background:var(--surface);border-radius:11px;padding:11px 15px;
  font-size:12.5px;font-weight:550;box-shadow:0 12px 32px rgba(0,0,0,.18)}
#toast .t.err{color:var(--danger)} #toast .t.ok{color:var(--ok)}
@media (max-width:680px){.tb-r .hide-sm{display:none}.card{padding:16px}}
</style>
</head>
<body>
<header class="topbar"><div class="shell tb">
  <div class="brand">Samryetha <small>control</small></div>
  <div class="tb-r">
    <span class="pill" id="live">连接中</span>
    {{if .Email}}<span class="muted hide-sm">{{.Email}}</span>{{end}}
  </div>
</div></header>

<main class="shell">
  {{if .Rescue}}
  <div class="rescue">
    <h2>⚠ 救援模式</h2>
    <p class="muted" style="margin:0 0 10px">内核存活，服务未加载。原因：{{.Rescue.Reason}}（阶段 {{.Rescue.Phase}}）</p>
    {{range .Rescue.Notes}}<div class="muted">· {{.}}</div>{{end}}
  </div>
  {{end}}

  {{with index .Slots "overview.cards"}}{{if nonempty .}}
  <section class="card">
    <div class="card-h"><h2>概览</h2><span class="muted" id="kernel-meta"></span></div>
    <div class="grid">
      {{range .}}
      <div class="stat">
        <div class="k">{{.Title}}{{if .Source}} <span class="badge">{{.Source}}</span>{{end}}</div>
        <div class="v {{toneClass .Tone}}">{{.Value}}</div>
      </div>
      {{end}}
    </div>
  </section>
  {{end}}{{end}}

  {{with index .Slots "actions"}}{{if nonempty .}}
  <section class="card">
    <div class="card-h"><h2>操作</h2></div>
    <div style="display:flex;gap:10px;flex-wrap:wrap">
      {{range .}}
        {{if eq .Kind "button"}}
        <button class="btn {{if eq .Tone "err"}}danger{{end}} {{if eq .Tone "ok"}}primary{{end}}"
          data-action="{{json .Action}}" data-confirm="{{.Confirm}}">{{.Label}}</button>
        {{else if eq .Kind "text"}}<span class="muted">{{.Text}}</span>{{end}}
      {{end}}
    </div>
  </section>
  {{end}}{{end}}

  {{with index .Slots "settings.sections"}}{{if nonempty .}}
  <section class="card">
    <div class="card-h"><h2>设置</h2></div>
    {{range .}}
      <h3 style="font-size:13px;margin:14px 0 10px">{{.Title}}</h3>
      {{range .Fields}}
      <div class="field">
        <label>{{.Label}}</label>
        {{if eq .Type "bool"}}
          <select data-field="{{.Key}}"><option value="true"{{if eq .Value "true"}} selected{{end}}>on</option><option value="false"{{if eq .Value "false"}} selected{{end}}>off</option></select>
        {{else if eq .Type "enum"}}
          <select data-field="{{.Key}}">{{range .Enum}}<option value="{{.}}"{{if eq . $.Value}} selected{{end}}>{{.}}</option>{{end}}</select>
        {{else if eq .Type "textarea"}}
          <textarea data-field="{{.Key}}" rows="3">{{.Value}}</textarea>
        {{else}}
          <input type="{{if eq .Type "secret"}}password{{else if eq .Type "int"}}number{{else}}text{{end}}" data-field="{{.Key}}" value="{{.Value}}">
        {{end}}
        {{if .Help}}<span class="help">{{.Help}}</span>{{end}}
      </div>
      {{end}}
    {{end}}
  </section>
  {{end}}{{end}}

  {{with index .Slots "logs.sources"}}{{if nonempty .}}
  <section class="card">
    <div class="card-h"><h2>日志</h2>
      <div style="display:flex;gap:8px">
        <select id="log-src">{{range .}}<option value="{{.ID}}">{{.Label}}</option>{{end}}</select>
        <button class="btn" id="log-refresh">刷新</button>
      </div>
    </div>
    <pre class="log" id="log-view">选择来源后加载…</pre>
  </section>
  {{end}}{{end}}

</main>

<footer class="shell footer">
  <span>内核 {{with .KernelMeta}}{{index . "syscallVersion"}}{{end}} · 已加载服务：{{range .Sources}}{{.}} {{end}}</span>
  <span>{{with .KernelMeta}}{{index . "note"}}{{end}}</span>
</footer>

<div id="toast"></div>
<script>
const CSRF = {{json .CSRF}};
function toast(msg, kind){const b=document.getElementById('toast');const e=document.createElement('div');
  e.className='t '+(kind||'');e.textContent=msg;b.appendChild(e);setTimeout(()=>e.remove(),3600);}
async function kernelCall(name,args){
  const r=await fetch('/api/kernel/call',{method:'POST',headers:{'Content-Type':'application/json','X-CSRF':CSRF},
    body:JSON.stringify({name,args:args||{},plugin:'ui'})});
  const d=await r.json();
  if(!d.ok) throw new Error((d.error&&d.error.message)||'call failed');
  return d.data;
}
document.addEventListener('click',async ev=>{
  const btn=ev.target.closest('button[data-action]'); if(!btn) return;
  let act={}; try{act=JSON.parse(btn.dataset.action||'{}')}catch(_){}
  const confirmText=btn.dataset.confirm||act.confirm||'';
  if(confirmText && !confirm(confirmText)) return;
  try{
    if(act.kind==='link'){ location.href=act.target; return; }
    if(act.kind==='api'){
      const r=await fetch(act.target,{method:act.method||'POST',headers:{'Content-Type':'application/json','X-CSRF':CSRF},
        body:act.body||undefined});
      if(!r.ok) throw new Error('HTTP '+r.status);
    } else if(act.kind==='call'){
      const parsed=act.body?JSON.parse(act.body):{};
      await kernelCall(act.target, parsed);
    }
    toast('已执行', 'ok'); setTimeout(()=>location.reload(), 800);
  }catch(e){ toast('失败：'+e.message,'err'); }
});
async function loadLog(){
  const src=document.getElementById('log-src'); if(!src) return;
  const r=await fetch('/api/kernel/log?source='+encodeURIComponent(src.value));
  const d=await r.json();
  const el=document.getElementById('log-view');
  el.textContent=(d.items||[]).map(x=>x.msg).join('\n')||'(空)';
  el.scrollTop=el.scrollHeight;
}
document.getElementById('log-refresh')?.addEventListener('click',loadLog);
document.getElementById('log-src')?.addEventListener('change',loadLog);
fetch('/api/kernel/meta').then(r=>r.json()).then(m=>{
  const el=document.getElementById('kernel-meta'); if(el) el.textContent=(m.syscalls||[]).length+' syscalls';
}).catch(()=>{});
</script>
</body>
</html>
`
