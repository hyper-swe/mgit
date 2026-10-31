class Mgit < Formula
  desc "Sandboxed, checkpointed working substrate for LLM coding agents"
  homepage "https://github.com/hyper-swe/mgit"
  # The four url lines and the four sha256 lines below are a template: the
  # tap's own update-formula.yml (hyper-swe/homebrew-tap) overwrites them at
  # every real release from that release's checksums.txt. The version is the
  # one in the urls (Homebrew reads it from there), so there is no version
  # line to keep in step. The urls are rewritten by
  # .github/scripts/rewrite_formula_urls.py, and the shas by a literal TEXT
  # regex over this file's declaration order (darwin arm/intel, linux
  # arm/intel) -- `sha256 "[A-Fa-f0-9]*"`, one quoted literal per line, NOT a
  # Ruby variable: a shared constant would render as `sha256 SOME_CONST`,
  # which the regex does not match at all. That regex also requires 64 HEX
  # characters -- the literal word "PLACEHOLDER" does NOT match it either.
  # Never hand-edit these eight lines; they are correct only right after a
  # real release's automated update.
  license "Apache-2.0"

  # libkrunfw, the library that carries the sandbox's guest kernel, comes from
  # this tap (hyper-swe/tap), never from mgit's archive: it is distributed
  # under LGPL-2.1-only and GPL-2.0-only, and the maintainers keep it out of
  # mgit's own release. The macOS archive carries libkrun (MGIT-259), so
  # libkrunfw is the one external library, and only Apple silicon macOS has a
  # sandbox backend that needs it. The Linux archive carries its own copy.
  #
  # The one setup step this needs is `brew trust hyper-swe/tap`. Homebrew
  # trusts a formula named on the command line (`brew install
  # hyper-swe/tap/mgit`) but refuses to load that formula's DEPENDENCY from a
  # tap nobody trusted, and resolves dependencies before it fetches anything.
  #
  # NEVER a dependency on a third-party tap, such as the libkrun/krun tap this
  # formula once named (`depends_on "libkrun/krun/libkrun"`). Trusting that
  # tap was a decision about a third-party VMM that belonged to the user, and
  # without it `brew install hyper-swe/tap/mgit` aborted with exit 1 having
  # installed nothing, not even core mgit, which never links libkrun (it is
  # CGO-free). The same trust step that installs mgit now covers libkrunfw.
  # Refs: MGIT-306, MGIT-75, MGIT-61.15
  on_macos do
    on_arm do
      depends_on "hyper-swe/tap/libkrunfw"
      url "https://github.com/hyper-swe/mgit/releases/download/v0.7.6/mgit_0.7.6_darwin_arm64.tar.gz"
      sha256 "d22423ab959113586b3ed1c6832bdb1b156fc14fbf60a404bf11aa8853f1268c"
    end
    on_intel do
      url "https://github.com/hyper-swe/mgit/releases/download/v0.7.6/mgit_0.7.6_darwin_amd64.tar.gz"
      sha256 "2809df46d53d7dde780c272f5b136db94150d1c1da4c2381e428043d55726783"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/hyper-swe/mgit/releases/download/v0.7.6/mgit_0.7.6_linux_arm64.tar.gz"
      sha256 "781a2b93ef6ae255b40098f4b7f789c90e61d2351ff1baab4c995d99881789be"
    end
    on_intel do
      url "https://github.com/hyper-swe/mgit/releases/download/v0.7.6/mgit_0.7.6_linux_amd64.tar.gz"
      sha256 "f119573dd3c4194c102742a061f84804d598c7e73a8f612ec2ab06adc8a05934"
    end
  end

  def install
    bin.install "mgit"
    # mgit-sandboxd ships in the Linux and macOS-arm64 archives only (the
    # sandbox has no macOS-Intel or Windows backend yet). File.exist? keeps
    # one install block correct across every bottle rather than branching
    # per-platform: it installs the daemon wherever the archive carries it
    # and silently skips it where the archive is mgit-only.
    # Refs: MGIT-44
    bin.install "mgit-sandboxd" if File.exist?("mgit-sandboxd")
    # The linux mgit + mgit-guest the archive carries under guest/. They are
    # what `mgit sandbox base from <image>` injects into a guest base, and a
    # host install can produce them no other way — cross-building needs the
    # mgit source and a Go toolchain, which a brew user has neither of.
    #
    # libexec, NOT bin: everything in bin is linked onto PATH, and mgit-guest
    # is guest-only (it refuses to run on a host). mgit looks for guest/ beside
    # its own binary first and then in ../libexec, so this layout is found.
    # Refs: MGIT-65, MGIT-61.15
    libexec.install "guest" if Dir.exist?("guest")
    # The Linux and macOS archives bundle the daemon's hypervisor libraries in
    # lib/ (Linux: libkrun, libkrunfw; macOS: libkrun). The daemon's run path
    # looks for them at $ORIGIN/../lib/mgit (@executable_path/../lib/mgit),
    # which from the keg's bin is the keg's lib/mgit; a subdirectory, so
    # nothing else on the system links against them by accident. Their license
    # texts and source notice go to pkgshare. Refs: MGIT-230.1, MGIT-229, MGIT-259
    (lib/"mgit").install Dir["lib/*"] if Dir.exist?("lib")
    (pkgshare/"THIRD_PARTY").install Dir["THIRD_PARTY/*"] if Dir.exist?("THIRD_PARTY")
  end

  # The sandbox's libkrun backend needs a libkrun BUILT WITH NETWORKING.
  # mgit attaches an explicit network device to every sandbox in every mode;
  # without one libkrun falls back to TSI and the guest gets full host egress,
  # so there is no NIC-less mode to fall back to and the daemon refuses to
  # start. Refs: MGIT-61.14, ADR-010
  #
  # Guest provisioning is the same on both platforms now: the release's
  # daemon is libkrun on macOS and on Linux (ADR-016), and libkrun composes
  # its guest from an OCI image with no kernel/rootfs of its own (libkrunfw
  # supplies the kernel). The kernel+rootfs path belongs to a firecracker
  # source build, which Homebrew does not install. Refs: MGIT-61.15, ADR-010,
  # ADR-016, MGIT-230.1
  def caveats
    <<~EOS
      Core mgit (init, commit, worktrees, squash, land) is ready to use.
      Nothing below is needed for any of it.

      To activate the microVM sandbox (mgit run, mgit work --sandbox):
        1. Prerequisites:
           - Linux: KVM (/dev/kvm read-writable by you) and glibc 2.31+;
             the daemon's hypervisor libraries are installed with mgit
           - macOS: Apple Silicon (arm64), macOS 14 or later. The daemon's
             libkrun is installed with mgit, and libkrunfw, the guest kernel
             library, is installed with it from this tap (hyper-swe/tap),
             which you trust once before installing:
               brew trust hyper-swe/tap
               brew install hyper-swe/tap/mgit
             libkrunfw is a separate library under its own licenses
             (LGPL-2.1-only AND GPL-2.0-only); its keg holds the license
             texts and the source offer.
           (Windows and Intel macOS have no sandbox backend yet)
        2. Provision the guest: compose one from any Linux image --
               mgit sandbox base from debian:12

      libkrun must be built WITH networking support. The libkrun installed
      with mgit is checked for it before release. If you build the daemon
      or libkrun yourself, build libkrun with `make NET=1`, and verify it:

        nm -gU /path/to/your/libkrun.dylib | grep krun_add_net_unixgram

      A libkrun without networking cannot host a sandbox: mgit requires an
      explicit network device in every mode, and without one the guest would
      get unrestricted host egress. mgit-sandboxd fails closed in that case
      -- core mgit is unaffected either way.

      Guide: https://github.com/hyper-swe/mgit/blob/main/docs/INSTALL-SANDBOX.md
    EOS
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/mgit --version")
  end
end
