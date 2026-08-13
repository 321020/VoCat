#!/bin/sh
set -eu

# Some kernels omit the EM7430's 1199:9077 serial ID. The helper exposes its
# AT port and repairs MBIM interfaces that option may claim after a USB reset.
if [ "$(id -u)" = "0" ] && [ -e /sys/bus/usb/devices ]; then
  /usr/local/bin/vocat-bind-em7430 || true
fi

# pcscd daemonizes after startup. Keep failure non-fatal so modem-only
# deployments remain usable and the UI can report a reader diagnostic.
if [ "$(id -u)" = "0" ] && command -v pcscd >/dev/null 2>&1; then
  mkdir -p /run/pcscd
  pcscd || echo "warning: pcscd failed to start; USB SIM readers may be unavailable" >&2
fi

exec /opt/vocat/bin/vocat "$@"
