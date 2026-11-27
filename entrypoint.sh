#!/bin/sh
# Preserve the existing entrypoint path while the Go supervisor becomes PID 1.
exec /usr/local/bin/ts-funnel "$@"
