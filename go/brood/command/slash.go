package command

var slashSpec = spec("slash", "Send a slash command to an actor session.", false, false, true,
	requiredText("name"), requiredText("command"))
