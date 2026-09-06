#!/usr/bin/env bash
set -euo pipefail

source_root=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
python3 - "$source_root" <<'PY'
import pathlib
import re
import sys

root = pathlib.Path(sys.argv[1])


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def job(text, name):
    match = re.search(r"^  " + re.escape(name) + r":\n(.*?)(?=^  [a-z][\w-]*:|\Z)", text, re.M | re.S)
    require(match is not None, "missing job " + name)
    return match[1]


def step(text, name):
    match = re.search(r"^      - name: " + re.escape(name) + r"\n(.*?)(?=^      - |\Z)", text, re.M | re.S)
    require(match is not None, "missing step " + name)
    return match[1]


def check(text, workflow):
    native_id = "provider-integration-contract" if workflow == "ci" else "provider-integration-verify"
    native = job(text, native_id)
    aggregate = job(text, "provider-contract-evidence")
    require("fail-fast: false" in native, "all native outcomes run")
    require(re.findall(r"- target: ([\w-]+)\n\s+runner: ([\w.-]+)", native) == [
        ("linux-x64", "ubuntu-latest"), ("linux-arm64", "ubuntu-24.04-arm"),
        ("macos-intel", "macos-15-intel"), ("macos-arm64", "macos-15"),
        ("windows-x64", "windows-latest")], "exact native matrix")
    for name in ("Universal installer artifact smoke", "Windows binary installer smoke",
                 "PowerShell static analysis", "Native platform safety primitives",
                 "Managed instruction platform primitives", "Codex Windows hook contract"):
        step(native, name)
    runner = step(native, "Run provider contracts and retain sanitized outcomes")
    require("shell: bash" in runner, "portable runner shell")
    require('"$reporter" run' in runner and 'go build -o "$reporter" ./tools/provider-contract-evidence' in runner,
            "fixed tool owns test execution and exit")
    require('if [[ "$RUNNER_OS" == Windows ]]; then executable_suffix=".exe"; fi' in runner,
            "Windows preserved installer output")
    require('--binary "$RUNNER_TEMP/witself-installed$executable_suffix" --dist dist' in runner,
            "actual installer archive input")
    require('PROVIDER_TARGET: ${{ matrix.target }}' in runner, "target passed through environment")
    upload = step(native, "Upload sanitized provider cell")
    require("if: always()" in upload, "failed cell retained")
    require("name: provider-contract-cell-${{ matrix.target }}-${{ github.run_id }}-${{ github.run_attempt }}" in upload,
            "cell attempt identity")
    require(re.findall(r"^\s+path: (.+)$", upload, re.M) ==
            ["${{ runner.temp }}/provider-contract-${{ matrix.target }}.json"], "only sanitized cell uploaded")
    require(f"needs: [{native_id}]" in aggregate and "if: always()" in aggregate,
            "aggregate observes every native outcome")
    result = step(aggregate, "Require every native provider job")
    require(f"NATIVE_RESULT: ${{{{ needs.{native_id}.result }}}}" in result and
            'run: test "$NATIVE_RESULT" = success' in result, "native failure cannot be hidden")
    download = step(aggregate, "Download this attempt's provider cells")
    require("pattern: provider-contract-cell-*-${{ github.run_id }}-${{ github.run_attempt }}" in download,
            "current attempt download")
    require("merge-multiple: false" in download and "digest-mismatch: error" in download,
            "no overwrite or digest fallback")
    require(not re.search(r"^\s+(?:github-token|run-id|repository):", download, re.M),
            "current workflow scoped download")
    validation = step(aggregate, "Validate complete provider matrix")
    require('go run ./tools/provider-contract-evidence aggregate' in validation,
            "strict aggregate tool")
    require('--input-dir "$RUNNER_TEMP/provider-contract-cells"' in validation,
            "separate downloaded cell directories")
    for command in (runner, validation):
        for argument in ('--expected-commit "$GITHUB_SHA"', '--repository "$GITHUB_REPOSITORY"',
                         '--run-id "$GITHUB_RUN_ID" --run-attempt "$GITHUB_RUN_ATTEMPT"',
                         '--source-ref "$GITHUB_REF" --release-tag "$release_tag"',
                         '--workflow ' + workflow):
            require(argument in command, "trusted identity argument " + argument)
        require('release_tag=""' in command, "no invented release tag")
        if workflow == "release":
            require('if [[ "$GITHUB_EVENT_NAME" == push ]]; then release_tag="$GITHUB_REF_NAME"; fi' in command,
                    "manual dispatch is not publishing")
        # Interpolation belongs in env, never directly in an executed script.
        script = command.split("run: |", 1)[1]
        require("${{" not in script and "|| true" not in script and "continue-on-error" not in command,
                "no direct context interpolation or ignored failure")
    aggregate_upload = step(aggregate, "Upload validated provider matrix")
    require("name: provider-contract-matrix-${{ github.run_id }}-${{ github.run_attempt }}" in aggregate_upload,
            "aggregate attempt identity")
    require(re.findall(r"^\s+path: (.+)$", aggregate_upload, re.M) ==
            ["${{ runner.temp }}/provider-contract-evidence.json"], "only aggregate JSON uploaded")
    for upload_step in (upload, aggregate_upload):
        require("retention-days: 90" in upload_step and "if-no-files-found: error" in upload_step,
                "retained evidence required")
    for block in (native, aggregate):
        require("secrets." not in block and "continue-on-error" not in block, "credential-free mandatory contracts")
        require(all(re.match(r"[\w/-]+@[0-9a-f]{40}(?:\s+#.*)?$", use)
                    for use in re.findall(r"^\s*-?\s*uses: (.+)$", block, re.M)), "immutable action pins")
    if workflow == "ci":
        gate = job(text, "go")
        require("provider-contract-evidence]" in gate and
                "PROVIDER_EVIDENCE_RESULT: ${{ needs.provider-contract-evidence.result }}" in gate and
                '"$GO_INFRA_RESULT" "$PROVIDER_EVIDENCE_RESULT"; do' in gate,
                "required go check enforces aggregate")
    else:
        publish = job(text, "goreleaser")
        require("provider-integration-verify, provider-contract-evidence," in publish,
                "publication requires aggregate")
        download = step(publish, "Download validated provider release evidence")
        require("name: provider-contract-matrix-${{ github.run_id }}-${{ github.run_attempt }}" in download
                and "digest-mismatch: error" in download, "exact publication aggregate")
        staging = step(publish, "Stage exact tagged provider evidence before signing")
        require("if: github.event_name == 'push'" in staging and "validate-release" in staging,
                "tag evidence revalidated before signing")
        for argument in ('--input "$report" --version "${GITHUB_REF_NAME#v}" --commit "$GITHUB_SHA"',
                         '--repository "$GITHUB_REPOSITORY"',
                         '--run-id "$GITHUB_RUN_ID" --run-attempt "$GITHUB_RUN_ATTEMPT"'):
            require(argument in staging, "trusted publication identity")
        require('cp "$report" evidence/provider-contract-release/provider-contract-evidence.json' in staging,
                "fixed ignored staging path")
        require(publish.index("name: Stage exact tagged") < publish.index("name: Run GoReleaser") <
                publish.index("name: Preserve signed provider evidence") <
                publish.index("name: Verify signed release artifact contract"), "publication order")
        preserve = step(publish, "Preserve signed provider evidence in local artifact set")
        require("if: github.event_name == 'push'" in preserve and
                "cmp evidence/provider-contract-release/provider-contract-evidence.json dist/provider-contract-evidence.json" in preserve,
                "unchanged signed bytes reach verifier")


try:
    canaries = [
        ("matrix omission", "- target: linux-arm64", "- target: unsupported"),
        ("missing neighbor", "name: Codex Windows hook contract", "name: Removed hook gate"),
        ("wrong archive input", '--dist dist', '--dist unrelated'),
        ("lost exit", '"$reporter" run', '"$reporter" obsolete'),
        ("failed upload skipped", "if: always()", "if: success()"),
        ("broad upload", "path: ${{ runner.temp }}/provider-contract-${{ matrix.target }}.json", "path: ./"),
        ("stale attempt", "--run-attempt \"$GITHUB_RUN_ATTEMPT\"", '--run-attempt 1'),
        ("stale downloads", "pattern: provider-contract-cell-*-${{ github.run_id }}-${{ github.run_attempt }}", "pattern: provider-contract-cell-*"),
        ("overwrite cells", "merge-multiple: false", "merge-multiple: true"),
        ("digest mismatch ignored", "digest-mismatch: error", "digest-mismatch: warn"),
        ("native gate bypass", 'run: test "$NATIVE_RESULT" = success', 'run: echo success'),
        ("short retention", "retention-days: 90", "retention-days: 1"),
    ]
    count = 0
    for workflow in ("ci", "release"):
        source = (root / ".github/workflows" / (workflow + ".yml")).read_text()
        check(source, workflow)
        for name, before, after in canaries:
            # Target the native/evidence jobs so older unrelated steps cannot
            # satisfy the fixture mutation accidentally.
            begin = source.index("  provider-integration-")
            prefix, suffix = source[:begin], source[begin:]
            require(before in suffix, "canary has a target: " + name)
            try:
                check(prefix + suffix.replace(before, after, 1), workflow)
            except AssertionError:
                count += 1
                continue
            raise AssertionError("accepted canary " + workflow + ": " + name)
    config = (root / ".goreleaser.yaml").read_text()
    conditional = "glob: '{{ if not .IsSnapshot }}evidence/provider-contract-release/provider-contract-evidence.json{{ end }}'"
    for section in ("checksum", "release"):
        block = re.search(r"^" + section + r":\n(.*?)(?=^[a-z][\w_]*:|\Z)", config, re.M | re.S)
        require(block is not None and block[1].count(conditional) == 1 and
                "name_template: provider-contract-evidence.json" in block[1],
                "tag-only evidence in " + section)
    for gate in (root / "Makefile", root / ".github/workflows/ci.yml", root / ".github/workflows/release.yml"):
        require("bash scripts/test-provider-contract-workflow.sh" in gate.read_text(),
                "regression gate wired into " + gate.name)
except (AssertionError, OSError) as error:
    print("provider contract workflow: FAIL: " + str(error), file=sys.stderr)
    sys.exit(1)
print(f"provider contract workflow: PASS (both workflows and {count} rejection canaries)")
PY
