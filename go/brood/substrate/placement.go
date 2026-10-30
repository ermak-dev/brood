package substrate

import (
	"fmt"
	"slices"
	"strings"

	ateapi "go.ate.dev/substrate/api/ateapi/v1"
)

// PlacementConstraints contains effective constraints for a failed native
// placement. Limits come from the template, not current actor usage. Both
// selectors must match. SandboxClass and RequiredNodes, if known, must come from
// native snapshot constraints, not guesses from pool names or template images.
// Runtime compatibility is intentionally not reproduced here.
type PlacementConstraints struct {
	Limits           *ateapi.Resources
	TemplateSelector *ateapi.Selector
	ActorSelector    *ateapi.Selector
	SandboxClass     string
	RequiredNodes    []string
}

// PlacementReasonCode identifies a visible blocker without parsing text.
type PlacementReasonCode string

// Visible placement blockers, independent of the native API's error codes.
const (
	PlacementRegistration     PlacementReasonCode = "incomplete registration"
	PlacementInactive         PlacementReasonCode = "worker is not active"
	PlacementSandboxClass     PlacementReasonCode = "snapshot sandbox class mismatch"
	PlacementNode             PlacementReasonCode = "worker is not on a required node"
	PlacementTemplateSelector PlacementReasonCode = "template selector mismatch"
	PlacementActorSelector    PlacementReasonCode = "actor selector mismatch"
	PlacementActorSlots       PlacementReasonCode = "insufficient actor slots"
	PlacementCPU              PlacementReasonCode = "insufficient CPU"
	PlacementMemory           PlacementReasonCode = "insufficient memory bytes"
)

// PlacementReason describes a blocker. Required and Available are meaningful
// only for CPU (millicores), memory (bytes), and actor slots. Available can be
// negative when a worker is overcommitted. State is set for PlacementInactive.
type PlacementReason struct {
	Code      PlacementReasonCode
	Required  int64
	Available int64
	State     ateapi.WorkerState
}

// WorkerPlacement lists all visible blockers for one supplied worker. Empty
// Reasons means no visible blocker, never that native placement will succeed.
type WorkerPlacement struct {
	Name    string
	Reasons []PlacementReason
}

// PlacementDiagnostic is an advisory snapshot, not an error, schedule, or
// reservation. It contains global worker information: callers must authorize
// administrative access before exposing it, including its String output. It
// deliberately does not implement error or wrap APIError, which ordinary API
// paths may expose verbatim. NoWorkers refers only to the supplied snapshot.
type PlacementDiagnostic struct {
	NoWorkers bool
	Workers   []WorkerPlacement
	Unchecked string
}

// DiagnosePlacement explains visible blockers after a native no-worker-room
// failure. It performs no I/O and never changes workers or constraints. The
// caller must supply a complete worker listing; a failed or partial listing is
// not evidence of an empty fleet. Inputs are expected to be native-validated.
//
// Checks follow SDK 4ebd668d scheduling.Registered, Applies, and checkRoom.
// Unlike a scheduler, this function keeps all reasons and never selects a worker.
// Runtime compatibility, fidelity relaxation, and constraints not supplied here
// remain unknown. Snapshots can race native allocation; the native failure must
// remain the operation's outcome even if no visible blocker is found.
func DiagnosePlacement(workers []*ateapi.Worker, c PlacementConstraints) PlacementDiagnostic {
	result := PlacementDiagnostic{
		NoWorkers: len(workers) == 0,
		Unchecked: "runtime compatibility and constraints not supplied by the caller were not evaluated; native placement remains authoritative",
	}
	for _, w := range workers {
		entry := WorkerPlacement{Name: w.GetMetadata().GetName()}
		add := func(code PlacementReasonCode) { entry.Reasons = append(entry.Reasons, PlacementReason{Code: code}) }
		status := w.GetStatus()
		classes := status.GetSandboxClasses()
		class := ""
		if len(classes) > 0 {
			class = classes[0].GetSandboxClass()
		}
		if status.GetRegisteredEpoch() == 0 || class == "" {
			add(PlacementRegistration)
		}
		if status.GetState() != ateapi.WorkerState_WORKER_STATE_ACTIVE {
			entry.Reasons = append(entry.Reasons, PlacementReason{Code: PlacementInactive, State: status.GetState()})
		}
		if c.SandboxClass != "" && class != c.SandboxClass {
			add(PlacementSandboxClass)
		}
		if len(c.RequiredNodes) > 0 && !slices.Contains(c.RequiredNodes, w.GetNodeName()) {
			add(PlacementNode)
		}
		if !placementLabelsMatch(w.GetLabels(), c.TemplateSelector) {
			add(PlacementTemplateSelector)
		}
		if !placementLabelsMatch(w.GetLabels(), c.ActorSelector) {
			add(PlacementActorSelector)
		}
		capacity, used := status.GetCapacity(), status.GetAllocated()
		if used.GetActors() >= capacity.GetActors() {
			entry.Reasons = append(entry.Reasons, PlacementReason{Code: PlacementActorSlots, Required: 1, Available: int64(capacity.GetActors()) - int64(used.GetActors())})
		}
		// SDK checkRoom пропускает compute целиком только при двух нулевых limits.
		if c.Limits.GetCpuMillicores() != 0 || c.Limits.GetMemoryBytes() != 0 {
			for _, d := range []struct {
				code       PlacementReasonCode
				want, free int64
			}{
				{PlacementCPU, c.Limits.GetCpuMillicores(), capacity.GetCpuMillicores() - used.GetCpuMillicores()},
				{PlacementMemory, c.Limits.GetMemoryBytes(), capacity.GetMemoryBytes() - used.GetMemoryBytes()},
			} {
				if d.free < d.want {
					entry.Reasons = append(entry.Reasons, PlacementReason{Code: d.code, Required: d.want, Available: d.free})
				}
			}
		}
		result.Workers = append(result.Workers, entry)
	}
	return result
}

func placementLabelsMatch(labels map[string]string, selector *ateapi.Selector) bool {
	for key, want := range selector.GetMatchLabels() {
		if value, ok := labels[key]; !ok || value != want {
			return false
		}
	}
	return true
}

// String formats an administrative-only diagnostic in supplied worker order.
// Worker names are quoted, and neither labels nor runtime attributes are copied
// into the text. This must not replace the original native refusal for users.
func (d PlacementDiagnostic) String() string {
	var parts []string
	if d.NoWorkers {
		parts = append(parts, "no workers in supplied snapshot")
	}
	for _, w := range d.Workers {
		var reasons []string
		for _, r := range w.Reasons {
			text := string(r.Code)
			switch r.Code {
			case PlacementCPU, PlacementMemory, PlacementActorSlots:
				text = fmt.Sprintf("%s (required %d, available %d)", text, r.Required, r.Available)
			case PlacementInactive:
				text = fmt.Sprintf("%s (%s)", text, r.State)
			}
			reasons = append(reasons, text)
		}
		if len(reasons) == 0 {
			reasons = append(reasons, "no visible blocker")
		}
		parts = append(parts, fmt.Sprintf("worker %q: %s", w.Name, strings.Join(reasons, ", ")))
	}
	parts = append(parts, d.Unchecked)
	return strings.Join(parts, "; ")
}
