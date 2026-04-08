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

  test do
    assert_match version.to_s, shell_output("#{bin}/mgit --version")
  end
end
