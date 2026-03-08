.PHONY: build clean

build:
	docker compose run --rm build

clean:
	rm -rf bin/ webterm.exe
