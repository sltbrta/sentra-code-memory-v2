# Gateway service

The gateway is Ouroboros' typed identity and session boundary. Stage 02 provides
the internal local-authority Unix transport; later stages add composition and
company-facing APIs without moving policy or canonical state into this service.

No executable is shipped from this leaf. The Stage 02 integration owner wires
the internal package to the authority kernel and TUI entry point.
