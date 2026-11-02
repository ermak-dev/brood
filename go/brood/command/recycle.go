package command

var recycleSpec = spec("recycle", "Restart Claude on a cleaned Git workspace inside the same container; untracked and ignored files are removed and explicit confirmation is required.", false, true, true,
	requiredText("name"), confirmArg())
