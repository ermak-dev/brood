package command

var wipeSpec = spec("wipe", "Clear a stopped actor's workspace; execution requires explicit confirmation and a stopped actor.", false, true, true,
	requiredText("name"), confirmArg())
