# Field Day Commander Relay security

Report vulnerabilities privately, rather than opening a public issue or pull request. If GitHub offers **Report a vulnerability** on this repository's **Security** tab, use it. Otherwise, contact a maintainer directly using contact information in the project's README or a contributor's GitHub profile.

Include the relay version or commit, your Windows version, steps to reproduce the problem, and what someone could do by exploiting it. A small example helps. Remove connection tokens, enrollment codes, and personal information from logs or screenshots before sending them.

## What to expect

The maintainer will do their best to acknowledge your report promptly, work with you to understand and validate the issue, and keep you informed while working on a fix. Once it's resolved, you'll be credited in the release notes unless you prefer to remain anonymous.

Field Day Commander Relay is a hobby project maintained in spare time, so response times may vary.

## Supported versions

Security updates go into the latest Field Day Commander Relay release. If you're using an older version, upgrade to the latest release to get all available fixes. Relay releases are separate from Field Day Commander server releases.

## Scope

Report problems in Field Day Commander Relay's code, builds, or installer here. Examples include exposed connection tokens, unsafe handling of logger data, and ways a website could change the relay's settings without your permission.

Logger connections don't have passwords. Options that let other PCs connect or send contacts should only be used on your club's own network. Never forward those ports from the internet. Report flaws in how the relay handles these connections, even though the protocols themselves don't authenticate senders.

For a problem in the separate Field Day Commander server, follow [its security policy](https://github.com/ckoval7/fielddaycommander/blob/main/SECURITY.md). Report flaws in third-party dependencies to their maintainers. If a dependency puts the relay at risk, let the relay maintainer know privately too.
