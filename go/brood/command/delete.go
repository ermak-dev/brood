package command

var deleteSpec = spec("delete", "Delete an actor; execution requires explicit confirmation.", false, true, true,
	requiredText("name"), confirmArg())
