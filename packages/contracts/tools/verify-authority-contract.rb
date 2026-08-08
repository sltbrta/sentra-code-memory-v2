#!/usr/bin/env ruby
# frozen_string_literal: true

# Verify the Stage 02 service shape from Buf's descriptor, including its required oneof.
require "json"
require "open3"

package_dir = File.expand_path("..", __dir__)
stdout, stderr, status = Open3.capture3("buf", "build", "--config", "buf.yaml", "proto", "-o", "-#format=json", chdir: package_dir)
abort "authority descriptor build failed: #{stderr}" unless status.success?

image = JSON.parse(stdout)
file = image.fetch("file").find { |entry| entry.fetch("name") == "ouroboros/contracts/v1/local_authority.proto" }
abort "local authority proto missing from descriptor" unless file
service = file.fetch("service").find { |entry| entry.fetch("name") == "LocalAuthorityService" }
abort "local authority service shape mismatch" unless service && service.fetch("method").map { |method| method.fetch("name") } == %w[OpenLocalSession ExecuteAuthorityCommand ReadStatus]
request = file.fetch("messageType").find { |entry| entry.fetch("name") == "ExecuteAuthorityCommandRequest" }
abort "authority command request missing" unless request
oneof = request.fetch("oneofDecl").find { |entry| entry.fetch("name") == "artifact_command" }
abort "authority command oneof missing" unless oneof
source = File.read(File.join(package_dir, "proto/ouroboros/contracts/v1/local_authority.proto"))
abort "authority command oneof is not required" unless source.include?("option (buf.validate.oneof).required = true;")
fields = request.fetch("field").select { |entry| entry["oneofIndex"] == 0 }.map { |entry| entry.fetch("name") }
abort "authority command oneof alternatives mismatch" unless fields.sort == %w[artifact_admit artifact_delete artifact_read]

security_file = image.fetch("file").find { |entry| entry.fetch("name") == "ouroboros/contracts/v1/security.proto" }
abort "security proto missing from descriptor" unless security_file
grant = security_file.fetch("messageType").find { |entry| entry.fetch("name") == "CapabilityGrant" }
abort "capability grant missing from descriptor" unless grant
grant_fields = grant.fetch("field").map { |field| field.fetch("name") }
required_grant_fields = %w[grant_id initiator actions resources nonce expires_at policy_digest command_fence]
abort "capability grant required field shape mismatch" unless (required_grant_fields - grant_fields).empty?
security_source = File.read(File.join(package_dir, "proto/ouroboros/contracts/v1/security.proto"))
required_security_rules = [
  "Identifier grant_id = 1 [(buf.validate.field).required = true];",
  "AuthenticatedPrincipalRef initiator = 2 [(buf.validate.field).required = true];",
  "google.protobuf.Timestamp expires_at = 15 [(buf.validate.field).required = true];",
  "Digest policy_digest = 16 [(buf.validate.field).required = true];",
  "uint64 command_fence = 17 [(buf.validate.field).uint64.gt = 0];",
  "pattern: \"^[a-z][a-z0-9_.:-]{0,127}$\"",
].freeze
abort "capability grant validation annotations missing" unless required_security_rules.all? { |rule| security_source.include?(rule) }

puts "validated local authority service, required command oneof, and capability grant schema shape"
