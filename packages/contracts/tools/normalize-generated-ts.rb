#!/usr/bin/env ruby
# frozen_string_literal: true

# Normalize tracked Protobuf-ES outputs to exactly one LF at EOF.
#
# Protobuf-ES currently emits an extra blank line. Keeping normalization in the
# generation path makes the checked-in output deterministic and keeps the full
# repository whitespace gate meaningful.

root = ARGV.fetch(0) do
  warn "OURO-CONTRACT-TS-NORMALIZE-ROOT: pass the generated TypeScript directory."
  exit 2
end

paths = Dir.glob(File.join(root, "**", "*_pb.ts")).sort
abort "OURO-CONTRACT-TS-NORMALIZE-EMPTY: no generated TypeScript files found." if paths.empty?

paths.each do |path|
  content = File.binread(path)
  normalized = content.sub(/[ \t\r\n]*\z/, "\n")
  File.binwrite(path, normalized) unless normalized == content
end
