# Security policy

## Supported versions

The latest minor release receives security fixes.

## Reporting a vulnerability

Please report vulnerabilities privately through
[GitHub security advisories](https://github.com/yousafkhamza/changefeed/security/advisories/new),
not in public issues. Include the version, the affected component and steps to reproduce.

You will get an acknowledgement within 3 working days and a fix or mitigation plan within 14 days
for confirmed issues. Credit is given in the release notes unless you prefer otherwise.

## Threat model in brief

| Asset | Protection |
|---|---|
| Secret values in cluster objects | Never stored: sensitive keys are masked, Secret-sourced env vars are shown by reference, diffs are scanned for credentials. Helm release Secrets are read for metadata only. |
| Write access to the timeline | Per-source, write-only tokens stored as SHA-256 hashes; GitHub OIDC with repository/environment allow-lists; actor and repository taken from signed claims. |
| Read access | Read-scoped tokens, or `anonymousRead` behind an SSO proxy. |
| Cluster access | get/list/watch only; namespaced Roles when namespaces are listed. |
| Cloud access | None: AWS pushes via EventBridge. |
| Browser | Strict CSP without inline script or style, links restricted to http(s), no third-party assets. |
| Supply chain | SHA-pinned actions, signed images with SBOM and SLSA provenance, attested binaries, govulncheck in CI. |
