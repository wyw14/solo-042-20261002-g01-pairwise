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
        def register(kind, payload):
            body = dict(payload)
            body["type"] = kind
            return request("POST", "/api/devices", body)

        def modify(dev_id, payload):
            return request("PUT", f"/api/devices/{dev_id}", payload)

        def deactivate(dev_id):
            return request("POST", f"/api/devices/{dev_id}/deactivate", {})
    else:
        kind_path = {"vessel": "vessels", "probe": "probes", "circuit": "circuits"}

        def register(kind, payload):
            return request("POST", f"/api/catalog/{kind_path[kind]}", payload)

        def modify(dev_id, payload):
            return request("POST", f"/api/catalog/circuits/{dev_id}", payload)

        def deactivate(dev_id):
            last = (-1, {})
            for kind in ("vessels", "probes", "circuits"):
                st, body = request("POST", f"/api/catalog/{kind}/{dev_id}/deactivate", {})
                last = (st, body)
                if st != 404:
                    return last
            return last

    def catalog():
        path = "/api/devices" if args.side == "A" else "/api/catalog"
        st, body = request("GET", path)
        devices = body.get("devices") or body.get("items")
        if devices is None:
            devices = []
            for key in ("vessels", "probes", "circuits"):
                devices.extend(body.get(key) or [])
        return st, devices

    def find(devices, dev_id):
        for d in devices:
            if d.get("id") == dev_id:
                status = d.get("status")
                if status is None and "active" in d:
                    status = "active" if d.get("active") else "deactivated"
                return {"id": d.get("id"), "status": status, "zone": d.get("zone"),
                        "valve_id": d.get("valve_id"), "vessel_id": d.get("vessel_id")}
        return None

    summary = {"side": args.side, "reviewer": "Codex acceptance session 2026-10-02",
               "tier": "normal", "fixture": "g01b_deep", "steps": {}}
    steps = summary["steps"]

    for _ in range(60):
        st, _ = request("GET", "/healthz")
        if st == 200:
            break
        time.sleep(0.5)

    st, devices = catalog()
    steps["D0_baseline"] = {"status": st, "count": len(devices)}

    def circuit_body(dev_id, zone, valve):
        if args.side == "A":
            return {"id": dev_id, "name": "Catalog " + dev_id, "zone": zone, "valve_id": valve, "enabled": True}
        return {"id": dev_id, "zone": zone, "valve_id": valve}

    # D1: a second active circuit in a zone that already has one
    st_z1, _ = register("circuit", circuit_body("C-Z1", "north", "VALVE-Z1"))
    st, devices = catalog()
    steps["D1_zone_duplicate"] = {"register_status": st_z1, "present": find(devices, "C-Z1") is not None,
                                  "count": len(devices)}
    if st_z1 == 201:
        steps["D1_zone_duplicate"]["cleanup_deactivate"] = deactivate("C-Z1")[0]
        time.sleep(0.2)

    # D2: a second circuit reusing another circuit's valve
    st_z2, _ = register("circuit", circuit_body("C-Z2", "west", "VALVE-A"))
    st, devices = catalog()
    steps["D2_valve_duplicate"] = {"register_status": st_z2, "absent": find(devices, "C-Z2") is None,
                                   "count": len(devices)}

    # D3: target circuit that sorts first, so the fill selects it
    st_c1, _ = register("circuit", circuit_body("C-01", "west", "VALVE-01"))
    st, devices = catalog()
    steps["D3_register_target"] = {"register_status": st_c1, "state": find(devices, "C-01")}

    # D4: start a fill; record which circuit it took
    st_fill, fill = request("POST", "/api/fill-sessions", {"vessel_id": "V-03", "liters": 1.0})
    target = fill.get("circuit_id") or (fill.get("session") or {}).get("circuit_id")
    fid = fill.get("id") or (fill.get("session") or {}).get("id")
    steps["D4_fill"] = {"status": st_fill, "circuit": target}
    st, devices = catalog()
    before = find(devices, target) if target else None

    # D5: modify zone/valve of the circuit while it is reserved by the active fill
    if target:
        st_mod, _ = modify(target, {"zone": "west2", "valve_id": "VALVE-02"})
        st, devices = catalog()
        steps["D5_modify_reserved"] = {"status": st_mod, "after": find(devices, target)}

    # D6: deactivation of the reserved circuit must be refused
    steps["D6_deactivate_reserved"] = {"status": deactivate(target)[0]} if target else {}

    # D7: abort and release
    if fid:
        steps["D7_abort"] = {"status": request("POST", f"/api/fill-sessions/{fid}/abort", {"reason": "deep probe cleanup"})[0]}
        time.sleep(0.4)

    # D8: restore original zone/valve after the fill released the circuit
    if target and before:
        st_mod2, _ = modify(target, {"zone": before["zone"], "valve_id": before["valve_id"]})
        steps["D8_modify_after_release"] = {"status": st_mod2}

    # D9/D10: idle deactivate, then a second deactivate of the same device
    st_d1 = deactivate(target)[0] if target else None
    steps["D9_deactivate_idle"] = {"status": st_d1}
    st_d2 = deactivate(target)[0] if target else None
    st, devices = catalog()
    steps["D10_deactivate_again"] = {"status": st_d2, "state": find(devices, target) if target else None}

    # D11: modify a deactivated device
    st_mod3 = modify(target, {"zone": "west3", "valve_id": "VALVE-03"})[0] if target else None
    st, devices = catalog()
    steps["D11_modify_deactivated"] = {"status": st_mod3, "state": find(devices, target) if target else None}

    # D12: a vessel with a bound active probe cannot be deactivated
    register("vessel", {"id": "V-Z1", "name": "Catalog V-Z1", "zone": "north"})
    probe_body = ({"id": "P-Z1", "name": "Catalog P-Z1", "probe_kind": "level", "vessel_id": "V-Z1"}
                  if args.side == "A" else {"id": "P-Z1", "kind": "level", "vessel_id": "V-Z1"})
    register("probe", probe_body)
    st_vd, _ = deactivate("V-Z1")
    st, devices = catalog()
    steps["D12_vessel_with_bound_probe"] = {"deactivate_status": st_vd, "vessel": find(devices, "V-Z1")}
    steps["D12_cleanup"] = {"probe_deactivate": deactivate("P-Z1")[0], "vessel_deactivate": deactivate("V-Z1")[0]}

    # D13: binding a new probe to the now-deactivated vessel must be refused
    probe_body2 = ({"id": "P-Z2", "name": "Catalog P-Z2", "probe_kind": "level", "vessel_id": "V-Z1"}
                   if args.side == "A" else {"id": "P-Z2", "kind": "level", "vessel_id": "V-Z1"})
    st_bad = register("probe", probe_body2)[0]
    st, devices = catalog()
    steps["D13_bind_deactivated_vessel"] = {"register_status": st_bad, "absent": find(devices, "P-Z2") is None}

    with open(args.out, "w", encoding="utf-8") as f:
        json.dump(summary, f, ensure_ascii=False, indent=1)
    print(json.dumps(summary, ensure_ascii=False, indent=1)[:9000])


if __name__ == "__main__":
    main()
