"""Read-only original policy import, with backup and no credential output."""
from pathlib import Path
import argparse,json,os,sqlite3

def same_endpoint(proxy,connection):
 return (str(proxy.get('type','')).lower()==str(connection.get('Protocol','')).lower() and proxy.get('server')==connection.get('Server') and int(proxy.get('port',0))==int(connection.get('Port',0)))

def migrate(source,root):
 marker=root/'data/chainpilot-v3-import.json'
 if marker.exists():return {'alreadyImported':True}
 path=root/'data/config.json';cfg=json.loads(path.read_text(encoding='utf-8'))
 if cfg.get('version',1)>=3:return {'skipped':'Existing v3 user policy retained'}
 db=sqlite3.connect((source/'chainpilot.db').as_uri()+'?mode=ro',uri=True)
 try:
  state=json.loads(db.execute('select state_json from chain_manager_state').fetchone()[0]);app=json.loads(db.execute('select document_json from documents where namespace=?',('chainpilot.application.v1',)).fetchone()[0])
 finally:db.close()
 backup=root/'data/backups/before-policy-import-v3.json';backup.parent.mkdir(parents=True,exist_ok=True);backup.write_bytes(path.read_bytes())
 relay_map={};matched=0
 for n in cfg['nodes']:
  found=[r for r in app['catalog']['Relays'].values() if same_endpoint(n['proxy'],r['Connection'])]
  if len(found)==1:
   r=found[0];relay_map[r['ID']]=n['id'];ids=list(r.get('SourceIDs') or [])
   if ids:n['sourceIDs']=ids;n['sourceID']=ids[0]
   n['retired']=bool(r.get('Retired'));matched+=1
 pool=state['RelayPool'];cfg['poolMode']=pool.get('mode','source')
 for s in cfg.get('sources',[]):
  policy=next((x for x in pool.get('sources',[]) if x.get('sourceId')==s['id']),None)
  if policy:s['priority']=policy.get('priority',100);s['paused']=bool(policy.get('paused'))
 groups=[]
 for i,g in enumerate(pool.get('groups',[])):
  members=[dict(nodeID=relay_map[r['relayId']],priority=r.get('priority',100)) for r in g.get('relays',[]) if r.get('relayId') in relay_map]
  if members:groups.append(dict(id='migrated-'+str(i),name=g['name'],priority=g.get('priority',100),paused=bool(g.get('paused')),members=members))
 cfg['relayGroups']=groups
 for p in cfg.get('profiles',[]):
  originals=[r for r in state['Profiles'].values() if same_endpoint(p['fixedExit'],r['FixedExit'])]
  if len(originals)!=1:continue
  old=originals[0];allow=old.get('AllowedRelayIDs');reject=set(old.get('RejectedRelayIDs') or [])
  p['restrictNodes']=allow is not None or bool(reject)
  p['allowedNodes']=[n for old_id,n in relay_map.items() if (allow is None or old_id in allow) and old_id not in reject]
  if old.get('Mode')==1 and old.get('CurrentRelayID') in relay_map:p['selectionMode']='manual';p['manualRelayID']=relay_map[old['CurrentRelayID']]
 cfg.update(version=3,autoRecover=True)
 temp=path.with_suffix('.tmp');temp.write_text(json.dumps(cfg,ensure_ascii=False,indent=2),encoding='utf-8');os.replace(temp,path)
 result=dict(ok=True,matchedNodes=matched,groups=len(groups),profiles=len(cfg.get('profiles',[])))
 marker.write_text(json.dumps(result),encoding='utf-8');return result

if __name__=='__main__':
 parser=argparse.ArgumentParser(description=__doc__);parser.add_argument('--source',type=Path,default=Path(os.environ['LOCALAPPDATA'])/'ChainPilot');parser.add_argument('--root',type=Path,default=Path(__file__).resolve().parent.parent);args=parser.parse_args()
 try:print(json.dumps(migrate(args.source,args.root)))
 except Exception as error:print(json.dumps(dict(ok=False,errorType=type(error).__name__)));raise SystemExit(1)
