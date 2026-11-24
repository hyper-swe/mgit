# Check the producer and consumer together. No workflow step is executed.
require "yaml"

workflows = File.expand_path("../../.github/workflows", __dir__)
release = YAML.load_file(File.join(workflows, "release.yml")).fetch("jobs")
e2e = YAML.load_file(File.join(workflows, "e2e.yml"))
jobs = e2e.fetch("jobs")
errors = []
check = ->(ok, label) { errors << label unless ok }
called = release.fetch("e2e")
check.call(called["uses"] == "./.github/workflows/e2e.yml" &&
           called.dig("with", "release_artifacts") == true, "release gate must request artifacts")
# Older Psych versions parse the unquoted YAML key 'on' as true.
events = e2e["on"] || e2e.fetch(true)
input = events.dig("workflow_call", "inputs", "release_artifacts") || {}
check.call(input["type"] == "boolean" && input["default"] == false, "standalone builds must not opt in")

source_gate = "matrix.arch == 'amd64' && (github.ref_type == 'tag' || inputs.release_artifacts)"
mac_gate = "github.ref_type == 'tag' || inputs.release_artifacts"
linux = jobs.fetch("linux-sandboxd").fetch("steps")
build = linux.select { |s| s.fetch("run", "").include?('src="$RUNNER_TEMP/dist-sources"') }
check.call(build.size == 1 && build.first["if"] == source_gate, "Linux source assembly must run once on release or rehearsal")
[["linux-sources", linux, source_gate], ["darwin-libkrunfw", jobs.fetch("darwin-libkrunfw").fetch("steps"), mac_gate]].each do |name, steps, gate|
  uploads = steps.select { |s| s.fetch("uses", "").start_with?("actions/upload-artifact@") && s.dig("with", "name") == name }
  check.call(uploads.size == 1 && uploads.first["if"] == gate, "missing release or rehearsal upload: #{name}")
  downloads = release.fetch("release").fetch("steps").select { |s| s.fetch("uses", "").start_with?("actions/download-artifact@") && s.dig("with", "name") == name }
  check.call(downloads.size == 1 && !downloads.first.key?("if"), "release must still require #{name}")
end
abort errors.join("\n") unless errors.empty?
puts "Release and rehearsal artifact contracts match."
