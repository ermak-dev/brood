package command

var tailSpec = spec("tail", "Read the recent output of an agent, optionally only the lines that contain a text.", true, false, true,
	requiredText("name"), integer("lines", 40, 1, 10000), optionalText("grep", ""))
