package command

var tailSpec = spec("tail", "Read recent actor output, optionally filtered by text.", true, false, true,
	requiredText("name"), integer("lines", 40, 1, 10000), optionalText("grep", ""))
