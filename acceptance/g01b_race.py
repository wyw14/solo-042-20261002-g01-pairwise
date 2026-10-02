import argparse
import http.client
import json
import threading
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
        c = http.client.HTTPConnection(host, port, timeout=20)
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

        def deactivate(dev_id):
            return request("POST", f"/api/devices/{dev_id}/deactivate", {})
    else:
        kind_path = {"vessel": "vessels", "probe": "probes", "circuit": "circuits"}

        def register(kind, payload):
            return request("POST", f"/api/catalog/{kind_path[kind]}", payload)

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
        return devices

    def find(devices, dev_id):
        for d in devices:
            if d.get("id") == dev_id:
                status = d.get("status")
                if status is None and "active" in d:
                    status = "active" if d.get("active") else "deactivated"
                return {"id": d.get("id"), "status": status}
        return None

    def sessions():
        st, body = request("GET", "/api/fill-sessions")
        return body.get("items") or []

    active_states = {"created", "prechecking", "filling", "soaking", "verified"}
    summary = {"side": args.side, "reviewer": "Codex acceptance session 2026-10-02",
               "tier": "normal", "fixture": "g01b_race",
               "note": "violation = device deactivated (200) while an active fill session occupies it",
               "steps": {}}
    steps = summary["steps"]

    for _ in range(60):
        st, _ = request("GET", "/healthz")
        if st == 200:
            break
        time.sleep(0.5)

    # R1: circuit deactivate racing a fill start that may select it
    circ_rounds = []
    circ_violations = 0
    vessels = ["V-03", "V-07", "V-09", "V-12", "V-15", "V-18"]
    for k, delay_ms in enumerate([0, 1, 2, 3, 0.5, 1.5, 2.5, 0, 1, 2, 3, 0.5]):
        vessel = vessels[k % len(vessels)]
        circ = f"C-0R{k:02d}"
        reg_body = ({"id": circ, "name": "Race " + circ, "zone": f"rz{k}", "valve_id": f"VALVE-R{k}"}
                    if args.side == "A" else {"id": circ, "zone": f"rz{k}", "valve_id": f"VALVE-R{k}"})
        st_reg = register("circuit", reg_body)[0]
        result = {}

        def do_deactivate(circ=circ):
            result["deactivate"] = deactivate(circ)[0]

        def do_fill(vessel=vessel, delay_ms=delay_ms):
            if delay_ms:
                time.sleep(delay_ms / 1000.0)
            result["fill"] = request("POST", "/api/fill-sessions", {"vessel_id": vessel, "liters": 1.0})[0]

        t1 = threading.Thread(target=do_deactivate)
        t2 = threading.Thread(target=do_fill)
        t1.start()
        t2.start()
        t1.join()
        t2.join()
        time.sleep(0.1)
        active = [s for s in sessions() if s.get("vessel_id") == vessel and s.get("state") in active_states]
        on_target = [s for s in active if s.get("circuit_id") == circ]
        rec = find(catalog(), circ)
        circ_down = rec is None or rec.get("status") == "deactivated"
        violation = bool(on_target) and circ_down
        if violation:
            circ_violations += 1
        circ_rounds.append({
            "k": k, "vessel": vessel, "circuit": circ, "register": st_reg,
            "deactivate_status": result.get("deactivate"), "fill_status": result.get("fill"),
            "active_sessions": [{"id": s.get("id"), "circuit": s.get("circuit_id"), "state": s.get("state")} for s in active],
            "circuit_after": rec, "violation": violation,
        })
        for s in active:
            request("POST", f"/api/fill-sessions/{s['id']}/abort", {"reason": "race cleanup"})
        time.sleep(0.25)
        deactivate(circ)
        time.sleep(0.15)
    steps["R1_circuit_race"] = {"rounds": circ_rounds, "violations": circ_violations}

    # R2: vessel deactivate racing a fill start on that same vessel
    vessel_rounds = []
    vessel_violations = 0
    for k, delay_ms in enumerate([0, 1, 2, 3, 0.5, 1.5, 2.5, 0, 1, 2]):
        vessel = f"V-R{k:02d}"
        reg_body = {"id": vessel, "name": "Race " + vessel, "zone": "north"}
        if args.side == "A":
            reg_body["type"] = "vessel"
        st_reg = register("vessel", reg_body)[0]
        result = {}

        def do_deactivate(vessel=vessel):
            result["deactivate"] = deactivate(vessel)[0]

        def do_fill(vessel=vessel, delay_ms=delay_ms):
            if delay_ms:
                time.sleep(delay_ms / 1000.0)
            result["fill"] = request("POST", "/api/fill-sessions", {"vessel_id": vessel, "liters": 1.0})[0]

        t1 = threading.Thread(target=do_deactivate)
        t2 = threading.Thread(target=do_fill)
        t1.start()
        t2.start()
        t1.join()
        t2.join()
        time.sleep(0.1)
        active = [s for s in sessions() if s.get("vessel_id") == vessel and s.get("state") in active_states]
        rec = find(catalog(), vessel)
        violation = bool(active) and result.get("deactivate") == 200
        if violation:
            vessel_violations += 1
        vessel_rounds.append({
            "k": k, "vessel": vessel, "register": st_reg,
            "deactivate_status": result.get("deactivate"), "fill_status": result.get("fill"),
            "active_sessions": [{"id": s.get("id"), "circuit": s.get("circuit_id"), "state": s.get("state")} for s in active],
            "vessel_after": rec, "violation": violation,
        })
        for s in active:
            request("POST", f"/api/fill-sessions/{s['id']}/abort", {"reason": "race cleanup"})
        time.sleep(0.25)
        deactivate(vessel)
        time.sleep(0.15)
    steps["R2_vessel_race"] = {"rounds": vessel_rounds, "violations": vessel_violations}

    steps["summary"] = {"circuit_race_violations": circ_violations, "vessel_race_violations": vessel_violations}
    with open(args.out, "w", encoding="utf-8") as f:
        json.dump(summary, f, ensure_ascii=False, indent=1)
    print(json.dumps(steps.get("summary"), ensure_ascii=False))
    print(json.dumps(circ_rounds[:4], ensure_ascii=False, indent=1)[:2500])


if __name__ == "__main__":
    main()
