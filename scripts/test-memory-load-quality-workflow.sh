#!/usr/bin/env bash
set -euo pipefail

SOURCE_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
python3 - "$SOURCE_ROOT" <<'PY'
import pathlib
import re
import sys

root = pathlib.Path(sys.argv[1])
path = root / ".github/workflows/memory-load-quality.yml"

def require(condition, name):
    if not condition:
        raise AssertionError(name)


def check(text):
    triggers = re.search(r"^on:\n(.*?)(?=^[^\s#])", text, re.M | re.S)
    require(triggers is not None, "dispatch trigger exists")
    require(re.findall(r"^  ([\w-]+):", triggers[1], re.M) == ["workflow_dispatch"], "dispatch-only triggers")
    permissions = re.search(r"^permissions:\n(.*?)(?=^[^\s#])", text, re.M | re.S)
    require(permissions is not None and permissions[1].strip() == "contents: read", "read-only permissions")
    require(not re.search(r"^[ \t]+(?:permissions|environment):", text, re.M), "no job permissions or protected environment dependency")
    require(not re.search(r"\$\{\{\s*secrets\s*[.\[]", text), "no secrets")
    require("self-hosted" not in text, "no self-hosted runners")
    require(re.findall(r"^\s+runs-on:\s*(.*)$", text, re.M) == ["ubuntu-latest", "ubuntu-latest"], "hosted runners only")
    uses = re.findall(r"^\s*-?\s*uses:\s*(.*)$", text, re.M)
    require(len(uses) == 3 and all(re.fullmatch(r"[\w/-]+@[0-9a-f]{40} # v[\w.+-]+", value) for value in uses), "version-commented SHA pins")
    require("group: memory-load-quality\n  cancel-in-progress: false" in text, "serialized uncancelled runs")
    require("timeout-minutes: 90" in text, "bounded job timeout")
    require("retention-days: 90" in text and "path: evidence/" in text, "90-day evidence artifact")
    require("if: always() && steps.manifest.outputs.safe_to_upload == 'true'" in text, "fail-closed artifact upload")
    require("name: Write manifest\n        id: manifest\n        if: always()" in text, "manifest runs after failure")
    require('"$RUNNER_TEMP/memory-load-quality-manifest" --evidence evidence --out evidence/workflow-manifest.json' in text, "prebuilt reporter handles evidence")
    require(text.index("name: Build evidence reporter") < text.index("name: Start PostgreSQL"), "reporter built before measurements")
    require("pg_isready -h 127.0.0.1 -U witself -d witself" in text, "readiness requires the final TCP server")
    require("POSTGRES_SERVER_VERSION_NUM: ${{ steps.postgres.outputs.server_version_num }}" in text, "database version linked to manifest")
    require("GITHUB_REF: ${{ github.ref }}" in text and "SLICES: ${{ inputs.slices }}" in text, "manifest validates ref and selection")
    require("POSTGRES_IMAGE_LABEL: ${{ needs.validate-ref.outputs.postgres_image_label }}" in text, "validated database image label")
    require("refs/heads/main" in text and '[[ "$RELEASE_REF" =~ $semver_tag ]]' in text, "release ref gate")
    require("pgvector/pgvector:pg16" in text and "pgvector/pgvector:pg17" in text and "pgvector/pgvector:pg18" in text, "three PostgreSQL tiers")
    selectors = {"lexical": ("LoadQuality", "LOAD_QUALITY", "30m"), "curation": ("CurationLoad", "CURATION_LOAD", "12m"), "recall": ("RecallLoad", "RECALL_LOAD", "12m"), "archive": ("ArchiveLoad", "ARCHIVE_LOAD", "15m"), "concurrency": ("ConcurrencyLoad", "CONCURRENCY_LOAD", "60m")}
    for name, (selector, prefix, timeout) in selectors.items():
        require(f"-run '^TestNarrativeMemory{selector}Postgres$'" in text, f"{name} exact selector")
        require(f"WITSELF_MEMORY_{prefix}: \"1\"" in text, f"{name} opt-in")
        require(f"WITSELF_MEMORY_{prefix}_RESULTS: ${{{{ github.workspace }}}}/evidence/memory-{name}.json" in text, f"{name} absolute result path")
        require(f"-count=1 -v -timeout {timeout} 2>&1 | tee evidence/test-{name}.log" in text, f"{name} measurement flags and retained log")
        require(f"if: always() && steps.postgres.outcome == 'success' && (inputs.slices == 'all' || inputs.slices == '{name}')" in text, f"{name} continues after another slice fails")
        require(f"{name.upper()}_OUTCOME: ${{{{ steps.{name}.outcome }}}}" in text, f"{name} exit status reaches manifest")
        for field, value in {"RELEASE": "${{ needs.validate-ref.outputs.release }}", "COMMIT": "${{ github.sha }}", "PROVIDER": "github-hosted", "HARDWARE_TIER": "ubuntu-latest-${{ needs.validate-ref.outputs.postgres_image_label }}"}.items():
            require(f"WITSELF_MEMORY_{prefix}_{field}: {value}" in text, f"{name} {field.lower()} metadata")
    require("WITSELF_MEMORY_CONCURRENCY_LOAD_ACCOUNTS: \"4\"" in text and "WITSELF_MEMORY_CONCURRENCY_LOAD_REALMS_PER_ACCOUNT: \"2\"" in text and "WITSELF_MEMORY_CONCURRENCY_LOAD_AGENTS_PER_REALM: \"4\"" in text, "default 32-principal topology")
    require("-race" not in text, "measurement parity without race instrumentation")

try:
    source = path.read_text()
    check(source)
    for fixture, before, after in [
        ("push trigger", "  workflow_dispatch:", "  push: {}\n  workflow_dispatch:"),
        ("write permission", "contents: read", "contents: write"),
        ("job environment", "    timeout-minutes: 90", "    environment: private\n    timeout-minutes: 90"),
        ("secret", "          POSTGRES_IMAGE:", "          SECRET: ${{ secrets.DSN }}\n          POSTGRES_IMAGE:"),
        ("self-hosted", "runs-on: ubuntu-latest", "runs-on: self-hosted"),
        ("floating pin", "actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1", "actions/checkout@v7"),
        ("temporary socket readiness", "pg_isready -h 127.0.0.1 -U witself", "pg_isready -U witself"),
        ("retention", "retention-days: 90", "retention-days: 7"),
        ("unsafe upload", "if: always() && steps.manifest.outputs.safe_to_upload == 'true'", "if: always()"),
        ("skipped manifest", "id: manifest\n        if: always()", "id: manifest\n        if: success()"),
        ("wrong artifact directory", "path: evidence/", "path: ./"),
        ("missing selector", "^TestNarrativeMemoryRecallLoadPostgres$", "^TestUnrelated$"),
        ("relative output", "${{ github.workspace }}/evidence/memory-archive.json", "evidence/memory-archive.json"),
        ("missing outcome", "LEXICAL_OUTCOME:", "IGNORED_OUTCOME:"),
        ("oversized topology", 'WITSELF_MEMORY_CONCURRENCY_LOAD_ACCOUNTS: "4"', 'WITSELF_MEMORY_CONCURRENCY_LOAD_ACCOUNTS: "32"'),
    ]:
        require(before in source, f"canary fixture exists: {fixture}")
        mutant = source.replace(before, after, 1)
        try:
            check(mutant)
        except AssertionError:
            continue
        raise AssertionError(f"canary accepted: {fixture}")
    for gate in (root / "Makefile", root / ".github/workflows/ci.yml"):
        require("bash scripts/test-memory-load-quality-workflow.sh" in gate.read_text(), f"contract wired into {gate.name}")
except (AssertionError, OSError) as error:
    print(f"memory load quality workflow test: FAIL: {error}", file=sys.stderr)
    sys.exit(1)
print("memory load quality workflow test: PASS (workflow contract and 15 rejecting canaries)")
PY
