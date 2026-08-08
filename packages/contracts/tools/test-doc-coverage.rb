#!/usr/bin/env ruby
# frozen_string_literal: true

# Exercise documentation coverage against real Buf descriptor source info.
require "fileutils"
require "minitest/autorun"
require "open3"
require "rbconfig"
require "tmpdir"

PACKAGE_DIR = File.expand_path("..", __dir__)
CHECKER = File.join(__dir__, "check-doc-coverage.rb")
FIXTURES = File.join(PACKAGE_DIR, "tests", "doc-coverage")

class ProtoDocCoverageTest < Minitest::Test
  def test_accepts_leading_block_detached_and_trailing_comments
    status, output = run_fixture("documented.proto")

    assert status.success?, output
  end

  def test_rejects_undocumented_declarations_after_unrelated_comment
    status, output = run_fixture("undocumented.proto")

    refute status.success?, "undocumented fixture unexpectedly passed"
    assert_includes output, "message UndocumentedMessage"
    assert_includes output, "enum UndocumentedState"
  end

  private

  def run_fixture(name)
    Dir.mktmpdir("ouroboros-doc-coverage-") do |directory|
      FileUtils.cp(File.join(FIXTURES, name), File.join(directory, "fixture.proto"))
      stdout, stderr, status = Open3.capture3(RbConfig.ruby, CHECKER, directory)
      [status, stdout + stderr]
    end
  end
end
