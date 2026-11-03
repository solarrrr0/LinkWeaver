'use strict';
let operationExpiryTimer;
function renderOperations(){
 clearTimeout(operationExpiryTimer);
 const now=Date.now(),byKind=new Map();
 for(const j of data.operations||[]){
  const end=Date.parse(j.finishedAt),expires=end+(j.state==='failed'?15000:6000);
  if(j.state!=='running'&&(!Number.isFinite(expires)||expires<=now))continue;
  const old=byKind.get(j.kind);if(!old||(j.state==='running'&&old.state!=='running')||(old.state!=='running'&&Date.parse(j.at)>Date.parse(old.at)))byKind.set(j.kind,j);
 }
 const jobs=[...byKind.values()];$('operation-panel').hidden=!jobs.length;const ops=$('operation-list');ops.replaceChildren();
 let nextExpiry=Infinity;
 for(const j of jobs){
  const row=el('div','operation-row'),text=el('div');text.append(el('strong','',j.title),el('p','secondary-text',(j.message||'')+' · '+({running:'进行中',completed:'完成',failed:'失败',cancelled:'已取消'}[j.state]||j.state)));row.append(text);
  if(j.state==='running'){const progress=el('progress');progress.max=Math.max(1,j.total);progress.value=j.completed;row.append(progress,button('取消',()=>action(()=>api('jobs/cancel',{id:j.id}))))}
  else nextExpiry=Math.min(nextExpiry,Date.parse(j.finishedAt)+(j.state==='failed'?15000:6000));
  ops.append(row);
 }
 if(Number.isFinite(nextExpiry))operationExpiryTimer=setTimeout(renderOperations,Math.max(1,nextExpiry-now+20));
}
function renderFeatures(){
 renderOperations();
 $('pool-mode').value=data.poolMode||'source';$('new-group').hidden=data.poolMode!=='custom';const root=$('groups-list');root.replaceChildren();
 if(data.poolMode==='custom'){for(const g of data.relayGroups||[]){const card=el('article','source-card');card.append(el('h3','',g.name),el('p','secondary-text',`${g.members.length} 个节点 · 组优先级 ${g.priority} · ${g.paused?'已暂停':'参与自动选路'}`));card.append(button('编辑',()=>openGroup(g.id),'link-button'),button(g.paused?'恢复':'暂停',()=>action(()=>api('groups/save',{mode:'custom',groups:data.relayGroups.map(x=>x.id===g.id?{...x,paused:!x.paused}:x)})),'link-button'),button('删除分组',()=>{if(confirm('删除分组？节点保留在未分组备用池。'))action(()=>api('groups/delete',{id:g.id,deleteNodes:false}))},'link-button danger-text'),button('删除组及节点',()=>{if(confirm('删除分组和其中节点？被出口使用的组无法删除。'))action(()=>api('groups/delete',{id:g.id,deleteNodes:true}))},'link-button danger-text'));root.append(card)}if(!root.children.length)root.append(el('p','secondary-text','尚无自定义分组。未分组节点作为备用。'))}
 else for(const s of data.sources){const card=el('article','source-card');card.append(el('h3','',s.name),el('p','secondary-text',`优先级 ${s.priority} · ${!s.enabled?'已停用':s.paused?'已暂停':'参与自动选路'}`),button(s.paused?'恢复自动选用':'暂停自动选用',()=>action(()=>api('sources/save',{id:s.id,name:s.name,enabled:s.enabled,priority:s.priority,userAgent:s.userAgent,url:'',paused:!s.paused})),'link-button'));root.append(card)}
 if(!data.profiles.length&&$('relay-test-mode').value!=='relay'){$('relay-test-mode').value='relay';renderRelays()}
}
function prepareProfileFeatures(p){
 const source=$('profile-source');source.multiple=true;source.size=Math.min(5,data.sources.length+1);for(const id of p?.allowedSources||[])if(![...source.options].some(o=>o.value===id)){const o=el('option','','原来源已删除');o.value=id;source.append(o)}for(const o of source.options)o.selected=p?.allowedSources?.length?p.allowedSources.includes(o.value):o.value==='';
 $('profile-restrict-nodes').checked=!!p?.restrictNodes;const root=$('profile-node-options');root.replaceChildren();for(const n of data.nodes){const row=el('label','check-row'),input=el('input');input.type='checkbox';input.value=n.id;input.checked=!!p?.allowedNodes?.includes(n.id);row.append(input,document.createTextNode(n.name+(n.retired?'（已退役）':'')));root.append(row)}root.hidden=!$('profile-restrict-nodes').checked;
}
function profileFeatureValues(){const values=[...$('profile-source').selectedOptions].map(o=>o.value);return {allowedSources:values.includes('')?[]:values,restrictNodes:$('profile-restrict-nodes').checked,allowedNodes:[...document.querySelectorAll('#profile-node-options input:checked')].map(x=>x.value)}}
function openSelection(id){const p=data.profiles.find(x=>x.id===id);if(!p)return;$('selection-profile').value=id;$('selection-mode').value=p.selectionMode||'automatic';const root=$('selection-node');root.replaceChildren();for(const n of data.nodes){if(n.retired||n.disabled)continue;if(p.restrictNodes&&!p.allowedNodes?.includes(n.id))continue;if(p.allowedSources?.length&&!(n.sourceIDs||[n.sourceID]).some(x=>p.allowedSources.includes(x)))continue;const o=el('option','',n.name);o.value=n.id;root.append(o)}root.value=p.manualRelayID||p.pinned||p.current?.id||root.firstChild?.value||'';root.disabled=$('selection-mode').value!=='manual';errorIn('selection-error','');$('selection-dialog').showModal()}
function openGroup(id){const g=(data.relayGroups||[]).find(x=>x.id===id);$('group-form').reset();$('group-id').value=g?.id||'';$('group-name').value=g?.name||'';$('group-priority').value=g?.priority??100;$('group-paused').checked=!!g?.paused;const root=$('group-members');root.replaceChildren();const occupied=new Set((data.relayGroups||[]).filter(x=>x.id!==id).flatMap(x=>x.members.map(m=>m.nodeID)));for(const n of data.nodes){if(occupied.has(n.id)||n.retired)continue;const row=el('label','check-row'),check=el('input'),priority=el('input');check.type='checkbox';check.value=n.id;const member=g?.members.find(x=>x.nodeID===n.id);check.checked=!!member;priority.type='number';priority.min=1;priority.max=10000;priority.value=member?.priority||n.priority;priority.style.width='85px';priority.setAttribute('aria-label',n.name+'组内优先级');row.dataset.search=n.name.toLowerCase();row.append(check,el('span','',n.name),priority);root.append(row)}errorIn('group-error','');$('group-dialog').showModal()}
for(const[id,kind]of [['refresh-all-sources','refresh'],['check-pool','pool-check'],['measure-all','measure']])$(id).addEventListener('click',()=>action(async()=>{await api('jobs/start',{kind});toast('任务已在后台开始')}));
$('relay-test-mode').addEventListener('change',renderRelays);
$('profile-restrict-nodes').addEventListener('change',()=>{$('profile-node-options').hidden=!$('profile-restrict-nodes').checked});
$('selection-mode').addEventListener('change',()=>{$('selection-node').disabled=$('selection-mode').value!=='manual'});
$('selection-form').addEventListener('submit',e=>{e.preventDefault();action(async()=>{const manual=$('selection-mode').value==='manual';if(manual&&!$('selection-node').value)throw Error('请选择可用的候选中转');await api(profilePath('select',$('selection-profile').value),{id:manual?$('selection-node').value:''});$('selection-dialog').close();toast('选择模式已保存')},'selection-error')});
$('new-group').addEventListener('click',()=>openGroup());
$('pool-mode').addEventListener('change',()=>action(()=>api('groups/save',{mode:$('pool-mode').value,groups:data.relayGroups||[]})));
$('group-search').addEventListener('input',()=>{for(const row of $('group-members').children)row.hidden=!row.dataset.search.includes($('group-search').value.toLowerCase())});
$('group-form').addEventListener('submit',e=>{e.preventDefault();action(async()=>{const g={id:$('group-id').value,name:$('group-name').value.trim(),priority:+$('group-priority').value,paused:$('group-paused').checked,members:[...$('group-members').children].filter(r=>r.querySelector('input[type=checkbox]').checked).map(r=>({nodeID:r.querySelector('input[type=checkbox]').value,priority:+r.querySelector('input[type=number]').value}))};const groups=(data.relayGroups||[]).filter(x=>x.id!==g.id);groups.push(g);await api('groups/save',{mode:'custom',groups});$('group-dialog').close()},'group-error')});
$('check-clash-native').addEventListener('click',()=>action(async()=>{const d=await api('clash/integration');$('clash-native-status').textContent=d.message}));
$('clash-independent').addEventListener('click',()=>action(async()=>{const u=new URL($('clash-link-value').value),profile=u.searchParams.get('profile')||'';await api('clash/install',{profile});toast('已请求 Clash 新增独立订阅')}));

let updatePollTimer,updateReconnectTimer,onlineUpdateStarted=false,updateStartVersion='';
async function updateInfo(){
 const d=await api('update/status'),o=d.online||{};
 $('update-status').textContent='当前 '+d.version+'。'+d.message+(o.downloading?' '+o.progress+'%':'')+(d.preparedVersion?' 已校验：'+d.preparedVersion:'')+(d.lastResult?' 最近结果：'+d.lastResult.message:'');
 $('update-rollback').dataset.unavailable=String(!d.canRollback||o.downloading||o.restarting);
 $('update-apply').dataset.unavailable=String(!d.preparedVersion||o.downloading||o.restarting);
 $('update-online').dataset.unavailable=String(!o.available||o.checking||o.downloading||o.restarting);
 $('update-check').dataset.unavailable=String(!!(o.checking||o.downloading||o.restarting));
 const banner=$('program-update-banner');banner.replaceChildren();banner.hidden=!(o.available||o.downloading||o.restarting);
 if(!banner.hidden){banner.append(el('span','',o.restarting?'更新已校验，正在重启…':o.downloading?'正在下载更新 '+o.progress+'%':'发现新版本 '+o.latestVersion+'。'));if(!o.downloading&&!o.restarting){const b=button('下载并安装',startOnlineUpdate);b.dataset.unavailable=String(!!o.checking);banner.append(b)}}
 updateDisabled();clearTimeout(updatePollTimer);
 if(o.checking||o.downloading||o.restarting)updatePollTimer=setTimeout(()=>updateInfo().catch(()=>{if(onlineUpdateStarted)waitForProgramRestart()}),1000);
 else if(onlineUpdateStarted){onlineUpdateStarted=false;clearInterval(updateReconnectTimer);toast(d.message)}
 if(o.restarting)waitForProgramRestart();return d;
}
async function checkProgramUpdate(){await api('update/check',{});await updateInfo()}
$('update-check').addEventListener('click',()=>action(checkProgramUpdate));
function waitForProgramRestart(){if(updateReconnectTimer)return;const deadline=Date.now()+90000;updateReconnectTimer=setInterval(async()=>{try{const r=await fetch('/',{cache:'no-store'});const text=await r.text();if(r.ok&&text.includes('name="lw-token"')&&!text.includes('content="'+token+'"')){clearInterval(updateReconnectTimer);location.reload();return}}catch{}if(Date.now()>deadline){clearInterval(updateReconnectTimer);updateReconnectTimer=null;onlineUpdateStarted=false;toast('重启等待超时，请从托盘重新打开管理页。')}},1500)}
async function startOnlineUpdate(){if(!confirm('下载并安装新版？程序会自动重启，代理连接会短暂断开，现有配置保留。'))return;await action(async()=>{await api('update/online',{});onlineUpdateStarted=true;await updateInfo();toast('正在下载更新，可在后台任务中查看进度')})}
$('update-online').addEventListener('click',startOnlineUpdate);
checkProgramUpdate().catch(()=>{$('update-status').textContent='更新检查暂不可用，可稍后点击“检查更新”。'});
$('update-prepare').addEventListener('click',()=>action(async()=>{await api('update/prepare',{path:$('update-path').value,sha256:$('update-hash').value});await updateInfo();toast('升级包已校验，点击应用后重启')}));
async function applyProgramUpdate(kind){if(!confirm(kind==='rollback'?'回退上次程序版本并重启？':'应用已校验升级包并重启？连接会短暂断开。'))return;await action(async()=>{await api('update/'+kind,{});toast('程序正在重启，请稍候…');setTimeout(()=>location.reload(),12000)})}
$('update-apply').addEventListener('click',()=>applyProgramUpdate('apply'));
$('update-rollback').addEventListener('click',()=>applyProgramUpdate('rollback'));
