# Execute the rendered class, not a second verifier implementation. Homebrew's
# platform/install DSL is stubbed; no candidate or package manager is executed.
class Formula
  def self.desc(*); end
  def self.homepage(*); end
  def self.license(*); end
  def self.depends_on(*); end
  def self.on_macos; end
  def self.on_linux; end
  def self.test; end
end
require "tmpdir"
load ARGV.fetch(0)
base = ARGV.fetch(1)
index = JSON.parse(File.read(File.join(base, "vectors", "index.json")))
positives, negatives, signature_negatives = 0, 0, 0
failures = []
index.fetch("cases").each do |entry|
  now = Time.iso8601(entry.fetch("now", "2026-10-27T12:00:00Z"))
  roots = entry.fetch("roots").map { |name| File.read(File.join(base, "keys", "TEST-ONLY-#{name}.public.pem")) }
  verifier = Witself::ComponentSetVerifier.new(roots, now: now)
  options = {expected_tag: entry["expected_tag"], os: entry["os"], arch: entry["arch"], allow_rollback: entry.fetch("allow_rollback", false)}
  apply = lambda do |item|
    verifier.accept(File.join(base, "vectors", item.fetch("document")),
                    File.join(base, "vectors", item.fetch("envelope")), item.fetch("kind"), **options)
  end
  entry.fetch("setup").each { |item| apply.call(item) }
  verifier.trusted_clock = !entry.fetch("untrusted_clock", false)
  reason = begin
    accepted = apply.call(entry)
    raise "accepted_metadata_mutable" unless accepted.frozen? && accepted.values.all?(&:frozen?)
    "accepted"
  rescue Witself::ComponentSetVerifier::Refusal => e
    e.message
  end
  # No reason subsets are needed for this frozen corpus: require exact parity.
  failures << "#{entry.fetch('name')}: #{reason}, expected #{entry.fetch('expected_reason')}" unless reason == entry.fetch("expected_reason")
  entry.fetch("positive") ? positives += 1 : negatives += 1
  signature_negatives += 1 if entry["negative_signature"]
end
unless failures.empty?
  warn failures.join("\n")
  exit 1
end
# The entry point itself is private and has no production/ambient-root fallback.
instance = Witself.new
raise "helper_not_private" unless instance.private_methods.include?(:verify_component_set!)
begin
  instance.send(:verify_component_set!, base, nil)
  raise "missing_root_accepted"
rescue Witself::ComponentSetVerifier::Refusal => e
  raise "missing_root_reason" unless e.message == "no_trust_root"
end
Dir.mktmpdir("witself-bootstrap-") do |dir|
  root = File.join(base, "keys", "TEST-ONLY-root-a.public.pem")
  rotation = File.join(dir, "root-rotation-1.json")
  File.binwrite(rotation, File.binread(File.join(base, "vectors", "positive-rotation.json")))
  File.binwrite(rotation + ".sig.json", File.binread(File.join(base, "vectors", "positive-rotation.json.sig.json")))
  prior_clock = ENV["WS_UNTRUSTED_CLOCK"]
  begin
    ENV["WS_UNTRUSTED_CLOCK"] = "1"
    begin
      instance.send(:verify_component_set!, dir, root)
      raise "untrusted_clock_accepted"
    rescue Witself::ComponentSetVerifier::Refusal => e
      # Also proves root-rotation-N.json.sig.json is not treated as a document.
      raise "rotation_bundle_dispatch" unless e.message == "unknown_clock"
    end
  ensure
    prior_clock.nil? ? ENV.delete("WS_UNTRUSTED_CLOCK") : ENV["WS_UNTRUSTED_CLOCK"] = prior_clock
  end
  oversized_root = File.join(dir, "root.pem")
  File.binwrite(oversized_root, "x" * 16_385)
  begin
    instance.send(:verify_component_set!, dir, oversized_root)
    raise "oversized_root_accepted"
  rescue Witself::ComponentSetVerifier::Refusal => e
    raise "root_cap_reason" unless e.message == "document_too_large"
  end
end
# A real archive from the deterministic B1 build recipe supplies the successful
# payload branch; corruption checks must fail before any extraction is possible.
archive_path = ARGV.fetch(2)
root = File.read(File.join(base, "keys", "TEST-ONLY-root-a.public.pem"))
make_manifest = lambda do
  verifier = Witself::ComponentSetVerifier.new([root], now: Time.iso8601("2026-10-27T12:00:00Z"))
  delegation = File.join(base, "vectors", "setup-delegation.json")
  verifier.accept(delegation, delegation + ".sig.json", "delegation")
  path = File.join(base, "vectors", "positive-built-dist.json")
  document = verifier.accept(path, path + ".sig.json", "component_set")
  [verifier, document.fetch("platforms").first.fetch("components").first]
end
verifier, component = make_manifest.call
raise "valid_archive_refused" unless verifier.verify_archive!(archive_path, component)
_, foreign_component = make_manifest.call
begin
  verifier.verify_archive!(archive_path, foreign_component)
  raise "mixed_set_accepted"
rescue Witself::ComponentSetVerifier::Refusal => e
  raise "mixed_set_reason" unless e.message == "mixed_set"
end
original = File.binread(archive_path)
[[original.byteslice(0, original.bytesize - 1), "archive_size_mismatch"],
 [original.sub("data", "tamp"), "archive_digest_mismatch"]].each do |bytes, expected|
  raise "tamper_did_not_change_archive" if bytes == original
  File.binwrite(archive_path, bytes)
  begin
    verifier.verify_archive!(archive_path, component)
    raise "tampered_archive_accepted"
  rescue Witself::ComponentSetVerifier::Refusal => e
    raise "archive_refusal_reason" unless e.message == expected
  end
end
File.binwrite(archive_path, original)
puts "ruby parity: positive=#{positives} negative=#{negatives} negative_signature=#{signature_negatives}; exact reasons; no subset exceptions"
