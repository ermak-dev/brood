package command

var stopSpec = spec("stop", "Stop an actor while retaining its workspace.", false, false, true, requiredText("name"))
