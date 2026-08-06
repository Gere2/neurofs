# Security policy

## Supported versions

Security fixes are provided for the latest tagged release and the current
`main` branch. Older releases should be upgraded before reporting a problem
that is already fixed in either of those versions.

## Reporting a vulnerability

Please report suspected vulnerabilities privately through
[GitHub Security Advisories](https://github.com/Gere2/neurofs/security/advisories/new).
Include the affected version or commit, impact, reproduction steps and any
suggested mitigation. Do not open a public issue for an undisclosed
vulnerability, and do not include real credentials or private repository
contents in the report.

## Security defaults

- `neurofs ui` and `neurofs proxy` bind to `127.0.0.1:7777` by default and
  sandbox file access to the selected repository root.
- A non-loopback bind requires both `--allow-remote` and an authentication
  token. Prefer `NEUROFS_UI_TOKEN` over `--auth-token` so the token is not
  exposed in process arguments. Remote API clients must send it in the
  `X-NeuroFS-Token` header. Use a trusted network boundary and TLS-terminating
  reverse proxy when traffic leaves the local machine.
- Cloud embeddings are opt-in. Set `NEUROFS_EMBEDDING_PROVIDER` explicitly to
  `openai`, `gemini` or `voyage` before repository text is sent to that
  provider; merely setting an API key does not enable cloud transfer. Without
  an explicit provider, NeuroFS uses a reachable Ollama service at
  `OLLAMA_HOST` (`http://localhost:11434` by default) or the deterministic
  mock. Treat a non-loopback `OLLAMA_HOST` as an explicit data-export choice.
- The local index, audit bundles and response evidence can contain source-code
  fragments. Protect the repository workspace and exclude sensitive generated
  paths with `.neurofsignore`.
