package command

var statSpec = spec("stat", "Read actor usage statistics for a bounded number of days.", true, false, true,
	integer("days", 14, 1, 3650), optionalText("name", ""))
