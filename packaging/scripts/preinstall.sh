#!/bin/sh
# Create the service account before the files are installed.
getent group gosyslog >/dev/null || groupadd -r gosyslog
getent passwd gosyslog >/dev/null || \
    useradd -r -g gosyslog -d /var/log/go-syslog -s /sbin/nologin -c "go-syslog daemon" gosyslog
exit 0
