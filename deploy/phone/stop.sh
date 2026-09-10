#!/data/data/com.termux/files/usr/bin/bash
if ! screen -list 2>/dev/null | grep -q "xingtabot"; then
    echo "[!] 未检测到运行中的 xingtabot"
    exit 0
fi

echo "正在优雅停止 xingtabot..."
# 查找进程并发送 SIGTERM 触发优雅退出（bbolt 干净刷盘）
PID=$(pgrep -f "./xingtabot -creds" 2>/dev/null)
if [ -n "$PID" ]; then
    kill -TERM "$PID" 2>/dev/null
    for i in 1 2 3 4 5 6 7 8 9 10; do
        if ! kill -0 "$PID" 2>/dev/null; then
            break
        fi
        sleep 0.5
    done
fi
screen -S xingtabot -X quit 2>/dev/null
echo "[✓] xingtabot 已停止"
