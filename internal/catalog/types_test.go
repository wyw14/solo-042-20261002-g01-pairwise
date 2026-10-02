package catalog

import (
	"testing"
	"time"

	"github.com/wyw14/cry-98/internal/model"
)

func TestValidateAcceptsDefaultConfig(t *testing.T) {
	if err := validateConfig(DefaultConfig(time.Unix(1, 0))); err != nil {
		t.Fatalf("default config rejected: %v", err)
	}
}

func TestValidateRejectsDuplicateDeviceIDs(t *testing.T) {
	config := DefaultConfig(time.Unix(1, 0))
	config.Devices = append(config.Devices, Device{
		ID: "V-03", Kind: KindProbe, Status: StatusActive,
		ProbeKind: model.ProbeLevel, VesselID: "V-07", UpdatedAt: time.Unix(2, 0),
	})
	err := validateConfig(config)
	if err == nil {
		t.Fatal("duplicate id across kinds was accepted")
	}
}

func TestValidateRejectsDanglingProbeBindings(t *testing.T) {
	config := Config{Version: 1, Devices: []Device{
		{ID: "P-1", Kind: KindProbe, Status: StatusActive, ProbeKind: model.ProbeLevel, VesselID: "V-MISSING"},
	}}
	if err := validateConfig(config); err == nil {
		t.Fatal("probe bound to unknown vessel accepted")
	}

	config = Config{Version: 1, Devices: []Device{
		{ID: "V-1", Kind: KindVessel, Status: StatusActive, Name: "n", Zone: "north"},
		{ID: "V-1", Kind: KindProbe, Status: StatusActive, ProbeKind: model.ProbeLevel, VesselID: "V-1"},
	}}
	if err := validateConfig(config); err == nil {
		t.Fatal("probe binding a non-vessel id accepted")
	}
}

func TestValidateRejectsProbeBoundToDeactivatedVessel(t *testing.T) {
	config := Config{Version: 1, Devices: []Device{
		{ID: "V-1", Kind: KindVessel, Status: StatusDeactivated, Name: "n", Zone: "north"},
		{ID: "P-1", Kind: KindProbe, Status: StatusActive, ProbeKind: model.ProbeLevel, VesselID: "V-1"},
	}}
	if err := validateConfig(config); err == nil {
		t.Fatal("active probe bound to deactivated vessel accepted")
	}
}

func TestValidateRejectsDuplicateValveAssignments(t *testing.T) {
	config := Config{Version: 1, Devices: []Device{
		{ID: "C-1", Kind: KindCircuit, Status: StatusActive, Zone: "north", ValveID: "VLV-1"},
		{ID: "C-2", Kind: KindCircuit, Status: StatusActive, Zone: "south", ValveID: "VLV-1"},
	}}
	if err := validateConfig(config); err == nil {
		t.Fatal("two circuits sharing a valve accepted")
	}
}

func TestValidateRejectsMissingRequiredFields(t *testing.T) {
	cases := []Config{
		{Devices: []Device{{ID: "", Kind: KindVessel, Status: StatusActive}}},
		{Devices: []Device{{ID: "V-1", Kind: KindVessel, Status: StatusActive, Zone: "north"}}},
		{Devices: []Device{{ID: "P-1", Kind: KindProbe, Status: StatusActive, ProbeKind: model.ProbeLevel}}},
		{Devices: []Device{{ID: "P-1", Kind: KindProbe, Status: StatusActive, VesselID: "V-1"}}},
		{Devices: []Device{{ID: "C-1", Kind: KindCircuit, Status: StatusActive, Zone: "north"}}},
		{Devices: []Device{{ID: "X-1", Kind: "weird", Status: StatusActive}}},
	}
	for index, config := range cases {
		if err := validateConfig(config); err == nil {
			t.Fatalf("case %d accepted", index)
		}
	}
}

func TestValidateJoinsEveryProblem(t *testing.T) {
	config := Config{Devices: []Device{
		{ID: "V-1", Kind: KindVessel, Status: StatusActive},
		{ID: "C-1", Kind: KindCircuit, Status: StatusActive},
	}}
	err := validateConfig(config)
	if err == nil {
		t.Fatal("invalid config accepted")
	}
	if joined := err.Error(); joined == "" {
		t.Fatal("empty validation error")
	}
}
