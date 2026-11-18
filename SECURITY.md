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
