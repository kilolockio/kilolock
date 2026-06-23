package main

const controlUI = `<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Kilolock Control</title>
<style>
body{font-family:system-ui,sans-serif;max-width:1180px;margin:20px auto;padding:0 12px;background:#fafafa;color:#1b1f23}
h1{margin:0 0 12px}
h3{margin:0 0 10px}
section{border:1px solid #ddd;border-radius:10px;padding:12px;margin:10px 0;background:#fff}
input,button,select{padding:6px 8px;margin:4px}
button{cursor:pointer}
table{border-collapse:collapse;width:100%}
td,th{border-bottom:1px solid #eee;padding:6px;text-align:left;font-size:14px;vertical-align:top}
pre{background:#111;color:#cfe;padding:8px;border-radius:6px;overflow:auto}
.muted{color:#5f6b76;font-size:13px}
.note{background:#f4f8ff;border:1px solid #d8e6ff}
.msg{padding:10px 12px;border-radius:8px;margin:0 0 12px;display:none}
.msg.ok{display:block;background:#ecfdf3;border:1px solid #b7ebc6;color:#166534}
.msg.err{display:block;background:#fff1f2;border:1px solid #fecdd3;color:#9f1239}
.pill{display:inline-block;padding:2px 8px;border-radius:999px;font-size:12px;background:#eef2ff}
.actions button{margin-right:6px}
</style></head><body>
<h1>Kilolock Control</h1>
<div id="msg" class="msg"></div>
<section class="note"><h3>Operator notes</h3>
<div class="muted">
- Billing is enforced per workspace.<br>
- Quotas count only managed Terraform resources.<br>
- Archived environments/states do not consume active customer quota.<br>
- Shared-host ownership transfer is supported in-product; dedicated/BYODB migration stays manual in MVP.
</div>
</section>
<section><h3>Auth</h3>
<input id="ctrlToken" placeholder="KL_CONTROL_TOKEN (Bearer token)" style="width:460px">
<button onclick="saveToken()">Use Token</button>
</section>
<section><h3>Create Workspace</h3>
<input id="newTenantSlug" placeholder="workspace label (optional)">
<input id="newTenantName" placeholder="workspace name (optional)">
<button onclick="createTenant()">Create Workspace</button>
<div class="muted">Creates an organization workspace with an auto-generated <code>ws_...</code> identifier. Provide label/name up front, or leave both empty to use a friendly fallback like <code>hungry_hippo</code>.</div>
</section>
<section><h3>Control Operators</h3>
<div>
<input id="opName" placeholder="operator email or name" style="width:260px">
<select id="opRole">
  <option value="support_readonly">support_readonly</option>
  <option value="support_admin">support_admin</option>
  <option value="security_admin">security_admin</option>
  <option value="tenant_admin">tenant_admin</option>
  <option value="billing_admin">billing_admin</option>
  <option value="provisioner">provisioner</option>
  <option value="platform_admin">platform_admin</option>
</select>
<select id="opScopeKind">
  <option value="global">global</option>
  <option value="tenant">tenant</option>
  <option value="environment">environment</option>
</select>
<input id="opScopeRef" placeholder="scope ref: workspace_id or workspace_id/env">
<input id="opGrantedBy" placeholder="granted by">
<button onclick="createOperatorToken()">Create Operator Token</button>
<label><input id="opIncludeInactive" type="checkbox" checked>include inactive</label>
<button onclick="loadOperatorTokens()">Refresh</button>
</div>
<div class="muted">
- Creates a control-login token with a scoped role; the raw token secret is shown only once below.<br>
- Suspended operator tokens stay visible here by default so you can recover them quickly.<br>
- Role guide: <span class="pill">support_readonly</span> read only, <span class="pill">support_admin</span> recover/archive state and environments, <span class="pill">security_admin</span> manage tokens and RBAC, <span class="pill">tenant_admin</span> tenant-scoped admin, <span class="pill">billing_admin</span> billing only, <span class="pill">provisioner</span> provisioning only, <span class="pill">platform_admin</span> full access.<br>
- Scope ref: leave blank for global, use <code>workspace_id</code> for tenant scope, or <code>workspace_id/environment_label</code> for environment scope.
</div>
<table id="operatorTokens"></table>
<div class="muted">Last created operator token (shown once):</div>
<pre id="operatorCreateOut"></pre>
<pre id="operatorOut"></pre>
</section>
<section><h3>Workspace Entitlements</h3>
<input id="entTenant" placeholder="workspace_id">
<input id="entPlan" placeholder="billing plan" value="starter">
<input id="entMaxEnv" type="number" min="0" value="1" placeholder="max envs">
<input id="entMaxStateResources" type="number" min="0" value="100" placeholder="soft state resources">
<input id="entMaxEnvironmentResources" type="number" min="0" value="500" placeholder="soft environment resources">
<input id="entActor" placeholder="actor">
<input id="entReason" placeholder="reason">
<button onclick="loadTenantEntitlements()">Load</button>
<button onclick="saveTenantEntitlements()">Save</button>
<div class="muted">Hard quota is computed as <span class="pill">soft + 50%</span>.</div>
<pre id="entOut"></pre>
</section>
<section><h3>Workspaces</h3><label><input id="tenantIncludeInactive" type="checkbox">include inactive</label><button onclick="loadTenants()">Refresh</button><table id="tenants"></table></section>
<section><h3>Display Workspace Members</h3>
<input id="membersTenant" placeholder="workspace_id"><button onclick="loadWorkspaceMembers()">Load Members</button>
<table id="workspaceMembers"></table>
</section>
<section><h3>Create Environment</h3>
<input id="envTenant" placeholder="workspace_id (ws_...)"><input id="envSlug" placeholder="environment label"><input id="envInstance" placeholder="instance key (shared)">
<select id="envTier"><option value="shared_host">shared_host</option><option value="dedicated_host">dedicated_host</option></select>
<label><input id="envProvision" type="checkbox" checked>provision</label>
<button onclick="createEnv()">Create</button>
<div class="muted">Use the real <code>workspace_id</code> from the Workspaces table here, not the human-friendly label.</div>
</section>
<section><h3>Environments by Workspace</h3>
<input id="listEnvTenant" placeholder="workspace_id"><label><input id="listEnvIncludeInactive" type="checkbox">include inactive</label><button onclick="loadTenantEnvs()">Load Environments</button>
<table id="tenantEnvs"></table>
</section>
<section><h3>Display Environment PAT Access</h3>
<input id="patAccessEnvID" placeholder="env_public_id"><button onclick="loadEnvironmentPATAccess()">Load PAT Access</button>
<table id="environmentPATAccess"></table>
</section>
<section><h3>Create Token</h3>
<input id="tokTenant" placeholder="workspace_id"><input id="tokEnv" placeholder="env id (or label)"><input id="tokName" placeholder="token name">
<button onclick="createToken()">Create</button>
<pre id="tokenOut"></pre>
</section>
<section><h3>Tokens by Workspace</h3>
<input id="listTokTenant" placeholder="workspace_id"><input id="listTokEnv" placeholder="optional env id or label"><label><input id="listTokIncludeInactive" type="checkbox">include inactive</label><button onclick="loadTenantTokens()">Load Tokens</button>
<table id="tenantTokens"></table>
</section>
<section><h3>Filters</h3>
<input id="fltSearch" placeholder="search text (slug/name/id)" style="width:260px">
<select id="fltLifecycle"><option value="">any lifecycle</option><option value="active">active</option><option value="suspended">suspended</option><option value="archived">archived</option></select>
<select id="fltTier"><option value="">any tier</option><option value="shared_host">shared_host</option><option value="dedicated_host">dedicated_host</option></select>
<input id="fltLimit" type="number" min="1" max="1000" value="200" style="width:90px" title="limit">
<button onclick="applyFilters()">Apply Filters</button>
<button onclick="clearFilters()">Clear</button>
</section>
<section><h3>Lifecycle</h3>
<label><input id="includeInactive" type="checkbox">include inactive</label>
<div>
<input id="lifeTenant" placeholder="workspace_id">
<select id="lifeTenantStatus"><option value="active">active</option><option value="suspended">suspended</option><option value="archived">archived</option></select>
<button onclick="setTenantLifecycle()">Set Workspace</button>
</div>
<div>
<input id="lifeEnvTenant" placeholder="workspace_id"><input id="lifeEnvSlug" placeholder="environment label">
<select id="lifeEnvStatus"><option value="active">active</option><option value="suspended">suspended</option><option value="archived">archived</option></select>
<button onclick="setEnvLifecycle()">Set Environment</button>
</div>
<div>
<input id="lifeTokenID" placeholder="token id">
<select id="lifeTokenStatus"><option value="active">active</option><option value="suspended">suspended</option><option value="archived">archived</option><option value="deleted">deleted</option></select>
<button onclick="setTokenLifecycle()">Set Token</button>
</div>
<div class="muted">Token status <span class="pill">deleted</span> performs a hard delete.</div>
</section>
<section><h3>Inspect Environment States</h3>
<input id="statesTenant" placeholder="workspace_id"><input id="statesEnv" placeholder="environment label"><label><input id="statesIncludeInactive" type="checkbox" checked>include inactive</label><button onclick="loadStates()">Load</button>
<div class="muted">Inactive states are shown by default here so support can recover or reactivate them directly from the table.</div>
<table id="statesTable"></table>
<pre id="statesOut"></pre>
</section>
<section><h3>State Policy</h3>
<input id="cfgTenant" placeholder="workspace_id">
<input id="cfgEnv" placeholder="environment label">
<input id="cfgState" placeholder="state name">
<label><input id="cfgExclusive" type="checkbox">exclusive locks</label>
<select id="cfgCoexistence"><option value="warn">warn</option><option value="strict">strict</option></select>
<button onclick="saveStatePolicy()">Save policy</button>
<pre id="cfgOut"></pre>
</section>
<section><h3>State Lifecycle / Support</h3>
<input id="stateLifeTenant" placeholder="workspace_id">
<input id="stateLifeEnv" placeholder="environment label">
<input id="stateLifeName" placeholder="full state name">
<select id="stateLifeStatus"><option value="archived">archived</option><option value="active">active</option><option value="suspended">suspended</option></select>
<input id="stateLifeActor" placeholder="actor">
<input id="stateLifeReason" placeholder="reason">
<button onclick="setStateLifecycle()">Set State</button>
<div class="muted">Use this for support restore/archive actions. Restoring requires the exact stored state name.</div>
</section>
<section><h3>Ownership Transfers</h3>
<div>
<input id="xferTenant" placeholder="filter workspace_id">
<select id="xferStatus"><option value="">any status</option><option value="pending">pending</option><option value="accepted">accepted</option><option value="rejected">rejected</option><option value="cancelled">cancelled</option></select>
<button onclick="loadTransfers()">Load</button>
</div>
<div>
<input id="xferSourceTenant" placeholder="source workspace_id">
<input id="xferEnvSlug" placeholder="environment label">
<input id="xferTargetTenant" placeholder="target workspace_id">
<input id="xferActor" placeholder="actor">
<input id="xferReason" placeholder="reason">
<button onclick="createTransfer()">Propose</button>
</div>
<table id="transfers"></table>
<pre id="transfersOut"></pre>
</section>
<section><h3>Retention Purge (Archived Tenants)</h3>
<input id="retHours" type="number" min="1" value="720" placeholder="older than hours">
<input id="retTenant" placeholder="optional workspace_id">
<input id="retReason" placeholder="reason (required for apply)">
<button onclick="previewRetention()">Preview (dry-run)</button>
<button onclick="applyRetention()">Purge (--apply)</button>
<pre id="retOut"></pre>
</section>
<section><h3>RBAC Grants</h3>
<label><input id="rbacIncludeRevoked" type="checkbox">include revoked</label>
<button onclick="loadRBACGrants()">Refresh Grants</button>
<table id="rbacGrants"></table>
<div>
<input id="rbacSubjectKind" placeholder="subject_kind (api_token)">
<input id="rbacSubjectID" placeholder="subject_id (token id)">
<input id="rbacRoleKey" placeholder="role_key (platform_admin)">
<select id="rbacScopeKind"><option value="global">global</option><option value="tenant">tenant</option><option value="environment">environment</option></select>
<input id="rbacScopeRef" placeholder="scope_ref (workspace_id or workspace/env)">
<input id="rbacGrantedBy" placeholder="granted_by">
<button onclick="grantRole()">Grant Role</button>
</div>
<div>
<button onclick="tplRole('platform_admin')">Template: platform_admin</button>
<button onclick="tplRole('tenant_admin')">Template: tenant_admin</button>
<button onclick="tplRole('provisioner')">Template: provisioner</button>
<button onclick="tplRole('support_readonly')">Template: support_readonly</button>
</div>
<div>
<input id="rbacGrantID" placeholder="grant id">
<input id="rbacRevokedBy" placeholder="revoked_by">
<button onclick="revokeRole()">Revoke Grant</button>
</div>
</section>
<script>
function token(){return (localStorage.getItem('kl_control_token')||'').trim()}
function setMsg(kind,text){
  const el=document.getElementById('msg')
  el.className='msg '+kind
  el.textContent=text||''
}
function clearMsg(){const el=document.getElementById('msg');el.className='msg';el.textContent=''}
function escapeHtml(v){return String(v??'').replaceAll('&','&amp;').replaceAll('<','&lt;').replaceAll('>','&gt;').replaceAll('"','&quot;').replaceAll("'","&#39;")}
function saveToken(){localStorage.setItem('kl_control_token',(ctrlToken.value||'').trim());setMsg('ok','Control token saved in browser localStorage.')}
const controlAPIPrefix='/v1/api'
async function j(url, opts={}){
  if(url==='/api' || String(url||'').startsWith('/api/')){
    url=controlAPIPrefix+String(url).slice(4)
  }
  const h={'content-type':'application/json',...(opts.headers||{})}
  const t=token(); if(t) h['Authorization']='Bearer '+t
  const r=await fetch(url,{...opts,headers:h})
  const b=await r.text();let o={};try{o=JSON.parse(b)}catch{};
  if(!r.ok)throw new Error((o.error||b||r.statusText));return o
}
function row(cells){return '<tr>'+cells.map(c=>'<td>'+String(c??'')+'</td>').join('')+'</tr>'}
function pascalFromKey(k){
  return String(k||'')
    .split('_')
    .filter(Boolean)
    .map(part=>part.charAt(0).toUpperCase()+part.slice(1))
    .join('')
}
function goFieldFromKey(k){
  return pascalFromKey(k)
    .replaceAll('Id','ID')
    .replaceAll('Api','API')
    .replaceAll('Dsn','DSN')
    .replaceAll('Url','URL')
    .replaceAll('Oidc','OIDC')
    .replaceAll('Tf','TF')
    .replaceAll('Rbac','RBAC')
    .replaceAll('Iac','IAC')
}
function val(v,k){return v?.[k]??v?.[k[0].toUpperCase()+k.slice(1)]??v?.[pascalFromKey(k)]??v?.[goFieldFromKey(k)]??''}
function copyId(v){navigator.clipboard.writeText(String(v||''));}
function q(v){return "'"+String(v??'').replaceAll('\\','\\\\').replaceAll("'","\\'")+"'";}
function flt(){return {
  q:(fltSearch.value||'').toLowerCase().trim(),
  life:(fltLifecycle.value||'').trim(),
  tier:(fltTier.value||'').trim(),
  limit:Math.max(1, Math.min(1000, Number(fltLimit.value||200))),
  offset:0
}}
function qFrom(obj){
  const p=new URLSearchParams()
  Object.entries(obj||{}).forEach(([k,v])=>{
    if(v===undefined||v===null||String(v)==='') return
    p.set(k,String(v))
  })
  const s=p.toString()
  return s?('?'+s):''
}
const workspaceNameAdjectives=['hungry','curious','brave','gentle','sleepy','speedy','clever','bright','calm','lucky','sunny','swift']
const workspaceNameAnimals=['hippo','otter','falcon','panda','badger','fox','koala','lynx','owl','tiger','whale','yak']
function titleizeWords(v){return String(v||'').split(/[_\s-]+/).filter(Boolean).map(part=>part.charAt(0).toUpperCase()+part.slice(1)).join(' ')}
function randomWorkspaceLabel(){
  const a=workspaceNameAdjectives[Math.floor(Math.random()*workspaceNameAdjectives.length)]
  const b=workspaceNameAnimals[Math.floor(Math.random()*workspaceNameAnimals.length)]
  return a+'_'+b
}
function requireWorkspaceID(value, label){
  const workspaceID=String(value||'').trim()
  if(!workspaceID){
    alert(label+' workspace_id is required')
    return ''
  }
  if(!/^ws_[a-z0-9]+$/i.test(workspaceID)){
    alert(label+' expects a workspace_id like ws_52fb8ea1ef3a, not the workspace label')
    return ''
  }
  return workspaceID
}
function fmtTs(v){return v?String(v):''}
async function runAction(label, fn){
  clearMsg()
  try{
    const res=await fn()
    setMsg('ok', label+' succeeded.')
    return res
  }catch(err){
    setMsg('err', label+' failed: '+(err?.message||String(err)))
    throw err
  }
}
let tenantsCache=[], envsCache=[]
function workspaceActions(v){
  const workspaceID=String(val(v,'workspace_id')||'')
  const status=String(val(v,'lifecycle_status')||'active')
  const btns=[]
  if(status==='archived'){
    btns.push('<button onclick="changeWorkspaceLifecycle('+q(workspaceID)+',\'active\')">Recover</button>')
  }else if(status==='suspended'){
    btns.push('<button onclick="changeWorkspaceLifecycle('+q(workspaceID)+',\'active\')">Activate</button>')
    btns.push('<button onclick="changeWorkspaceLifecycle('+q(workspaceID)+',\'archived\')">Archive</button>')
  }else{
    btns.push('<button onclick="changeWorkspaceLifecycle('+q(workspaceID)+',\'suspended\')">Suspend</button>')
    btns.push('<button onclick="changeWorkspaceLifecycle('+q(workspaceID)+',\'archived\')">Archive</button>')
  }
  btns.push('<button onclick="useWorkspace('+q(workspaceID)+')">Use</button>')
  btns.push('<button onclick="deleteWorkspace('+q(workspaceID)+')">Delete</button>')
  return '<span class="actions">'+btns.join('')+'</span>'
}
function operatorTokenActions(v){
  const id=String(val(v,'token_id')||'')
  const status=String(val(v,'lifecycle_status')||'active')
  const btns=[]
  if(status==='suspended'){
    btns.push('<button onclick="setOperatorTokenStatus('+q(id)+',\'active\')">Recover</button>')
  }else{
    btns.push('<button onclick="setOperatorTokenStatus('+q(id)+',\'suspended\')">Suspend</button>')
  }
  btns.push('<button onclick="setOperatorTokenStatus('+q(id)+',\'deleted\')">Delete</button>')
  return '<span class="actions">'+btns.join('')+'</span>'
}
function environmentActions(v){
  const tenant=(listEnvTenant.value||'').trim()
  const slug=String(val(v,'slug')||'')
  const envID=String(val(v,'env_public_id')||'')
  const status=String(val(v,'lifecycle_status')||'active')
  const btns=[]
  if(status==='archived'){
    btns.push('<button onclick="changeEnvironmentLifecycle('+q(tenant)+','+q(slug)+',\'active\')">Recover</button>')
  }else if(status==='suspended'){
    btns.push('<button onclick="changeEnvironmentLifecycle('+q(tenant)+','+q(slug)+',\'active\')">Activate</button>')
    btns.push('<button onclick="changeEnvironmentLifecycle('+q(tenant)+','+q(slug)+',\'archived\')">Archive</button>')
  }else{
    btns.push('<button onclick="changeEnvironmentLifecycle('+q(tenant)+','+q(slug)+',\'suspended\')">Suspend</button>')
    btns.push('<button onclick="changeEnvironmentLifecycle('+q(tenant)+','+q(slug)+',\'archived\')">Archive</button>')
  }
  btns.push('<button onclick="useEnvironmentID('+q(envID)+')">Use env id</button>')
  btns.push('<button onclick="deleteEnvironment('+q(tenant)+','+q(slug)+')">Delete</button>')
  return '<span class="actions">'+btns.join('')+'</span>'
}
function stateActions(tenant, env, v){
  const name=String(val(v,'name')||'')
  const status=String(val(v,'lifecycle_status')||'active')
  const btns=[]
  if(status==='archived'){
    btns.push('<button onclick="changeStateLifecycle('+q(tenant)+','+q(env)+','+q(name)+',\'active\')">Recover</button>')
  }else if(status==='suspended'){
    btns.push('<button onclick="changeStateLifecycle('+q(tenant)+','+q(env)+','+q(name)+',\'active\')">Activate</button>')
    btns.push('<button onclick="changeStateLifecycle('+q(tenant)+','+q(env)+','+q(name)+',\'archived\')">Archive</button>')
  }else{
    btns.push('<button onclick="changeStateLifecycle('+q(tenant)+','+q(env)+','+q(name)+',\'suspended\')">Suspend</button>')
    btns.push('<button onclick="changeStateLifecycle('+q(tenant)+','+q(env)+','+q(name)+',\'archived\')">Archive</button>')
  }
  btns.push('<button onclick="deleteState('+q(tenant)+','+q(env)+','+q(name)+')">Delete</button>')
  btns.push('<button onclick="copyId('+q(name)+')">copy name</button>')
  return '<span class="actions">'+btns.join('')+'</span>'
}
function renderTenants(){
  const rows=(tenantsCache||[]).sort((a,b)=>String(val(a,'slug')).localeCompare(String(val(b,'slug'))))
  const t=document.getElementById('tenants')
  t.innerHTML='<tr><th>workspace_id</th><th>label</th><th>name</th><th>kind</th><th>status</th><th>plan</th><th>max envs</th><th>soft/state</th><th>soft/env</th><th>changed_at</th><th>changed_by</th><th>reason</th><th>actions</th><th>id</th></tr>'+
    rows.map(v=>row([
      '<code>'+escapeHtml(val(v,'workspace_id'))+'</code> <button onclick="copyId(\''+String(val(v,'workspace_id')).replaceAll("'","&#39;")+'\')">copy</button>',
      escapeHtml(val(v,'slug')),
      escapeHtml(val(v,'name')),
      val(v,'kind'),
      val(v,'lifecycle_status'),
      val(v,'billing_plan'),
      val(v,'max_environments'),
      val(v,'max_state_resources'),
      val(v,'max_environment_resources'),
      fmtTs(val(v,'lifecycle_changed_at')),
      val(v,'lifecycle_changed_by'),
      escapeHtml(val(v,'lifecycle_reason')),
      workspaceActions(v),
      '<button onclick="copyId(\''+String(val(v,'id')).replaceAll("'","&#39;")+'\')">copy id</button>'
    ])).join('')
}
async function loadTenants(){
  const f=flt()
  const x=await runAction('Load workspaces',()=>j('/api/tenants'+qFrom({include_inactive:tenantIncludeInactive.checked?'true':'',q:f.q,lifecycle:f.life,limit:f.limit,offset:f.offset})))
  tenantsCache=x.tenants||[]
  renderTenants()
}
async function loadWorkspaceMembers(){
  const tenant=requireWorkspaceID(membersTenant.value,'Load workspace members')
  if(!tenant) return
  const x=await runAction('Load workspace members',()=>j('/api/tenants/'+tenant+'/members'))
  const rows=Array.isArray(x.members)?x.members:[]
  const t=document.getElementById('workspaceMembers')
  t.innerHTML='<tr><th>email</th><th>company</th><th>plan</th><th>role</th><th>has PAT</th><th>PAT last used</th><th>account id</th><th>created</th></tr>'+
    rows.map(v=>row([
      escapeHtml(val(v,'email')),
      escapeHtml(val(v,'company')),
      escapeHtml(val(v,'plan')),
      escapeHtml(val(v,'role')),
      String(val(v,'has_pat'))==='true'?'yes':'no',
      fmtTs(val(v,'pat_last_used_at')),
      '<button onclick="copyId(\''+String(val(v,'id')).replaceAll("'","&#39;")+'\')">copy id</button>',
      fmtTs(val(v,'created_at'))
    ])).join('')
}
async function createTenant(){
  let slug=(newTenantSlug.value||'').trim()
  let name=(newTenantName.value||'').trim()
  if(!slug && !name){
    slug=randomWorkspaceLabel()
    name=titleizeWords(slug)
  }else if(!slug && name){
    slug=String(name).trim().toLowerCase().replace(/[^a-z0-9]+/g,'_').replace(/^_+|_+$/g,'') || randomWorkspaceLabel()
  }else if(slug && !name){
    name=titleizeWords(slug)
  }
  await runAction('Create workspace',()=>j('/api/tenants',{method:'POST',body:JSON.stringify({slug,name})}))
  if(!newTenantSlug.value.trim()) newTenantSlug.value=slug
  if(!newTenantName.value.trim()) newTenantName.value=name
  loadTenants()
}
function useWorkspace(workspaceID){
  envTenant.value=workspaceID
  listEnvTenant.value=workspaceID
  listTokTenant.value=workspaceID
  membersTenant.value=workspaceID
  entTenant.value=workspaceID
  lifeTenant.value=workspaceID
  lifeEnvTenant.value=workspaceID
  tokTenant.value=workspaceID
  statesTenant.value=workspaceID
  cfgTenant.value=workspaceID
  stateLifeTenant.value=workspaceID
  xferTenant.value=workspaceID
  xferSourceTenant.value=workspaceID
}
function useEnvironmentID(envID){
  tokEnv.value=envID
  listTokEnv.value=envID
  patAccessEnvID.value=envID
}
async function loadOperatorTokens(){
  const x=await runAction('Load operator tokens',()=>j('/api/operators/tokens'+qFrom({include_inactive:opIncludeInactive.checked?'true':''})))
  const rows=Array.isArray(x.tokens)?x.tokens:[]
  const t=document.getElementById('operatorTokens')
  t.innerHTML='<tr><th>name</th><th>role</th><th>scope</th><th>status</th><th>prefix</th><th>last used</th><th>created</th><th>granted by</th><th>actions</th><th>id</th></tr>'+
    rows.map(v=>row([
      escapeHtml(val(v,'name')),
      escapeHtml(val(v,'role_key')),
      escapeHtml((val(v,'scope_kind')||'global')+(val(v,'scope_ref')?': '+val(v,'scope_ref'):'')),
      escapeHtml(val(v,'lifecycle_status')),
      escapeHtml(val(v,'token_prefix')),
      fmtTs(val(v,'last_used_at')),
      fmtTs(val(v,'created_at')),
      escapeHtml(val(v,'granted_by')),
      operatorTokenActions(v),
      '<button onclick="copyId(\''+String(val(v,'token_id')).replaceAll("'","&#39;")+'\')">copy id</button>'
    ])).join('')
  operatorOut.textContent=JSON.stringify(x,null,2)
}
async function createOperatorToken(){
  const x=await runAction('Create operator token',()=>j('/api/operators/tokens',{method:'POST',body:JSON.stringify({
    name:opName.value,
    role_key:opRole.value,
    scope_kind:opScopeKind.value,
    scope_ref:opScopeRef.value,
    granted_by:opGrantedBy.value
  })}))
  operatorCreateOut.textContent=JSON.stringify(x,null,2)
  if(x && x.token_secret){
    try{navigator.clipboard.writeText(String(x.token_secret))}catch{}
    setMsg('ok','Create operator token succeeded. The raw token was copied to clipboard and is shown once below — share it securely now.')
  }
  loadOperatorTokens()
}
async function loadTenantEntitlements(){
  const tenant=(entTenant.value||'').trim()
  if(!tenant){alert('workspace_id is required');return}
  const x=await runAction('Load entitlements',()=>j('/api/tenants/'+tenant))
  entPlan.value=String(val(x,'billing_plan')||'starter')
  entMaxEnv.value=String(val(x,'max_environments')||0)
  entMaxStateResources.value=String(val(x,'max_state_resources')||0)
  entMaxEnvironmentResources.value=String(val(x,'max_environment_resources')||500)
  entOut.textContent=JSON.stringify(x,null,2)
}
async function saveTenantEntitlements(){
  const tenant=(entTenant.value||'').trim()
  if(!tenant){alert('workspace_id is required');return}
  const x=await runAction('Save entitlements',()=>j('/api/tenants/entitlements',{method:'POST',body:JSON.stringify({
    slug:tenant,
    billing_plan:entPlan.value,
    max_environments:Number(entMaxEnv.value||0),
    max_state_resources:Number(entMaxStateResources.value||0),
    max_environment_resources:Number(entMaxEnvironmentResources.value||0),
    actor:entActor.value,
    reason:entReason.value
  })}))
  entOut.textContent=JSON.stringify(x,null,2)
  loadTenants()
}
async function createEnv(){
  const tenant=requireWorkspaceID(envTenant.value,'Create environment')
  if(!tenant) return
  await runAction('Create environment',()=>j('/api/tenants/'+tenant+'/environments',{method:'POST',body:JSON.stringify({slug:envSlug.value,instance:envInstance.value,tier:envTier.value,provision:envProvision.checked})}))
  listEnvTenant.value=tenant
  loadTenantEnvs()
}
async function loadTenantEnvs(){
  const tenant=requireWorkspaceID(listEnvTenant.value,'Load environments')
  if(!tenant) return
  const f=flt()
  const x=await runAction('Load environments',()=>j('/api/tenants/'+tenant+'/environments'+qFrom({include_inactive:listEnvIncludeInactive.checked?'true':'',q:f.q,lifecycle:f.life,tier:f.tier,limit:f.limit,offset:f.offset})))
  envsCache=x.environments||[]
  const rows=envsCache.sort((a,b)=>{
    const as=String(val(a,'status')), bs=String(val(b,'status'))
    if(as!==bs) return as.localeCompare(bs)
    return String(val(a,'slug')).localeCompare(String(val(b,'slug')))
  })
  const t=document.getElementById('tenantEnvs')
  t.innerHTML='<tr><th>label</th><th>env id</th><th>lifecycle</th><th>tier</th><th>status</th><th>instance</th><th>database</th><th>changed_at</th><th>changed_by</th><th>reason</th><th>actions</th><th>id</th></tr>' +
    rows.map(v=>row([
      escapeHtml(val(v,'slug')),
      '<code>'+escapeHtml(val(v,'env_public_id'))+'</code> <button onclick="copyId(\''+String(val(v,'env_public_id')).replaceAll("'","&#39;")+'\')">copy</button>',
      val(v,'lifecycle_status'),
      val(v,'tier'),
      val(v,'status'),
      val(v,'database_instance_key'),
      escapeHtml(val(v,'database_name')),
      fmtTs(val(v,'lifecycle_changed_at')),
      escapeHtml(val(v,'lifecycle_changed_by')),
      escapeHtml(val(v,'lifecycle_reason')),
      environmentActions(v),
      '<button onclick="copyId(\''+String(val(v,'id')).replaceAll("'","&#39;")+'\')">copy id</button>'
    ])).join('')
}
async function loadEnvironmentPATAccess(){
  const envID=(patAccessEnvID.value||'').trim()
  if(!envID){alert('env_public_id is required');return}
  const x=await runAction('Load environment PAT access',()=>j('/api/environments/access'+qFrom({env_id:envID})))
  const rows=Array.isArray(x.access)?x.access:[]
  const t=document.getElementById('environmentPATAccess')
  t.innerHTML='<tr><th>email</th><th>company</th><th>role</th><th>access</th><th>PAT prefix</th><th>PAT last used</th><th>granted by</th><th>grant created</th><th>account id</th></tr>'+
    rows.map(v=>row([
      escapeHtml(val(v,'email')),
      escapeHtml(val(v,'company')),
      escapeHtml(val(v,'role')),
      escapeHtml(val(v,'access_mode')),
      escapeHtml(val(v,'token_prefix')),
      fmtTs(val(v,'pat_last_used_at')),
      escapeHtml(val(v,'granted_by')),
      fmtTs(val(v,'grant_created_at')),
      '<button onclick="copyId(\''+String(val(v,'account_id')).replaceAll("'","&#39;")+'\')">copy id</button>'
    ])).join('')
}
async function loadTenantTokens(){
  const tenant=requireWorkspaceID(listTokTenant.value,'Load tokens')
  const env=(listTokEnv.value||'').trim()
  if(!tenant) return
  const x=await runAction('Load tokens',()=>j('/api/tenants/'+tenant+'/tokens'+qFrom({include_inactive:listTokIncludeInactive.checked?'true':''})))
  let rows=Array.isArray(x.tokens)?x.tokens:[]
  if(env){
    let envSlug=env
    const cached=(envsCache||[]).find(v=>String(val(v,'env_public_id')||'').trim()===env)
    if(cached) envSlug=String(val(cached,'slug')||'').trim()
    rows=rows.filter(v=>String(val(v,'env_slug')||'').trim()===envSlug)
  }
  const t=document.getElementById('tenantTokens')
  t.innerHTML='<tr><th>environment</th><th>name</th><th>status</th><th>prefix</th><th>last used</th><th>changed_at</th><th>changed_by</th><th>reason</th><th>id</th></tr>'+
    rows.map(v=>row([
      escapeHtml(val(v,'env_slug')),
      escapeHtml(val(v,'name')),
      escapeHtml(val(v,'lifecycle_status')),
      escapeHtml(val(v,'token_prefix')),
      fmtTs(val(v,'last_used_at')),
      fmtTs(val(v,'lifecycle_changed_at')),
      escapeHtml(val(v,'lifecycle_changed_by')),
      escapeHtml(val(v,'lifecycle_reason')),
      '<button onclick="copyId(\''+String(val(v,'id')).replaceAll("'","&#39;")+'\')">copy id</button>'
    ])).join('')
}
async function createToken(){const x=await runAction('Create token',()=>j('/api/tenants/'+tokTenant.value+'/tokens',{method:'POST',body:JSON.stringify({environment:tokEnv.value,name:tokName.value})}));tokenOut.textContent=JSON.stringify(x,null,2)}
async function loadStates(){
  const tenant=(statesTenant.value||'').trim()
  const env=(statesEnv.value||'').trim()
  const x=await runAction('Load states',()=>j('/api/states/'+tenant+'/'+env+qFrom({include_inactive:statesIncludeInactive.checked?'true':''})))
  const states=Array.isArray(x.states)?x.states:[]
  const t=document.getElementById('statesTable')
  t.innerHTML='<tr><th>state</th><th>lifecycle</th><th>serial</th><th>managed resources</th><th>locked</th><th>exclusive</th><th>coexistence</th><th>actions</th></tr>'+
    states.map(v=>row([
      '<code>'+escapeHtml(val(v,'name'))+'</code>',
      escapeHtml(val(v,'lifecycle_status')),
      val(v,'serial'),
      val(v,'resource_count'),
      String(val(v,'locked'))==='true'?'yes':'no',
      String(val(v,'exclusive_locks'))==='true'?'yes':'no',
      escapeHtml(val(v,'coexistence_mode')),
      stateActions(tenant, env, v)
    ])).join('')
  statesOut.textContent=JSON.stringify(x,null,2)
  if(states.length>0){
    const first=states[0]
    cfgTenant.value=tenant
    cfgEnv.value=env
    cfgState.value=String(val(first,'name')||'')
    cfgExclusive.checked=String(val(first,'exclusive_locks'))==='true'
    cfgCoexistence.value=String(val(first,'coexistence_mode')||'warn')
    stateLifeTenant.value=tenant
    stateLifeEnv.value=env
    stateLifeName.value=String(val(first,'name')||'')
  }
}
function prefillStateSupport(tenant, env, stateName, status){
  stateLifeTenant.value=tenant||''
  stateLifeEnv.value=env||''
  stateLifeName.value=stateName||''
  stateLifeStatus.value=status||'archived'
}
async function saveStatePolicy(){
  const tenant=(cfgTenant.value||'').trim()
  const env=(cfgEnv.value||'').trim()
  const state=(cfgState.value||'').trim()
  if(!tenant||!env||!state){alert('tenant, environment, and state are required');return}
  const x=await runAction('Save state policy',()=>j('/api/states/'+tenant+'/'+env+'/config',{method:'POST',body:JSON.stringify({
    state:state,
    exclusive_locks:cfgExclusive.checked,
    coexistence_mode:cfgCoexistence.value
  })}))
  cfgOut.textContent=JSON.stringify(x,null,2)
  if(statesTenant.value===tenant && statesEnv.value===env) loadStates()
}
async function setStateLifecycle(){
  const tenant=(stateLifeTenant.value||'').trim()
  const env=(stateLifeEnv.value||'').trim()
  const state=(stateLifeName.value||'').trim()
  if(!tenant||!env||!state){alert('workspace, environment, and state are required');return}
  await runAction('Set state lifecycle',()=>j('/api/states/'+tenant+'/'+env+'/lifecycle',{method:'POST',body:JSON.stringify({
    state,
    status:stateLifeStatus.value,
    actor:stateLifeActor.value,
    reason:stateLifeReason.value
  })}))
  if(statesTenant.value===tenant && statesEnv.value===env) loadStates()
}
async function changeWorkspaceLifecycle(slug,status){
  const reason=status==='active'?'support restore':'control ui action'
  await runAction('Set workspace lifecycle',()=>j('/api/tenants/lifecycle',{method:'POST',body:JSON.stringify({slug,status,reason})}))
  loadTenants()
}
async function deleteWorkspace(slug){
  if(!confirm('Delete workspace '+slug+' permanently?')) return
  await runAction('Delete workspace',()=>j('/api/tenants/delete',{method:'POST',body:JSON.stringify({slug,reason:'control ui delete'})}))
  if((listEnvTenant.value||'').trim()===slug) tenantEnvs.innerHTML=''
  loadTenants()
}
async function changeEnvironmentLifecycle(tenant,environment,status){
  const reason=status==='active'?'support restore':'control ui action'
  await runAction('Set environment lifecycle',()=>j('/api/tenants/'+tenant+'/environments/lifecycle',{method:'POST',body:JSON.stringify({environment,status,reason})}))
  if((listEnvTenant.value||'').trim()===tenant) loadTenantEnvs()
}
async function deleteEnvironment(tenant,environment){
  if(!confirm('Delete environment '+environment+' permanently from workspace '+tenant+'?')) return
  await runAction('Delete environment',()=>j('/api/tenants/'+tenant+'/environments/delete',{method:'POST',body:JSON.stringify({environment,reason:'control ui delete'})}))
  if((listEnvTenant.value||'').trim()===tenant) loadTenantEnvs()
  if((statesTenant.value||'').trim()===tenant && (statesEnv.value||'').trim()===environment) statesTable.innerHTML=''
}
async function changeStateLifecycle(tenant,env,state,status){
  stateLifeTenant.value=tenant||''
  stateLifeEnv.value=env||''
  stateLifeName.value=state||''
  stateLifeStatus.value=status||'archived'
  const reason=status==='active'?'support restore':'control ui action'
  await runAction('Set state lifecycle',()=>j('/api/states/'+tenant+'/'+env+'/lifecycle',{method:'POST',body:JSON.stringify({state,status,reason})}))
  if((statesTenant.value||'').trim()===tenant && (statesEnv.value||'').trim()===env) loadStates()
}
async function deleteState(tenant,env,state){
  if(!confirm('Delete state '+state+' permanently? This removes state history too.')) return
  await runAction('Delete state',()=>j('/api/states/'+tenant+'/'+env+'/destroy',{method:'POST',body:JSON.stringify({state,reason:'control ui delete'})}))
  if((statesTenant.value||'').trim()===tenant && (statesEnv.value||'').trim()===env) loadStates()
}
async function loadTransfers(){
  const tenant=(xferTenant.value||'').trim()
  const status=(xferStatus.value||'').trim()
  const x=await runAction('Load transfers',()=>j('/api/ownership-transfers'+qFrom({tenant,status})))
  const rows=Array.isArray(x.transfers)?x.transfers:[]
  const t=document.getElementById('transfers')
  t.innerHTML='<tr><th>environment</th><th>target label</th><th>from</th><th>to</th><th>status</th><th>reason</th><th>created</th><th>action</th></tr>'+
    rows.map(v=>{
      const id=String(val(v,'id'))
      const status=String(val(v,'status'))
      const defaultTarget=String(val(v,'target_resource_name')||val(v,'resource_name')||'')
      let actions=''
      if(status==='pending'){
        actions='<button onclick="actTransfer(\''+id.replaceAll("'","&#39;")+'\',\'accept\',\''+defaultTarget.replaceAll("'","&#39;")+'\')">Accept</button>'+
          '<button onclick="actTransfer(\''+id.replaceAll("'","&#39;")+'\',\'reject\')">Reject</button>'+
          '<button onclick="actTransfer(\''+id.replaceAll("'","&#39;")+'\',\'cancel\')">Cancel</button>'
      }
      return row([
        val(v,'resource_name'),
        defaultTarget,
        val(v,'current_owner_ref'),
        val(v,'target_owner_ref'),
        status,
        val(v,'initiated_reason'),
        fmtTs(val(v,'created_at')),
        actions
      ])
    }).join('')
  transfersOut.textContent=JSON.stringify(x,null,2)
}
async function createTransfer(){
  const x=await runAction('Create transfer proposal',()=>j('/api/ownership-transfers',{method:'POST',body:JSON.stringify({
    source_tenant:xferSourceTenant.value,
    environment_slug:xferEnvSlug.value,
    target_tenant_slug:xferTargetTenant.value,
    actor:xferActor.value,
    reason:xferReason.value
  })}))
  transfersOut.textContent=JSON.stringify(x,null,2)
  loadTransfers()
}
async function actTransfer(id,action,currentLabel){
  const actor=(xferActor.value||'').trim()
  let body={actor}
  if(action==='accept'){
    const next=prompt('Environment label to use in the target workspace?', String(currentLabel||'').trim())
    if(next===null) return
    const trimmed=String(next||'').trim()
    if(!trimmed){alert('Target label is required'); return}
    body.target_new_slug=trimmed
  }
  const x=await runAction('Transfer '+action,()=>j('/api/ownership-transfers/'+id+'/'+action,{method:'POST',body:JSON.stringify(body)}))
  transfersOut.textContent=JSON.stringify(x,null,2)
  loadTransfers()
}
async function setTenantLifecycle(){await runAction('Set workspace lifecycle',()=>j('/api/tenants/lifecycle',{method:'POST',body:JSON.stringify({slug:lifeTenant.value,status:lifeTenantStatus.value})}));loadTenants()}
async function setEnvLifecycle(){await runAction('Set environment lifecycle',()=>j('/api/tenants/'+lifeEnvTenant.value+'/environments/lifecycle',{method:'POST',body:JSON.stringify({environment:lifeEnvSlug.value,status:lifeEnvStatus.value})}));if((listEnvTenant.value||'').trim()===(lifeEnvTenant.value||'').trim())loadTenantEnvs()}
async function setTokenLifecycle(){await runAction('Set token lifecycle',()=>j('/api/tokens/lifecycle',{method:'POST',body:JSON.stringify({token_id:lifeTokenID.value,status:lifeTokenStatus.value})}))}
async function setOperatorTokenStatus(tokenID,status){
  if(status==='deleted' && !confirm('Delete operator token permanently?')) return
  await runAction('Set operator token status',()=>j('/api/tokens/lifecycle',{method:'POST',body:JSON.stringify({token_id:tokenID,status,reason:'control operator action'})}))
  loadOperatorTokens()
}
function applyFilters(){loadTenants();if((listEnvTenant.value||'').trim())loadTenantEnvs()}
function clearFilters(){fltSearch.value='';fltLifecycle.value='';fltTier.value='';fltLimit.value='200';loadTenants();if((listEnvTenant.value||'').trim())loadTenantEnvs()}
async function previewRetention(){
  const x=await runAction('Preview retention purge',()=>j('/api/retention/purge',{method:'POST',body:JSON.stringify({older_than_hours:Number(retHours.value||720),tenant:retTenant.value,reason:retReason.value,apply:false})}))
  retOut.textContent=JSON.stringify(x,null,2)
}
async function applyRetention(){
  if(!(retReason.value||'').trim()){alert('reason is required for apply');return}
  if(!confirm('This will permanently delete archived workspace metadata matching cutoff. Continue?')) return
  const x=await runAction('Apply retention purge',()=>j('/api/retention/purge',{method:'POST',body:JSON.stringify({older_than_hours:Number(retHours.value||720),tenant:retTenant.value,reason:retReason.value,apply:true})}))
  retOut.textContent=JSON.stringify(x,null,2)
  loadTenants()
}
async function loadRBACGrants(){
  const x=await runAction('Load RBAC grants',()=>j('/api/rbac/grants'+(rbacIncludeRevoked.checked?'?include_revoked=true':'')))
  const t=document.getElementById('rbacGrants')
  t.innerHTML='<tr><th>role</th><th>subject_kind</th><th>subject_id</th><th>scope</th><th>scope_ref</th><th>granted_by</th><th>granted_at</th><th>revoked_at</th><th>id</th></tr>'+
    (x.grants||[]).map(v=>row([
      val(v,'role_key'),
      val(v,'subject_kind'),
      val(v,'subject_id'),
      val(v,'scope_kind'),
      val(v,'scope_ref'),
      val(v,'granted_by'),
      val(v,'granted_at'),
      val(v,'revoked_at'),
      '<button onclick="copyId(\''+String(val(v,'id')).replaceAll("'","&#39;")+'\')">copy</button>'
    ])).join('')
}
async function grantRole(){
  await runAction('Grant RBAC role',()=>j('/api/rbac/grants',{method:'POST',body:JSON.stringify({
    subject_kind:rbacSubjectKind.value,subject_id:rbacSubjectID.value,role_key:rbacRoleKey.value,
    scope_kind:rbacScopeKind.value,scope_ref:rbacScopeRef.value,granted_by:rbacGrantedBy.value
  })}))
  loadRBACGrants()
}
async function revokeRole(){
  await runAction('Revoke RBAC grant',()=>j('/api/rbac/grants/revoke',{method:'POST',body:JSON.stringify({grant_id:rbacGrantID.value,revoked_by:rbacRevokedBy.value})}))
  loadRBACGrants()
}
function tplRole(role){
  rbacRoleKey.value=role
  if(role==='platform_admin'){
    rbacScopeKind.value='global'
    rbacScopeRef.value=''
  }else if(role==='tenant_admin'){
    rbacScopeKind.value='tenant'
    if(!rbacScopeRef.value) rbacScopeRef.value='tenant-slug'
  }else if(role==='provisioner'){
    rbacScopeKind.value='global'
    rbacScopeRef.value=''
  }else if(role==='support_readonly'){
    rbacScopeKind.value='global'
    rbacScopeRef.value=''
  }
}
ctrlToken.value=token();
loadTenants();
loadOperatorTokens();
loadTransfers();
loadRBACGrants();
</script>
</body></html>`
