package command

var landingSpec = spec("landing", "Inspect or change landing coordination for an actor and request.", false, false, true,
	choice(optionalText("action", "show"), "show", "take", "give"), optionalText("name", ""), optionalText("request_id", ""))
