"""The Pagerless evaluation harness.

The harness runs every Scenario against a running Pagerless stack, approves the
Proposal as the dedicated eval-approver user, and scores the run by deterministic
code against the answer key in docs/scenarios.md. It is a development tool: it
never calls a model itself, and the agent's provider is whatever the stack is
configured with.
"""
