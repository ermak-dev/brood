package command

var restartSpec = spec("restart", "Restart the agent's session inside the same container, keeping the workspace.", false, false, true, requiredText("name"))
