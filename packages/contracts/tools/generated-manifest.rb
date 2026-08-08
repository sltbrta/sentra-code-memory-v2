#!/usr/bin/ruby
# frozen_string_literal: true

# Writes or verifies the source/output digest manifest for generated contracts.
#
# Ordinary Stage 01 checks must detect stale or manually edited projections
# without invoking credentialed remote Buf plugins. The manifest therefore
# binds every generation input, normalization script, and generated output by
# path, byte length, and SHA-256. Only the explicit `write` mode mutates the
# tracked manifest; `check` is read-only and returns one static failure code.

require "digest"
require "json"

PACKAGE_ROOT = File.realpath(File.join(__dir__, ".."))
MANIFEST_PATH = File.join(PACKAGE_ROOT, "generated-manifest.json")
MAX_FILES = 20_000
MAX_FILE_BYTES = 64 * 1024 * 1024
INPUT_PATTERNS = [
  "buf.gen.yaml",
  "buf.lock",
  "buf.yaml",
  "proto/**/*.proto",
  "tools/generate.sh",
  "tools/normalize-generated-go.rb",
  "tools/normalize-generated-ts.rb",
  "tools/templates/**/*"
].freeze
OUTPUT_PATTERNS = ["gen/**/*"].freeze

# Returns deterministic metadata for regular, bounded files.
def digest_map(patterns, label)
  paths = patterns.flat_map { |pattern| Dir.glob(File.join(PACKAGE_ROOT, pattern), File::FNM_DOTMATCH) }
                  .uniq
                  .select { |path| File.file?(path) }
                  .sort
  abort "OURO-CONTRACT-MANIFEST-#{label}-EMPTY" if paths.empty?
  abort "OURO-CONTRACT-MANIFEST-#{label}-OVERSIZED" if paths.length > MAX_FILES

  paths.to_h do |path|
    stat = File.lstat(path)
    abort "OURO-CONTRACT-MANIFEST-UNSAFE-FILE" unless stat.file? && stat.size <= MAX_FILE_BYTES

    relative = path.delete_prefix("#{PACKAGE_ROOT}/")
    [relative, { "bytes" => stat.size, "sha256" => Digest::SHA256.file(path).hexdigest }]
  end
end

# Builds the canonical manifest from current sources and outputs.
def manifest_payload
  {
    "generator" => { "buf" => "1.71.0" },
    "inputs" => digest_map(INPUT_PATTERNS, "INPUTS"),
    "outputs" => digest_map(OUTPUT_PATTERNS, "OUTPUTS"),
    "schema_version" => "stage1.contract-generated-manifest.v1"
  }
end

# Encodes the insertion-ordered payload with stable formatting.
def manifest_content
  "#{JSON.pretty_generate(manifest_payload)}\n"
end

case ARGV
when ["write"]
  part_path = "#{MANIFEST_PATH}.part"
  begin
    File.open(part_path, File::WRONLY | File::CREAT | File::EXCL, 0o600) do |file|
      file.write(manifest_content)
      file.flush
      file.fsync
    end
    File.rename(part_path, MANIFEST_PATH)
  ensure
    File.delete(part_path) if File.file?(part_path)
  end
  puts '{"schema_version":"stage1.contract-generated-manifest.v1","status":"written"}'
when ["check"]
  actual = File.binread(MANIFEST_PATH)
  abort "OURO-CONTRACT-GENERATED-DRIFT: run just generate and review the output." unless actual == manifest_content

  puts '{"schema_version":"stage1.contract-generated-manifest.v1","status":"ok"}'
else
  warn "OURO-CONTRACT-MANIFEST-INVALID-COMMAND: use write or check."
  exit 64
end
