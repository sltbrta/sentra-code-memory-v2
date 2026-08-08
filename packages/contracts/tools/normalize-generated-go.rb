#!/usr/bin/env ruby
# frozen_string_literal: true

# Normalize tracked Go generated outputs to the repository import-order gate.
#
# The pinned remote Go plugins emit one import block with standard-library
# paths last. The repository format gate (goimports) requires standard-library
# imports first, one blank line, then external imports. Keeping this
# normalization in the generation path makes the checked-in output
# deterministic and keeps the full repository format gate meaningful. The
# script is strict: any import line shape it does not recognize aborts the run.

root = ARGV.fetch(0) do
  warn "OURO-CONTRACT-GO-NORMALIZE-ROOT: pass the generated Go directory."
  exit 2
end

paths = Dir.glob(File.join(root, "**", "*.go")).sort
abort "OURO-CONTRACT-GO-NORMALIZE-EMPTY: no generated Go files found." if paths.empty?

import_block = /import \(\n(?<body>(?:\t[^\n]*\n)+)\)/
import_line = /\A\t(?:[A-Za-z_][A-Za-z0-9_]* )?"[^"]+"\n\z/

paths.each do |path|
  content = File.binread(path)
  normalized = content.gsub(import_block) do
    body = Regexp.last_match[:body]
    lines = body.scan(/^\t[^\n]*\n/)
    unless lines.join == body && lines.all? { |line| line.match?(import_line) }
      abort "OURO-CONTRACT-GO-NORMALIZE-SHAPE: unexpected import block in #{path}."
    end
    stdlib, external = lines.partition do |line|
      !line[/"([^"]+)"/, 1].split("/").first.include?(".")
    end
    by_path = ->(line) { line[/"([^"]+)"/, 1] }
    groups = []
    groups << stdlib.sort_by(&by_path).join unless stdlib.empty?
    groups << external.sort_by(&by_path).join unless external.empty?
    "import (\n#{groups.join("\n")})"
  end
  File.binwrite(path, normalized) unless normalized == content
end
