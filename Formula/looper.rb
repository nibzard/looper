class Looper < Formula
  desc "Continuous local development with coding agents"
  homepage "https://github.com/nibzard/looper"
  url "https://github.com/nibzard/looper/archive/refs/tags/v0.5.0.tar.gz"
  sha256 "0a0a39e4ed8e4dfcf51b26949a858468ba6b5059cdf85b0e05e7405ec68f7fb6"
  head "https://github.com/nibzard/looper.git", branch: "main"

  depends_on "go" => :build

  def install
    system "go", "build", "-trimpath", "-ldflags",
           "-X github.com/nibzard/looper/internal/looper.Version=#{version}",
           "-o", "looper", "./cmd/looper"
    bin.install "looper"
    bin.install "bin/looper.sh"
    bin.install "install.sh" => "looper-install"
    bin.install "uninstall.sh" => "looper-uninstall"
    pkgshare.install "skills"
    pkgshare.install "README.md"
  end

  def caveats
    <<~EOS
      Go is the default implementation. looper.sh is a compatibility launcher.

      To install skills into ~/.codex/skills:
        looper-install --skip-bin

      To install a user-local copy of the commands:
        looper-install
    EOS
  end

  test do
    system "#{bin}/looper", "--help"
    system "#{bin}/looper.sh", "--help"
    assert_match version.to_s, shell_output("#{bin}/looper --version")
  end
end
