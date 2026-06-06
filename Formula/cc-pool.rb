# Homebrew formula for cc-pool. Builds from source at a tagged release.
#
#   brew tap yasyf/cc-pool https://github.com/yasyf/cc-pool
#   brew install yasyf/cc-pool/cc-pool
#
# The build auto-detects fuse-t: if its headers + dylib are present at build
# time, cc-pool is compiled with the live-mirror overlay (-tags fuse, cgo);
# otherwise it ships pure-Go with the symlink overlay (which needs nothing).
# To enable the mirror: `brew install macos-fuse-t/cask/fuse-t` then
# `brew reinstall cc-pool`. A fuse build still runs if fuse-t is later removed —
# it simply falls back to symlinks.
class CcPool < Formula
  desc "Predictive multi-account load-balancing for Claude Code"
  homepage "https://github.com/yasyf/cc-pool"
  url "https://github.com/yasyf/cc-pool/archive/refs/tags/v0.2.0.tar.gz"
  sha256 "a19b49c0baff315bd488a0568f54579a1da6995c8f90531f4616b01176ad56cf"
  license "PolyForm-Noncommercial-1.0.0"
  head "https://github.com/yasyf/cc-pool.git", branch: "main"

  depends_on "go" => :build
  depends_on :macos

  # fuse-t (the kext-less FUSE for the live-mirror overlay). Not a hard dep — a
  # cask can't be a formula build dep — so we detect it at build time instead.
  FUSE_T_HEADER = "/usr/local/include/fuse/fuse.h"
  FUSE_T_DYLIB = "/usr/local/lib/libfuse-t.dylib"

  def install
    ldflags = %W[
      -s -w
      -X github.com/yasyf/cc-pool/internal/version.Version=#{version}
    ]
    args = std_go_args(ldflags: ldflags.join(" "), output: bin/"cc-pool")
    if File.exist?(FUSE_T_HEADER) && File.exist?(FUSE_T_DYLIB)
      ohai "fuse-t detected — building the live-mirror overlay (-tags fuse)"
      ENV["CGO_ENABLED"] = "1"
      args += ["-tags", "fuse"]
    else
      ENV["CGO_ENABLED"] = "0"
    end
    system "go", "build", *args, "./cmd/cc-pool"
    bin.install_symlink "cc-pool" => "clp"
  end

  # Run the daemon as a USER LaunchAgent (no sudo): it needs the user's login
  # Keychain, which a root daemon cannot read. `brew services start cc-pool`
  # installs ~/Library/LaunchAgents/homebrew.mxcl.cc-pool.plist.
  service do
    run [opt_bin/"cc-pool", "daemon"]
    keep_alive true
    run_at_load true
    environment_variables PATH: std_service_path_env,
                          CGOFUSE_LIBFUSE_PATH: "/usr/local/lib/libfuse-t.dylib"
    log_path "#{Dir.home}/.cc-pool/daemon.log"
    error_log_path "#{Dir.home}/.cc-pool/daemon.log"
  end

  def caveats
    <<~EOS
      Get started:
        clp init        # register ~/.claude as acct-00 (plain `claude` keeps working)
        clp add         # pool another Claude subscription
        CLAUDE_CONFIG_DIR=$(clp select) claude

      Enable the background daemon (keeps idle tokens fresh, scores live):
        brew services start cc-pool

      Optional live-mirror overlay (instead of per-entry symlinks): install
      fuse-t, then rebuild so cc-pool picks it up automatically:
        brew install macos-fuse-t/cask/fuse-t
        brew reinstall cc-pool
      The default symlink overlay needs nothing extra.
    EOS
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/cc-pool --version")
    assert_match "emptiest account", shell_output("#{bin}/clp --help")
  end
end
