import argparse
import http.client
import json
import time


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", required=True)
    ap.add_argument("--side", required=True)
    ap.add_argument("--out", required=True)
    args = ap.parse_args()
    stripped = args.base.split("://", 1)[-1]
    host, port = stripped.rsplit(":", 1)
    host, port = host, int(port)

    def request(method, path, body=None):
        c = http.client.HTTPConnection(host, port, timeout=15)
        headers = {"Content-Type": "application/json"} if body is not None else {}
        c.request(method, path, json.dumps(body) if body is not None else None, headers)
        r = c.getresponse()
        raw = r.read()
        c.close()
        try:
            parsed = json.loads(raw) if raw else {}
        except json.JSONDecodeError:
            parsed = {"_raw": raw.decode(errors="replace")[:200]}
        return r.status, parsed

    list_path = "/api/devices" if args.side == "A" else "/api/catalog"

    def deactivate(dev_id):
        if args.side == "A":
            return request("POST", f"/api/devices/{dev_id}/deactivate", {})
        last = (-1, {})
        for kind in ("vessels", "probes", "circuits"):
            st, body = request("POST", f"/api/catalog/{kind}/{dev_id}/deactivate", {})
            last = (st, body)
            if st != 404:
                return last
        return last

    def find(devices, dev_id):
        for d in devices:
            if d.get("id") == dev_id:
                status = d.get("status") or d.get("state")
                if status is None and "active" in d:
                    status = "active" if d.get("active") else "deactivated"
                return {"id": d.get("id"), "type": d.get("type") or d.get("kind"), "status": status,
                        "active": d.get("active"), "zone": d.get("zone"), "vessel_id": d.get("vessel_id")}
        return None

    summary = {"side": args.side, "reviewer": "Codex acceptance session 2026-10-02",
               "tier": "normal", "steps": {}}
    steps = summary["steps"]

    for _ in range(60):
        st, _ = request("GET", "/healthz")
        if st == 200:
            break
        time.sleep(0.5)

    st, body = request("GET", list_path)
    devices = body.get("devices") or body.get("items")
    if devices is None:
        devices = []
        for key in ("vessels", "probes", "circuits"):
            devices.extend(body.get(key) or [])
    steps["P1_catalog_after_restart"] = {"status": st, "count": len(devices),
                                         "V-90": find(devices, "V-90"),
                                         "P-90": find(devices, "P-90"),
                                         "C-90": find(devices, "C-90")}

    st_p, rt_p = request("GET", "/api/probes")
    st_v, rt_v = request("GET", "/api/vessels")
    probe_ids = [p.get("id") for p in (rt_p.get("items") or [])]
    vessel_ids = [v.get("id") for v in (rt_v.get("items") or [])]
    steps["P2_runtime_loaded"] = {"P-90_in_probes": "P-90" in probe_ids, "V-90_in_vessels": "V-90" in vessel_ids}

    st_dc, _ = deactivate("C-90")
    st, body = request("GET", list_path)
    devices = body.get("devices") or body.get("items")
    if devices is None:
        devices = []
        for key in ("vessels", "probes", "circuits"):
            devices.extend(body.get(key) or [])
    steps["P3_deactivate_idle_circuit"] = {"status": st_dc, "C-90": find(devices, "C-90")}

    with open(args.out, "w", encoding="utf-8") as f:
        json.dump(summary, f, ensure_ascii=False, indent=1)
    print(json.dumps(summary, ensure_ascii=False, indent=1)[:5000])


if __name__ == "__main__":
    main()
