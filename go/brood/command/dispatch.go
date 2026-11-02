package command

var dispatchSpec = spec("dispatch", "Reserve a target allocation and send a task, or inspect/release your exact request; a lease does not prove a clean workspace.", false, false, true,
	requiredText("to"), Arg{Name: "message", Type: "string", Positional: true, Default: "", MaxLength: 262144}, requestIDArg(), choice(optionalText("action", "send"), "send", "show", "release"))

func requestIDArg() Arg { a := requiredText("request_id"); a.Positional = false; return a }
