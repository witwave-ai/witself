# Validation for research documentation

Run `make check-local` before a push. It refreshes `origin/main`, checks the
complete branch and all staged, unstaged, and untracked changes, and selects the
required validation. Research-only changes can now select `documentation`.
There is no manual skip flag, approval label, or admin-merge requirement.

## Initial scope

The first allowlist contains non-hidden, direct-child Markdown files under
`docs/research/local-cell/` and the exact path `docs/README.md`. Only ordinary
non-executable file additions and modifications qualify. Deletions, renames,
mode/type changes, symlinks, unknown paths, and mixed changes select the full
application gate. Adding another prose area requires reviewing its consumers
and changing the checked allowlist, which itself requires full validation.

The extension alone does not establish that a file is ordinary documentation.
Legal publications, generated feature status, embedded support policy, command
contracts, operational runbooks, integration assets, and infrastructure READMEs
retain their existing validation. The brochure-only and verified-release-roll
local gates keep their own eligibility rules. A mixture of research and
brochure changes is an application/mixed change in this first version.

## Shared checks

`make check-docs` runs the validator's and runner's regression tests, then checks
the audited document set for conflict markers, valid text, local file links,
Markdown heading anchors, and source-line reference bounds. Reference links,
explicit anchors, images, and fenced/inline code are handled by the parser.
Git checks whitespace in the complete changed document patch, including
untracked additions. Code examples are never executed. External website
availability is not a merge
dependency; external URLs are counted and not fetched.

If the complete change touches `docs/README.md`, the same command also runs:

```sh
go test ./featurestatus -run '^TestRunbookDocsIndexes$' -count=1
```

This preserves the existing public-runbook index contract. It does not start
PostgreSQL, but cold Go test compilation can require module downloads. A
research-only change requires no Go invocation, database, Docker, Pulumi,
Cloudflare Worker installation, or provider matrix. Missing comparison evidence
conservatively includes the index contract rather than omitting it.

A valid source-line reference proves that the cited range exists, not that the
claim is accurate. Reviewers must still check technical claims, distinguish
historical assessments from current behavior, and review snippets and links.

The automatic local selector retains its Git-context and raw-file integrity
checks. Documentation approval also compares HEAD, comparison base, index,
working changes, untracked content, and audited document bytes before and after
the gate. A changed validation state or a failed document check stops approval.
`make check-docs` alone is a focused check, not permission to push a mixed branch.

## GitHub checks and merge evidence

The CI workflow continues to run for every PR and main push. Its classifier
uses the complete PR merge-base comparison or the complete push before/after
comparison, including both sides of renames. Unavailable or unsupported
comparison evidence cannot select the fast path.

The six required check names remain unchanged. Each required wrapper must see
successful classification, successful applicable document validation, and the
exact expected results of its other dependencies. In the documentation lane,
application and website work must be intentionally skipped. A missing, failed,
cancelled, or unexpectedly skipped applicable job cannot yield a successful
wrapper. Application and mixed changes retain their full applicable checks.

Wait for every applicable check to settle at the exact reviewed head, use the
head-matched squash merge, and verify the exact merged commit. The main push
runs the same documentation checks. Do not demand the application lane's full
matrix for a positively classified documentation change, and do not treat an
unexpected skip as successful validation. Main runs remain independent so a
documentation merge cannot cancel validation of another feature.

The separate product-site workflow still publishes only a positively classified
brochure-only main push or its existing authorized manual path. Documentation
changes do not publish a site, release software, deploy infrastructure, or
change account policy.

## Changing or reverting the gate

Changes to the classifier, validator, runner, workflow, tests, Makefile, or
repository instructions use full validation. Classifier and workflow regression
tests cover invalid evidence, unsafe paths, mixed changes, and missing or failed
dependencies. The fast path must not be used to validate a change to itself.

If a defect is found, remove documentation eligibility in a reviewed change;
ordinary application validation remains the conservative fallback. Preserve
failed evidence and fix the checker rather than substituting a locally authored
passing receipt. This gate is validation only, with no production actions.
