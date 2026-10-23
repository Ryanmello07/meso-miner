//go:build bulkbench

package connect

const bulkEngineName = "meso"

func bulkDest(id Id) TransferPath { return DestinationId(id) }
