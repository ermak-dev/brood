package command

var resetSpec = spec("reset", "Replace the agent's container with a fresh one. The workspace is destroyed. Requires confirmation.", false, true, true,
	requiredText("name"), confirmArg())
