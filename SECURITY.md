# Security policy

## Supported versions

CloudBurrow is before 1.0. Security fixes go to `main` and ship in the next release; they are
not backported to earlier releases.

| Version | Supported |
|---|---|
| Latest release (currently [v0.1.0](https://github.com/cloudburrow/cloudburrow/releases/tag/v0.1.0)) | Yes |
| `main` | Yes — fixed first, released next |
| Any older release | No — upgrade to the latest |

What changed in each release, including security fixes, is in [CHANGELOG.md](CHANGELOG.md).

## Reporting a vulnerability

**Do not open a public issue, discussion or pull request for a vulnerability.**

Report it privately through GitHub's private vulnerability reporting: on the repository's
**Security** tab, choose **Report a vulnerability**
(<https://github.com/cloudburrow/cloudburrow/security/advisories/new>). The report is visible
only to you and the maintainers, and a fix and advisory can be prepared in private from it.

Include what you can of:

- the CloudBurrow version (`cloudburrow version`) and your operating system;
- what an attacker can do, and from where (the same machine, the local network, a container in
  the cluster, a malicious release artifact, …);
- steps to reproduce, or a proof of concept.

If private reporting is not available to you, open an issue that says only that you have a
security report and asks a maintainer to contact you. Put no details in it.

## What to expect

- **Acknowledgement within 7 days** of the report.
- **An initial assessment within 14 days**: whether it is accepted as a vulnerability, its
  severity, and a planned fix or the reason it is not one.
- Updates on the report at least every 14 days until it is resolved.
- A fix released, and a GitHub security advisory published crediting you unless you would rather
  not be named. Please give us the chance to release a fix before disclosing publicly; we aim
  to do so within 90 days of the report.

## Scope

CloudBurrow is a local emulator for development and testing, and several things that would be
vulnerabilities in Google Cloud are documented properties here, not bugs
([docs/status.md](docs/status.md)):

- **No IAM enforcement anywhere.** No identity is checked and no policy is evaluated; stored IAM
  policies are never enforced.
- **The generated credentials authorise nothing.** They exist so tooling that insists on
  credentials runs offline ([docs/credentials.md](docs/credentials.md)).
- **Secret Manager is not a secret store, and Cloud KMS is not a security boundary.** Key
  material is stored unencrypted.

Reports we want include, for example:

- anything reachable from beyond the loopback interface by default, or a way past the admin
  API's per-instance token;
- a way for a web page to reach a CloudBurrow port through the developer's browser, including
  DNS rebinding past the Host allowlist every HTTP listener enforces
  ([ADR-0004](docs/adr/0004-local-access-and-no-authentication.md), #676);
- a way to make the installer, the Homebrew formula or the GitHub Action install something
  other than a verified release artifact, or a weakness in how releases are built, attested and
  published;
- CloudBurrow reading or changing host files or Kubernetes resources it does not own, such as
  resource names that escape their directory or namespace;
- secrets or credentials leaking into logs, the console, events or a `cloudburrow diagnose`
  bundle;
- a vulnerable dependency that CloudBurrow's own code actually reaches.
