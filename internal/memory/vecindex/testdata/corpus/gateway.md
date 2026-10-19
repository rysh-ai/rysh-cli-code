# Gateway

The gateway is the control plane that hands out token leases to daemons.

## Lease grant

A daemon asks the gateway for a lease before it spends tokens. The grant may be
zero tokens, and a zero grant is still a success, not an error.

## Token ceiling

Each workspace has a daily token ceiling from its plan. The overspend bound is
the ceiling plus daemons times the largest lease.

## Policy document

The policy document is fetched with an ETag. An unchanged policy answers 304,
and a missing policy answers 404 rather than an empty policy.
