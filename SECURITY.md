# Security policy

foliot has one maintainer, Abhijeet Halder, and no security team or bug bounty.
Everything promised below is something one maintainer can keep.

## Reporting a vulnerability

Do not report a vulnerability anywhere public.
Report it privately through GitHub: open this repository's **Security** tab and choose **Report a vulnerability**, or go straight to <https://github.com/Abhijeet34/foliot/security/advisories/new>.
That opens a draft advisory only the maintainer can see, and the two of you discuss it there in private until a fix ships.

A useful report names the version or commit you tested, your operating system and runtime, the steps that reproduce the problem, and what an attacker ends up with.
A proof of concept helps, and it is never required.

## What happens next

- An acknowledgement within 7 days of the report arriving.
- An assessment within 30 days of that acknowledgement: whether it reproduces, how severe it is, and whether a fix is coming.
- A fix as soon as it is ready, with credit to you if you want it.

If either date is going to slip, you are told before it passes.

## Supported versions

foliot has no release yet.
The head of `main` is the only supported version, so report against a commit on it.

## What makes a report actionable

A report is actionable when it shows a path through this project's own code.
Scanner output, or an advisory against a dependency, with no demonstrated path through foliot is worth a note but is not a vulnerability in it.
