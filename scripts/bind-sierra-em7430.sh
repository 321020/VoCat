#!/bin/sh

# Expose the EM7430's diagnostic/NMEA/AT ports while keeping its MBIM
# control/data pair on cdc_mbim. Linux's dynamic option ID matches every USB
# interface after a reset, including interfaces 12/13, so loading cdc_mbim
# first is not enough on its own.

SYS_ROOT="${VOCAT_SYS_ROOT:-/sys}"
USB_ROOT="$SYS_ROOT/bus/usb/devices"
OPTION_NEW_ID="$SYS_ROOT/bus/usb-serial/drivers/option1/new_id"
OPTION_UNBIND="$SYS_ROOT/bus/usb/drivers/option/unbind"
MBIM_BIND="$SYS_ROOT/bus/usb/drivers/cdc_mbim/bind"

command -v modprobe >/dev/null 2>&1 && modprobe cdc_mbim >/dev/null 2>&1 || true
command -v modprobe >/dev/null 2>&1 && modprobe option >/dev/null 2>&1 || true

repair_mbim_bindings() {
    [ -d "$USB_ROOT" ] || return 0
    for device in "$USB_ROOT"/*; do
        [ -f "$device/idVendor" ] || continue
        [ "$(tr '[:upper:]' '[:lower:]' < "$device/idVendor" 2>/dev/null)" = "1199" ] || continue
        [ "$(tr '[:upper:]' '[:lower:]' < "$device/idProduct" 2>/dev/null)" = "9077" ] || continue

        control=""
        control_needs_bind=0
        for interface in "${device}":*; do
            [ -d "$interface" ] || continue
            class=$(tr '[:upper:]' '[:lower:]' < "$interface/bInterfaceClass" 2>/dev/null || true)
            subclass=$(tr '[:upper:]' '[:lower:]' < "$interface/bInterfaceSubClass" 2>/dev/null || true)
            protocol=$(tr '[:upper:]' '[:lower:]' < "$interface/bInterfaceProtocol" 2>/dev/null || true)
            is_control=0
            is_data=0
            [ "$class/$subclass" = "02/0e" ] && is_control=1
            [ "$class/$protocol" = "0a/02" ] && is_data=1
            [ "$is_control" -eq 1 ] || [ "$is_data" -eq 1 ] || continue

            name=$(basename "$interface")
            driver=""
            if [ -L "$interface/driver" ]; then
                driver=$(basename "$(readlink "$interface/driver")")
            fi
            if [ "$is_control" -eq 1 ]; then
                control="$name"
                [ "$driver" = "cdc_mbim" ] || control_needs_bind=1
            fi
            if [ "$driver" = "option" ] && [ -w "$OPTION_UNBIND" ]; then
                printf '%s' "$name" > "$OPTION_UNBIND" 2>/dev/null || true
            fi
        done
        if [ "$control_needs_bind" -eq 1 ] && [ -n "$control" ] && [ -w "$MBIM_BIND" ]; then
            printf '%s' "$control" > "$MBIM_BIND" 2>/dev/null || true
        fi
    done
}

# Repair a previous dynamic binding first, register the serial ID, then repair
# once more for the synchronous probes triggered by new_id.
repair_mbim_bindings
if [ -w "$OPTION_NEW_ID" ]; then
    printf '%s\n' '1199 9077' > "$OPTION_NEW_ID" 2>/dev/null || true
fi
repair_mbim_bindings
