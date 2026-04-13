FROM golang:1.24 AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o water-tank-reader

FROM debian:stable-slim

WORKDIR /app

COPY --from=builder /app/water-tank-reader .
COPY .env .env

CMD ["./water-tank-reader"]