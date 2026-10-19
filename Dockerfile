# --- сборка: CSS (Tailwind standalone) + Go-бинарник без cgo ---
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH
ARG TAILWIND=v4.3.3
RUN apk add --no-cache libstdc++ libgcc && arch=$(uname -m | sed 's/x86_64/x64/;s/aarch64/arm64/') \
 && wget -qO /usr/local/bin/tailwindcss https://github.com/tailwindlabs/tailwindcss/releases/download/$TAILWIND/tailwindcss-linux-$arch-musl \
 && chmod +x /usr/local/bin/tailwindcss
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN tailwindcss -i input.css -o static/app.css --minify \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /nfc . \
 && mkdir /data

# --- запуск: минимальный образ без shell, не root ---
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /nfc /nfc
COPY --from=build --chown=nonroot:nonroot /data /data
ENV ADDR=:8080 DB_PATH=/data/nfc.db
EXPOSE 8080
ENTRYPOINT ["/nfc"]
