class Looper < Formula
  desc "Codex RALF loop runner and skills pack"
  homepage "https://github.com/nibzard/looper"
  url "https://github.com/nibzard/looper/archive/refs/tags/v0.2.2.tar.gz"
  sha256 "13ec62ef7eb9a5bdb49b7b7e1597fb6a19eb7d445ddd6e8e34ae9a5cda2f6706"

  head "https://github.com/nibzard/looper.git", branch: "main"

  def install
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
