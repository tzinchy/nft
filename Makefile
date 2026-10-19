css:
	tailwindcss -i input.css -o static/app.css --minify

build: css
	go build -o nfc .

# бинарник для VPS (Linux x86_64), cgo не нужен
linux: css
	GOOS=linux GOARCH=amd64 go build -o nfc-linux .

dev: css
	go run .

test:
	go test ./...
