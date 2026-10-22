# APIServer Network Proxy

## Rules for contributors
- You are responsible for everything your agent submits. Read the "Using AI" section of `COLLABORATING.md`.
- Say in the PR description that you used AI, and how. Don't add AI co-author or assisted-by trailers. Submit commits and PRs from your own account. We can't accept contributions from agent or bot accounts (a human must sign the contribution agreement).
- Before asking for review, review your own diff. For substantial changes, also have a fresh agent session review it adversarially, then validate its findings and fix the confirmed issues.
- **Be brief.** Code comments, commit messages, PR descriptions and issues should be short, plain English, and read the way an experienced maintainer writes by hand. Reviewer time and agent context windows are limited.
  - `COLLABORATING.md` doesn't allow AI-generated commit messages, because they have usually been low quality and unreviewed. If you are asked to write one anyway, hold it to the same standard, and the person submitting it must review it.
  - Use the real identifiers from the code. Don't coin new names, and avoid noun stacks like "the front-end request mux http connect buffer".
  - Please use active voice: "evicts" instead of "performs eviction".
  - Please put things in priority order. Make sure the key important ideas come first.
  - Explain why only when the code doesn't make it obvious, in a clause or a line.
  - Avoid:
    - em-dashes, and chains of semicolons
    - headings, bold, or test plans in commit messages
    - epigrams and contrast pairs, like "X, not Y" or "So treat ENOENT as what it is."
    - numbers added for color, like "measured at a flat 51ms"
    - giving code motives or editorializing about it, like "wants", "decides", "is responsible for", "lands", "owns", or "X is what makes Y". Plain active voice is fine: "the reaper collects" is how Go doc comments read.
    - filler words: load-bearing, footgun, surfaced, robust, comprehensive, seamless, leverage
    - "No behavior change" unless the diff is a pure refactor
    - long sentences that pile up clauses, or put the subject after a long lead-in

### Scope

- Keep each PR to one change. Send refactors, cleanups and follow-ups as separate PRs. For how to batch small fixes, see "Sizing PRs for review" in `CONTRIBUTING.md`.
- Follow "Contribution process" in `CONTRIBUTING.md`: open an issue first for anything beyond a small fix, and search open PRs and issues before you start.
- Start the PR description with why the change is needed, or link the issue. Only write "Fixes #N" if the PR fully fixes it, and keep the description in sync with the final diff.
- When updating a PR, keep rebases and other changes in separate pushes, so reviewers can see what changed:
  - To rebase, push only the rebase.
- After a rebase, check your diff against what main renamed or rewrote since you branched: packages, the module path, helpers you call.

### Code

- Search the repo and its dependencies before writing a helper, cache, client, flag set or test fixture. Reuse or extend what exists, and handle a case the way nearby code already does.
- Build only what the change needs. Don't add options, interfaces, wrappers, flags, exports or API fields for later. Prefer a sensible default to a new knob. Delete code your change makes unused.
- Pass dependencies through constructors or functional options. Don't add callback hooks, setters or package globals just so tests can swap behavior.
- Fail loudly. Ignore only the specific errors you expect, like ENOENT. Don't guard against states the code can't reach, and don't fall back silently to an insecure or no-op mode.
- Comment the final state of the code. Bugs that only existed while you were writing the change, and links to planning docs outside the repository, are noise to the next reader.
- Check every claim in comments, docs and the PR description against the code. Don't claim a guarantee the code doesn't enforce. When you change behavior, update the comments, docs and scripts elsewhere that describe it. Link to existing docs instead of copying them.
- Find the root cause before fixing a symptom. Back performance claims with a measurement.
- When adding a new flag or feature, please keep the existing behavior as the backward compatible default.
- konnectivity-client/** is a library consumed by kubernetes.
  - Please put konnectivity-client/** changes into a separate PR from change elsewhere in the code base.
  - konnectivity-client/go.mod should track with https://github.com/kubernetes/kubernetes/blob/master/go.mod at the oldest still supported release from https://kubernetes.io/releases/

### Tests

- Test new behavior in ways that help future changes. Update tests when you change existing behavior.
- Make sure each new test fails without your change. Prefer adding rows to an existing table test.
- Don't use fixed sleeps or wall-clock waits. Run new tests locally with `-count=3 -shuffle=on`.

## Project Overview

APIServer Network Proxy is a project which allows a system like the Kubernetes APIServer to call into 
a data plane when there is no route available. It does this by relying on systems in the data plane
being able to call into the system. The APIServer Network Proxy (ANP) Agent in the data plane creates 
a tunnel to the ANP Server running with the system. The system can then send its traffic to the ANP
Server, which authenticates the system and then forwards the traffic over the tunnel into the data plane. 

