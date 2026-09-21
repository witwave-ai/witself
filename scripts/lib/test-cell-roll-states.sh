#!/usr/bin/env bash
# Test-only fixture writer. The binary is the real gitops-cell-values command,
# so positive states use exactly the writer invoked by roll-cell.sh, offline.
apply_roll_state() {
  local fixture_root=$1 generator=$2 state=$3
  ruby -ryaml - "$fixture_root" "$generator" "$state" <<'RUBY'
root, generator, state = ARGV
catalog = YAML.safe_load(File.read(File.join(root, ".gitops/cells/catalog.yaml")), aliases: false)
cells = catalog.fetch("cells").select { |_, c| c["cloud"] == "civo" && %w[backup serving].include?(c["role"]) }
abort "roll-state fixture requires serving and backup cells" unless cells.values.map { |c| c["role"] }.sort == %w[backup serving]
def values_path(root, cell)
  File.join(root, ".gitops/cells", cell, "values.yaml")
end
def run_generator(generator, root, *args)
  abort "roll-state fixture generation failed" unless system(generator, *args, "--root", root)
end
case state
when "committed"
  # Preserve the source bytes, including whatever pins are currently committed.
when "unpinned"
  cells.each_key do |cell|
    path = values_path(root, cell)
    values = YAML.safe_load(File.read(path), aliases: false)
    values.fetch("apps").fetch("witselfServer").delete("imageDigest")
    pg = values.fetch("apps").fetch("civoPostgres")
    pg.fetch("backup").delete("image")
    pg.delete("image")
    pg.delete("allowInsecureImages")
    File.write(path, YAML.dump(values))
  end
  # Restore canonical overlay bytes and upstream PostgreSQL content pins.
  run_generator(generator, root, "--write")
when "serving-backup", "serving-postgres", "all-pins"
  cells.each do |cell, config|
    next if state != "all-pins" && config["role"] != "serving"
    values = YAML.safe_load(File.read(values_path(root, cell)), aliases: false)
    version = "0.0.292"
    server_digest = values.fetch("apps").fetch("witselfServer")["imageDigest"]
    server_digest = "sha256:" + "a" * 64 if server_digest.to_s.empty?
    args = ["--roll-cell", cell, "--version", version, "--image-digest", server_digest]
    if %w[serving-backup all-pins].include?(state)
      args.concat(["--backup-image-repository", "ghcr.io/witwave-ai/images/witself-postgres-backup",
                   "--backup-image-tag", version, "--backup-image-digest", "sha256:" + "b" * 64])
    end
    if %w[serving-postgres all-pins].include?(state)
      digest = values.fetch("apps").fetch("civoPostgres").fetch("image").fetch("digest")
      args.concat(["--postgres-image-registry", "ghcr.io",
                   "--postgres-image-repository", "witwave-ai/images/postgresql",
                   "--postgres-image-tag", "#{version}-#{cell}", "--postgres-image-digest", digest])
    end
    run_generator(generator, root, *args)
  end
else
  abort "unknown roll-state fixture"
end
run_generator(generator, root, "--check")
RUBY
}
