# Homebrew formula for claude-pool. Builds from source at a tagged release.
#
#   brew tap yasyf/claude-pool https://github.com/yasyf/claude-pool
#   brew install yasyf/claude-pool/claude-pool
#
# The default build is pure Go (no cgo). The optional fuse-t live-mirror overlay
# requires a separate `-tags fuse` build and `brew install` of fuse-t; the
# default symlink overlay works without it.
class ClaudePool < Formula
  desc "Predictive multi-account load-balancing for Claude Code"
  homepage "https://github.com/yasyf/claude-pool"
  url "https://github.com/yasyf/claude-pool/archive/refs/tags/v0.1.0.tar.gz"
  sha256 "0000000000000000000000000000000000000000000000000000000000000000"
  license "MIT"
  head "https://github.com/yasyf/claude-pool.git", branch: "main"

  depends_on "go" => :build
  depends_on :macos

  def install
    ldflags = %W[
      -s -w
      -X github.com/yasyf/claude-pool/internal/version.Version=#{version}
    ]
    system "go", "build", *std_go_args(ldflags: ldflags.join(" "), output: bin/"claude-pool"),
           "./cmd/claude-pool"
    bin.install_symlink "claude-pool" => "clp"
  end

  # Run the daemon as a USER LaunchAgent (no sudo): it needs the user's login
  # Keychain, which a root daemon cannot read. `brew services start claude-pool`
  # installs ~/Library/LaunchAgents/homebrew.mxcl.claude-pool.plist.
  service do
    run [opt_bin/"claude-pool", "daemon"]
    keep_alive true
    run_at_load true
    environment_variables PATH: std_service_path_env,
                          CGOFUSE_LIBFUSE_PATH: "/usr/local/lib/libfuse-t.dylib"
    log_path "#{Dir.home}/.claude-pool/daemon.log"
    error_log_path "#{Dir.home}/.claude-pool/daemon.log"
  end

  def caveats
    <<~EOS
      Get started:
        clp init        # register ~/.claude as acct-00 (plain `claude` keeps working)
        clp add         # pool another Claude subscription
        CLAUDE_CONFIG_DIR=$(clp select) claude

      Enable the background daemon (keeps idle tokens fresh, scores live):
        brew services start claude-pool

      Optional live-mirror overlay (instead of per-entry symlinks) needs fuse-t:
        brew install macos-fuse-t/cask/fuse-t
      and a fuse-enabled build. The default symlink overlay needs nothing extra.
    EOS
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/claude-pool --version")
    assert_match "emptiest account", shell_output("#{bin}/clp --help")
  end
end
