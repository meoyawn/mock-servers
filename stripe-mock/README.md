# Stripe billing fixture

An in-process Go HTTP handler for billing E2E tests. Start it on a fresh local
port with unique credentials per environment and point the official Stripe SDKs
at its origin; production keeps Stripe's default origin.

Supported boundaries are Customers (including credit balances), Subscriptions,
Checkout Sessions, Customer Portal update confirmations, and Events. Prices come
from `Config.Prices`. Unknown operations fail explicitly.
Resource IDs and idempotency receipts belong to one environment; tests use
separate customers rather than resetting shared state.

Checkout and Portal URLs serve local HTML forms. Submitting a form changes the
provider state and sends an HMAC-signed subscription webhook to the running API
before redirecting. A failed webhook returns 502; resubmitting the same session
redelivers the same event without charging or changing the subscription again.
API-created subscriptions do not automatically deliver webhooks, allowing tests
to send delayed snapshots explicitly and verify reconciliation of current state.

This fixture models a subscription billing contract, not Stripe's card
processing, tax, fraud detection, or automatic payment retry engines.
