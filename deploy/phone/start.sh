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

chmod +x ./xingtabot ./chrome-headless
# 参数不再走命令行：机器级的值在 config.yml，各功能的值在它旁边的 config/<功能>.yml。
# 上面已经 cd 到本目录，所以凭据、data/xingta.db、./chrome-headless 都按这里相对定位。
# 少传一份配置文件现在是"启动即失败并打印缺哪个键"，而不是静默用旧默认值跑起来。
screen -dmS xingtabot bash -c 'exec ./xingtabot >> run.log 2>&1'
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
