# Linux GUI self-test image: Go, Xvfb and webkit2gtk-4.1 (docs/dev/gui.md).
# The official Go image is multi-arch (amd64 and arm64).
FROM golang:1.26.8-trixie
RUN apt-get update \
 && apt-get install -y --no-install-recommends xvfb xauth libgtk-3-dev libwebkit2gtk-4.1-dev dbus-x11 \
 && apt-get clean
