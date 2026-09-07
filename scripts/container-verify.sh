#!/bin/sh
# 在 Linux 容器里跑并发检查 + 基准 + 压测。
#
# 为什么必须走容器：Windows 上的 gcc 装不上 race 运行库，交叉编译 -race 又要求 cgo，
# 所以并发检查只能在 Linux 里做。
#
# 为什么源码走 stdin 而不是挂载目录：项目路径含中文，Docker Desktop 的 bind mount
# 与 docker cp 在这条路径上都出过问题；tar 从标准输入喂进去就完全绕开了 Windows 路径。
#
# 为什么要 --entrypoint bash：镜像默认 entrypoint 把传进去的参数当文件名打开
# （于是 -c '脚本' 变成"去读一个叫 -c 的文件"），显式换成 bash 才拿得到 shell 语义。
#
# 用法：
#   sh scripts/container-verify.sh          # 全套
#   sh scripts/container-verify.sh race     # 只跑 -race
#   sh scripts/container-verify.sh load     # 只跑压测
# 同一份压测报告换到 Termux 上直接跑（不经容器）：
#   go run ./cmd/qqsim load -total 50000
#   go run ./cmd/qqsim sweep -rates 200,500,1000,2000,4000 -seconds 3
set -u

IMAGE="${IMAGE:-xingta-race:go1.26-1.27}"
GOBIN_DIR="${GOBIN_DIR:-/opt/g127/bin}"
WHAT="${1:-all}"

cd "$(dirname "$0")/.." || exit 1
go mod vendor || exit 1

tar -cf - --exclude=./.git --exclude=./.probe --exclude=./out . |
	MSYS_NO_PATHCONV=1 docker run --rm -i \
		-e WHAT="$WHAT" -e GOBIN_DIR="$GOBIN_DIR" \
		--entrypoint bash "$IMAGE" -lc '
set -u
export PATH="$GOBIN_DIR:$PATH"
export CGO_ENABLED=1 GOFLAGS=-mod=vendor GOCACHE=/tmp/gocache GOPATH=/tmp/gopath
mkdir -p /src && tar -xf - -C /src && cd /src || exit 1
echo "### $(go version)  阶段=$WHAT"

run_race() {
	echo "### vet"
	go vet ./internal/... ./cmd/...
	echo "### test -race"
	go test -race -count=1 -timeout 10m ./internal/...
	echo "### internal/qq 覆盖率（黑盒同目录，默认 -cover 就是真数字）"
	go test -cover -count=1 ./internal/qq
	echo "### race 退出码 $?"
}

run_bench() {
	echo "### 基准（成本归因）"
	go test -run XXXXXX -bench . -benchtime 300000x ./internal/qq/
}

run_load() {
	echo "### 压测：闭环上限"
	go run ./cmd/qqsim load -total 50000
	echo "### 压测：开环 5000/s"
	go run ./cmd/qqsim load -total 30000 -rate 5000
	echo "### 压测：分级找崩点（空载业务处理器）"
	go run ./cmd/qqsim sweep -rates 1000,2000,4000,8000,16000,32000 -seconds 3
	echo "### 压测：分级找崩点（每条消息烧 1ms 业务 CPU）"
	go run ./cmd/qqsim sweep -rates 100,200,400,800,1600 -seconds 3 -cpu 1ms
}

case "$WHAT" in
	all)   run_race; run_bench; run_load ;;
	race)  run_race ;;
	bench) run_bench ;;
	load)  run_load ;;
	*)     echo "未知阶段 $WHAT，可用 all/race/bench/load"; exit 2 ;;
esac
'
