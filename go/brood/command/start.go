package command

var startSpec = spec("start", "Start a stopped agent in its existing container, keeping the workspace. A running agent is left unchanged.", false, false, true, requiredText("name"))
