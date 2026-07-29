class Mgit < Formula
  desc "Sandboxed, checkpointed working substrate for LLM coding agents"
  homepage "https://github.com/hyper-swe/mgit"
  license "Apache-2.0"
  version "0.1.0"

  on_macos do
    on_arm do
      url "https://github.com/hyper-swe/mgit/releases/download/v#{version}/mgit_#{version}_darwin_arm64.tar.gz"
      sha256 "PLACEHOLDER"
    end
    on_intel do
      url "https://github.com/hyper-swe/mgit/releases/download/v#{version}/mgit_#{version}_darwin_amd64.tar.gz"
      sha256 "PLACEHOLDER"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/hyper-swe/mgit/releases/download/v#{version}/mgit_#{version}_linux_arm64.tar.gz"
      sha256 "PLACEHOLDER"
    end
    on_intel do
      url "https://github.com/hyper-swe/mgit/releases/download/v#{version}/mgit_#{version}_linux_amd64.tar.gz"
      sha256 "PLACEHOLDER"
    end
  end

  def install
    bin.install "mgit"
    # mgit-sandboxd ships in the Linux and macOS-arm64 archives only (the
    # sandbox has no macOS-Intel or Windows backend yet). File.exist? keeps
    # one install block correct across every bottle rather than branching
    # per-platform: it installs the daemon wherever the archive carries it
    # and silently skips it where the archive is mgit-only.
    # Refs: MGIT-44, docs/release/homebrew-tap-formula.md
    bin.install "mgit-sandboxd" if File.exist?("mgit-sandboxd")
  end

  # The macOS daemon LINKS libkrun (the GA default backend, ADR-010), so it
  # is a hard runtime dependency there — not an optional extra. The tap was
  # renamed from slp/krun; `brew tap slp/krun` no longer resolves.
  on_macos do
    depends_on "libkrun/krun/libkrun"
  end

  # The sandbox's libkrun backend needs a libkrun BUILT WITH NETWORKING.
  # mgit attaches an explicit network device to every sandbox in every mode;
  # without one libkrun falls back to TSI and the guest gets full host egress,
  # so there is no NIC-less mode to fall back to and the daemon refuses to
  # start. The libkrun/krun tap builds with NET=1, so `brew install libkrun`
  # is covered — this caveat exists for anyone using a hand-built library.
  # Refs: MGIT-61.14, ADR-010
  def caveats
    <<~EOS
      Sandbox: mgit-sandboxd uses the libkrun backend on macOS, which needs
      macOS 14+ on Apple Silicon. libkrun is installed as a dependency; it
      must be built WITH networking support, which the libkrun/krun tap does.

      If you build libkrun yourself, build it with `make NET=1`. Verify:

        nm -gU "$(brew --prefix libkrun)/lib/libkrun.dylib" | grep krun_add_net_unixgram

      A libkrun without that symbol cannot host a sandbox: mgit requires an
      explicit network device in every mode, and without one the guest would
      get unrestricted host egress. mgit-sandboxd fails closed in that case.
    EOS
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/mgit --version")
  end
end
