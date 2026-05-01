FROM golang:1.25-alpine AS builder

WORKDIR /app

COPY go.mod ./
# COPY go.sum ./ 
# In a real scenario, we would copy go.sum too, but I haven't generated it.
# Running go mod tidy inside the container will generate it.

COPY . .

RUN go env -w GOPROXY=https://goproxy.cn,direct
RUN go mod tidy
RUN go build -o server main.go

FROM alpine:3.22

WORKDIR /app

RUN apk add --no-cache tzdata

COPY --from=builder /app/server .

EXPOSE 8080

CMD ["./server"]
