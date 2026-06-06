# Homebrew formula for cc-pool. Builds from source at a tagged release.
#
#   brew tap yasyf/cc-pool https://github.com/yasyf/cc-pool
#   brew install yasyf/cc-pool/cc-pool
#
# The default build is pure Go (no cgo). The optional fuse-t live-mirror overlay
# requires a separate `-tags fuse` build and `brew install` of fuse-t; the
# default symlink overlay works without it.
class CcPool < Formula
  desc "Predictive multi-account load-balancing for Claude Code"
  homepage "https://github.com/yasyf/cc-pool"
  url "https://github.com/yasyf/cc-pool/archive/refs/tags/v0.1.0.tar.gz"
  sha256 "0000000000000000000000000000000000000000000000000000000000000000"
  license "MIT"
  head "https://github.com/yasyf/cc-pool.git", branch: "main"

  depends_on "go" => :build
  depends_on :macos

  def install
    ldflags = %W[
      -s -w
      -X github.com/yasyf/cc-pool/internal/version.Version=#{version}
    ]
    system "go", "build", *std_go_args(ldflags: ldflags.join(" "), output: bin/"cc-pool"),
           "./cmd/cc-pool"
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

      Optional live-mirror overlay (instead of per-entry symlinks) needs fuse-t:
        brew install macos-fuse-t/cask/fuse-t
      and a fuse-enabled build. The default symlink overlay needs nothing extra.
    EOS
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/cc-pool --version")
    assert_match "emptiest account", shell_output("#{bin}/clp --help")
  end
end
