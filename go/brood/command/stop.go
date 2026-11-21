package command

var stopSpec = spec("stop", "Stop an agent. Its container and workspace are kept.", false, false, true, requiredText("name"))
