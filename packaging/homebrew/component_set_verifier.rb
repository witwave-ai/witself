  # This helper is dark: current release products never render or call it.
  # Metadata acceptance is not extraction, activation, or durable installed state.
  class ComponentSetVerifier
    class Refusal < StandardError; end
    JSONNumber = Struct.new(:text)
    UINT64 = (1 << 64) - 1
    DIGEST = /\A[0-9a-f]{64}\z/
    COMMON = %w[format_version critical kind issued_at expires_at].freeze
    PUBLIC_KEY = {"key_id" => String, "spki" => String}.freeze
    ARCHIVE = {"name" => String, "size" => Integer, "sha256" => String, "format" => String}.freeze
    INVENTORY = {"path" => String, "size" => Integer, "sha256" => String, "mode" => String, "executable" => :boolean}.freeze
    POSTGRESQL = {"major" => Integer, "version" => String, "distribution" => String, "build" => String,
                  "runtime_libraries" => [String], "data_layout_version" => Integer}.freeze
    COMPONENT = {"role" => String, "version" => String, "archive" => ARCHIVE, "inventory" => [INVENTORY],
                 "depends_on" => [String], "required_extensions" => [{"name" => String, "version" => String, "abi" => String}],
                 "schema" => {"target" => Integer, "accepted_min" => Integer, "accepted_max" => Integer},
                 "ownership" => {"package_owned_paths" => [String], "private_payload_paths" => [String]},
                 "postgresql?" => POSTGRESQL}.freeze
    PLATFORM = {"os" => String, "arch" => String, "min_os" => String, "components" => [COMPONENT], "glibc_floor?" => String}.freeze
    COMMON_SHAPE = {"format_version" => Integer, "critical" => [String], "kind" => String,
                    "issued_at" => String, "expires_at" => String}.freeze
    SHAPES = {
      "delegation" => COMMON_SHAPE.merge("root_sequence" => Integer, "delegation_sequence" => Integer,
                                        "keys" => [PUBLIC_KEY], "revoked_key_ids" => [String]),
      "root_rotation" => COMMON_SHAPE.merge("root_sequence" => Integer, "new_sequence" => Integer, "new_root" => PUBLIC_KEY),
      "component_set" => COMMON_SHAPE.merge("root_sequence" => Integer, "delegation_sequence" => Integer,
                                           "release_sequence" => Integer, "release_tag" => String, "channel" => String,
                                           "source" => {"repository" => String, "commit" => String},
                                           "provenance" => {"checksums_sha256" => String, "sigstore_bundle" => String},
                                           "license" => String, "sbom" => String,
                                           "rollback" => {"allowed_targets" => [Integer]}, "platforms" => [PLATFORM])
    }.freeze
    attr_accessor :trusted_clock

    def initialize(root_pems, now: Time.now.utc, trusted_clock: true)
      refuse("no_trust_root") if root_pems.empty?
      @roots = {}
      root_pems.each do |pem|
        # TEST-ONLY public labels are fixture labels, never ambient authority.
        match = /\A-----BEGIN (?:TEST-ONLY )?PUBLIC KEY-----\s*([A-Za-z0-9+\/=\r\n]+)-----END (?:TEST-ONLY )?PUBLIC KEY-----\s*\z/.match(pem)
        refuse("invalid_key") unless match
        der = decode64(match[1].gsub(/\s/, ""))
        id = OpenSSL::Digest::SHA256.hexdigest(der)
        refuse("invalid_key") if @roots.key?(id)
        @roots[id] = parse_key({"key_id" => id, "spki" => [der].pack("m0")})
      end
      @now, @trusted_clock = now, trusted_clock
      @root_sequence, @delegation_sequence = 1, 0
      @root_history, @release_keys, @revoked = [], {}, []
    end

    def refuse(reason)
      raise Refusal, reason
    end

    # Bound both allocation and reads, including replacement/growth after stat.
    def self.open_regular(path)
      before = File.lstat(path)
      raise Refusal, "filesystem_error" unless before.file? && !before.symlink?
      flags = File::RDONLY | File::NONBLOCK
      flags |= File::NOFOLLOW if File.const_defined?(:NOFOLLOW)
      File.open(path, flags) do |file|
        stat = file.stat
        raise Refusal, "filesystem_error" unless stat.file? && stat.dev == before.dev && stat.ino == before.ino
        yield file
      end
    rescue SystemCallError, IOError
      raise Refusal, "filesystem_error"
    end

    def self.read_bounded(path, cap, reason)
      open_regular(path) do |file|
        raise Refusal, reason if file.stat.size > cap
        raw = file.read(cap + 1) || "".b
        raise Refusal, reason if raw.bytesize > cap
        raw
      end
    end

    def read_bounded(path, cap, reason)
      self.class.read_bounded(path, cap, reason)
    end

    def deep_freeze(value)
      case value
      when Hash
        value.each { |key, item| deep_freeze(key); deep_freeze(item) }
      when Array
        value.each { |item| deep_freeze(item) }
      when JSONNumber
        deep_freeze(value.text)
      end
      value.freeze
    end

    # A small strict parser keeps duplicate escaped names and numeric spellings,
    # which ordinary JSON.parse would otherwise lose. No schema data is parsed
    # until its raw bytes have authenticated; only the bounded envelope precedes it.
    def parse(raw)
      source = raw.dup.force_encoding(Encoding::UTF_8)
      refuse("profile_violation") unless source.valid_encoding?
      scanner = StringScanner.new(source)
      value = json_value(scanner, 0)
      scanner.skip(/[\x20\t\r\n]*/)
      refuse("invalid_json") unless scanner.eos?
      refuse("profile_violation") unless value.is_a?(Hash)
      value
    end

    def json_string(scanner)
      token = scanner.scan(/"(?:[^"\\\x00-\x1f]|\\(?:["\\\/bfnrt]|u[0-9A-Fa-f]{4}))*"/)
      refuse("invalid_json") unless token
      # Explicitly pair escaped UTF-16 surrogates, including on Ruby JSON versions
      # that silently replace unpaired values.
      escapes = StringScanner.new(token[1...-1])
      until escapes.eos?
        if escapes.scan(/\\u([0-9A-Fa-f]{4})/)
          n = escapes[1].to_i(16)
          if n.between?(0xd800, 0xdbff)
            refuse("profile_violation") unless escapes.scan(/\\u([0-9A-Fa-f]{4})/) && escapes[1].to_i(16).between?(0xdc00, 0xdfff)
          elsif n.between?(0xdc00, 0xdfff)
            refuse("profile_violation")
          end
        elsif escapes.scan(/\\./m)
          next
        else
          escapes.getch
        end
      end
      decoded = JSON.parse(token)
      refuse("profile_violation") unless decoded.valid_encoding? && decoded.bytesize <= 4096
      decoded
    rescue JSON::ParserError
      refuse("invalid_json")
    end

    def json_value(scanner, depth)
      scanner.skip(/[\x20\t\r\n]*/)
      return json_string(scanner) if scanner.peek(1) == '"'
      if scanner.scan(/[\{\[]/)
        refuse("profile_violation") if depth >= 8
        object = scanner.matched == "{"
        result, closing = object ? [{}, "}"] : [[], "]"]
        scanner.skip(/[\x20\t\r\n]*/)
        return result if scanner.scan(Regexp.new(Regexp.escape(closing)))
        loop do
          if object
            scanner.skip(/[\x20\t\r\n]*/)
            key = json_string(scanner)
            refuse("duplicate_key") if result.key?(key)
            scanner.skip(/[\x20\t\r\n]*/)
            refuse("invalid_json") unless scanner.scan(/:/)
            result[key] = json_value(scanner, depth + 1)
          else
            result << json_value(scanner, depth + 1)
          end
          scanner.skip(/[\x20\t\r\n]*/)
          break if scanner.scan(Regexp.new(Regexp.escape(closing)))
          refuse("invalid_json") unless scanner.scan(/,/)
        end
        return result
      end
      return true if scanner.scan(/true/)
      return false if scanner.scan(/false/)
      return nil if scanner.scan(/null/)
      number = scanner.scan(/-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?/)
      refuse("invalid_json") unless number
      # Frozen B1 ignores numeric spelling in unknown top-level subtrees.
      # Preserve it until shape checks a known unsigned integer field.
      return number.to_i if /\A(?:0|[1-9][0-9]*)\z/.match?(number) && number.to_i <= UINT64
      JSONNumber.new(number)
    end

    def shape(value, schema, top = false)
      case schema
      when Hash
        refuse("invalid_document") unless value.is_a?(Hash)
        names = schema.keys.map { |key| key.delete_suffix("?") }
        refuse("unknown_field") unless top || (value.keys - names).empty?
        schema.each do |name, spec|
          key = name.delete_suffix("?")
          next if name.end_with?("?") && !value.key?(key)
          refuse("invalid_document") unless value.key?(key)
          shape(value[key], spec)
        end
      when Array
        refuse("invalid_document") unless value.is_a?(Array)
        value.each { |item| shape(item, schema.first) }
      when :boolean
        refuse("invalid_document") unless value == true || value == false
      else
        refuse("profile_violation") if schema == Integer && !value.is_a?(Integer)
        refuse("invalid_document") unless value.is_a?(schema)
      end
    end

    def decode64(encoded)
      refuse("invalid_encoding") unless encoded.is_a?(String) && /\A(?:[A-Za-z0-9+\/]{4})*(?:[A-Za-z0-9+\/]{2}==|[A-Za-z0-9+\/]{3}=)?\z/.match?(encoded)
      raw = encoded.unpack1("m0")
      refuse("invalid_encoding") unless [raw].pack("m0") == encoded
      raw
    rescue ArgumentError
      refuse("invalid_encoding")
    end

    def parse_key(metadata)
      der = decode64(metadata["spki"])
      refuse("invalid_key") unless DIGEST.match?(metadata["key_id"]) && OpenSSL::Digest::SHA256.hexdigest(der) == metadata["key_id"]
      key = OpenSSL::PKey.read(der)
      refuse("invalid_key") unless key.is_a?(OpenSSL::PKey::RSA) && key.n.num_bits == 3072 && key.e.to_i == 65537 && public_der(key) == der
      key
    rescue OpenSSL::OpenSSLError
      refuse("invalid_key")
    end

    def public_der(key)
      return key.public_to_der if key.respond_to?(:public_to_der)
      # Ruby 2.6 ships an older OpenSSL binding without public_to_der.
      key.public_key.to_der
    end

    def verify_signature(raw, signature, key)
      bytes = decode64(signature["signature"])
      refuse("signature_length") unless bytes.bytesize == 384
      refuse("signature_invalid") unless key.verify(OpenSSL::Digest::SHA256.new, bytes, raw)
    rescue OpenSSL::OpenSSLError
      refuse("signature_invalid")
    end

    def envelope(raw, kind)
      object = parse(raw)
      shape(object, {"format_version" => Integer, "kind" => String, "algorithm" => String,
                     "signatures" => [{"key_id" => String, "signature" => String}]})
      refuse("invalid_document") unless object["format_version"] == 1
      refuse("wrong_algorithm") unless object["algorithm"] == "rsa3072-pkcs1v15-sha256"
      refuse("wrong_kind") unless object["kind"] == kind
      signatures = object["signatures"]
      count = kind == "root_rotation" ? 2 : 1
      refuse("signature_count") unless signatures.length == count && signatures.map { |s| s["key_id"] }.uniq.length == count
      signatures.each do |signature|
        refuse("invalid_key") unless DIGEST.match?(signature["key_id"])
        refuse("signature_length") unless decode64(signature["signature"]).bytesize == 384
      end
      signatures
    end

    def common(object, kind)
      shape(object, SHAPES.fetch(kind), true)
      refuse("invalid_document") unless object["format_version"] == 1
      refuse("wrong_kind") unless object["kind"] == kind
      critical = object["critical"]
      refuse("unknown_critical") unless (critical - SHAPES.fetch(kind).keys).empty?
      refuse("invalid_document") unless critical.uniq == critical
      times = %w[issued_at expires_at].map do |name|
        text = object[name]
        refuse("invalid_document") unless /\A\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z\z/.match?(text)
        parsed = Time.iso8601(text).utc
        refuse("invalid_document") unless parsed.strftime("%Y-%m-%dT%H:%M:%SZ") == text
        parsed
      end
      refuse("invalid_document") unless times[1] > times[0]
    rescue ArgumentError
      refuse("invalid_document")
    end

    def fresh(object)
      refuse("unknown_clock") unless @trusted_clock
      now = @now.utc.strftime("%Y-%m-%dT%H:%M:%SZ")
      refuse("not_yet_valid") if now < object["issued_at"]
      refuse("expired") if now >= object["expires_at"]
    end

    def path_valid?(path)
      path.is_a?(String) && path.bytesize.between?(1, 1024) && !/[\\\x00:]/.match?(path) &&
        path.split("/", -1).none? { |part| ["", ".", ".."].include?(part) } && !path.match?(/\p{Cn}/) && path.unicode_normalize(:nfc) == path
    end

    def component(object)
      roles = %w[app service_host server worker postgresql extension]
      refuse("roles_invalid") unless roles.include?(object["role"])
      refuse("invalid_document") if object["version"].empty?
      archive = object["archive"]
      refuse("inventory_invalid") unless path_valid?(archive["name"]) && !archive["name"].include?("/") &&
        archive["size"].between?(1, 2_147_483_648) && DIGEST.match?(archive["sha256"]) && %w[tar.gz zip].include?(archive["format"])
      inventory = object["inventory"]
      refuse("inventory_invalid") unless inventory.length.between?(1, 4096)
      paths, total = [], 0
      inventory.each do |entry|
        refuse("path_invalid") unless path_valid?(entry["path"])
        refuse("inventory_invalid") if paths.include?(entry["path"])
        paths << entry["path"]
        refuse("mode_invalid") unless %w[0644 0755].include?(entry["mode"]) && entry["executable"] == (entry["mode"] == "0755")
        total += entry["size"]
        refuse("inventory_invalid") unless DIGEST.match?(entry["sha256"]) && total <= archive["size"]
      end
      paths.each do |path|
        parts = path.split("/")
        (1...parts.length).each { |count| refuse("inventory_invalid") if paths.include?(parts.take(count).join("/")) }
      end
      schema = object["schema"]
      refuse("schema_invalid") unless schema["accepted_min"] <= schema["target"] && schema["target"] <= schema["accepted_max"]
      refuse("extensions_unsupported") unless object["required_extensions"].empty?
      if object["role"] == "postgresql"
        pg = object["postgresql"]
        refuse("postgresql_invalid") unless pg && pg["major"] == 16 && pg["version"] == object["version"] &&
          pg["version"].start_with?("16.") && !pg["distribution"].empty? && !pg["build"].empty? && pg["data_layout_version"] > 0
        pg["runtime_libraries"].each { |path| refuse("path_invalid") unless path_valid?(path) }
      elsif object.key?("postgresql")
        refuse("postgresql_invalid")
      end
      owned = object["ownership"].values.flatten
      owned.each { |path| refuse("path_invalid") unless path_valid?(path) }
      refuse("inventory_invalid") unless owned.uniq.length == owned.length && owned.sort == paths.sort
    end

    def component_set(object)
      refuse("invalid_document") unless %w[root_sequence delegation_sequence release_sequence].all? { |key| object[key] > 0 } && object["channel"] == "stable"
      refuse("wrong_tag") unless /\Av(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\z/.match?(object["release_tag"])
      refuse("invalid_document") unless !object["source"]["repository"].empty? && /\A[0-9a-f]{40}\z/.match?(object["source"]["commit"]) &&
        DIGEST.match?(object["provenance"]["checksums_sha256"]) && path_valid?(object["provenance"]["sigstore_bundle"]) &&
        path_valid?(object["license"]) && path_valid?(object["sbom"])
      targets = object["rollback"]["allowed_targets"]
      refuse("invalid_document") unless targets.uniq == targets && targets.all? { |target| target > 0 && target < object["release_sequence"] }
      platforms = object["platforms"]
      refuse("wrong_platform") unless platforms.length.between?(1, 6) && platforms.map { |p| [p["os"], p["arch"]] }.uniq.length == platforms.length
      platforms.each do |platform|
        refuse("wrong_platform") unless %w[linux darwin windows].include?(platform["os"]) && %w[amd64 arm64].include?(platform["arch"]) && !platform["min_os"].empty?
        floor = platform.fetch("glibc_floor", "")
        refuse("wrong_platform") unless platform["os"] == "linux" ? /\A2\.[0-9]{2}\z/.match?(floor) : floor.empty?
        components = platform["components"]
        components.each { |item| component(item) }
        archives = components.map { |item| item["archive"]["name"] }
        paths = components.flat_map { |item| item["inventory"].map { |entry| entry["path"] } }
        refuse("inventory_invalid") unless archives.uniq == archives && paths.uniq == paths
        roles = components.map { |item| item["role"] }
        refuse("roles_invalid") unless %w[app service_host server postgresql].all? { |role| roles.count(role) == 1 } && roles.count("worker") >= 1 && roles.count("extension") <= 1
        graph = {}
        components.each do |item|
          dependencies = item["depends_on"]
          refuse("roles_invalid") unless dependencies.uniq == dependencies && dependencies.all? { |role| role != item["role"] && roles.include?(role) }
          (graph[item["role"]] ||= []).concat(dependencies)
        end
        active, visited = {}, {}
        visit = lambda do |role|
          refuse("roles_invalid") if active[role]
          next if visited[role]
          active[role] = true
          graph.fetch(role, []).each { |dependency| visit.call(dependency) }
          active.delete(role)
          visited[role] = true
        end
        graph.keys.each { |role| visit.call(role) }
      end
    end

    def root_signer(signatures)
      roots = @root ? {@root_id => @root} : @roots
      signature = signatures.find { |item| roots.key?(item["key_id"]) }
      unless signature
        refuse("wrong_signer") if signatures.any? { |item| @release_keys.key?(item["key_id"]) }
        refuse("unknown_key")
      end
      [signature, roots.fetch(signature["key_id"])]
    end

    def accept(document_path, envelope_path, kind, expected_tag: nil, os: nil, arch: nil, allow_rollback: false)
      refuse("wrong_kind") unless SHAPES.key?(kind)
      raw = read_bounded(document_path, kind == "component_set" ? 262_144 : 16_384, "document_too_large")
      signatures = envelope(read_bounded(envelope_path, 8192, "envelope_too_large"), kind)
      if kind == "component_set"
        signature = signatures.first
        id = signature["key_id"]
        refuse("wrong_signer") if @roots.key?(id) || @root_history.include?(id)
        refuse("unknown_key") unless @delegation
        refuse("revoked_key") if @revoked.include?(id)
        refuse("unknown_key") unless @release_keys.key?(id)
        key = @release_keys.fetch(id)
      else
        signature, key = root_signer(signatures)
      end
      verify_signature(raw, signature, key)
      object = parse(raw)
      common(object, kind)
      digest = OpenSSL::Digest::SHA256.hexdigest(raw)
      case kind
      when "delegation"
        refuse("invalid_document") unless object["root_sequence"] > 0 && object["delegation_sequence"] > 0 && !object["keys"].empty?
        keys = {}
        object["keys"].each do |metadata|
          id = metadata["key_id"]
          refuse("invalid_key") if keys.key?(id)
          keys[id] = parse_key(metadata)
        end
        revoked = object["revoked_key_ids"]
        refuse("invalid_key") unless revoked.uniq == revoked && revoked.all? { |id| DIGEST.match?(id) }
        refuse("sequence_invalid") unless object["root_sequence"] == @root_sequence
        refuse("replay") if object["delegation_sequence"] < @delegation_sequence || (object["delegation_sequence"] == @delegation_sequence && digest != @delegation_digest)
        refuse("wrong_signer") if keys.keys.any? { |id| @roots.key?(id) || @root_history.include?(id) }
        fresh(object)
        @root, @root_id = key, signature["key_id"]
        @root_history << @root_id if @root_history.empty?
        @delegation, @delegation_sequence, @delegation_digest = object, object["delegation_sequence"], digest
        @release_keys, @revoked = keys, revoked
      when "root_rotation"
        refuse("sequence_invalid") unless object["root_sequence"] == @root_sequence && @root_sequence < UINT64 && object["new_sequence"] == @root_sequence + 1
        metadata = object["new_root"]
        new_key = parse_key(metadata)
        # Either explicit bootstrap root may authenticate the first parse. The
        # distinct old root still authenticates before a chain anchor is selected.
        if metadata["key_id"] == signature["key_id"]
          signature, key = root_signer(signatures.reject { |item| item["key_id"] == metadata["key_id"] })
          verify_signature(raw, signature, key)
        end
        refuse("sequence_invalid") if metadata["key_id"] == signature["key_id"] || @root_history.include?(metadata["key_id"])
        new_signature = signatures.find { |item| item["key_id"] == metadata["key_id"] }
        refuse("unknown_key") unless new_signature
        verify_signature(raw, new_signature, new_key)
        fresh(object)
        @root_history << signature["key_id"] if @root_history.empty?
        @root, @root_id, @root_sequence = new_key, metadata["key_id"], object["new_sequence"]
        @root_history << @root_id
        @delegation, @release_keys, @revoked = nil, {}, []
      when "component_set"
        component_set(object)
        refuse("sequence_invalid") unless object["root_sequence"] == @root_sequence && object["delegation_sequence"] == @delegation_sequence
        refuse("wrong_tag") if expected_tag && object["release_tag"] != expected_tag
        refuse("wrong_platform") if (!!os != !!arch) || (os && !object["platforms"].any? { |p| p["os"] == os && p["arch"] == arch })
        fresh(object)
        fresh(@delegation)
        refuse("invalid_document") unless object["issued_at"] >= @delegation["issued_at"] && object["expires_at"] <= @delegation["expires_at"]
        if @release
          if object["release_sequence"] < @release["release_sequence"]
            refuse("replay") unless allow_rollback
            refuse("rollback_denied") unless @release["rollback"]["allowed_targets"].include?(object["release_sequence"])
          elsif object["release_sequence"] == @release["release_sequence"]
            refuse("replay") unless digest == @release_digest
          end
        end
        if !@release || object["release_sequence"] > @release["release_sequence"]
          @release, @release_digest = object, digest
        end
      else
        refuse("wrong_kind")
      end
      @accepted_manifest = object if kind == "component_set"
      deep_freeze(object)
    end

    # Bound archive verification to a component object from this exact accepted
    # document, not an independently parsed or previously accepted set. This
    # streams bytes only; extraction and activation remain the B3 boundary.
    def verify_archive!(path, selected_component)
      refuse("mixed_set") unless @accepted_manifest && @accepted_manifest["platforms"].any? do |platform|
        platform["components"].any? { |item| item.equal?(selected_component) }
      end
      archive = selected_component["archive"]
      self.class.open_regular(path) do |file|
        refuse("archive_size_mismatch") unless file.stat.size == archive["size"]
        digest = OpenSSL::Digest::SHA256.new
        total = 0
        while total <= archive["size"]
          chunk = file.read([65_536, archive["size"] + 1 - total].min)
          break unless chunk
          total += chunk.bytesize
          digest.update(chunk)
        end
        refuse("archive_size_mismatch") unless total == archive["size"]
        refuse("archive_digest_mismatch") unless digest.hexdigest == archive["sha256"]
      end
      true
    end
  end

  private

  # An opting-in future formula supplies its reviewed root explicitly. Current
  # products expose no root, no invocation and no production local-cell variant.
  def verify_component_set!(dir, root_pem)
    raise ComponentSetVerifier::Refusal, "no_trust_root" if root_pem.nil? || root_pem.empty?
    verifier = ComponentSetVerifier.new([ComponentSetVerifier.read_bounded(root_pem, 16_384, "document_too_large")], trusted_clock: ENV["WS_UNTRUSTED_CLOCK"] != "1")
    rotations = Dir.glob(File.join(dir, "root-rotation-*.json")).reject { |path| path.end_with?(".sig.json") }
    raise ComponentSetVerifier::Refusal, "sequence_invalid" if rotations.length > 32
    numbered_rotations = rotations.sort_by do |path|
      match = /\Aroot-rotation-([1-9][0-9]*)\.json\z/.match(File.basename(path))
      raise ComponentSetVerifier::Refusal, "sequence_invalid" unless match
      match[1].to_i
    end
    numbered_rotations.each_with_index do |path, index|
      raise ComponentSetVerifier::Refusal, "sequence_invalid" unless File.basename(path) == "root-rotation-#{index + 1}.json"
      verifier.accept(path, path + ".sig.json", "root_rotation")
    end
    delegation = File.join(dir, "delegation.json")
    verifier.accept(delegation, delegation + ".sig.json", "delegation")
    manifest = File.join(dir, "component-set.json")
    accepted = verifier.accept(manifest, manifest + ".sig.json", "component_set")
    @component_set_verifier = verifier
    accepted
  rescue SystemCallError, IOError
    raise ComponentSetVerifier::Refusal, "filesystem_error"
  end

  def verify_component_archive!(path, component)
    raise ComponentSetVerifier::Refusal, "mixed_set" unless @component_set_verifier
    @component_set_verifier.verify_archive!(path, component)
  end
