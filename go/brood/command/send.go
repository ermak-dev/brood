package command

var sendSpec = spec("send", "Send a message to an actor and return delivery status, not an assistant response; optionally wait for an idle notice.", false, false, true,
	requiredText("to"), requiredText("message"), integer("wait_seconds", 0, 0, 300))
