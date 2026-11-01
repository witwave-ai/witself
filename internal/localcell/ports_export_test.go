package localcell

// SetPortAllocationFailpointForTest exposes only the allocation/probe seam.
func SetPortAllocationFailpointForTest(c *Cell, failpoint func(Ports)) {
	c.afterPortAllocation = failpoint
}
