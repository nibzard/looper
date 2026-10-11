class Looper < Formula
  desc "Continuous local development with coding agents"
  homepage "https://github.com/nibzard/looper"
  url "https://github.com/nibzard/looper/archive/refs/tags/v0.4.0.tar.gz"
  sha256 "632361e2f63e5bf3d93b0b6b8373642eef44855a0691764c578a7ab27c1b6d9b"

  head do
    url "https://github.com/nibzard/looper.git", branch: "main"
    depends_on "go" => :build
  end

  def install
    if build.head?
      system "go", "build", "-trimpath", "-o", "looper", "./cmd/looper"
      bin.install "looper"
    else
      # The published v0.4.0 archive still contains the Python runtime.
      bin.install "lib/looper_parallel.py"
    end
    bin.install "bin/looper.sh"
    bin.install "install.sh" => "looper-install"
    bin.install "uninstall.sh" => "looper-uninstall"
    pkgshare.install "skills"
    pkgshare.install "README.md"
  end

  def caveats
    <<~EOS
      To install skills into ~/.codex/skills:
        looper-install --skip-bin

      looper.sh is already in Homebrew's bin, but you can also install
      a user-local copy with:
        looper-install
    EOS
  end

  test do
    system "#{bin}/looper.sh", "--help"
  end
end
