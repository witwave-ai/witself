#!/usr/bin/env ruby
# Offline, two-stage resource contracts for every committed or rolled cell.
require "yaml"
require "open3"
require "tmpdir"

SERVER_RESOURCE_CELLS = [].freeze
CANARY = "civo-sandbox-use1-backup keeps the server chart's 256Mi limit as the OOM early-warning canary (design 2026-10-09 §3 rank 4); changing it is a separate founder decision".freeze
BACKUP_CELL = "civo-sandbox-use1-backup".freeze
SERVING_CELL = "civo-sandbox-use1-serving".freeze

# Keep this before any Open3 call, including when only selected cases run.
if SERVER_RESOURCE_CELLS.include?(BACKUP_CELL)
  abort "case step 0 / cell #{BACKUP_CELL}: #{CANARY}"
end

def deep_merge(default, override)
  default.merge(override) do |_, old, new|
    old.is_a?(Hash) && new.is_a?(Hash) ? deep_merge(old, new) : new
  end
end

class ResourceChecks
  def initialize(root, cases)
    @root = root
    @cases = cases
    @apps_chart = File.join(root, ".gitops/charts/apps")
    @server_chart = File.join(root, "charts/witself-server")
    @helm_processes = 0
  end

  def fail_case(label, cell, message)
    if ["step 0", "A", "F"].include?(label) && cell == BACKUP_CELL
      message = "#{CANARY}; #{message}"
    end
    abort "case #{label} / cell #{cell}: #{message}".gsub(/[a-fA-F0-9]{64}/, "<digest-redacted>")
  end

  def check(label, cell, message, condition)
    fail_case(label, cell, message) if @cases.include?(label) && !condition
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
    # Render failures are prerequisites, even when resource assertions are selected.
    fail_case(label, cell, "helm render failed: #{errors}") unless status.success?
    YAML.load_stream(output).compact
  rescue Psych::Exception => error
    fail_case(label, cell, "cannot parse rendered manifests: #{error.message}")
  end

  def nested_values(result, label, cell)
    app = documents(result, label, cell).find do |doc|
      doc["kind"] == "Application" && doc.dig("metadata", "name") == "witself-server"
    end
    fail_case(label, cell, "missing Application/witself-server") unless app
    text = app.dig("spec", "source", "helm", "values")
    fail_case(label, cell, "missing nested Helm values") unless text.is_a?(String)
    parse_values(text, label, cell)
  end

  def container_resources(docs, name, label, cell)
    deployment = docs.find do |doc|
      doc["kind"] == "Deployment" && doc.dig("metadata", "name") == name
    end
    fail_case(label, cell, "missing Deployment/#{name}") unless deployment
    containers = deployment.dig("spec", "template", "spec", "containers") || []
    container = containers.find { |item| item["name"] == name }
    fail_case(label, cell, "missing container #{name}") unless container
    container["resources"]
  end

  def child_render(nested, dir, label, cell)
    path = File.join(dir, "#{label}-#{cell}-nested.yaml")
    File.write(path, YAML.dump(nested))
    documents(helm("witself-server", @server_chart, "--namespace", "witself", "--values", path), label, cell)
  end

  def override_render(resources, dir, label, cell_path)
    path = File.join(dir, "#{label}-override.yaml")
    File.write(path, YAML.dump("apps" => {"witselfServer" => {"resources" => resources}}))
    apps_render(cell_path, "--values", path)
  end

  def run
    paths = Dir.glob(File.join(@root, ".gitops/cells/*/values.yaml")).sort
    cells = paths.map { |path| File.basename(File.dirname(path)) }
    catalog = read_values(File.join(@root, ".gitops/cells/catalog.yaml"), "A", "catalog")
    expected_cells = catalog.fetch("cells").keys.sort
    mismatch = "cell sets differ: files=#{cells.inspect}; catalog=#{expected_cells.inspect}"
    if cells.include?(BACKUP_CELL) != expected_cells.include?(BACKUP_CELL)
      mismatch = "#{CANARY}; #{mismatch}"
    end
    check("A", "catalog", mismatch, cells == expected_cells)

    baseline = {}
    # A's renders always run: B and F reuse these exact nested values.
    paths.each do |path|
      cell = File.basename(File.dirname(path))
      values = nested_values(apps_render(path), "A", cell)
      baseline[cell] = values
      configured = read_values(path, "A", cell).dig("apps", "witselfServer", "resources")
      check("A", cell, "resources presence must match SERVER_RESOURCE_CELLS", values.key?("resources") == SERVER_RESOURCE_CELLS.include?(cell))
      if values.key?("resources")
        check("A", cell, "nested resources must equal the cell override exactly", values["resources"] == configured)
      end
    end

    serving_path = File.join(@root, ".gitops/cells", SERVING_CELL, "values.yaml")
    defaults = read_values(File.join(@server_chart, "values.yaml"), "B", SERVING_CELL)
    server_defaults = defaults.fetch("resources")
    worker_defaults = defaults.fetch("worker").fetch("resources")
    Dir.mktmpdir("apps-server-resources-") do |dir|
      fixture = {"requests" => {"cpu" => "75m", "memory" => "96Mi"}, "limits" => {"memory" => "384Mi"}}
      full = nested_values(override_render(fixture, dir, "B", serving_path), "B", SERVING_CELL)
      check("B", SERVING_CELL, "full nested resources must equal the fixture exactly", full["resources"] == fixture)
      check("B", SERVING_CELL, "resources must be the only changed nested key", full.reject { |key, _| key == "resources" } == baseline.fetch(SERVING_CELL))
      full_docs = child_render(full, dir, "B", SERVING_CELL)
      check("B", SERVING_CELL, "server container must use the full override merged with chart defaults", container_resources(full_docs, "witself-server", "B", SERVING_CELL) == deep_merge(server_defaults, fixture))
      worker_override = baseline.fetch(SERVING_CELL).fetch("worker").fetch("resources")
      check("B", SERVING_CELL, "server resources must not leak into the worker container", container_resources(full_docs, "witself-worker", "B", SERVING_CELL) == deep_merge(worker_defaults, worker_override))

      partial_fixture = {"limits" => {"memory" => "384Mi"}}
      partial = nested_values(override_render(partial_fixture, dir, "C", serving_path), "C", SERVING_CELL)
      explicit = "S5 must set requests and limits explicitly"
      check("C", SERVING_CELL, "partial nested resources must equal the fixture exactly; #{explicit}", partial["resources"] == partial_fixture)
      partial_docs = child_render(partial, dir, "C", SERVING_CELL)
      check("C", SERVING_CELL, "partial container resources must retain chart-default requests; #{explicit}", container_resources(partial_docs, "witself-server", "C", SERVING_CELL) == deep_merge(server_defaults, partial_fixture))

      {"empty" => {}, "null" => nil}.each do |name, value|
        nested = nested_values(override_render(value, dir, "D-#{name}", serving_path), "D", SERVING_CELL)
        check("D", SERVING_CELL, "explicit #{name} must forward no resources key", !nested.key?("resources"))
      end

      {
        "top-level string" => "apps.witselfServer.resources=512Mi",
        "limits not an object" => "apps.witselfServer.resources.limits=512Mi",
        "unknown key" => "apps.witselfServer.resources.limit.memory=512Mi",
        "unknown resource name memroy" => "apps.witselfServer.resources.limits.memroy=512Mi",
        "invalid quantity 512MB" => "apps.witselfServer.resources.limits.memory=512MB"
      }.each do |name, argument|
        _, schema_errors, schema_status = apps_render(serving_path, "--set-string", argument)
        _, skip_errors, skip_status = apps_render(serving_path, "--set-string", argument, "--skip-schema-validation")
        schema_lines = schema_errors.lines.count { |line| line.include?("resources") }
        skip_lines = skip_errors.lines.count { |line| line.include?("resources") }
        if skip_lines > schema_lines
          fail_case("E", SERVING_CELL, "#{name}: skip run has more resources stderr lines (schema=#{schema_lines}, skip=#{skip_lines}); stop and report")
        end
        check("E", SERVING_CELL, "#{name}: schema must reject the value", !schema_status.success?)
        check("E", SERVING_CELL, "#{name}: skipping schema must render successfully: #{skip_errors}", skip_status.success?)
        check("E", SERVING_CELL, "#{name}: schema must add resources stderr lines (schema=#{schema_lines}, skip=#{skip_lines})", schema_lines > skip_lines)
      end
      numeric = apps_render(serving_path, "--set", "apps.witselfServer.resources.requests.cpu=1")
      check("E", SERVING_CELL, "numeric quantity must render: #{numeric[1]}", numeric[2].success?)
      ephemeral = apps_render(serving_path, "--set-string", "apps.witselfServer.resources.requests.ephemeral-storage=1Gi")
      check("E", SERVING_CELL, "ephemeral-storage request must render: #{ephemeral[1]}", ephemeral[2].success?)

      backup_docs = child_render(baseline.fetch(BACKUP_CELL), dir, "F", BACKUP_CELL)
      backup_resources = container_resources(backup_docs, "witself-server", "F", BACKUP_CELL)
      check("F", BACKUP_CELL, "server container limits.memory must remain 256Mi", backup_resources.is_a?(Hash) && backup_resources.dig("limits", "memory") == "256Mi")
    end
    puts "apps server resources: PASS (cases #{@cases.join(',')}; #{cells.length} catalog cells; #{@helm_processes} helm processes)"
  end
end

unless ARGV.length.between?(1, 2)
  abort "case setup / cell all: usage: apps-server-resources.rb ROOT [A,B,C,D,E,F]"
end
cases = ARGV.fetch(1, "A,B,C,D,E,F").split(",", -1).uniq
unknown = cases - %w[A B C D E F]
unless unknown.empty?
  abort "case selector / cell all: unknown cases #{unknown.inspect}"
end
ResourceChecks.new(File.expand_path(ARGV[0]), cases).run
