class Looper < Formula
  desc "Codex RALF loop runner and skills pack"
  homepage "https://github.com/nibzard/looper"
  url "https://github.com/nibzard/looper/archive/refs/tags/v0.3.3.tar.gz"
  sha256 "47dfb9dbcb842eefb9520c9f548e764ec77f88081736b1bab1c462730e960d17"

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
