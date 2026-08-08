#!/usr/bin/env ruby
# frozen_string_literal: true

# Fail when a generated TypeScript file does not end in exactly one LF.

root = ARGV.fetch(0) do
  warn "OURO-CONTRACT-TS-EOF-ROOT: pass the generated TypeScript directory."
  exit 2
end

paths = Dir.glob(File.join(root, "**", "*_pb.ts")).sort
abort "OURO-CONTRACT-TS-EOF-EMPTY: no generated TypeScript files found." if paths.empty?

invalid = paths.reject do |path|
  content = File.binread(path)
  content.end_with?("\n") && !content.end_with?("\n\n") && !content.include?("\r\n")
end

unless invalid.empty?
  warn "OURO-CONTRACT-TS-EOF: generated TypeScript must end in exactly one LF:"
  invalid.each { |path| warn path }
  exit 1
end
