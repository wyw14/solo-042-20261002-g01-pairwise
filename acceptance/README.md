solo-042-G01 side B acceptance evidence (2026-10-02)

Scripts and result files produced by the independent acceptance session
(Windows + Docker Desktop isolated copies of the product commit).

How they were run (inside the acceptance container, product snapshot at /workspace):
- g01_phase1.py / g01_phase2.py: registration, conflict rejection, fill occupancy,
  modify + restart reload (phase2 runs after a process restart).
- g01b_deep.py: same-zone circuit registration, valve conflicts, reserved-circuit
  modify/deactivate, repeated deactivation, deactivated-record modify, bound-probe guard.
- g01b_race.py: concurrent deactivation vs fill start on circuit and vessel
  (violation = device deactivated while an active fill occupies it).

Result files under results/ are the unmodified JSON captures of those runs.
