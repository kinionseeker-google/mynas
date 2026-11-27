# 构建：docker build -t mynas .
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY web ./web
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /mynas .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /mynas /usr/local/bin/mynas
ENV MYNAS_ADDR=:8580 MYNAS_DATA=/data MYNAS_PROXY_PORTS=18600-18649
VOLUME /data
EXPOSE 8580 18600-18649
ENTRYPOINT ["mynas"]
