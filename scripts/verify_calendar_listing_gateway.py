#!/usr/bin/env python3
"""Synthetic calendar MCP -> local gateway acceptance. No real data or config."""
import json,os,pathlib,socket,subprocess,tempfile,time,urllib.request
ROOT=pathlib.Path(__file__).resolve().parents[1]
TOKEN='synthetic-calendar-listing-client-123456'
def main():
 processes=[]
 try:
  with tempfile.TemporaryDirectory(prefix='calendar-listing-integration-') as tmp:
   fixture=subprocess.Popen(['/tmp/calendar-listing-fixture','-serve'],stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,text=True);processes.append(fixture);info=json.loads(fixture.stdout.readline())
   with socket.socket() as sock:sock.bind(('127.0.0.1',0));port=sock.getsockname()[1]
   path=pathlib.Path(tmp);cfg={'servers':{'calendar':{'url':info['endpoint'],'enabled':True,'tools':{'list_calendars':{'enabled':True,'access_mode':'shared'}}}},'principals':{'local':{'token_env':'CALENDAR_FIXTURE_CLIENT','grants':{}}}}
   (path/'registry.json').write_text(json.dumps(cfg));(path/'policy.json').write_text(json.dumps({'version':1,'operations':[{'server_id':'calendar','tool_name':'list_calendars','endpoint':info['endpoint']}]}))
   env={k:v for k,v in os.environ.items() if not k.startswith('GATEWAY_')};env.update(CALENDAR_FIXTURE_CLIENT=TOKEN,GATEWAY_STORAGE='file',GATEWAY_CONFIG=str(path/'registry.json'),GATEWAY_LISTEN_ADDR=f'127.0.0.1:{port}',GATEWAY_TOOL_SEARCH_ENABLED='true',GATEWAY_READ_BATCH_ENABLED='true',GATEWAY_READ_BATCH_ALLOWLIST=str(path/'policy.json'))
   processes.append(subprocess.Popen(['/tmp/mcp-gateway-read-batch'],env=env,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL))
   for _ in range(100):
    try:
     with urllib.request.urlopen(f'http://127.0.0.1:{port}/healthz',timeout=1) as r:
      if r.status==200:break
    except OSError:time.sleep(.05)
   else:raise RuntimeError('fixture gateway unavailable')
   def rpc(name,args):
    body=json.dumps({'jsonrpc':'2.0','id':1,'method':'tools/call','params':{'name':name,'arguments':args}}).encode()
    req=urllib.request.Request(f'http://127.0.0.1:{port}/mcp',data=body,headers={'Authorization':'Bearer '+TOKEN,'Content-Type':'application/json','Accept':'application/json, text/event-stream','MCP-Protocol-Version':'2025-03-26'})
    with urllib.request.urlopen(req,timeout=10) as r:data=json.load(r)
    assert 'error' not in data and not data['result'].get('isError');return data['result']
   def calendars(r):return json.loads(next(x['text'] for x in r['content'] if x['type']=='text'))
   reference=calendars(rpc('invoke_tool',{'server_id':'calendar','tool_name':'list_calendars','arguments':{}}));assert len(reference)==3
   rows=[]
   for count in [1,3]:
    with urllib.request.urlopen(info['metrics']) as r:before=json.load(r)['apple_http_requests']
    start=time.monotonic();r=rpc('invoke_read_batch',{'operations':[{'id':f'read_{i}','server_id':'calendar','tool_name':'list_calendars','arguments':{}} for i in range(count)]});ms=(time.monotonic()-start)*1000;v=r['structuredContent'];assert v['complete'] and len(v['results'])==count
    for i,x in enumerate(v['results']):assert x['id']==f'read_{i}' and x['status']=='ok' and x['dispatched'] and x['upstream_is_error'] is False and calendars(x['tool_result'])==reference
    with urllib.request.urlopen(info['metrics']) as r:after=json.load(r)['apple_http_requests']
    assert after-before==count
    rows.append({'batch_items':count,'ms':ms,'calendars_per_item':3,'result_parity':True,'apple_http_requests':after-before})
   report={'synthetic':True,'path':'actual calendar MCP handler -> actual local gateway invoke_read_batch','rows':rows,'all_passed':True};(ROOT/'docs/verification/list-calendars-performance-gateway.json').write_text(json.dumps(report,indent=2)+'\n');print(json.dumps(report))
 finally:
  for p in reversed(processes):
   if p.poll() is None:p.terminate();p.wait(timeout=10)
if __name__=='__main__':main()
