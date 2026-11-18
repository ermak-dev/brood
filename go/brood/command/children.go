package command

func childParentArgs(extra ...Arg) []Arg {
	return append([]Arg{
		{Name: "parent_sequence", Type: "integer", Required: true, Min: number(1), Max: number(9007199254740991)},
		{Name: "parent_request_id", Type: "string", Required: true, MaxLength: 4096},
	}, extra...)
}

var childCreateSpec = spec("child_create", "Queue one durable role-addressed child of your current graph step. The node's childJoin definition owns mode/quorum; absent policy means all. Legacy mode/quorum arguments are ignored. Exact request retries never resend; existing allocations only, no provisioning or workspace attestation.", false, false, true, childParentArgs(
	Arg{Name: "request_id", Type: "string", Required: true, MaxLength: 128},
	Arg{Name: "role", Type: "string", Required: true, MaxLength: 63},
	Arg{Name: "task", Type: "string", Required: true, MaxLength: 4096},
	Arg{Name: "total", Type: "integer", Required: true, Min: number(1), Max: number(64)},
	choice(Arg{Name: "mode", Type: "string", Default: "all", Description: "Legacy compatibility field, ignored; mode belongs to the node's childJoin definition"}, "all", "any", "quorum"),
	Arg{Name: "quorum", Type: "integer", Default: 0, Min: number(0), Max: number(64), Description: "Legacy compatibility field, ignored; quorum belongs to the node's childJoin definition"},
)...)
var childSpawnSpec = spec("child_spawn", "Enqueue one durable ephemeral FIFO job for your current graph step. The node owns join policy. Exact retries return the original job; this does not test capacity, allocate, create, start or deliver a child.", false, false, true, childParentArgs(
	Arg{Name: "request_id", Type: "string", Required: true, MaxLength: 128},
	Arg{Name: "task", Type: "string", Required: true, MaxLength: 4096},
	Arg{Name: "total", Type: "integer", Required: true, Min: number(1), Max: number(64)},
	Arg{Name: "pool_uid", Type: "string", Required: true, MaxLength: 128},
)...)
var childJobSpec = spec("child_job", "Read retained ephemeral job facts as its exact current authorized parent. Queuing is not child registration, delivery, a report or physical success; no aggregate execution state is inferred.", true, false, true,
	Arg{Name: "job_id", Type: "string", Required: true, MaxLength: 128})
var childGetSpec = spec("child_get", "Read one currently authorized child task, delivery evidence, independent outcome and full bounded report. Query success does not imply task completion.", true, false, true,
	Arg{Name: "child_id", Type: "string", Required: true, MaxLength: 128})
var childJoinSpec = spec("child_join", "Evaluate your child group, optionally wait up to 15 seconds, and let the server compute the parent outcome/report only after every obligation settles. Early any/quorum readiness never releases running work.", false, false, true, childParentArgs(integer("wait_seconds", 0, 0, 15))...)
var childCancelSpec = spec("child_cancel", "Cancel only your queued or demonstrably not-sent child intent. Running or uncertain external effects remain held until completion or reconciliation.", false, true, true, childParentArgs(Arg{Name: "request_id", Type: "string", Required: true, MaxLength: 128})...)
var childCompleteSpec = spec("child_complete", "Report your exact child task's independent outcome and report. Child identity is distinct from graph-step identity; this never completes the parent directly.", false, false, true,
	Arg{Name: "child_id", Type: "string", Required: true, MaxLength: 128},
	Arg{Name: "request_id", Type: "string", Required: true, MaxLength: 128},
	choice(Arg{Name: "outcome", Type: "string", Required: true}, "success", "fail"),
	Arg{Name: "report", Type: "string", MaxLength: 16384})
