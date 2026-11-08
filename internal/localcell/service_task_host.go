package localcell

// ValidateTaskHost reads an existing installation and its service descriptor
// before a scheduler-started executable selects the local runtime. It neither
// prepares a service nor adopts an installation. Run repeats these checks before
// acquiring its lease, so a descriptor replacement cannot bypass host ownership.
func (c *Cell) ValidateTaskHost(generation string) (ServiceDescriptor, error) {
	return c.validateTaskHost(generation, validateTaskHostOwner)
}

func (c *Cell) validateTaskHost(generation string, owner func(string) error) (ServiceDescriptor, error) {
	d, err := c.ServiceDescriptor()
	if err != nil {
		return ServiceDescriptor{}, err
	}
	if d.Adapter != "windows-logon-task" || generation == "" || d.Payload.Generation != generation {
		return ServiceDescriptor{}, ErrServiceOwnershipConflict
	}
	if err := owner(d.UID); err != nil {
		return ServiceDescriptor{}, err
	}
	if err := validateServiceHost(d); err != nil {
		return ServiceDescriptor{}, err
	}
	return d, nil
}
