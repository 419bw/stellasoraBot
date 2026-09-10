#!/data/data/com.termux/files/usr/bin/bash
DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$DIR" || exit 1

if screen -list 2>/dev/null | grep -q "xingtabot"; then
    echo "[!] xingtabot 已经在运行中 (screen: xingtabot)"
    exit 0
fi

if [ -z "$SSL_CERT_FILE" ] && [ -f "/data/data/com.termux/files/usr/etc/tls/cert.pem" ]; then
    export SSL_CERT_FILE=/data/data/com.termux/files/usr/etc/tls/cert.pem
fi

chmod +x ./xingtabot
screen -dmS xingtabot bash -c 'exec ./xingtabot -creds creds.json -db data/xingta.db >> run.log 2>&1'
sleep 1

if screen -list 2>/dev/null | grep -q "xingtabot"; then
    echo "[✓] xingtabot 启动成功 (screen: xingtabot)"
    echo "    查看状态: ./status.sh"
    echo "    查看日志: ./logs.sh"
    echo "    停止服务: ./stop.sh"
else
    echo "[x] xingtabot 启动失败，请检查 run.log"
    tail -n 20 run.log 2>/dev/null
fi
