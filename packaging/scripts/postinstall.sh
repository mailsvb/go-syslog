#!/bin/sh
# $1 = 1 on first install, >= 2 on upgrade (rpm convention)
sysctl -q -p /usr/lib/sysctl.d/90-go-syslog.conf >/dev/null 2>&1 || :
if [ -d /run/systemd/system ]; then
    systemctl daemon-reload >/dev/null 2>&1 || :
    if [ "${1:-1}" -ge 2 ]; then
        systemctl try-restart go-syslog.service >/dev/null 2>&1 || :
    else
        echo "go-syslog installed. Start it with: systemctl enable --now go-syslog"
    fi
fi
exit 0
