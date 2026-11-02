package command

var updateSpec = spec("update", "Request an actor profile update; current immutable managed profiles refuse this operation without changing the allocation.", false, false, true, requiredText("name"))
