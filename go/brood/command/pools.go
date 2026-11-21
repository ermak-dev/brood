package command

var poolsSpec = spec("pools", "List the Proxmox pools with their state and agent limit. Administrators only.", true, false, false)
var poolDrainSpec = spec("pool_drain", "Stop placing new agents in a pool; agents already in it keep running. Administrators only.", false, false, false, requiredText("name"))
var poolEnableSpec = spec("pool_enable", "Allow new agents to be placed in a drained pool again. Administrators only.", false, false, false, requiredText("name"))
