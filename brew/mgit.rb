class Mgit < Formula
  desc "Sandboxed, checkpointed working substrate for LLM coding agents"
  homepage "https://github.com/hyper-swe/mgit"
  license "Apache-2.0"
  # version + all four sha256 below are a template: the tap's own
  # update-formula.yml (hyper-swe/homebrew-tap) overwrites them at every real
  # release from that release's checksums.txt via a literal TEXT regex over
  # this file's declaration order (darwin arm/intel, linux arm/intel) --
  # `sha256 "[A-Fa-f0-9]*"`, one quoted literal per line, NOT a Ruby
  # variable: a shared constant would render as `sha256 SOME_CONST` in the
  # file, which the regex does not match at all. That regex also requires
  # 64 HEX characters -- the literal word "PLACEHOLDER" that used to be here
  # does NOT match it either, so the automated update would silently leave a
  # non-hex value in place forever instead of overwriting it. 64 zeros is a
  # valid (obviously fake) sha256-shaped placeholder the real regex actually
  # replaces. Never hand-edit these four lines; they are correct only right
  # after a real release's automated update.
  version "0.1.0"

  on_macos do
    on_arm do
      url "https://github.com/hyper-swe/mgit/releases/download/v#{version}/mgit_#{version}_darwin_arm64.tar.gz"
      sha256 "0000000000000000000000000000000000000000000000000000000000000000"
    end
    on_intel do
      url "https://github.com/hyper-swe/mgit/releases/download/v#{version}/mgit_#{version}_darwin_amd64.tar.gz"
      sha256 "0000000000000000000000000000000000000000000000000000000000000000"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/hyper-swe/mgit/releases/download/v#{version}/mgit_#{version}_linux_arm64.tar.gz"
      sha256 "0000000000000000000000000000000000000000000000000000000000000000"
    end
    on_intel do
      url "https://github.com/hyper-swe/mgit/releases/download/v#{version}/mgit_#{version}_linux_amd64.tar.gz"
      sha256 "0000000000000000000000000000000000000000000000000000000000000000"
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
  end

  # NO `depends_on "libkrun/krun/libkrun"`. DO NOT ADD ONE BACK.
  #
  # The macOS daemon does link libkrun (the GA default backend, ADR-010), but
  # declaring that here broke `brew install hyper-swe/tap/mgit` outright for
  # every user who did not already have libkrun (MGIT-75). Homebrew refuses to
  # LOAD a formula from an untrusted third-party tap, and dependency
  # resolution happens before anything is fetched, so the install aborted with
  # exit 1 having installed NOTHING — not even core mgit, which never links
  # libkrun at all (it is CGO-free). Reproduced on a Homebrew prefix with
  # libkrun genuinely absent, in both states a real user can be in:
  #
  #   tap absent:    Warning: No available formula with the name
  #                  "libkrun/krun/libkrun". This command requires the tap
  #                  libkrun/krun.
  #   tap untrusted: Error: Refusing to load formula libkrun/krun/libkrun
  #                  from untrusted tap libkrun/krun.
  #
  # `=> :optional` is not the fix either. It does dodge the load (measured),
  # but its opt-in path `--with-libkrun` still fails — on libkrunfw, which
  # libkrun itself depends on from the same untrusted tap, and which no
  # command-line argument can whitelist. Only `brew trust libkrun/krun`
  # unblocks it, and a trust decision about a third-party VMM belongs to the
  # user who wants a sandbox, not to everyone who wants a commit substrate.
  #
  # So the sandbox is a documented second step (see caveats). The daemon
  # itself fails closed with an actionable message when libkrun is missing:
  # mgit captures the dynamic loader's error and names the remedy
  # (cmd/mgit/sandbox_activation.go). Refs: MGIT-75, MGIT-61.15

  # The sandbox's libkrun backend needs a libkrun BUILT WITH NETWORKING.
  # mgit attaches an explicit network device to every sandbox in every mode;
  # without one libkrun falls back to TSI and the guest gets full host egress,
  # so there is no NIC-less mode to fall back to and the daemon refuses to
  # start. The libkrun/krun tap builds with NET=1, so `brew install libkrun`
  # is covered — this caveat exists for anyone using a hand-built library.
  # Refs: MGIT-61.14, ADR-010
  #
  # Guest provisioning differs by BACKEND, not just by platform: macOS
  # (libkrun) composes its guest from an OCI image with no kernel/rootfs of
  # its own (libkrunfw supplies the kernel); Linux (firecracker) still needs
  # a real kernel + rootfs pair. Presenting one unified step here would be
  # firecracker's framing leaking onto a platform that does not use
  # firecracker at all. Refs: MGIT-61.13, MGIT-61.15, ADR-010
  def caveats
    <<~EOS
      Core mgit (init, commit, worktrees, squash, land) is ready to use.
      Nothing below is needed for any of it.

      To activate the microVM sandbox (mgit run, mgit work --sandbox):
        1. Prerequisites:
           - Linux: KVM (/dev/kvm) and the `firecracker` binary on PATH
           - macOS: Apple Silicon (arm64), macOS 14 or later, plus the libkrun
             hypervisor, which is NOT installed with mgit -- it lives in a
             third-party tap you have to trust before Homebrew will load it:
               brew tap libkrun/krun
               brew trust libkrun/krun
               brew install libkrun
             (`brew trust` is required, and whole-tap trust specifically:
             libkrun pulls in libkrunfw from the same tap.)
           (Windows and Intel macOS have no sandbox backend yet)
        2. Provision the guest (the two backends do this differently):
           - macOS (libkrun): compose one from any Linux image --
               mgit sandbox base from debian:12
           - Linux (firecracker): register a kernel + rootfs pair --
               mgit sandbox image install
             (or, from artifacts you already have:
               mgit sandbox image add --kernel <vmlinux> --rootfs <rootfs>)

      libkrun must be built WITH networking support, which the libkrun/krun
      tap does. If you build libkrun yourself, build it with `make NET=1`.
      Verify:

        nm -gU "$(brew --prefix libkrun)/lib/libkrun.dylib" | grep krun_add_net_unixgram

      A libkrun without that symbol cannot host a sandbox: mgit requires an
      explicit network device in every mode, and without one the guest would
      get unrestricted host egress. mgit-sandboxd fails closed in that case,
      as it does when libkrun is absent entirely -- core mgit is unaffected
      either way.

      Guide: https://github.com/hyper-swe/mgit/blob/main/docs/INSTALL-SANDBOX.md
    EOS
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/mgit --version")
  end
end
