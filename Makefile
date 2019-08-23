UPX := $(shell command -v upx 2> /dev/null)
build/k8ts: k8ts.go
	go build -ldflags="-s -w" -o $@
ifdef UPX
	upx --best $@
endif
clean :
	rm -f build/k8ts
