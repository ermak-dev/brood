package command

var rmSpec = spec("rm", "Delete an agent and its container. Requires confirmation.", false, true, true,
	requiredText("name"), confirmArg())
