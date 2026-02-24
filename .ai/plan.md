What the Control-Plane Is (and Is Not)

It is:

The single source of truth for site lifecycle state.

The orchestrator that derives infra state from DB state.

The policy engine that validates and enforces invariants.

The system that guarantees deterministic provisioning.

It is NOT:

A place that blindly mutates infra.

A collection of imperative steps glued together.

A DB write followed by hope.

Right now, your biggest weakness is implicit assumptions and partial state mutation without lifecycle modeling.

We fix that.

Core Principles of the Control-Plane

1. Single Source of Truth

The database is the canonical state.
Nginx config, cloudflared config, DNS routes — all are derived artifacts.

Never manually mutate infra without being able to regenerate it from DB.

If infra cannot be reconstructed from DB → design flaw.

2. State Machine Over Linear Scripts

Sites must move through defined states.

Example:

CREATED
PROVISIONING
ACTIVE
DOMAIN_PENDING_DNS
DOMAIN_VALIDATED
DOMAIN_ROUTED
ERROR
DELETING

No direct jumps.
Transitions must be explicit and validated.

3. Validate Before Mutate

Never:

Write DB

Then try infra

Then fail

Always:

Validate

Perform infra mutation

Confirm success

Commit DB state

If something fails mid-process:

Rollback infra

Or do not advance state

4. Idempotency Everywhere

Every operation must be safe to retry.

Calling:

regenerateNginx()

addTunnelRoute()

updateCloudflaredConfig()

Multiple times must not break state.

If re-running the same command changes behavior → weak design.

5. Derived Infrastructure Model

Control-plane responsibilities:

Generate full nginx config from DB

Generate cloudflared ingress from DB

Reload services deterministically

Not:

Append lines to config

Mutate partial state

Depend on historical config state

Generate, don’t patch.

6. Observability First

Every lifecycle step must:

Log intent

Log outcome

Log external response

Surface actionable errors

Silent failure is architectural decay.

7. Isolation of Concerns

Control-plane should not:

Know Nginx internals deeply

Know Cloudflare internals deeply

It should delegate through clean interfaces:

type EdgeProvider interface {
ApplySite(site Site) error
}

type TunnelProvider interface {
RouteDomain(domain string) error
}

type DNSValidator interface {
Validate(domain string) error
}

Interfaces reduce future coupling.

What the Control-Plane Does (High-Level Summary)

The control-plane is responsible for:

Managing site metadata and lifecycle state.

Validating user input (domains, resources, ownership).

Generating deterministic infrastructure configs.

Orchestrating:

Nginx routing

Cloudflare tunnel routing

DNS validation

Enforcing invariants:

No ACTIVE site without validated DNS

No custom domain without tunnel route

No config drift between DB and infra

Providing safe rollback paths.

Exposing API endpoints for provisioning, deletion, and mutation.

It is not responsible for:

Serving traffic

Handling requests

Acting as data-plane

It is the control logic, not the runtime.

Critical Gaps in Your Current Control-Plane

Based on your custom domain handler:

DB state is updated before infra success.

No domain validation before activation.

No rollback strategy.

No idempotency guarantees.

No lifecycle state modeling.

Cloudflared config file mutation duplicates state.

No verification that tunnel route actually succeeded.

No reconciliation loop (infra drift detection).

This is fragile.

It works in the happy path.
It collapses under edge cases.

Task List to Strengthen the Control-Plane
Phase 1 – Lifecycle Modeling

Introduce explicit SiteStatus enum

Enforce allowed state transitions

Add domain-specific states

Refactor handlers to transition state only after infra success

Phase 2 – Domain Validation Layer

Implement DNS resolution validation

Verify Cloudflare IP ranges

Verify proxy enabled via API (optional but ideal)

Block activation until validated

Phase 3 – Idempotent Infra Layer

Refactor nginx generation to full-regenerate model

Refactor cloudflared ingress to full-regenerate model

Remove config patching logic

Ensure reloads are safe and atomic

Phase 4 – Transactional Thinking

Implement compensation logic if tunnel fails

Do not mark ACTIVE until infra confirms

Add structured logging per step

Phase 5 – Reconciliation Loop (Advanced but Important)

Background worker that:

Compares DB desired state

Checks nginx/tunnel reality

Repairs drift

This is what makes a control-plane feel professional.
