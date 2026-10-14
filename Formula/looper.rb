class Looper < Formula
  desc "Continuous local development with coding agents"
  homepage "https://github.com/nibzard/looper"
  url "https://github.com/nibzard/looper/archive/refs/tags/v0.8.3.tar.gz"
  sha256 "c029e2d230bb789a1e4623fec4d94c2321b0d1d0a8ed600c90830cf847c2d92b"
  head "https://github.com/nibzard/looper.git", branch: "main"

  depends_on "go" => :build

  def install
    system "go", "build", "-trimpath", "-ldflags",
           "-X github.com/nibzard/looper/internal/looper.Version=#{version}",
           "-o", "looper", "./cmd/looper"
    bin.install "looper"
    bin.install "install.sh" => "looper-install"
    bin.install "uninstall.sh" => "looper-uninstall"
    pkgshare.install "skills"
    pkgshare.install "README.md"
  end

  def caveats
    <<~EOS
      Go is the only implementation.

      To install skills into ~/.codex/skills:
        looper-install --skip-bin

      To install a user-local copy of the commands:
        looper-install
    EOS
  end

  test do
    system "#{bin}/looper", "--help"
    assert_match version.to_s, shell_output("#{bin}/looper --version")
  end
end
