#!/bin/sh
# Запускает tgbot и поднимает его заново при падении. Лог — /data/local/tmp/ap2/tgbot.log
#   tgbot.sh start | stop | run
AP=/data/local/opt/ap2
LOG=/data/local/tmp/ap2/tgbot.log
mkdir -p /data/local/tmp/ap2
case "$1" in
  start)
    "$0" stop
    setsid "$0" run </dev/null >/dev/null 2>&1 &
    ;;
  stop)
    for p in $(ps -o pid,args | grep -E "tgbot.sh run|$AP/bin/tgbot" | grep -v grep | awk '{print $1}'); do
      [ "$p" != "$$" ] && kill "$p" 2>/dev/null
    done
    ;;
  run)
    # сеть и время на раннем старте могут быть не готовы
    sleep 10
    while :; do
      [ -f "$LOG" ] && [ "$(wc -c < "$LOG")" -gt 262144 ] && : > "$LOG"
      "$AP/bin/tgbot" >> "$LOG" 2>&1
      echo "$(date): tgbot завершился, код $?" >> "$LOG"
      sleep 10
    done
    ;;
esac
exit 0
