#!/bin/sh
# Create the service account before the files are installed. The service
# reads the logs through the supplementary group gosyslog (see the unit).
getent group gosyslog >/dev/null || groupadd -r gosyslog
getent group gosyslog-view >/dev/null || groupadd -r gosyslog-view
getent passwd gosyslog-view >/dev/null || \
    useradd -r -g gosyslog-view -d / -s /sbin/nologin -c "go-syslog-view MCP server" gosyslog-view
exit 0
