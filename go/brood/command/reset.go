package command

var resetSpec = spec("reset", "Replace an actor's container with a new generation; the workspace is destroyed and cloned again from the actor origin when one is configured. Execution requires explicit confirmation.", false, true, true,
	requiredText("name"), confirmArg())
