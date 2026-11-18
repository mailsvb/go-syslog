#!/bin/sh
# $1 = 1 on first install, >= 2 on upgrade (rpm convention)
token=/etc/go-syslog-view/token
if [ ! -e "$token" ]; then
    # 32 random bytes as hex; readable by root and the service only.
    umask 027
    head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' > "$token" && echo >> "$token"
    chown root:gosyslog-view "$token" 2>/dev/null || :
    chmod 0640 "$token"
fi
if [ -d /run/systemd/system ]; then
    systemctl daemon-reload >/dev/null 2>&1 || :
    if [ "${1:-1}" -ge 2 ]; then
        systemctl try-restart go-syslog-view.service >/dev/null 2>&1 || :
    else
        echo "go-syslog-view installed. Start it with: systemctl enable --now go-syslog-view"
        echo "MCP endpoint: http://<host>:8514/mcp, bearer token in $token"
    fi
fi
exit 0
