class Mgit < Formula
  desc "Safety-critical micro version control for LLM coding agents"
  homepage "https://github.com/hyper-swe/mgit-dev"
  license "Apache-2.0"
  version "0.1.0"

  on_macos do
    on_arm do
      url "https://github.com/hyper-swe/mgit-dev/releases/download/v#{version}/mgit_#{version}_darwin_arm64.tar.gz"
      sha256 "PLACEHOLDER"
    end
    on_intel do
      url "https://github.com/hyper-swe/mgit-dev/releases/download/v#{version}/mgit_#{version}_darwin_amd64.tar.gz"
      sha256 "PLACEHOLDER"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/hyper-swe/mgit-dev/releases/download/v#{version}/mgit_#{version}_linux_arm64.tar.gz"
      sha256 "PLACEHOLDER"
    end
    on_intel do
      url "https://github.com/hyper-swe/mgit-dev/releases/download/v#{version}/mgit_#{version}_linux_amd64.tar.gz"
      sha256 "PLACEHOLDER"
    end
  end

  def install
    bin.install "mgit"
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
      Sandbox (optional): the libkrun backend requires macOS 14+ on Apple
      Silicon and a libkrun built WITH networking support:

        brew tap libkrun/krun && brew install libkrun

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
