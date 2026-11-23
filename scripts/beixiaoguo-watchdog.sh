#!/bin/sh
# Health watchdog for the procd-managed Beixiaoguo service.
HEALTH_URL="http://127.0.0.1:8780/api/v1/health"
CHECK_INTERVAL=15
REQUEST_TIMEOUT=4
FAILURE_LIMIT=3
failures=0

while :; do
  if wget -T "$REQUEST_TIMEOUT" -qO /dev/null "$HEALTH_URL"; then
    if [ "$failures" -gt 0 ]; then
      logger -t beixiaoguo-watchdog "health check recovered"
    fi
    failures=0
  else
    failures=$((failures + 1))
    logger -t beixiaoguo-watchdog "local health check failed ($failures/$FAILURE_LIMIT)"
    if [ "$failures" -ge "$FAILURE_LIMIT" ]; then
      logger -t beixiaoguo-watchdog "health endpoint unreachable; asking procd to restart app instance"
      ubus call service signal '{"name":"beixiaoguo","instance":"app","signal":"15"}' >/dev/null 2>&1 || true
      failures=0
      sleep 10
    fi
  fi
  sleep "$CHECK_INTERVAL"
done
