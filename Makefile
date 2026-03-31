#
ARCH=$(shell uname -s)
SERVERS:=$(shell for d in app/*/; do [ -n "$$(find $$d -maxdepth 1 -name '*.go' 2>/dev/null | head -1)" ] && echo $${d%/} | sed 's|app/||'; done)

WORK_DIR = $(shell pwd)

PB_VERSION = 31.1
PB_FILES = $(shell find . -name '*.proto')
PB_DIRS = $(sort $(dir $(PB_FILES)))
PB_GO_FILES = $(shell find . -name '*.pb.go')
PB_DIR_TGTS = $(addprefix _PB, $(PB_DIRS))

.PHONY: servers
servers: $(SERVERS)

.PHONY: $(SERVERS)
$(SERVERS):
	@echo -n "Now build $@ ... "
	@mkdir -p bin && touch bin/.gitignore
	@go mod tidy && go build -ldflags "-X google.golang.org/protobuf/reflect/protoregistry.conflictPolicy=warn" ./app/$@/
	@mv $@ bin && echo Done
# Reference: [解决《panic: proto: file “xxx.proto“ is already registered》问题](https://blog.csdn.net/zhaolinfenggg/article/details/135776526)

.PHONY: all
all: pb wire gogenerate $(SERVERS)
	go mod tidy

.PHONY: fmt
fmt:
	find . -name "*.go" | xargs goimports -e -d -local git.woa.com -w && \
    find . -name "*.go" | xargs gofmt -e -d -s -w

.PHONY: test
test:
	go test -v ./... -gcflags "all=-N -l"

.PHONY: cover
cover:
	go test -v ./... -gcflags "all=-N -l" -coverprofile=tmp_coverage.out
	go tool cover -html=tmp_coverage.out
	if [ -f "tmp_coverage.out" ]; then rm tmp_coverage.out; fi

.PHONY: convey
convey:
	sudo goconvey

.PHONY: check
check:	build test fmt lint

.PHONY: install
install: installpb installtrpc installmock
	@echo
	@mockgen -version | xargs echo mockgen version:
	@protoc --version | xargs echo "Protobuf version:"
	@trpc version

# ======== protobuf 文件编译支持 ========

# pb 编译规则
.PHONY: pb
pb: $(PB_DIR_TGTS)

# 寻找包含 .proto 的目录并编译
#
# 这个脚本有两个循环, 其中第一个循环是调用 trpc 编译, 这个暂不必说
#
# 第二个循环是调用 protoc-gen-go-setters, 可能由于我在 proto 文件中声明的 go_package
# 字段, 导致这个工具会将 setter 文件写入到很长的一串子目录中 (你可以尝试将第二个 find
# 之后的两行删掉看效果), 因此只好加了一个 find 和 mv 的操作, 把文件拉回来
.PHONY: $(PB_DIR_TGTS)
$(PB_DIR_TGTS):
	@for dir in $(subst _PB,, $@); do \
		echo Now Build proto in directory: $$dir; \
		cd $$dir; rm -rf mock; \
		export PATH=$(PATH); \
		rm -f *.pb.go; rm -f *.trpc.go; rm -f *.setters.go; \
		find . -name '*.proto' | xargs -I DD \
			trpc create -f --protofile=DD --protocol=trpc --rpconly --nogomod --alias --mock=false --protodir=$(WORK_DIR)/protocol; \
		ls *.trpc.go 2>/dev/null | xargs -I DD mockgen -source=DD -destination=mock/DD -package=mock ; \
		find `pwd` -name '*.pb.go'; \
		find `pwd` -name '*.pb.go' | xargs -I XXXX sed -i 's/err_code,omitempty/err_code/g' XXXX; \
		find `pwd` -name '*.pb.go' | xargs -I XXXX sed -i 's/code,omitempty/code/g' XXXX; \
	done
	@for dir in $(subst _PB,, $@); do \
		echo Now Build proto setter in directory: $$dir; \
		find $$dir -name '*.proto' | xargs -I DD \
			protoc --go-setters_out=$$dir --proto_path=protocol DD; \
		find $$dir/github.com -name '*.pb.setters.go' | xargs -I DD mv DD $$dir; \
		rm -rf $$dir/github.com; \
	done

_PROTOC_PKG_URL=https://github.com/protocolbuffers/protobuf/releases/download/v$(PB_VERSION)/protoc-$(PB_VERSION)-linux-x86_64.zip

# installpb 用于在设备上安装 protobuf 编译器, 仅适用于 Linux 环境。
# 如果环境 OK 那么不用执行
.PHONY: installpb
installpb:
	wget $(_PROTOC_PKG_URL)
	7z x $(notdir $(_PROTOC_PKG_URL)) -o/usr/local -y
	rm -f $(notdir $(_PROTOC_PKG_URL))*
	chmod +x /usr/local/bin/protoc
	which protoc | xargs -I DD cp /usr/local/bin/protoc DD || echo Installed protoc
	go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	go install github.com/lcmaguire/protoc-gen-go-setters@latest
	@echo ---- $@ done ----
	@protoc --version | xargs echo "Protobuf version:"

.PHONY: installtrpc
installtrpc:
	go install trpc.group/trpc-go/trpc-cmdline/trpc@latest
	@echo ---- $@ done ----
	@trpc version | xargs echo "TRPC version:"

# ======== 自动 go generate 支持 ========

GO_GENERATE_DIRS=
ifeq ($(ARCH), Darwin)
	GO_GENERATE_DIRS=$(sort $(dir $(shell grep -lr --include='*.go' '//go:generate ' .)))
else ifeq ($(ARCH), Linux)
	GO_GENERATE_DIRS=$(sort $(dir $(shell grep -lr --include='*.go' '//go:generate ')))
else
	$(error 不支持的系统: $(ARCH))
endif

.PHONY: gogenerate
gogenerate: installmock $(GO_GENERATE_DIRS)
	@go mod tidy

.PHONY: installmock
installmock:
	go install go.uber.org/mock/mockgen@latest
	@echo ---- $@ done ----
	@mockgen -version | xargs echo "mockgen version:"

.PHONY: $(GO_GENERATE_DIRS)
$(GO_GENERATE_DIRS):
	@for dir in $@; do \
		echo ==== go generate $$dir ====; \
		cd $$dir; \
		go generate; \
	done

# ======== Google wire 支持 ========

WIRE_DIRS=
ifeq ($(ARCH), Darwin)
	WIRE_DIRS=$(sort $(dir $(shell grep -lr --include='*.go' '//go:build wireinject' .)))
else ifeq ($(ARCH), Linux)
	WIRE_DIRS=$(sort $(dir $(shell grep -lr --include='*.go' '//go:build wireinject')))
else
	$(error 不支持的系统: $(ARCH))
endif

.PHONY: wire
wire:
	@echo Start: `date`
	@echo wire targets: $(WIRE_DIRS)
	@for dir in $(WIRE_DIRS); do \
		echo ===== wire gen $$dir =======; \
		cd $(WORK_DIR)/$$dir; \
		wire gen ./; \
		for gofile in $$(find . -name "*.go" -type f); do \
			grep -v '^//go:generate go run -mod=mod github.com/google/wire/cmd/wire$$' $$gofile > $$gofile.tmp && mv $$gofile.tmp $$gofile; \
		done; \
		echo "Cleaned up go:generate wire comments in $$dir"; \
	done
	@echo Done: `date`

.PHONY: installwire
installwire:
	go install github.com/google/wire/cmd/wire@latest
	@which wire | xargs echo "wire installed in: "

.PHONY: debug
debug:
	@echo $(WIRE_MAX_CPU_NO)
	@echo $(PATH)
	@echo $(PB_FILES)

# ======== lint 支持 ========

.PHONY: lint
lint:
	./lint_blame.sh
