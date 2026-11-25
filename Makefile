# AI 玩家集群 / 对局压测 —— 常用命令
#
# 用 `make help` 看全部目标。Windows 上没有 make 的话，
# 每条命令都能在 README / docs 里找到等价写法。

SHELL := /bin/sh
BIN   := bin
GO    ?= go
PY    ?= python

ifeq ($(OS),Windows_NT)
EXE := .exe
endif

.PHONY: help build vet test test-race lint smoke baseline fixed client demo clean fmt cover

help:
	@echo "构建"
	@echo "  make build        编译 sutd / runner / controller / client 到 $(BIN)/"
	@echo "  make fmt          gofmt -w"
	@echo "  make clean        清理 $(BIN)/ 与 results/"
	@echo "检查"
	@echo "  make vet          go vet ./..."
	@echo "  make test         全部测试"
	@echo "  make test-race    核心包 race 测试（需 CGO）"
	@echo "  make cover        覆盖率报告"
	@echo "实验"
	@echo "  make smoke        冒烟：30 bot，一局 10s"
	@echo "  make baseline     基线：阶梯到 300 bot（带刻意瓶颈）"
	@echo "  make fixed        同一份基线计划跑修复版 SUT，用于前后对比"
	@echo "客户端"
	@echo "  make client       编译真人客户端"
	@echo "  make demo         一条命令开玩（1v1 + 陪练 bot）"

build:
	@mkdir -p $(BIN)
	$(GO) build -o $(BIN)/sutd$(EXE) ./cmd/sutd
	$(GO) build -o $(BIN)/runner$(EXE) ./cmd/runner
	$(GO) build -o $(BIN)/controller$(EXE) ./cmd/controller
	$(GO) build -o $(BIN)/client$(EXE) ./cmd/client
	@echo "构建完成 -> $(BIN)/"

client:
	@mkdir -p $(BIN)
	$(GO) build -o $(BIN)/client$(EXE) ./cmd/client
	@echo "客户端已编译。先起 SUT，再：$(BIN)/client -account me"

# 一条命令开玩：起被测系统 + 陪练 bot + 交互客户端。
demo:
	powershell -NoProfile -ExecutionPolicy Bypass -File scripts/demo.ps1

fmt:
	gofmt -w ./internal ./srv ./cmd ./analysis

vet:
	$(GO) vet ./...

test:
	$(GO) test ./... -count=1 -timeout 300s

# -race 需要 CGO。Windows 上没有可用的 C 工具链时会失败，这不是代码问题。
test-race:
	$(GO) test ./internal/wire ./internal/protocol ./internal/sched ./internal/behavior \
		./internal/clock ./internal/hist ./internal/metrics ./srv/room -race -count=1 -timeout 300s

cover:
	$(GO) test ./... -count=1 -coverprofile=coverage.out -timeout 300s
	$(GO) tool cover -func=coverage.out | tail -n 20

# 冒烟：前台起 SUT、跑完 runner、收尾。
smoke: build
	@echo "== 冒烟：30 bot / 一局 10s =="
	@$(BIN)/sutd$(EXE) -config deploy/sut.smoke.yaml & SUT_PID=$$!; \
	sleep 1; \
	$(BIN)/runner$(EXE) -plan plans/smoke.yaml -metrics :9091 -timeout 180s; \
	kill $$SUT_PID 2>/dev/null || true

baseline: build
	@echo "== 基线：带三个刻意瓶颈 =="
	@$(BIN)/sutd$(EXE) -config deploy/sut.yaml & SUT_PID=$$!; \
	sleep 1; \
	$(BIN)/runner$(EXE) -plan plans/baseline.yaml -metrics :9091 -timeout 600s; \
	kill $$SUT_PID 2>/dev/null || true
	@$(PY) analysis/analyze.py $$(ls -dt results/*/ | head -n 1)

fixed: build
	@echo "== 修复版 SUT + 同一份基线计划 =="
	@$(BIN)/sutd$(EXE) -config deploy/sut.fixed.yaml & SUT_PID=$$!; \
	sleep 1; \
	$(BIN)/runner$(EXE) -plan plans/baseline.yaml -metrics :9091 -timeout 600s; \
	kill $$SUT_PID 2>/dev/null || true
	@$(PY) analysis/analyze.py $$(ls -dt results/*/ | head -n 1)
	@echo "对比两轮结果：python analysis/analyze.py results/<修复前> results/<修复后>"

clean:
	rm -rf $(BIN) results coverage.out
