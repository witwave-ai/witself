#!/usr/bin/env ruby
# Offline, two-stage account request budget contracts for committed or rolled cells.
require "yaml"
require "open3"
require "tmpdir"

# Helm v4 schema errors contain non-ASCII text (for example "×10⁰⁶"); read child
# output as UTF-8 so a shell without a UTF-8 locale does not crash the suite.
Encoding.default_external = Encoding::UTF_8

SERVING_CELL = "civo-sandbox-use1-serving".freeze
LIVE_CELLS = %w[civo-prod-use1-serving civo-sandbox-use1-backup civo-sandbox-use1-serving].freeze
REQUEST_BUDGET_CELLS = [].freeze
FIXTURE = {"enabled" => true, "readPerMinute" => 600, "readBurst" => 60,
           "writePerMinute" => 120, "writeBurst" => 20, "maxTrackedAccounts" => 1000000}.freeze
PLANS_FIXTURE = {
  "free" => {"readPerMinute" => 60, "readBurst" => 1, "writePerMinute" => 30, "writeBurst" => 2},
  "team" => {"readPerMinute" => 600, "readBurst" => 5, "writePerMinute" => 120, "writeBurst" => 10}
}.freeze
# The canonical JSON asserted by slice 143a's child-chart case C6.
PLANS_JSON = '{"free":{"read_burst":1,"read_per_minute":60,"write_burst":2,"write_per_minute":30},"team":{"read_burst":5,"read_per_minute":600,"write_burst":10,"write_per_minute":120}}'.freeze
OFF_WITH_VALUES = {"enabled" => false, "readPerMinute" => 5, "readBurst" => 5,
                   "writePerMinute" => 5, "writeBurst" => 5, "maxTrackedAccounts" => 500}.freeze
RATE_KEYS = %w[readPerMinute readBurst writePerMinute writeBurst].freeze
ENV_PREFIX = "WITSELF_ACCOUNT_REQUEST_BUDGET_".freeze

class RequestBudgetChecks
  def initialize(root, cases)
    @root = root
    @cases = cases
    @apps_chart = File.join(root, ".gitops/charts/apps")
    @server_chart = File.join(root, "charts/witself-server")
    @helm_processes = 0
    @override_sequence = 0
  end

  def fail_case(label, cell, message)
    abort "case #{label} / cell #{cell}: #{message}".gsub(/[a-fA-F0-9]{64}/, "<digest-redacted>")
  end

  def check(label, cell, message, condition)
    fail_case(label, cell, message) unless condition
  end

  def parse_values(text, label, cell)
    values = YAML.safe_load(text, aliases: false)
    fail_case(label, cell, "expected a values map") unless values.is_a?(Hash)
    values
  rescue Psych::Exception => error
    fail_case(label, cell, "cannot parse values: #{error.message}")
  end

  def read_values(path, label, cell)
    parse_values(File.read(path), label, cell)
  rescue SystemCallError => error
    fail_case(label, cell, "cannot read values: #{error.message}")
  end

  def helm(*args)
    @helm_processes += 1
    Open3.capture3("helm", "template", *args)
  end

  def apps_render(cell_path, *args)
    helm("witself-apps", @apps_chart, "--values", cell_path, *args)
  end

  def documents(result, label, cell)
    output, errors, status = result
    fail_case(label, cell, "helm render failed: #{errors}") unless status.success?
    YAML.load_stream(output).compact
  rescue Psych::Exception => error
    fail_case(label, cell, "cannot parse rendered manifests: #{error.message}")
  end

  def nested_text(result, label, cell)
    app = documents(result, label, cell).find do |doc|
      doc["kind"] == "Application" && doc.dig("metadata", "name") == "witself-server"
    end
    fail_case(label, cell, "missing Application/witself-server") unless app
    text = app.dig("spec", "source", "helm", "values")
    fail_case(label, cell, "missing nested Helm values") unless text.is_a?(String)
    text
  end

  def nested_values(result, label, cell)
    parse_values(nested_text(result, label, cell), label, cell)
  end

  def configmap_budget(docs, name, label, cell)
    configmap = docs.find do |doc|
      doc["kind"] == "ConfigMap" && doc.dig("metadata", "name") == name
    end
    fail_case(label, cell, "missing ConfigMap/#{name}") unless configmap
    (configmap["data"] || {}).select { |key, _| key.start_with?(ENV_PREFIX) }
  end

  def child_render(nested, dir, label, cell)
    path = File.join(dir, "#{label}-#{cell}-nested.yaml")
    File.write(path, YAML.dump(nested))
    documents(helm("witself-server", @server_chart, "--namespace", "witself", "--values", path), label, cell)
  end

  def override_file(server_values, dir)
    @override_sequence += 1
    path = File.join(dir, "override-#{@override_sequence}.yaml")
    File.write(path, YAML.dump("apps" => {"witselfServer" => server_values}))
    path
  end

  def version_file(dir, chart = READY, image = READY)
    override_file({"chartVersion" => chart, "imageTag" => image}, dir)
  end

  def budget_render(budget, dir, cell_path, *args)
    apps_render(cell_path, "--values", override_file({"requestBudget" => budget}, dir), *args)
  end

  def case_a(paths, cells, dir)
    catalog = read_values(File.join(@root, ".gitops/cells/catalog.yaml"), "A0", "catalog")
    expected_cells = catalog.fetch("cells").keys.sort
    directories = Dir.glob(File.join(@root, ".gitops/cells/*")).select { |path| File.directory?(path) }.map { |path| File.basename(path) }.sort
    check("A0", "catalog", "cell directories must equal catalog keys", directories == expected_cells)
    check("A0", "catalog", "cell values files must equal catalog keys", cells == expected_cells)
    LIVE_CELLS.each do |cell|
      check("A0", cell, "required live cell missing", cells.include?(cell))
    end
    paths.each do |path|
      cell = File.basename(File.dirname(path))
      plain = apps_render(path)
      nested = nested_values(plain, "A1", cell)
      cell_values = read_values(path, "A1", cell).fetch("apps").fetch("witselfServer")
      allowed = REQUEST_BUDGET_CELLS.include?(cell)
      check("A1", cell, "requestBudget presence must match REQUEST_BUDGET_CELLS", nested.key?("requestBudget") == allowed)
      if allowed
        check("A1", cell, "nested requestBudget must equal the cell override exactly", nested["requestBudget"] == cell_values["requestBudget"])
        next
      end
      check("A2", cell, "cell values must have no requestBudget unless allow-listed", !cell_values.key?("requestBudget"))
      off = budget_render(OFF_WITH_VALUES, dir, path)
      documents(off, "A2", cell)
      check("A2", cell, "disabled values must leave the apps render byte-identical", off[0] == plain[0])
      next unless LIVE_CELLS.include?(cell)
      docs = child_render(nested, dir, "A3", cell)
      keys = docs.select { |doc| doc["kind"] == "ConfigMap" }.flat_map { |doc| (doc["data"] || {}).keys }
      check("A3", cell, "disabled child must have no account request budget ConfigMap keys", keys.none? { |key| key.start_with?(ENV_PREFIX) })
    end
  end

  def case_b(serving_path, dir)
    version = version_file(dir)
    base = nested_values(apps_render(serving_path, "--values", version), "B2", SERVING_CELL)
    enabled = budget_render(FIXTURE, dir, serving_path, "--values", version)
    text = nested_text(enabled, "B1", SERVING_CELL)
    nested = parse_values(text, "B1", SERVING_CELL)
    check("B1", SERVING_CELL, "nested requestBudget must equal FIXTURE exactly", nested["requestBudget"] == FIXTURE)
    check("B2", SERVING_CELL, "requestBudget must be the only changed nested key", nested.reject { |key, _| key == "requestBudget" } == base.reject { |key, _| key == "requestBudget" })
    check("B3", SERVING_CELL, "maxTrackedAccounts must retain integer notation", text.include?("maxTrackedAccounts: 1000000") && !text.include?("1e+06"))
    docs = child_render(nested, dir, "B4", SERVING_CELL)
    expected = {"ENABLED" => "true", "READ_PER_MINUTE" => "600", "READ_BURST" => "60",
                "WRITE_PER_MINUTE" => "120", "WRITE_BURST" => "20", "MAX_ACCOUNTS" => "1000000"}
    expected = expected.map { |key, value| [ENV_PREFIX + key, value] }.to_h
    check("B4", SERVING_CELL, "server ConfigMap must contain exactly the six fixture keys and string values", configmap_budget(docs, "witself-server", "B4", SERVING_CELL) == expected)
    check("B5", SERVING_CELL, "worker ConfigMap must contain no account request budget keys", configmap_budget(docs, "witself-worker", "B5", SERVING_CELL).empty?)
  end

  def case_c(serving_path, dir)
    [[BELOW, READY], [READY, BELOW], [BELOW, BELOW]].each_with_index do |versions, index|
      label = "C#{index + 1}"
      result = budget_render(FIXTURE, dir, serving_path, "--values", version_file(dir, *versions))
      expected = "apps.witselfServer.requestBudget requires chart and image v#{READY} or newer"
      check(label, SERVING_CELL, "old chart or image must fail with the request budget version gate: #{result[1]}", !result[2].success? && result[1].include?(expected))
    end
    rc = "#{READY}-rc.1"
    nested_values(budget_render(FIXTURE, dir, serving_path, "--values", version_file(dir, rc, rc)), "C4", SERVING_CELL)
    off = nested_values(budget_render(OFF_WITH_VALUES, dir, serving_path, "--values", version_file(dir, BELOW, BELOW)), "C5", SERVING_CELL)
    check("C5", SERVING_CELL, "disabled budget must not be forwarded to old chart and image", !off.key?("requestBudget"))
  end

  def case_d(serving_path, dir)
    apps = read_values(File.join(@apps_chart, "values.yaml"), "D1", SERVING_CELL)
    server = read_values(File.join(@server_chart, "values.yaml"), "D1", SERVING_CELL)
    check("D1", SERVING_CELL, "apps and server chart requestBudget defaults must match", apps.dig("apps", "witselfServer", "requestBudget") == server["requestBudget"])
    null = nested_values(budget_render(nil, dir, serving_path), "D2", SERVING_CELL)
    check("D2", SERVING_CELL, "explicit null must forward no requestBudget key", !null.key?("requestBudget"))
    disabled = budget_render({"enabled" => false}, dir, serving_path)
    off = budget_render(OFF_WITH_VALUES, dir, serving_path)
    [disabled, off].each do |result|
      nested = nested_values(result, "D3", SERVING_CELL)
      check("D3", SERVING_CELL, "disabled budget must forward no requestBudget key", !nested.key?("requestBudget"))
    end
    check("D3", SERVING_CELL, "disabled-only and disabled-with-values renders must be byte-identical", disabled[0] == off[0])
  end

  def schema_refusal(name, serving_path, args, skip: false)
    _, errors, status = apps_render(serving_path, *args)
    check("E", SERVING_CELL, "#{name}: schema must reject the value", !status.success?)
    unless errors.include?("requestBudget")
      fail_case("E", SERVING_CELL, "#{name}: refusal does not name requestBudget; stop and report stderr shape: #{errors}")
    end
    check("E", SERVING_CELL, "#{name}: refusal must be schema validation: #{errors}", errors.match?(/schema|specification/i))
    return unless skip
    _, skip_errors, skip_status = apps_render(serving_path, *args, "--skip-schema-validation")
    check("E", SERVING_CELL, "#{name}: skipping schema must render successfully: #{skip_errors}", skip_status.success?)
  end

  def case_e(serving_path, dir)
    version = version_file(dir)
    RATE_KEYS.each do |key|
      path = override_file({"requestBudget" => FIXTURE.merge(key => 0)}, dir)
      schema_refusal("enabled #{key}=0", serving_path, ["--values", version, "--values", path], skip: key == "readPerMinute")
    end
    path = override_file({"requestBudget" => FIXTURE.merge("readPerMinute" => 1000001)}, dir)
    schema_refusal("readPerMinute above maximum", serving_path, ["--values", version, "--values", path], skip: true)
    [99, 1000001].each do |value|
      path = override_file({"requestBudget" => OFF_WITH_VALUES.merge("maxTrackedAccounts" => value)}, dir)
      schema_refusal("disabled maxTrackedAccounts=#{value}", serving_path, ["--values", version, "--values", path], skip: value == 99)
    end
    schema_refusal("unknown key", serving_path, ["--values", version, "--set", "apps.witselfServer.requestBudget.foo=1"], skip: true)
    # Keep rates valid so this case isolates the boolean type even on an off cell.
    fixture = override_file({"requestBudget" => FIXTURE}, dir)
    schema_refusal("string enabled", serving_path, ["--values", version, "--values", fixture, "--set-string", "apps.witselfServer.requestBudget.enabled=true"], skip: true)
    fraction = override_file({"requestBudget" => FIXTURE.merge("readPerMinute" => 1.5)}, dir)
    schema_refusal("non-integer readPerMinute", serving_path, ["--values", version, "--values", fraction], skip: true)
    schema_refusal("null maxTrackedAccounts", serving_path, ["--values", version, "--set", "apps.witselfServer.requestBudget.maxTrackedAccounts=null"], skip: true)
    [[1, 100], [1000000, 1000000]].each do |rate, maximum|
      budget = FIXTURE.merge(RATE_KEYS.map { |key| [key, rate] }.to_h).merge("maxTrackedAccounts" => maximum)
      nested_values(budget_render(budget, dir, serving_path, "--values", version), "E", SERVING_CELL)
    end
  end

  def case_f(dir)
    blocks = LIVE_CELLS.map do |cell|
      path = File.join(@root, "internal/gitopsvalues/overlays", "#{cell}.yaml.tmpl")
      lines = File.readlines(path)
      starts = lines.each_index.select { |index| lines[index].chomp == "{{- with .RequestBudget }}" }
      check("F", cell, "overlay must have exactly one request budget block", starts.length == 1)
      first = starts.first + 1
      last = (first...lines.length).find do |index|
        lines[index].chomp == "{{- end }}" && lines[index + 1]&.chomp == "    worker:"
      end
      check("F", cell, "overlay request budget block must terminate immediately before worker", !last.nil?)
      lines[first...last].join
    end
    check("F", "live cells", "overlay request budget blocks must be byte-identical", blocks.uniq.length == 1)
    lines = blocks.first.lines
    plans_starts = lines.each_index.select { |index| lines[index].chomp == "{{- with .Plans }}" }
    check("F", "live cells", "overlay request budget block must contain exactly one plans sub-block", plans_starts.length == 1)
    plans_first = plans_starts.first
    depth = 0
    plans_last = (plans_first...lines.length).find do |index|
      depth += 1 if lines[index].match?(/\A\{\{- (?:with|range|if)\b/)
      depth -= 1 if lines[index].chomp == "{{- end }}"
      depth.zero?
    end
    check("F", "live cells", "overlay plans sub-block must terminate", !plans_last.nil?)
    lines.slice!(plans_first..plans_last)
    expected = <<~BLOCK.lines.map { |line| "    " + line }.join
      # From catalog switches.request_budget: per account, per minute, per replica.
      requestBudget:
        enabled: true
        readPerMinute: {{ .ReadPerMinute }}
        readBurst: {{ .ReadBurst }}
        writePerMinute: {{ .WritePerMinute }}
        writeBurst: {{ .WriteBurst }}
        maxTrackedAccounts: {{ .MaxTrackedAccounts }}
    BLOCK
    check("F", "live cells", "overlay without plans must retain the original eight lines", lines.join == expected)
    block = lines.join
    {"ReadPerMinute" => "readPerMinute", "ReadBurst" => "readBurst", "WritePerMinute" => "writePerMinute",
     "WriteBurst" => "writeBurst", "MaxTrackedAccounts" => "maxTrackedAccounts"}.each do |field, key|
      block.gsub!("{{ .#{field} }}", FIXTURE.fetch(key).to_s)
    end
    check("F", "live cells", "unsubstituted template action in request budget block", !block.include?("{{"))
    version = version_file(dir)
    (LIVE_CELLS - REQUEST_BUDGET_CELLS).each do |cell|
      text = File.read(File.join(@root, ".gitops/cells", cell, "values.yaml"))
      marker = "\n    worker:\n"
      check("F", cell, "cell must have exactly one worker insertion point", text.scan(marker).length == 1)
      path = File.join(dir, "F-#{cell}.yaml")
      File.write(path, text.sub(marker, "\n#{block}    worker:\n"))
      nested = nested_values(apps_render(path, "--values", version), "F", cell)
      check("F", cell, "overlay fixture must forward the exact requestBudget map", nested["requestBudget"] == FIXTURE)
    end
  end

  def case_g(serving_path, dir)
    budget = FIXTURE.merge("plans" => PLANS_FIXTURE)
    version = version_file(dir, READY_PLANS, READY_PLANS)
    nested = nested_values(budget_render(budget, dir, serving_path, "--values", version), "G1", SERVING_CELL)
    check("G1", SERVING_CELL, "nested requestBudget must equal the complete plans fixture", nested["requestBudget"] == budget)
    docs = child_render(nested, dir, "G1", SERVING_CELL)
    expected = {"ENABLED" => "true", "READ_PER_MINUTE" => "600", "READ_BURST" => "60",
                "WRITE_PER_MINUTE" => "120", "WRITE_BURST" => "20", "MAX_ACCOUNTS" => "1000000",
                "PLANS" => PLANS_JSON}.map { |key, value| [ENV_PREFIX + key, value] }.to_h
    check("G1", SERVING_CELL, "server ConfigMap must contain seven budget keys and the exact C6 plans JSON", configmap_budget(docs, "witself-server", "G1", SERVING_CELL) == expected)
    check("G1", SERVING_CELL, "worker ConfigMap must contain no account request budget keys", configmap_budget(docs, "witself-worker", "G1", SERVING_CELL).empty?)
    rc = "#{READY_PLANS}-rc.1"
    prerelease = nested_values(budget_render(budget, dir, serving_path, "--values", version_file(dir, rc, rc)), "G1-prerelease", SERVING_CELL)
    check("G1-prerelease", SERVING_CELL, "prerelease chart and image must forward the complete plans fixture", prerelease["requestBudget"] == budget)

    below_budget = budget_render(budget, dir, serving_path, "--values", version_file(dir, BELOW, BELOW))
    expected = "apps.witselfServer.requestBudget requires chart and image v#{READY} or newer"
    check("G2", SERVING_CELL, "plans below the budget floor must fail the original budget gate first: #{below_budget[1]}", !below_budget[2].success? && below_budget[1].include?(expected))

    below_plans = "0.0.#{READY_PLANS.split('.').last.to_i - 1}"
    old_versions = [READY]
    old_versions << below_plans if below_plans.split('.').last.to_i >= READY.split('.').last.to_i
    old_versions.uniq.each do |old|
      [[old, READY_PLANS], [READY_PLANS, old], [old, old]].each do |chart, image|
        result = budget_render(budget, dir, serving_path, "--values", version_file(dir, chart, image))
        expected = "apps.witselfServer.requestBudget.plans requires chart and image v#{READY_PLANS} or newer"
        check("G2", SERVING_CELL, "plans with chart #{chart} and image #{image} must fail the plans version gate: #{result[1]}", !result[2].success? && result[1].include?(expected))
      end
    end

    no_plans = nested_values(budget_render(FIXTURE, dir, serving_path, "--values", version_file(dir)), "G3", SERVING_CELL)
    check("G3", SERVING_CELL, "enabled budget without plans must forward exactly the six fixture keys and no plans", no_plans["requestBudget"] == FIXTURE && !no_plans["requestBudget"].key?("plans"))
    [BELOW, READY_PLANS].each do |disabled_version|
      disabled = nested_values(budget_render(budget.merge("enabled" => false), dir, serving_path,
                                             "--values", version_file(dir, disabled_version, disabled_version)), "G4", SERVING_CELL)
      check("G4", SERVING_CELL, "disabled plans at #{disabled_version} must forward no requestBudget", !disabled.key?("requestBudget"))
    end

    tier = PLANS_FIXTURE.fetch("free")
    invalid = {
      "plans array" => [],
      "plans string" => "free",
      "seventeen plans" => (1..17).map { |index| ["plan_#{index}", tier] }.to_h,
      "tier array" => {"free" => []},
      "tier string" => {"free" => "budget"},
      "unknown tier key" => {"free" => tier.merge("unknown" => 1)}
    }
    ["Free", "1free", "_free", "free-plan", "", "a" * 33].each do |id|
      invalid["invalid plan id #{id.inspect}"] = {id => tier}
    end
    RATE_KEYS.each do |key|
      invalid["missing #{key}"] = {"free" => tier.reject { |field, _| field == key }}
      [0, 1000001, 1.5, "1", true, nil].each do |value|
        invalid["#{key}=#{value.inspect}"] = {"free" => tier.merge(key => value)}
      end
    end
    invalid.each do |name, plans|
      _, errors, status = budget_render(FIXTURE.merge("plans" => plans), dir, serving_path, "--values", version)
      check("G5", SERVING_CELL, "#{name}: schema must reject the value", !status.success?)
      check("G5", SERVING_CELL, "#{name}: refusal must identify requestBudget.plans: #{errors}", errors.match?(/requestBudget[.\/]plans/))
      check("G5", SERVING_CELL, "#{name}: refusal must be schema validation: #{errors}", errors.match?(/schema|specification/i))
    end
    # Both field bounds, the longest id and the maximum map size are valid.
    boundary_plans = (1..15).map do |index|
      ["plan_#{index}", RATE_KEYS.map { |key| [key, 1] }.to_h]
    end.to_h.merge("a" * 32 => RATE_KEYS.map { |key| [key, 1000000] }.to_h)
    boundary = FIXTURE.merge("plans" => boundary_plans)
    actual = nested_values(budget_render(boundary, dir, serving_path, "--values", version), "G5", SERVING_CELL)
    check("G5", SERVING_CELL, "schema bounds must preserve the complete plans map", actual["requestBudget"] == boundary)
  end

  def run
    paths = Dir.glob(File.join(@root, ".gitops/cells/*/values.yaml")).sort
    cells = paths.map { |path| File.basename(File.dirname(path)) }
    serving_path = File.join(@root, ".gitops/cells", SERVING_CELL, "values.yaml")
    Dir.mktmpdir("apps-server-request-budget-") do |dir|
      neutral_values = read_values(serving_path, "setup", SERVING_CELL)
      neutral_values.fetch("apps").fetch("witselfServer").delete("requestBudget")
      neutral_path = File.join(dir, "serving-neutral.yaml")
      File.write(neutral_path, YAML.dump(neutral_values))
      case_a(paths, cells, dir) if @cases.include?("A")
      case_b(neutral_path, dir) if @cases.include?("B")
      case_c(neutral_path, dir) if @cases.include?("C")
      case_d(serving_path, dir) if @cases.include?("D")
      case_e(neutral_path, dir) if @cases.include?("E")
      case_f(dir) if @cases.include?("F")
      case_g(neutral_path, dir) if @cases.include?("G")
    end
    puts "apps server request budget: PASS (cases #{@cases.join(',')}; #{cells.length} catalog cells; #{@helm_processes} helm processes)"
  end
end

unless ARGV.length.between?(1, 2)
  abort "case setup / cell all: usage: apps-server-request-budget.rb ROOT [A,B,C,D,E,F,G]"
end
cases = ARGV.fetch(1, "A,B,C,D,E,F,G").split(",", -1).uniq
unknown = cases - %w[A B C D E F G]
unless unknown.empty?
  abort "case selector / cell all: unknown cases #{unknown.inspect}".gsub(/[a-fA-F0-9]{64}/, "<digest-redacted>")
end
root = File.expand_path(ARGV[0])
version_lines = File.readlines(File.join(root, ".gitops/charts/apps/templates/witself-server.yaml")).grep(/\$requestBudgetMinVersion\s*:=/)
version = version_lines.first&.match(/\A\{\{- \$requestBudgetMinVersion := "(0\.0\.(\d+))" -\}\}\r?\n?\z/)
unless version_lines.length == 1 && version && version[2].to_i.positive?
  abort "case setup / cell all: expected exactly one requestBudgetMinVersion assignment with version 0.0.N"
end
READY = version[1].freeze
BELOW = "0.0.#{version[2].to_i - 1}".freeze
plans_version_lines = File.readlines(File.join(root, ".gitops/charts/apps/templates/witself-server.yaml")).grep(/\$requestBudgetPlansMinVersion\s*:=/)
plans_version = plans_version_lines.first&.match(/\A\{\{- \$requestBudgetPlansMinVersion := "(0\.0\.(\d+))" -\}\}\r?\n?\z/)
unless plans_version_lines.length == 1 && plans_version && plans_version[2].to_i >= version[2].to_i
  abort "case setup / cell all: expected exactly one requestBudgetPlansMinVersion assignment at least requestBudgetMinVersion"
end
READY_PLANS = plans_version[1].freeze
RequestBudgetChecks.new(root, cases).run
