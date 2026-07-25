export class AccountLifecycleBusyError extends Error {
  constructor() {
    super("account lifecycle operation already in progress");
    this.name = "AccountLifecycleBusyError";
  }
}

// Durable Object requests can interleave at await boundaries. run() claims
// the object synchronously before invoking the async operation, so one account
// can never have two evacuation/restore state machines active at once.
export class AccountLifecycleFence {
  constructor() {
    this.busy = false;
  }

  async run(operation) {
    if (this.busy) {
      throw new AccountLifecycleBusyError();
    }
    this.busy = true;
    try {
      return await operation();
    } finally {
      this.busy = false;
    }
  }
}
