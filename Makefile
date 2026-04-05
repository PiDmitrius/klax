.PHONY: build install clean

build:
	go build -o klax ./cmd/klax

install:
	go install ./cmd/klax

clean:
	rm -f klax
