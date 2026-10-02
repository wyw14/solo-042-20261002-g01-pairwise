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

    if args.side == "A":
        list_path = "/api/devices"

        def register(kind, payload):
            body = dict(payload)
            body["type"] = kind
            return request("POST", "/api/devices", body)

        def modify(kind, dev_id, payload):
            body = dict(payload)
            body["type"] = kind
            return request("PUT", f"/api/devices/{dev_id}", body)

        def deactivate(dev_id):
            return request("POST", f"/api/devices/{dev_id}/deactivate", {})
    else:
        list_path = "/api/catalog"
        kind_path = {"vessel": "vessels", "probe": "probes", "circuit": "circuits"}

        def register(kind, payload):
            return request("POST", f"/api/catalog/{kind_path[kind]}", payload)

        def modify(kind, dev_id, payload):
            return request("POST", f"/api/catalog/{kind_path[kind]}/{dev_id}", payload)

        def deactivate(dev_id):
            last = (-1, {})
            for kind in ("vessels", "probes", "circuits"):
                st, body = request("POST", f"/api/catalog/{kind}/{dev_id}/deactivate", {})
                last = (st, body)
                if st != 404:
                    return last
            return last

    def catalog():
        st, body = request("GET", list_path)
        devices = body.get("devices") or body.get("items")
        if devices is None:
            devices = []
            for key in ("vessels", "probes", "circuits"):
                devices.extend(body.get(key) or [])
        return st, devices

    def find(devices, dev_id):
        for d in devices:
            if d.get("id") == dev_id:
                status = d.get("status") or d.get("state")
                if status is None and "active" in d:
                    status = "active" if d.get("active") else "deactivated"
                return {"id": d.get("id"), "type": d.get("type") or d.get("kind"), "status": status,
                        "active": d.get("active"), "zone": d.get("zone"), "vessel_id": d.get("vessel_id"),
                        "probe_kind": d.get("probe_kind"), "enabled": d.get("enabled")}
        return None

    summary = {"side": args.side, "reviewer": "Codex acceptance session 2026-10-02",
               "tier": "normal", "steps": {}}
    steps = summary["steps"]

    for _ in range(60):
        st, _ = request("GET", "/healthz")
        if st == 200:
            break
        time.sleep(0.5)

    st, devices = catalog()
    steps["S0_baseline"] = {"status": st, "count": len(devices)}

    def probe_payload(dev_id, vessel, kind):
        if args.side == "A":
            return {"id": dev_id, "name": "Catalog " + dev_id, "probe_kind": kind, "vessel_id": vessel}
        return {"id": dev_id, "kind": kind, "vessel_id": vessel}

    def circuit_payload(dev_id):
        if args.side == "A":
            return {"id": dev_id, "name": "Catalog " + dev_id, "zone": "east", "valve_id": "VALVE-90", "enabled": True}
        return {"id": dev_id, "zone": "east", "valve_id": "VALVE-90"}

    st_v, b_v = register("vessel", {"id": "V-90", "name": "Catalog V90", "zone": "north"})
    st_p, b_p = register("probe", probe_payload("P-90", "V-90", "pressure"))
    st_c, b_c = register("circuit", circuit_payload("C-90"))
    steps["S1_register"] = {"vessel": st_v, "probe": st_p, "circuit": st_c}
    st, devices = catalog()
    steps["S1_after"] = {"count": len(devices), "V-90": find(devices, "V-90"), "P-90": find(devices, "P-90"), "C-90": find(devices, "C-90")}
    count_after_register = len(devices)

    st_dup, _ = register("vessel", {"id": "V-90", "name": "dup", "zone": "north"})
    st, devices = catalog()
    steps["S2_duplicate"] = {"status": st_dup, "count_unchanged": len(devices) == count_after_register}

    st_bad, _ = register("probe", probe_payload("P-91", "V-XX", "pressure"))
    st_kind, _ = register("probe", probe_payload("P-92", "V-90", "bogus"))
    st, devices = catalog()
    steps["S3_invalid_atomic"] = {"missing_vessel": st_bad, "bad_kind": st_kind,
                                  "P-91_absent": find(devices, "P-91") is None,
                                  "P-92_absent": find(devices, "P-92") is None,
                                  "count_unchanged": len(devices) == count_after_register}

    st_rt_p, rt_p = request("GET", "/api/probes")
    st_rt_v, rt_v = request("GET", "/api/vessels")
    probe_ids = [p.get("id") for p in (rt_p.get("items") or [])]
    vessel_ids = [v.get("id") for v in (rt_v.get("items") or [])]
    steps["S4_runtime_live"] = {"status": st_rt_p, "P-90_in_probes": "P-90" in probe_ids,
                                "V-90_in_vessels": "V-90" in vessel_ids}

    st_fill, fill = request("POST", "/api/fill-sessions", {"vessel_id": "V-90", "liters": 1.0})
    circuit_id = fill.get("circuit_id") or (fill.get("session") or {}).get("circuit_id")
    steps["S5_fill"] = {"status": st_fill, "circuit": circuit_id}
    occ = {}
    if circuit_id:
        occ["circuit"] = deactivate(circuit_id)[0]
    occ["vessel"] = deactivate("V-90")[0]
    occ["probe"] = deactivate("P-90")[0]
    steps["S5_occupied_deactivate"] = occ
    if fill.get("id") or (fill.get("session") or {}).get("id"):
        fid = fill.get("id") or fill["session"]["id"]
        st_ab, _ = request("POST", f"/api/fill-sessions/{fid}/abort", {"reason": "catalog test cleanup"})
        steps["S5_abort"] = {"status": st_ab}
    time.sleep(0.5)
    occ2 = {"probe_after_release": deactivate("P-90")[0]}
    st, devices = catalog()
    occ2["P-90_status"] = find(devices, "P-90")
    steps["S5_after_release"] = occ2

    if args.side == "A":
        st_mod, _ = modify("vessel", "V-90", {"id": "V-90", "name": "Renamed V90", "zone": "north"})
    else:
        st_mod, _ = modify("vessel", "V-90", {"name": "Renamed V90", "zone": "north"})
    st, devices = catalog()
    steps["S6_modify"] = {"status": st_mod, "V-90": find(devices, "V-90")}

    steps["ids"] = {"count_after_register": count_after_register}
    with open(args.out, "w", encoding="utf-8") as f:
        json.dump(summary, f, ensure_ascii=False, indent=1)
    print(json.dumps(summary, ensure_ascii=False, indent=1)[:8000])


if __name__ == "__main__":
    main()
