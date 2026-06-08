# Homebrew formula for cc-pool. Installs the prebuilt universal binary from
# GitHub Releases — no Go toolchain needed. `brew install --HEAD` builds from
# source instead.
#
#   brew tap yasyf/cc-pool https://github.com/yasyf/cc-pool
#   brew install yasyf/cc-pool/cc-pool
#
# Two prebuilt variants ship per release: pure-Go (symlink overlay) and
# -tags fuse (live-mirror overlay; dlopens libfuse-t.dylib at runtime and
# falls back to symlinks without it). Install picks the fuse build iff fuse-t
# is present. To enable the mirror: `brew install macos-fuse-t/cask/fuse-t`
# then `brew reinstall cc-pool`.
#
# release.yml's bump-formula job rewrites the version embedded in both
# download urls and both sha256 lines on every tagged release — the trailing
# `# pure` / `# fuse` markers anchor its seds; keep them.
class CcPool < Formula
  desc "Predictive multi-account load-balancing for Claude Code"
  homepage "https://github.com/yasyf/cc-pool"
  url "https://github.com/yasyf/cc-pool/releases/download/v0.12.1/cc-pool-v0.12.1-darwin-universal.tar.gz"
  sha256 "a485fae215001690b4480c68e2d19ffc90ed5f6a6e246771d08f5a0f7224983c" # pure
  license "PolyForm-Noncommercial-1.0.0"

  livecheck do
    url :stable
    strategy :github_latest
  end

  head do
    url "https://github.com/yasyf/cc-pool.git", branch: "main"
    depends_on "go" => :build
  end

  depends_on :macos

  # fuse-t (the kext-less FUSE for the live-mirror overlay). Not a hard dep — a
  # cask can't be a formula dep — so we detect it at install time instead.
  FUSE_T_HEADER = "/usr/local/include/fuse/fuse.h".freeze
  FUSE_T_DYLIB = "/usr/local/lib/libfuse-t.dylib".freeze

  # The fuse-variant binary (cgo, -tags fuse). A resource keeps the second
  # artifact checksummed; it is only downloaded when staged below.
  resource "fuse" do
    url "https://github.com/yasyf/cc-pool/releases/download/v0.12.1/cc-pool-v0.12.1-darwin-universal-fuse.tar.gz"
    sha256 "d5f7954cb91b4861ee435e1d30b74d2142ed8219fc672ca654a54d005dba290e" # fuse
  end

  def install
    if build.head?
      install_from_source
    elsif File.exist?(FUSE_T_DYLIB)
      ohai "fuse-t detected — installing the live-mirror (fuse) build"
      resource("fuse").stage { bin.install "cc-pool" }
    else
      bin.install "cc-pool"
    end
    bin.install_symlink "cc-pool" => "ccp"
  end

  # HEAD builds compile from source with the build-time fuse autodetect: cgo +
  # -tags fuse when fuse-t headers are present, pure Go otherwise.
  def install_from_source
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
  end

  # Run the daemon as a USER LaunchAgent (no sudo): it needs the user's login
  # Keychain, which a root daemon cannot read. `brew services start cc-pool`
  # installs ~/Library/LaunchAgents/homebrew.mxcl.cc-pool.plist.
  service do
    run [opt_bin/"cc-pool", "daemon"]
    keep_alive true
    run_at_load true
    environment_variables PATH:                 std_service_path_env,
                          CGOFUSE_LIBFUSE_PATH: "/usr/local/lib/libfuse-t.dylib"
    log_path "#{Dir.home}/.cc-pool/daemon.log"
    error_log_path "#{Dir.home}/.cc-pool/daemon.log"
  end

  def caveats
    <<~EOS
      Get started:
        ccp add         # pool a Claude subscription (auto-inits; plain `claude` keeps working)
        CLAUDE_CONFIG_DIR=$(ccp select) claude
    EOS
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/cc-pool --version")
    assert_match "emptiest account", shell_output("#{bin}/ccp --help")
  end
end
