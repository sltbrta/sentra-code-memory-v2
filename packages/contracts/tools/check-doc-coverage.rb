#!/usr/bin/env ruby
# frozen_string_literal: true

# Enforce documentation for public top-level Proto messages and enums by using
# the parser-owned SourceCodeInfo in a Buf image. This handles line comments,
# block comments, detached comment blocks, and declaration-line trailing
# comments without trying to infer syntax from adjacent source lines.
require "json"
require "open3"
require "optparse"
require "pathname"

options = {}
OptionParser.new do |parser|
  parser.banner = "usage: check-doc-coverage.rb [--config PATH] PROTO_ROOT"
  parser.on("--config PATH", "Buf configuration used to resolve module dependencies") do |path|
    options[:config] = File.expand_path(path)
  end
end.parse!

source_root = Pathname.new(ARGV.fetch(0)).realpath
abort "Proto root is not a directory: #{source_root}" unless source_root.directory?

if options[:config]
  config_path = Pathname.new(options[:config]).realpath
  working_directory = config_path.dirname
  input = source_root.relative_path_from(working_directory).to_s
  command = ["buf", "build", "--config", config_path.to_s, input, "-o", "-#format=json"]
else
  working_directory = source_root.dirname
  input = source_root.basename.to_s
  command = ["buf", "build", input, "-o", "-#format=json"]
end

stdout, stderr, status = Open3.capture3(*command, chdir: working_directory.to_s)
abort "Buf descriptor build failed:\n#{stderr}" unless status.success?

image = JSON.parse(stdout)
source_names = Dir.glob(source_root.join("**", "*.proto")).sort.map do |path|
  Pathname.new(path).relative_path_from(source_root).to_s
end
abort "no Proto sources found under #{source_root}" if source_names.empty?

files = image.fetch("file").to_h { |descriptor| [descriptor.fetch("name"), descriptor] }
declarations = []

source_names.each do |source_name|
  descriptor = files.fetch(source_name) do
    abort "Buf image omitted source descriptor: #{source_name}"
  end
  locations = descriptor.fetch("sourceCodeInfo", {}).fetch("location", []).to_h do |location|
    [location.fetch("path", []), location]
  end

  {
    "message" => [4, descriptor.fetch("messageType", [])],
    "enum" => [5, descriptor.fetch("enumType", [])],
  }.each do |kind, (field_number, values)|
    values.each_with_index do |value, index|
      location = locations.fetch([field_number, index], {})
      comment_parts = [
        location["leadingComments"],
        location["trailingComments"],
        *location.fetch("leadingDetachedComments", []),
      ]
      documented = comment_parts.any? { |comment| !comment.to_s.strip.empty? }
      declarations << [source_name, kind, value.fetch("name"), documented]
    end
  end
end

abort "no public Proto declarations found" if declarations.empty?
undocumented = declarations.reject { |(_, _, _, documented)| documented }
unless undocumented.empty?
  details = undocumented.map { |path, kind, name, _| "#{path}: #{kind} #{name}" }.join("\n")
  abort "undocumented public Proto declarations:\n#{details}"
end

puts "validated documentation for #{declarations.length} public Proto declarations"
