class Looper < Formula
  desc "Continuous local development with coding agents"
  homepage "https://github.com/nibzard/looper"
  url "https://github.com/nibzard/looper/archive/refs/tags/v0.12.1.tar.gz"
  sha256 "0ea100836b2eb827b3811c57616269b51ffbecd633e7df76a2ebb19bb74330d4"
  head "https://github.com/nibzard/looper.git", branch: "main"
  license "MIT"

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
