class Looper < Formula
  desc "Codex RALF loop runner and skills pack"
  homepage "https://github.com/nibzard/looper"
  url "https://github.com/nibzard/looper/archive/refs/tags/v0.1.0.tar.gz"
  sha256 "7d05bb624c7c6156f432f303878759098174db277fbbb16f84a707020e5fb58f"

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
