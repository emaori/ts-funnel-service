#!/usr/bin/env python3
"""Run only isolated containers; verify real PromQL and import in clean Grafana."""
import argparse
import hashlib
import json
from pathlib import Path
import subprocess
import tempfile
import uuid

ROOT = Path(__file__).resolve().parents[1]
CLIENT_IMAGE = "python:3.14.1-alpine"
PROM_IMAGE = "prom/prometheus:v3.16.0"
GRAFANA_IMAGE = "grafana/grafana:13.3.2"

CLIENT = r'''
import base64,json,time,urllib.request,urllib.parse
dashboard=json.loads(DASHBOARD_DATA)
expected=EXPECTED_TARGETS
def request(url,data=None,auth=False):
 body=None if data is None else json.dumps(data).encode()
 headers={"Content-Type":"application/json"}
 if auth:headers["Authorization"]="Basic "+base64.b64encode(b"admin:isolated-test-password").decode()
 req=urllib.request.Request(url,data=body,headers=headers)
 with urllib.request.urlopen(req,timeout=30) as response:return json.load(response)
def wait(url):
 end=time.time()+75
 while time.time()<end:
  try:return request(url)
  except Exception:time.sleep(1)
 raise RuntimeError("not ready: "+url)
wait("http://prometheus:9090/api/v1/status/buildinfo")
wait("http://grafana:3000/api/health")
def expr(q):
 for name,value in {"__rate_interval":"5m","component":"tailscale","instance":".*","service":"grafana","environment":".*","host":".*","job":"ts-funnel"}.items():
  q=q.replace("$"+name,value)
 return q
def prom(q):
 data=request("http://prometheus:9090/api/v1/query?"+urllib.parse.urlencode({"query":q}))
 if data["status"]!="success":raise RuntimeError(data)
 return data["data"]["result"]
end=time.time()+60
while time.time()<end:
 rows=prom('up{job="ts-funnel"}')
 if len(rows)==expected and sum(float(r["value"][1]) for r in rows)==expected-1:break
 time.sleep(1)
else:raise RuntimeError("scrape targets did not reach expected state: "+repr(rows))
table=next(p for p in dashboard["panels"] if p["type"]=="table")
all_rows=None
for panel in dashboard["panels"]:
 for target in panel.get("targets",[]):
  try:result=prom(expr(target["expr"]))
  except Exception as e:raise RuntimeError(panel["title"]+" / "+target["refId"]+": "+str(e))
  if panel is table and target["refId"]=="A":all_rows=result
assert len(all_rows)==expected,all_rows
by_instance={r["metric"]["instance"]:r for r in all_rows}
assert by_instance["paperless-1"]["metric"]["current_version"]=="v1.104.1"
assert by_instance["paperless-2"]["metric"]["current_version"]=="v1.104.7"
assert by_instance["paperless-1"]["metric"]["last_result"]=="rolled_back"
assert by_instance["immich"]["value"][1]=="3",by_instance["immich"]
assert by_instance["immich"]["metric"]["current_version"]=="Unavailable"
assert by_instance["immich"]["metric"]["service_name"]=="immich"
if expected==8:
 assert "new-service" in by_instance
 assert by_instance["updating"]["value"][1]=="1"
 assert by_instance["rollback-failed"]["value"][1]=="2"
 assert by_instance["rollback-failed"]["metric"]["last_result"]=="rollback_failed"
 disabled=prom(expr(next(t["expr"] for t in table["targets"] if t["refId"]=="B")))
 assert next(r["value"][1] for r in disabled if "disabled" in r["metric"]["target"])=="0"

try:request("http://grafana:3000/api/datasources/uid/ts-funnel-test",auth=True)
except Exception:
 request("http://grafana:3000/api/datasources",{"name":"Test Prometheus","uid":"ts-funnel-test","type":"prometheus","url":"http://prometheus:9090","access":"proxy","isDefault":True},True)
for variable in dashboard["templating"]["list"]:
 if variable["name"]=="datasource":variable["current"]={"text":"Test Prometheus","value":"ts-funnel-test"}
saved=request("http://grafana:3000/api/dashboards/db",{"dashboard":dashboard,"overwrite":True},True)
assert saved.get("status")=="success",saved
loaded=request("http://grafana:3000/api/dashboards/uid/ts-funnel-services",auth=True)
assert len(loaded["dashboard"]["panels"])==len(dashboard["panels"])
now=int(time.time()*1000)
targets=[]
for t in table["targets"]:
 q=dict(t);q["expr"]=expr(q["expr"]);q["datasource"]={"type":"prometheus","uid":"ts-funnel-test"};q["intervalMs"]=30000;q["maxDataPoints"]=1000;targets.append(q)
frames=request("http://grafana:3000/api/ds/query",{"from":str(now-3600000),"to":str(now),"queries":targets},True)
for ref,result in frames.get("results",{}).items():
 assert not result.get("error"),(ref,result)
print(json.dumps({"targets":len(all_rows),"unreachable_preserved":True,"replicas_distinct":True,"grafana_import":saved["status"],"new_service_discovered":expected==8}))
'''

def run(*args, data=None, capture=False):
    result = subprocess.run(["docker", *args], cwd=ROOT, input=data, text=True,
                            stdout=subprocess.PIPE if capture else None, check=True)
    return result.stdout.strip() if capture else None

def config_text(extra=False):
    groups=[]
    for instance,service in [("grafana","grafana"),("paperless-1","paperless"),
                             ("paperless-2","paperless"),("immich","immich")]+([("new-service","new-service"),("updating","test-updating"),("disabled","test-disabled"),("rollback-failed","test-rollback-failed")] if extra else []):
        groups.append({"targets":["fixture:1" if instance=="immich" else "fixture:9092"],
                       "labels":{"instance":instance,"service_name":service,"environment":"production",
                                 "__metrics_path__":"/"+instance}})
    # JSON is a valid YAML subset and avoids an extra development dependency.
    return json.dumps({"global":{"scrape_interval":"1s","scrape_timeout":"500ms"},
                       "scrape_configs":[{"job_name":"ts-funnel","static_configs":groups}]})

def main():
    parser=argparse.ArgumentParser()
    parser.add_argument("--fixture-image",required=True)
    parser.add_argument("--browser-image")
    args=parser.parse_args()
    suffix=uuid.uuid4().hex[:10]
    network="tsf-observability-"+suffix
    names={name:"tsf-"+suffix+"-"+name for name in ["fixture","prometheus","grafana"]}
    source=ROOT/"docs/grafana/ts-funnel-service.json"
    content=source.read_text()
    before=hashlib.sha256(content.encode()).hexdigest()
    run("network","create",network,capture=True)
    try:
        run("run","-d","--network",network,"--network-alias","fixture","--name",names["fixture"],args.fixture_image,capture=True)
        with tempfile.TemporaryDirectory(prefix="tsf-observability-") as tmp:
            config=Path(tmp)/"prometheus.yml";config.write_text(config_text())
            run("create","--network",network,"--network-alias","prometheus","--name",names["prometheus"],
                PROM_IMAGE,"--config.file=/etc/prometheus/prometheus.yml","--web.enable-lifecycle",capture=True)
            run("cp",str(config),names["prometheus"]+":/etc/prometheus/prometheus.yml")
            run("start",names["prometheus"],capture=True)
            run("run","-d","--network",network,"--network-alias","grafana","--name",names["grafana"],
                "-e","GF_SECURITY_ADMIN_PASSWORD=isolated-test-password","-e","GF_ANALYTICS_REPORTING_ENABLED=false",
                "-e","GF_ANALYTICS_CHECK_FOR_UPDATES=false",GRAFANA_IMAGE,capture=True)
            for count in [4,8]:
                if count==8:
                    config.write_text(config_text(True))
                    run("cp",str(config),names["prometheus"]+":/etc/prometheus/prometheus.yml")
                    run("kill","--signal=HUP",names["prometheus"])
                program=CLIENT.replace("DASHBOARD_DATA",repr(content)).replace("EXPECTED_TARGETS",str(count))
                run("run","--rm","-i","--network",network,CLIENT_IMAGE,"python","-",data=program)
        if args.browser_image:
            run("run","--rm","--network",network,args.browser_image)
        assert hashlib.sha256(source.read_bytes()).hexdigest()==before,"test modified the dashboard JSON"
    finally:
        for name in names.values():
            subprocess.run(["docker","rm","-f",name],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
        subprocess.run(["docker","network","rm",network],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)

if __name__=="__main__":
    main()
