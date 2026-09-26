#!/data/data/com.termux/files/usr/bin/bash
if ! screen -list 2>/dev/null | grep -q "xingtabot"; then
    echo "[!] 未检测到运行中的 xingtabot"
    exit 0
fi

echo "正在优雅停止 xingtabot..."
# 按进程名精确匹配，不按命令行：以前这里是 pgrep -f "./xingtabot -creds"，参数一挪个
# 位置就永不匹配 —— 于是下面那段 SIGTERM 整块被跳过，只剩 screen 硬拆，bbolt 的优雅
# 收尾不声不响就没了。所以匹配不到必须说出来，不能假装停止成功。
PID=$(pgrep -x xingtabot 2>/dev/null || pgrep -x ./xingtabot 2>/dev/null)
if [ -z "$PID" ]; then
    echo "[x] 没 pgrep 到 xingtabot 进程：这次不会优雅退出（下面只关 screen 会话）"
fi
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
