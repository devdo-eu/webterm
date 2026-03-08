.PHONY: build clean

build:
	-@taskkill /F /IM webterm.exe 2>NUL
	docker compose run --rm build

clean:
	rm -rf bin/ webterm.exe
