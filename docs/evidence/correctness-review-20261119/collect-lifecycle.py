"""Read-only observations during this acceptance run; not a platform agent."""
import datetime
import json
import subprocess
import time
from pathlib import Path

folder = Path(__file__).resolve().parent
stats = {"samples": 0, "unknownWithZeroAvailable": 0,
         "reservedAndSessionFreeWithZeroAvailable": 0, "violations": [], "readErrors": []}
with (folder / "lifecycle-samples.jsonl").open("x") as stream:
    for _ in range(240):
        if (folder / "collector.stop").exists():
            break
        observed = datetime.datetime.now(datetime.timezone.utc).isoformat()
        result = subprocess.run(
            ["kubectl", "get", "acceleratorendpoints,acceleratorleases", "-A", "-o", "json", "--request-timeout=5s"],
            capture_output=True, text=True, timeout=10)
        if result.returncode:
            stats["readErrors"].append({"at": observed, "error": result.stderr[:1000]})
        else:
            objects = json.loads(result.stdout)["items"]
            endpoints = [{"name": o["metadata"]["name"], "resourceVersion": o["metadata"]["resourceVersion"],
                          "status": o.get("status", {})} for o in objects if o["kind"] == "AcceleratorEndpoint"]
            leases = [{"name": o["metadata"]["name"], "namespace": o["metadata"]["namespace"],
                       "status": o.get("status", {})} for o in objects if o["kind"] == "AcceleratorLease"]
            stream.write(json.dumps({"at": observed, "endpoints": endpoints, "leases": leases}) + "\n")
            stream.flush()
            stats["samples"] += 1
            for endpoint in endpoints:
                status = endpoint["status"]
                available = status.get("available", 0)
                reserved = status.get("reserved", 0)
                unknown = status.get("confidence") != "Observed" or status.get("health") != "Healthy" or status.get("activeSessions") is None
                if unknown and available == 0:
                    stats["unknownWithZeroAvailable"] += 1
                if reserved > 0 and status.get("activeSessions") == 0 and available == 0:
                    stats["reservedAndSessionFreeWithZeroAvailable"] += 1
                if available > 0 and (unknown or reserved > 0 or status.get("activeSessions", 0) > 0):
                    stats["violations"].append({"at": observed, **endpoint})
        time.sleep(5)
stats["scope"] = "Endpoint status invariants only; observations across objects are not atomic"
(folder / "lifecycle-summary.json").write_text(json.dumps(stats, indent=2) + "\n")
print(json.dumps({k: v for k, v in stats.items() if k != "readErrors"}), flush=True)
