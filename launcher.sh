#!/bin/sh
# Lancement depuis NickelMenu
APP_DIR=/mnt/onboard/.adds/kobo-skyboard
LOG_FILE="$APP_DIR/skyboard.log"
cd "$APP_DIR" || exit 1
printf '\n--- SkyBoard launch %s ---\n' "$(date)" >> "$LOG_FILE"
exec "$APP_DIR/skyboard-arm" >> "$LOG_FILE" 2>&1
