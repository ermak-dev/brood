package command

var restartSpec = spec("restart", "Restart the proven Claude session inside the same managed container, preserving workspace; this does not reboot or reset the container.", false, true, true, requiredText("name"), confirmArg())
