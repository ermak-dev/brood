package command

var startSpec = spec("start", "Start a stopped actor in its retained container and workspace; a running actor is left unchanged.", false, false, true, requiredText("name"))
