#!/usr/bin/env ruby
# Independent cell-derived oracle, also run against every non-committed roll state.
Encoding.default_external = Encoding::UTF_8
require 'yaml'
require 'open3'

LIVE_CELLS = %w[civo-sandbox-use1-serving civo-sandbox-use1-backup].freeze
SERVER_REPOSITORY = 'ghcr.io/witwave-ai/images/witself-server'.freeze
BACKUP_REPOSITORY = 'ghcr.io/witwave-ai/images/witself-postgres-backup'.freeze

def safe_output(text)
  text.gsub(/[a-fA-F0-9]{64}/, '<digest-redacted>')
end

def image_entry(source, repository, tag, digest)
  raise 'invalid image fields' unless repository.is_a?(String) && repository.match?(/\A[a-z0-9][a-z0-9._\/-]*\z/) &&
    tag.is_a?(String) && tag.match?(/\A[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}\z/)
  digest = nil if digest == ''
  raise 'invalid image digest' unless digest.nil? || (digest.is_a?(String) && digest.match?(/\Asha256:[a-f0-9]{64}\z/))

  { source: source, repository: repository, tag: tag, digest: digest,
    tagged: "#{repository}:#{tag}", ref: digest ? "#{repository}@#{digest}" : "#{repository}:#{tag}" }
end

begin
  raise 'usage: targets-coverage.rb ROOT VERSION' unless ARGV.length == 2 && ARGV[1].match?(/\A[0-9]+\.[0-9]+\.[0-9]+\z/)
  root, version = ARGV
  script = File.join(root, 'scripts/security-scan.sh')
  live_line = "LIVE_CELLS=\"#{LIVE_CELLS.join(' ')}\"\n"
  raise 'LIVE_CELLS differs' unless File.readlines(script, encoding: 'UTF-8').grep(/^LIVE_CELLS=/) == [live_line]

  incomplete = []
  pinned = LIVE_CELLS.flat_map do |cell|
    values = YAML.safe_load(File.read(File.join(root, '.gitops/cells', cell, 'values.yaml'), encoding: 'UTF-8'), aliases: false)
    apps = values.fetch('apps')
    entries = []
    server = apps.fetch('witselfServer', {})
    if server['enabled'] == true
      entries << image_entry("cell:#{cell}", SERVER_REPOSITORY, server.fetch('imageTag'), server['imageDigest'])
    end
    backup = apps.fetch('civoPostgres', {}).fetch('backup', {})
    if backup['enabled'] == true
      image = backup['image']
      if image.nil?
        incomplete << 'unpinned backup image'
      else
        begin
          case image
          when String
            match = /\A(.+):([^:]+)\z/.match(image)
            raise 'invalid backup image' unless match
            entries << image_entry("cell:#{cell}", match[1], match[2], nil)
          when Hash
            entries << image_entry("cell:#{cell}", image.fetch('repository'), image.fetch('tag'), image['digest'])
          else
            raise 'invalid backup image'
          end
        rescue StandardError
          incomplete << 'invalid backup image'
        end
      end
    end
    entries
  end
  # Ref dedupe preserves the first cell. Release tag suppression considers even
  # dropped pins, because a shared digest can have different tags in a real roll.
  kept = pinned.uniq { |entry| entry.fetch(:ref) }
  pinned_tags = pinned.map { |entry| entry.fetch(:tagged) }
  [SERVER_REPOSITORY, BACKUP_REPOSITORY].each do |repository|
    release = image_entry('release', repository, version, nil)
    kept << release unless pinned_tags.include?(release.fetch(:tagged))
  end
  expected = kept.map do |entry|
    [entry.fetch(:source), entry.fetch(:repository), entry.fetch(:tag), entry[:digest] ? entry[:digest][0, 19] : '-'].join("\t") + "\n"
  end.join
  expected_status = incomplete.empty? ? 0 : 3
  expected_stderr = incomplete.empty? ? '' : "security-scan: targets incomplete: #{incomplete.first}\n"
  expected = '' unless incomplete.empty?
  stdout, stderr, status = Open3.capture3('bash', script, 'targets', '--root', root, '--release-version', version)
  if status.exitstatus != expected_status || stdout != expected || stderr != expected_stderr || [stdout, stderr].any? { |stream| stream.match?(/[a-fA-F0-9]{64}/) }
    warn "security scan targets coverage: FAIL (exit=#{status.exitstatus || 'signal'})"
    warn "expected:\n#{safe_output(expected)}"
    warn "actual:\n#{safe_output(stdout)}"
    warn "stderr:\n#{safe_output(stderr)}" unless stderr.empty?
    exit 1
  end
  puts "security scan targets coverage: PASS (#{kept.length} targets, #{incomplete.length} incomplete)"
rescue StandardError => error
  # YAML/parser diagnostics can contain the input or a full digest. Emit only a
  # fixed reason or exception class; never an untrusted exception message.
  reason = error.instance_of?(RuntimeError) ? safe_output(error.message) : error.class.name
  warn "security scan targets coverage: FAIL (#{reason})"
  exit 1
end
