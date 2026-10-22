---
name: site-generic
description: >-
  Fallback site skill for an HPC user whose institution has no site skill of
  its own: how to find the site-specific facts that the generic slurm-user and
  research-computing skills leave out (login and transfer hosts, accounts and
  two-factor sign-in, partitions and GPU types, Slurm accounts and QOS,
  storage folders, proxy quirks, software modules, the node driver/CUDA
  version) by asking the user,
  reading their institution's documentation, or querying the cluster. Use it
  whenever one of those facts is needed and no `site-<institution domain>`
  skill is installed.
---

# No site skill for this institution

The generic skills (`slurm-user`, `research-computing`) hold no site-specific facts.
Normally a site skill named `site-<institution domain>` (for example
`site-example-edu`) supplies them. This institution has none, so work the
facts out instead of guessing:

1. **Ask the user** for what only they or their institution know: the login
   host they `ssh` to, their account name, which Slurm account to charge, and where their data lives. Offer
   choices when you can (see below), with your best guess first.
2. **Read the documentation** the user points to (their research computing
   center's pages). Quote the page when a fact comes from it.
3. **Query the cluster** once signed in: most facts are discoverable, and a
   discovered value beats a remembered one.

Never invent a host name, account name, partition or storage path. If a fact
can't be found, say so and ask.

Where to look for each fact, and the commands that reveal it:
[References/Finding-site-facts.md](References/Finding-site-facts.md).

To make these facts permanent, an institution can add its own site profile
(`sites/<domain>/`: a site skill plus `Site.yaml`, with one folder per
cluster). The `_cluster-template/` folder next to this
file is a template for such a cluster folder, not part of this skill.
