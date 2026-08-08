#!/usr/bin/env ruby
# frozen_string_literal: true

# Validate contract-vector coverage and shape without claiming runtime enforcement.
require "json"

fixture_path = ARGV.fetch(0)
fixture = JSON.parse(File.read(fixture_path))
expected_categories = %w[
  missing empty malformed wrong_type oversized duplicate concurrent stale revoked principal_mismatch
].freeze
required_case_keys = %w[id category targetType request expected].freeze
required_expected_keys = %w[admitted receiptStatus reasonCode].freeze

abort "fixture schemaVersion mismatch" unless fixture.fetch("schemaVersion") == "ouroboros.contracts.boundary.v1"
cases = fixture.fetch("cases")
abort "fixtures must be an array" unless cases.is_a?(Array)
categories = cases.map { |entry| entry.fetch("category") }.uniq
abort "fixture category coverage mismatch" unless (expected_categories - categories).empty?
abort "fixture ids must be unique" unless cases.map { |entry| entry.fetch("id") }.uniq.length == cases.length
required_capability_cases = %w[
  capability-empty-actions capability-wildcard-action capability-expired
  capability-wrong-principal capability-oversized-nonce
].freeze
abort "capability fixture coverage mismatch" unless (required_capability_cases - cases.map { |entry| entry.fetch("id") }).empty?

cases.each do |entry|
  abort "fixture keys mismatch for #{entry.inspect}" unless entry.keys.sort == required_case_keys.sort
  abort "fixture request must be an object" unless entry.fetch("request").is_a?(Hash)
  expected = entry.fetch("expected")
  abort "fixture expected keys mismatch for #{entry.fetch("id")}" unless expected.keys.sort == required_expected_keys.sort
  abort "fixture #{entry.fetch("id")} must reject" unless expected.fetch("admitted") == false
  abort "fixture #{entry.fetch("id")} has invalid receipt status" unless expected.fetch("receiptStatus") == "RECEIPT_STATUS_REJECTED"
  abort "fixture #{entry.fetch("id")} has empty reason code" if expected.fetch("reasonCode").empty?
end
