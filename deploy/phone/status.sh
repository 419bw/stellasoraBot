#!/data/data/com.termux/files/usr/bin/bash
DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$DIR" || exit 1

echo "=== 星塔机器人运行状态 ==="
if screen -list 2>/dev/null | grep -q "xingtabot"; then
    echo "状态: 正在运行 (screen: xingtabot)"
    PID=$(pgrep -f "./xingtabot -creds" 2>/dev/null)
    if [ -n "$PID" ]; then
        echo "PID: $PID"
        ps -o pid,user,%cpu,%mem,vsz,rss,comm -p "$PID" 2>/dev/null
    fi
else
    echo "状态: 已停止"
fi
echo ""
if [ -f "run.log" ]; then
    echo "--- 最新日志 (最后 10 行) ---"
    tail -n 10 run.log
fi
