#!/bin/sh
set -eu

# Some kernels omit the EM7430's 1199:9077 serial ID. Load MBIM before option
# so interfaces 12/13 stay with cdc_mbim, then expose interface 3 as the AT
# port. This is intentionally restricted to the exact device ID.
if [ "$(id -u)" = "0" ] && [ -e /sys/bus/usb/devices ]; then
  modprobe cdc_mbim >/dev/null 2>&1 || true
  modprobe option >/dev/null 2>&1 || true
  if [ -w /sys/bus/usb-serial/drivers/option1/new_id ]; then
    printf '%s\n' '1199 9077' > /sys/bus/usb-serial/drivers/option1/new_id 2>/dev/null || true
  fi
fi

# pcscd daemonizes after startup. Keep failure non-fatal so modem-only
# deployments remain usable and the UI can report a reader diagnostic.
if [ "$(id -u)" = "0" ] && command -v pcscd >/dev/null 2>&1; then
  mkdir -p /run/pcscd
  pcscd || echo "warning: pcscd failed to start; USB SIM readers may be unavailable" >&2
fi

exec /opt/vocat/bin/vocat "$@"
