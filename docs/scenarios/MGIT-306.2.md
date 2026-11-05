# MGIT-306.2: libkrunfw 5.5.0 to 5.6.2

1. One pin, `LIBKRUNFW_VERSION` in `scripts/sandbox-image/pins.env`, names v5.6.2.
   Its kernel (6.12.109, digest from kernel.org's published sums), upstream's
   prebuilt asset and the source archive each have their own pinned digest.
2. `install.sh` tells a macOS user it is downloading libkrunfw 5.6.2 with
   linux-6.12.109 inside, and asks for `libkrunfw_5.6.2_darwin_arm64.tar.gz`.
3. A release builds the Linux bundle and the macOS tarball from those pins and
   attaches the three corresponding-source files named with 5.6.2.
4. The source archive is the tap release's file, not built here. The tap
   release `libkrunfw-5.6.2` must carry the kernel, source and prebuilt assets
   at these digests before a tag is cut; until then this change stays a draft.
