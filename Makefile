PROJ = yingwu
EXT =
ifeq ($(OS),Windows_NT)
    GODS = windows
	EXT = .exe
else
    UNAME_S := $(shell uname -s)
    ifeq ($(UNAME_S),Linux)
        GODS = linux
    endif
    ifeq ($(UNAME_S),Darwin)
        GODS = darwin
    endif
endif

all: build

build:
	CGO_ENABLED=0 GOOS=$(GODS) go build -a -ldflags "-s -w" -installsuffix cgo -o $(PROJ)$(EXT)

clean:
	rm -f $(PROJ)$(EXT)
