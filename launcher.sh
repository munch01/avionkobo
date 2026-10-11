#!/bin/sh
# Lancement depuis NickelMenu
APP_DIR=/mnt/onboard/.adds/kobo-skyboard
LOG_FILE="$APP_DIR/skyboard.log"
cd "$APP_DIR" || exit 1
printf '\n--- SkyBoard launch %s ---\n' "$(date)" >> "$LOG_FILE"
if [ -x /sbin/ifconfig ]; then
    if ! /sbin/ifconfig lo 127.0.0.1 up >> "$LOG_FILE" 2>&1; then
        echo "Could not enable the loopback interface." >> "$LOG_FILE"
    fi
elif [ -x /bin/ifconfig ]; then
    if ! /bin/ifconfig lo 127.0.0.1 up >> "$LOG_FILE" 2>&1; then
        echo "Could not enable the loopback interface." >> "$LOG_FILE"
    fi
else
    echo "ifconfig not found; the local dashboard may not be reachable." >> "$LOG_FILE"
fi
exec "$APP_DIR/skyboard-arm" >> "$LOG_FILE" 2>&1
