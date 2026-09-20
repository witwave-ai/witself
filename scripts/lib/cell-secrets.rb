#!/usr/bin/env ruby
# All manifest parsing and subprocess I/O stays in memory. Only ciphertext and
# the explicitly requested short-lived 1Password identity may be written to disk.
require 'base64'
require 'find'
require 'json'
require 'open3'
require 'tempfile'
require 'time'
require 'yaml'

module CellSecrets
  ROOT = File.expand_path('../..', __dir__)
  DIRECTORY = File.join(ROOT, '.gitops/secrets')
  RECIPIENT = 'age10ck02we3wzes85qd0e0eylxqupw7ectqjvv7wlykzdtts8nsugwshkp8ue'.freeze
  CELLS = %w[civo-sandbox-use1-serving civo-sandbox-use1-backup].freeze
  PATH_REGEX = '^\.gitops/secrets/.*\.sops$'.freeze
  LIMIT = 4 * 1024 * 1024
  class Refusal < StandardError; end

  # JSON and YAML parsers otherwise silently accept duplicate keys.
  class UniqueObject < Hash
    def []=(key, value)
      raise Refusal, 'duplicate mapping key' if key?(key)
      super
    end
  end

  # Duplicate JSON keys are refused by the parser itself where the json gem
  # supports it (allow_duplicate_key), otherwise by UniqueObject's []= hook.
  # Newer parsers may collapse duplicates before object_class sees them.
  def self.parse_json(raw)
    JSON.parse(raw, object_class: UniqueObject, allow_duplicate_key: false)
  rescue ArgumentError => e
    raise unless e.message.include?('allow_duplicate_key')
    JSON.parse(raw, object_class: UniqueObject)
  end

  def self.fail!(message)
    raise Refusal, message
  end

  def self.require_binary(name)
    fail!("required binary missing: #{name}") unless ENV.fetch('PATH', '').split(File::PATH_SEPARATOR).any? do |part|
      path = File.join(part, name)
      File.file?(path) && File.executable?(path)
    end
  end

  def self.safe_path(path)
    # Paths are public, but do not permit terminal control characters in errors.
    path.delete_prefix(ROOT + '/').inspect
  end

  def self.regular!(path)
    stat = File.lstat(path)
    fail!("unsafe file #{safe_path(path)}") unless stat.file? && stat.nlink == 1
    stat
  end

  def self.directory!(path)
    fail!("unsafe directory #{safe_path(path)}") unless File.lstat(path).directory?
  end

  def self.read_file(path)
    stat = regular!(path)
    fail!("file too large #{safe_path(path)}") if stat.size > LIMIT
    File.open(path, File::RDONLY | File::NOFOLLOW) do |file|
      opened = file.stat
      fail!("file changed #{safe_path(path)}") unless opened.file? && opened.nlink == 1 &&
        opened.dev == stat.dev && opened.ino == stat.ino
      raw = file.read(LIMIT + 1) || ''
      fail!("file too large #{safe_path(path)}") if raw.bytesize > LIMIT
      raw
    end
  end

  def self.yaml(raw)
    stream = YAML.parse_stream(raw)
    fail!('expected one YAML document') unless stream.children.length == 1
    visit = lambda do |node|
      fail!('YAML aliases are forbidden') if node.is_a?(Psych::Nodes::Alias)
      if node.is_a?(Psych::Nodes::Mapping)
        keys = node.children.each_slice(2).map do |key, _value|
          fail!('mapping keys must be strings') unless key.is_a?(Psych::Nodes::Scalar)
          key.value
        end
        fail!('duplicate mapping key') unless keys.uniq == keys
      end
      (node.children || []).each { |child| visit.call(child) }
    end
    visit.call(stream)
    YAML.safe_load(raw, permitted_classes: [], permitted_symbols: [], aliases: false)
  end

  def self.json(raw)
    parse_json(raw)
  end

  def self.config
    directory!(File.join(ROOT, '.gitops'))
    directory!(DIRECTORY)
    doc = yaml(read_file(File.join(DIRECTORY, '.sops.yaml')))
    fail!('invalid SOPS configuration') unless doc.is_a?(Hash) && doc.keys == ['creation_rules'] &&
      doc['creation_rules'].is_a?(Array) && doc['creation_rules'].length == 1
    rule = doc['creation_rules'].first
    fail!('invalid SOPS creation rule') unless rule.is_a?(Hash) && rule.keys.sort == %w[age path_regex] &&
      rule['path_regex'] == PATH_REGEX && rule['age'].is_a?(String)
    recipients = rule['age'].split(',').map(&:strip)
    fail!('SOPS configuration must include the committed age recipient') unless recipients.include?(RECIPIENT) &&
      recipients.uniq == recipients && recipients.all? { |r| r.match?(/\Aage1[0-9a-z]{58}\z/) }
    recipients
  end

  def self.dns_name?(value, namespace: false)
    return false unless value.is_a?(String) && value.bytesize <= (namespace ? 63 : 253)
    parts = namespace ? [value] : value.split('.', -1)
    !parts.empty? && parts.all? { |part| part.match?(/\A[a-z0-9](?:[a-z0-9-]*[a-z0-9])?\z/) }
  end

  def self.selection(value)
    pieces = value.split('/', -1)
    fail!('expected namespace/name') unless pieces.length == 2 && dns_name?(pieces[0], namespace: true) && dns_name?(pieces[1])
    pieces
  end

  def self.cell!(cell)
    fail!('unknown cell') unless CELLS.include?(cell)
    directory!(File.join(DIRECTORY, cell))
  end

  def self.manifest(raw, cell, expected = nil)
    fail!('manifest too large') if raw.bytesize > LIMIT
    doc = yaml(raw)
    fail!('invalid Secret manifest') unless doc.is_a?(Hash) &&
      (doc.keys - %w[apiVersion kind immutable metadata type data]).empty? &&
      doc['apiVersion'] == 'v1' && doc['kind'] == 'Secret' && doc['immutable'] == true &&
      doc['type'].is_a?(String) && !doc['type'].empty? && doc['data'].is_a?(Hash)
    meta = doc['metadata']
    fail!('invalid Secret metadata') unless meta.is_a?(Hash) &&
      (meta.keys - %w[name namespace labels annotations]).empty? &&
      dns_name?(meta['name']) && dns_name?(meta['namespace'], namespace: true)
    %w[labels annotations].each do |key|
      next unless meta.key?(key)
      fail!('invalid Secret metadata') unless meta[key].is_a?(Hash) &&
        meta[key].all? { |k, v| k.is_a?(String) && v.is_a?(String) }
    end
    fail!('cell/manifest binding mismatch') unless meta.fetch('annotations', {})['witself.io/cell'] == cell
    total = 0
    doc['data'].each do |key, value|
      fail!('invalid Secret data') unless key.is_a?(String) && key.bytesize.between?(1, 253) &&
        key.match?(/\A[A-Za-z0-9._-]+\z/) && value.is_a?(String)
      decoded = Base64.strict_decode64(value)
      fail!('noncanonical Secret base64') unless Base64.strict_encode64(decoded) == value
      total += decoded.bytesize
    end
    fail!('Secret data exceeds Kubernetes limit') if total > 1024 * 1024
    binding = [meta['namespace'], meta['name']]
    fail!('path/manifest binding mismatch') if expected && binding != expected
    doc
  end

  def self.ciphertext?(value)
    return false unless value.is_a?(String)
    match = /\AENC\[AES256_GCM,data:([A-Za-z0-9+\/=]*),iv:([A-Za-z0-9+\/=]+),tag:([A-Za-z0-9+\/=]+),type:str\]\z/.match(value)
    return false unless match
    fields = match.captures.map { |part| Base64.strict_decode64(part) }
    !fields[0].empty? && fields[1].bytesize == 32 && fields[2].bytesize == 16
  end

  def self.envelope(raw, recipients)
    doc = json(raw)
    fail!('invalid binary SOPS envelope') unless doc.is_a?(Hash) && doc.keys.sort == %w[data sops] && ciphertext?(doc['data'])
    sops = doc['sops']
    fail!('invalid SOPS metadata') unless sops.is_a?(Hash) &&
      (sops.keys - %w[age lastmodified mac version kms gcp_kms azure_kv hc_vault pgp unencrypted_suffix]).empty? &&
      ciphertext?(sops['mac']) && sops['version'].is_a?(String) && sops['version'].match?(/\A3\.\d+\.\d+\z/) &&
      sops['lastmodified'].is_a?(String) && sops['lastmodified'].match?(/\A\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ\z/)
    Time.iso8601(sops['lastmodified'])
    %w[kms gcp_kms azure_kv hc_vault pgp].each do |key|
      fail!('non-age SOPS metadata forbidden') if sops.key?(key) && sops[key] != []
    end
    fail!('invalid SOPS metadata') if sops.key?('unencrypted_suffix') && sops['unencrypted_suffix'] != '_unencrypted'
    entries = sops['age']
    fail!('invalid age recipients') unless entries.is_a?(Array) && !entries.empty?
    found = entries.map do |entry|
      fail!('invalid age stanza') unless entry.is_a?(Hash) && entry.keys.sort == %w[enc recipient] && entry['enc'].is_a?(String)
      match = /\A-----BEGIN AGE ENCRYPTED FILE-----\n([A-Za-z0-9+\/=\n]+)\n-----END AGE ENCRYPTED FILE-----\n?\z/.match(entry['enc'])
      fail!('invalid age ciphertext') unless match
      body = Base64.strict_decode64(match[1].delete("\n"))
      fail!('invalid age ciphertext') unless body.start_with?("age-encryption.org/v1\n-> X25519 ") && body.bytesize > 150
      entry['recipient']
    end
    fail!('unexpected age recipient') unless found.sort == recipients.sort && found.include?(RECIPIENT)
    # Whitelisted JSON shape already excludes plaintext manifests and YAML tokens;
    # also reject textual manifest markers anywhere in the envelope.
    fail!('plaintext manifest token') if raw.match?(/apiVersion|stringData|\bkind\b|(?:^|\s)data:/)
    doc
  end

  def self.inventory(recipients)
    files = []
    Find.find(DIRECTORY) do |path|
      stat = File.lstat(path)
      fail!("unsafe entry #{safe_path(path)}") unless stat.directory? || (stat.file? && stat.nlink == 1)
      next if stat.directory?
      relative = path.delete_prefix(DIRECTORY + '/')
      next if %w[.sops.yaml README.md].include?(relative)
      if File.basename(path) == '.gitkeep'
        fail!("invalid placeholder #{safe_path(path)}") unless read_file(path).empty?
        next
      end
      begin
        parts = relative.split('/')
        fail!('invalid artifact path') unless parts.length == 3 && CELLS.include?(parts[0]) && parts[2].end_with?('.sops')
        selection([parts[1], parts[2].delete_suffix('.sops')].join('/'))
        envelope(read_file(path), recipients)
      rescue StandardError
        fail!("invalid encrypted artifact #{safe_path(path)}")
      end
      files << path
    end
    files.sort
  end

  def self.process(argv, input = '', env = {})
    output, _errors, status = Open3.capture3(env, *argv, stdin_data: input, binmode: true, chdir: ROOT)
    # Never relay child errors: SOPS/YAML/kubectl may include secret values.
    fail!("#{File.basename(argv.first)} operation failed") unless status.success?
    output
  end

  def self.sops_env(identity = nil)
    ENV.keys.grep(/\ASOPS_/).each_with_object({}) { |key, out| out[key] = nil }.merge('SOPS_AGE_KEY_FILE' => identity)
  end

  def self.with_identity
    selected = ENV['SOPS_AGE_KEY_FILE']
    if selected
      fail!('SOPS_AGE_KEY_FILE must name a readable regular file') if selected.empty?
      stat = regular!(selected)
      fail!('age identity must have private file permissions') unless (stat.mode & 0o077).zero?
      fail!('age identity is not readable') unless File.readable?(selected)
      yield File.expand_path(selected)
    else
      require_binary('op')
      file = Tempfile.new(['witself-cell-secrets-age-', '.key'])
      begin
        file.chmod(0o600)
        # op output goes only into the identity file, never argv or logs.
        ok = system('op', 'read', 'op://Private/witself-cell-secrets-age/credential', out: file, err: File::NULL)
        file.flush
        fail!('1Password identity read failed') unless ok && file.size.positive?
        yield file.path
      ensure
        file.close!
      end
    end
  end

  def self.encrypt(cell, raw, rotate, recipients, files, expected = nil)
    doc = manifest(raw, cell, expected)
    namespace, name = doc['metadata'].values_at('namespace', 'name')
    parent = File.join(DIRECTORY, cell, namespace)
    path = File.join(parent, name + '.sops')
    fail!("already exists #{namespace}/#{name}; use a new versioned name") if File.exist?(path) || File.symlink?(path)
    if rotate
      version = /\A(.+)-v([1-9][0-9]*)\z/.match(name)
      fail!('rotation requires a new -vN name') unless version
      cell_files = files.select { |p| p.start_with?(File.join(DIRECTORY, cell) + '/') }
      fail!('rotation name already exists in cell') if cell_files.any? { |p| File.basename(p) == name + '.sops' }
      predecessors = cell_files.map do |p|
        next unless File.dirname(p) == parent
        # The initial recovery inventory includes two unversioned backup names.
        next 0 if File.basename(p) == version[1] + '.sops'
        other = /\A(.+)-v([1-9][0-9]*)\.sops\z/.match(File.basename(p))
        other[2].to_i if other && other[1] == version[1]
      end.compact
      fail!('rotation requires an existing family and a greater version') if predecessors.empty? || version[2].to_i <= predecessors.max
    end
    relative = path.delete_prefix(ROOT + '/')
    # The committed policy uses repository-relative paths; SOPS resolves rules
    # relative to its config directory instead. We validate that policy above
    # and pass its recipients explicitly, disabling ambient config discovery.
    encrypted = process(['sops', '--encrypt', '--input-type', 'binary', '--output-type', 'binary',
                         '--config', File::NULL, '--filename-override', relative,
                         '--age', recipients.join(','), '/dev/stdin'], raw, sops_env)
    envelope(encrypted, recipients)
    Dir.mkdir(parent, 0o700) unless File.exist?(parent) || File.symlink?(parent)
    directory!(parent)
    # Publish atomically without replacing even if another writer wins the race.
    temporary = Tempfile.new(['.cell-secrets-', '.sops'], parent)
    begin
      temporary.binmode
      temporary.write(encrypted)
      temporary.flush
      temporary.fsync
      File.link(temporary.path, path)
    ensure
      temporary.close!
    end
    puts relative
  end

  def self.kube_args(cell, namespace)
    ['kubectl', '--context', 'witself-' + cell, '--namespace', namespace]
  end

  def self.existing(cell, namespace, name)
    raw = process(kube_args(cell, namespace) + ['get', 'secret', name, '--ignore-not-found', '-o', 'json'])
    return nil if raw.strip.empty?
    doc = json(raw)
    fail!('invalid existing Secret response') unless doc.is_a?(Hash) && doc['kind'] == 'Secret' && doc['apiVersion'] == 'v1' &&
      doc['metadata'].is_a?(Hash) && doc['metadata'].values_at('namespace', 'name') == [namespace, name]
    doc
  end

  def self.apply(cell, selected, mode, recipients, identity)
    # Validate every selected plaintext before any cluster mutation.
    records = selected.map do |path|
      namespace = File.basename(File.dirname(path))
      name = File.basename(path, '.sops')
      encrypted = read_file(path)
      envelope(encrypted, recipients)
      raw = process(['sops', '--decrypt', '--input-type', 'binary', '--output-type', 'binary', '/dev/stdin'], encrypted, sops_env(identity))
      [namespace, name, raw, manifest(raw, cell, [namespace, name])]
    end
    outcomes = records.map do |namespace, name, raw, doc|
      live = existing(cell, namespace, name)
      state = if live.nil?
                'created'
              elsif live['immutable'] == true && live['data'] == doc['data'] && live['type'] == doc['type'] && !live.key?('stringData')
                'identical'
              else
                'conflict'
              end
      [namespace, name, raw, state]
    end
    if mode == '--diff-names'
      outcomes.each { |namespace, name, _raw, state| puts "#{state} #{namespace}/#{name}" }
    end
    conflicts = outcomes.select { |record| record[3] == 'conflict' }
    fail!("conflict #{conflicts.map { |r| r[0, 2].join('/') }.join(', ')}") unless conflicts.empty?
    return if mode == '--diff-names'
    outcomes.each do |namespace, name, raw, state|
      if state == 'identical'
        puts "identical #{namespace}/#{name}"
        next
      end
      args = kube_args(cell, namespace) + ['apply', '-f', '-']
      args << '--dry-run=client' if mode == '--dry-run'
      process(args, raw)
      puts "#{mode == '--dry-run' ? 'would-create' : 'created'} #{namespace}/#{name}"
    end
  end

  def self.main(args)
    command = args.shift
    fail!('usage: cell-secrets.sh encrypt|decrypt-apply|export|check ...') unless %w[encrypt decrypt-apply export check].include?(command)
    recipients = config
    files = inventory(recipients)
    if command == 'check'
      fail!('check takes no arguments') unless args.empty?
      puts 'cell-secrets: check passed'
      return
    end
    cell = args.shift
    cell!(cell)
    case command
    when 'encrypt'
      rotate = args.delete('--rotate')
      fail!('encrypt requires one manifest file or -') unless args.length == 1
      raw = args.first == '-' ? STDIN.read(LIMIT + 1) : read_file(args.first)
      encrypt(cell, raw, rotate, recipients, files)
    when 'decrypt-apply'
      modes = args.select { |arg| arg.start_with?('--') }
      fail!('choose --dry-run or --diff-names') unless modes.length <= 1 && (modes - %w[--dry-run --diff-names]).empty?
      args -= modes
      cell_files = files.select { |p| p.start_with?(File.join(DIRECTORY, cell) + '/') }
      selected = args.empty? ? cell_files : args.map do |arg|
        namespace, name = selection(arg)
        path = File.join(DIRECTORY, cell, namespace, name + '.sops')
        fail!("encrypted artifact missing #{namespace}/#{name}") unless cell_files.include?(path)
        path
      end.uniq
      fail!('no encrypted Secrets for cell') if selected.empty?
      require_binary('kubectl')
      with_identity { |identity| apply(cell, selected, modes.first, recipients, identity) }
    when 'export'
      fail!('export requires one namespace/name') unless args.length == 1
      namespace, name = selection(args.first)
      require_binary('kubectl')
      with_identity do |_identity|
        live = existing(cell, namespace, name)
        fail!("Secret missing #{namespace}/#{name}") unless live
        # Remove resourceVersion, UID, managedFields and last-applied annotations.
        doc = live.select { |key, _| %w[apiVersion kind immutable type data].include?(key) }
        doc['metadata'] = { 'namespace' => namespace, 'name' => name, 'annotations' => { 'witself.io/cell' => cell } }
        encrypt(cell, JSON.generate(doc) + "\n", false, recipients, files, [namespace, name])
      end
    end
  end
end

begin
  File.umask(0o077)
  Signal.trap('TERM') { raise Interrupt }
  Signal.trap('HUP') { raise Interrupt }
  CellSecrets.main(ARGV)
rescue CellSecrets::Refusal => error
  warn "cell-secrets: #{error.message}"
  exit 1
rescue Interrupt
  warn 'cell-secrets: interrupted'
  exit 1
rescue StandardError
  # Parser, OS and subprocess exception messages can contain private input.
  warn 'cell-secrets: operation refused (invalid input or failed local operation)'
  exit 1
end
