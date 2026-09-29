#!/usr/bin/env ruby
# Copies or compares the agent-email receive-cohort and retry-canary Secrets
# between two cells. Secret data stays in this process's memory and reaches a
# cluster only through the standard input of kubectl create. It is never
# printed, logged, stored on disk, placed in an argument, or reduced to a
# length or a digest. Output is one line per Secret: its namespace/name and
# the word match or mismatch.
require 'json'
require 'open3'

module CopyCellSecrets
  # Keep equal to CELLS in scripts/lib/cell-secrets.rb; the test compares them.
  CELLS = %w[civo-prod-use1-serving civo-sandbox-use1-serving civo-sandbox-use1-backup].freeze
  # The backup cell runs no agent email and holds none of these Secrets.
  NO_AGENT_EMAIL = %w[civo-sandbox-use1-backup].freeze
  # Only these two families are copies. Every other operator Secret of a new
  # cell holds a new value (founder answer 7, 2026-09-29).
  ALLOWED = %r{\Awitself/witself-agent-email-(?:receive-cohort|retry-canary)-v[1-9][0-9]{0,2}\z}.freeze
  ALLOWED_TEXT = 'only witself/witself-agent-email-receive-cohort-vN and ' \
                 'witself/witself-agent-email-retry-canary-vN may be copied or compared'.freeze
  USAGE = 'usage: copy-cell-secrets.sh copy|compare SOURCE_CELL TARGET_CELL [--replace] NAMESPACE/NAME...'.freeze
  MAX_SECRETS = 8
  WORKLOADS = 'deployments,statefulsets,daemonsets,jobs,cronjobs'.freeze
  class Refusal < StandardError; end

  def self.refuse(message)
    raise Refusal, message
  end

  def self.invalid(cell)
    refuse("kubectl get failed or returned an invalid response on #{cell}")
  end

  # The one place that starts another program. The argument list holds fixed
  # words, the cell's context, a namespace and a Secret name, never data.
  # Standard error is discarded unread: kubectl can echo object content in
  # its errors.
  def self.kubectl(cell, args, input = '')
    argv = ['kubectl', '--context', "witself-#{cell}", '--request-timeout=20s'] + args
    output, _unread, status = Open3.capture3(*argv, stdin_data: input, binmode: true)
    [output, status.success?]
  end

  def self.read_json(cell, args)
    output, ok = kubectl(cell, args)
    invalid(cell) unless ok
    return nil if output.strip.empty?
    doc = begin
      JSON.parse(output)
    rescue JSON::ParserError, EncodingError
      nil
    end
    invalid(cell) unless doc.is_a?(Hash)
    doc
  end

  def self.field(doc, *keys)
    keys.reduce(doc) { |node, key| node.is_a?(Hash) ? node[key] : nil }
  end

  def self.list(value)
    value.is_a?(Array) ? value : []
  end

  # Before any Secret is read: the two contexts reach two different clusters,
  # and each cluster's Argo CD Application names the expected cell.
  def self.bind!(source, target)
    uids = [source, target].map do |cell|
      uid = field(read_json(cell, %w[get namespace kube-system -o json]), 'metadata', 'uid')
      invalid(cell) unless uid.is_a?(String) && !uid.empty?
      uid
    end
    refuse("witself-#{source} and witself-#{target} reach the same cluster") if uids[0] == uids[1]
    [source, target].each do |cell|
      app = read_json(cell, %w[--namespace argocd get applications.argoproj.io witself-server --ignore-not-found -o json])
      label = field(app, 'metadata', 'labels', 'witself.io/cell')
      refuse("context witself-#{cell} does not identify cell #{cell}") unless label == cell
    end
  end

  def self.secret(cell, namespace, name)
    doc = read_json(cell, ['--namespace', namespace, 'get', 'secret', name, '--ignore-not-found', '-o', 'json'])
    return nil if doc.nil?
    invalid(cell) unless doc['apiVersion'] == 'v1' && doc['kind'] == 'Secret' &&
                         field(doc, 'metadata', 'namespace') == namespace &&
                         field(doc, 'metadata', 'name') == name
    doc
  end

  def self.canonical?(value)
    value.is_a?(String) && [value.unpack1('m0')].pack('m0') == value
  rescue ArgumentError
    false
  end

  # Comparable: immutable, a non-empty type, no stringData, and a non-empty
  # data map whose keys are valid and whose values are canonical base64, so
  # equal strings mean equal bytes.
  def self.comparable?(doc)
    data = doc['data']
    doc['immutable'] == true && doc['type'].is_a?(String) && !doc['type'].empty? &&
      !doc.key?('stringData') && data.is_a?(Hash) && !data.empty? &&
      data.all? { |key, value| key.match?(/\A[A-Za-z0-9._-]{1,253}\z/) && canonical?(value) }
  end

  def self.same?(source, target)
    !target.nil? && comparable?(target) &&
      target['type'] == source['type'] && target['data'] == source['data']
  end

  def self.referenced?(cell, namespace, name)
    items = field(read_json(cell, ['--namespace', namespace, 'get', WORKLOADS, '-o', 'json']), 'items')
    invalid(cell) unless items.is_a?(Array)
    items.any? do |item|
      path = field(item, 'kind') == 'CronJob' ? %w[spec jobTemplate spec template spec] : %w[spec template spec]
      pod_references?(field(item, *path), name)
    end
  end

  def self.pod_references?(spec, name)
    return false unless spec.is_a?(Hash)
    (list(spec['containers']) + list(spec['initContainers'])).each do |container|
      return true if list(field(container, 'env')).any? { |env| field(env, 'valueFrom', 'secretKeyRef', 'name') == name }
      return true if list(field(container, 'envFrom')).any? { |from| field(from, 'secretRef', 'name') == name }
    end
    list(spec['volumes']).any? do |volume|
      field(volume, 'secret', 'secretName') == name ||
        list(field(volume, 'projected', 'sources')).any? { |source| field(source, 'secret', 'name') == name }
    end
  end

  def self.manifest(source, target, namespace, name)
    doc = {
      'apiVersion' => 'v1',
      'kind' => 'Secret',
      'metadata' => { 'name' => name, 'namespace' => namespace,
                      'annotations' => { 'witself.io/cell' => target } },
      'type' => source['type'],
      'immutable' => true,
      'data' => source['data']
    }
    JSON.generate(doc) + "\n"
  end

  # The one function that changes a cluster. It is given the target cell
  # only; nothing is ever written through the source context.
  def self.write!(target, namespace, name, source, delete_first)
    if delete_first
      _output, ok = kubectl(target, ['--namespace', namespace, 'delete', 'secret', name])
      unless ok
        refuse("kubectl delete failed on #{target} for #{namespace}/#{name}; " \
               'the Secret may now be absent there, re-run copy')
      end
    end
    _output, ok = kubectl(target, ['--namespace', namespace, 'create', '--save-config=false', '-f', '-'],
                          manifest(source, target, namespace, name))
    refuse("kubectl create failed on #{target} for #{namespace}/#{name}") unless ok
    return if same?(source, secret(target, namespace, name))
    refuse("#{namespace}/#{name} did not read back identical on #{target} after the write")
  end

  def self.report(namespace, name, matched)
    puts("#{namespace}/#{name} #{matched ? 'match' : 'mismatch'}")
  end

  def self.parse(args)
    verb = args.shift
    refuse(USAGE) unless %w[copy compare].include?(verb)
    flags, positional = args.partition { |arg| arg.start_with?('-') }
    refuse(USAGE) unless flags.empty? || flags == ['--replace']
    replace = flags == ['--replace']
    refuse('--replace is only valid with copy') if replace && verb != 'copy'
    source, target, *names = positional
    refuse(USAGE) if names.empty? || names.length > MAX_SECRETS
    refuse('unknown cell') unless CELLS.include?(source) && CELLS.include?(target)
    refuse('civo-sandbox-use1-backup runs no agent email') unless (NO_AGENT_EMAIL & [source, target]).empty?
    refuse('source and target cells must differ') if source == target
    refuse(ALLOWED_TEXT) unless names.all? { |name| name.match?(ALLOWED) }
    refuse('each Secret may be named once') unless names.uniq == names
    [verb, replace, source, target, names.map { |name| name.split('/', 2) }]
  end

  def self.main(args)
    verb, replace, source, target, names = parse(args)
    bind!(source, target)
    sources = names.map do |namespace, name|
      doc = secret(source, namespace, name)
      refuse("source Secret #{namespace}/#{name} is absent on #{source}") if doc.nil?
      unless comparable?(doc)
        refuse("source Secret #{namespace}/#{name} on #{source} is not an immutable Secret with data")
      end
      doc
    end
    targets = names.map { |namespace, name| secret(target, namespace, name) }
    matched = sources.each_index.map { |index| same?(sources[index], targets[index]) }
    differing = targets.each_index.select { |index| !targets[index].nil? && !matched[index] }
    if verb == 'compare' || (!replace && !differing.empty?)
      names.each_with_index { |(namespace, name), index| report(namespace, name, matched[index]) }
      return matched.all? ? 0 : 3
    end
    differing.each do |index|
      namespace, name = names[index]
      next unless referenced?(target, namespace, name)
      refuse("#{namespace}/#{name} is referenced by a workload on #{target}; " \
             'create a new -vN Secret instead of replacing it')
    end
    names.each_with_index do |(namespace, name), index|
      write!(target, namespace, name, sources[index], differing.include?(index)) unless matched[index]
      report(namespace, name, true)
    end
    0
  end
end

message = nil
status = 1
begin
  # Open3's reader threads die when a signal interrupts a kubectl call; their
  # reports would add lines to standard error.
  Thread.report_on_exception = false
  Signal.trap('TERM') { raise Interrupt }
  Signal.trap('HUP') { raise Interrupt }
  status = CopyCellSecrets.main(ARGV.dup)
rescue CopyCellSecrets::Refusal => error
  message = error.message
rescue Interrupt
  message = 'interrupted'
rescue Exception
  # Parser, OS and subprocess exception texts can contain Secret data.
  message = 'operation refused (invalid input or failed local operation)'
end
warn("copy-cell-secrets: #{message}") if message
exit(message ? 1 : status)
