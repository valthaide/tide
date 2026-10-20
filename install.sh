#!/bin/sh
# Install cps CLI from the latest private release

set -e

REPO="valthaide/conops-private"

get_latest_release() {
  curl -Ls -o /dev/null -w %{url_effective} "https://github.com/$REPO/releases/latest" | rev | cut -d'/' -f1 | rev
}

echo "Finding latest release..."
VERSION=$(get_latest_release)

if [ -z "$VERSION" ]; then
    echo "Error: Unable to find latest release version."
    exit 1
fi

OS=$(uname -s)
ARCH=$(uname -m)

case "$OS" in
    Linux) OS="Linux" ;;
    Darwin) OS="Darwin" ;;
    *) echo "Unsupported OS: $OS"; exit 1 ;;
esac

case "$ARCH" in
    x86_64) ARCH="x86_64" ;;
    arm64|aarch64) ARCH="arm64" ;;
    *) echo "Unsupported Architecture: $ARCH"; exit 1 ;;
esac

FILENAME="cps_${OS}_${ARCH}.tar.gz"
URL="https://github.com/$REPO/releases/download/$VERSION/$FILENAME"

echo "Downloading $VERSION for $OS/$ARCH..."
curl -sfL "$URL" -o "/tmp/$FILENAME"

echo "Extracting..."
tar -xzf "/tmp/$FILENAME" -C /tmp cps

echo "Installing to /usr/local/bin..."
if [ -w /usr/local/bin ]; then
    mv "/tmp/cps" "/usr/local/bin/"
else
    sudo mv "/tmp/cps" "/usr/local/bin/"
fi
chmod +x /usr/local/bin/cps

rm "/tmp/$FILENAME"

echo "Successfully installed cps $VERSION!"
