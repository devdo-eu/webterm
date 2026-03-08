.PHONY: build test clean

build:
	-@taskkill /F /IM webterm.exe 2>NUL
	docker compose run --rm build

test:
	docker compose run --rm test
	bin/webterm_test.exe -test.v

clean:
	rm -rf bin/ webterm.exe
