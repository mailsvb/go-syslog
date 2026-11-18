#!/bin/sh
# $1 = 0 on erase, >= 1 on upgrade (rpm convention)
if [ "${1:-0}" -eq 0 ] && [ -d /run/systemd/system ]; then
    systemctl disable --now go-syslog.service >/dev/null 2>&1 || :
fi
exit 0
