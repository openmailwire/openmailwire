# Security policy

Please report vulnerabilities privately through GitHub's **"Report a vulnerability"** button on this repository's Security tab. Do not open a public issue.

Include:

- what an attacker can do;
- the steps to reproduce;
- the version or commit.

You will get an acknowledgement within 3 working days. We will agree on a disclosure date with you.

Areas of particular interest:

- authentication between apps, workers and the control plane;
- data that crosses projects;
- header injection into outgoing mail;
- anything that lets one project send as another project's domain.
- server-side request forgery through webhook endpoints.

## Verifying releases

Release archives, `checksums.txt` and the container image carry signed build provenance (SLSA, signed through Sigstore by the release workflow). Check an archive before you run it:

```sh
gh attestation verify openmailwire_<version>_linux_amd64.tar.gz -R openmailwire/openmailwire
gh attestation verify oci://ghcr.io/openmailwire/openmailwire:<tag> -R openmailwire/openmailwire
sha256sum -c checksums.txt --ignore-missing
```

Each release also includes `provenance.sigstore.json`, the attestation bundle, for verifying offline (`gh attestation verify … --bundle provenance.sigstore.json`).

How releases are built:

- with the newest patch release of the current Go (`check-latest`), never the patch named in `go.mod`, so standard library fixes are in;
- from a clean checkout, with build output outside the source tree (`go version -m` shows `vcs.modified=false`);
- by GitHub Actions pinned to commit SHAs, with the least permissions each job needs;
- `govulncheck` runs on every push and weekly, and fails the build when the code reaches a known vulnerability.

## Hardening notes for operators

- Keep `OMW_SECRET_KEY` (or `OMW_SECRET_KEYS`) outside the database backups, and rotate it with `openmailwire rotate-secrets` (see [docs/production.md](docs/production.md#rotating-secrets)).
- Webhook endpoints are dialed only at global unicast addresses: loopback, private, link-local, shared (CGNAT), documentation and every other IANA special-purpose range are refused, as are NAT64 addresses that wrap a non-public IPv4 address and every 6to4 and Teredo address. Leave `OMW_WEBHOOK_ALLOW_PRIVATE` off in production.
- The SDKs never follow redirects with your API key.

