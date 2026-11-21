package command

var slashSpec = spec("slash", "Send a slash command to the agent's session.", false, false, true,
	requiredText("name"), requiredText("command"))
