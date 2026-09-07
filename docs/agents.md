# Agents

## Claude Code authentication on macOS

On macOS, Claude Code stores its credentials in the login Keychain rather than
in `~/.claude/.credentials.json`. The adapter therefore reports
`auth_mode: unknown` even when the Claude Code CLI is authenticated and works.

Here, `unknown` does not mean unauthenticated and does not block the agent.
`ready` is decided by the executable and its advertised capabilities, not by
whether the adapter can observe its authentication state.

Under the auth-truth rule, an unobserved authentication state remains `unknown`.
The adapter must keep reporting `unknown` rather than inferring an authenticated
session from a Keychain entry.
