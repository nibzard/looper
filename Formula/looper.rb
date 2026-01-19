class Looper < Formula
  desc "Codex RALF loop runner and skills pack"
  homepage "https://github.com/niko/looper"
  # Update url + sha256 after publishing a release tarball.
  url "https://github.com/niko/looper/archive/refs/tags/v0.1.0.tar.gz"
  sha256 "REPLACE_WITH_SHA256"

  head "https://github.com/niko/looper.git", branch: "main"

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
